// Package engine evaluates configured rules against an alert's labels,
// applying set/drop actions in declared order.
package engine

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/splattner/alertmanager-label-enricher/internal/alert"
	"github.com/splattner/alertmanager-label-enricher/internal/config"
	"github.com/splattner/alertmanager-label-enricher/internal/extract"
	"github.com/splattner/alertmanager-label-enricher/internal/metrics"
	"github.com/splattner/alertmanager-label-enricher/internal/source"
	"github.com/splattner/alertmanager-label-enricher/internal/tmpl"
)

// Sources looks up a named lookup source. Implemented by source.Registry.
type Sources interface {
	Get(name string) (source.Source, bool)
}

// Result records what happened evaluating one rule against one alert, for
// metrics and dry-run logging. The Annotations* slices mirror their label
// counterparts but carry no fingerprint-blast-radius meaning: overwriting
// or dropping an annotation never changes how Alertmanager identifies the
// alert.
type Result struct {
	Rule                   string
	Skipped                bool // matchers did not match
	RequiredFailed         bool // a required action in this rule could not be satisfied
	DryRun                 bool
	Added                  []string
	Overwritten            []string
	Dropped                []string
	AnnotationsAdded       []string
	AnnotationsOverwritten []string
	AnnotationsDropped     []string
}

// RequiredFailure is returned when a `required: true` rule could not be
// satisfied; the caller (proxy handler) must fail the whole batch closed.
type RequiredFailure struct {
	Rule   string
	Kind   string // "label" or "annotation"
	Target string
	Reason error
}

func (e *RequiredFailure) Error() string {
	return fmt.Sprintf("rule %q: required %s %q could not be set: %v", e.Rule, e.Kind, e.Target, e.Reason)
}
func (e *RequiredFailure) Unwrap() error { return e.Reason }

type compiledMatch struct {
	label string
	op    config.MatchOp
	value string
	regex *regexp.Regexp
}

type compiledSet struct {
	spec  config.SetAction
	query *extract.Query // nil unless spec.From != nil
}

type compiledRule struct {
	spec    config.RuleConfig
	matches []compiledMatch
	sets    []compiledSet
}

// Engine holds the compiled rules ready to evaluate against alerts.
type Engine struct {
	rules   []compiledRule
	sources Sources
}

// Compile builds an Engine from validated config. cfg must already have
// passed config.Validate.
func Compile(cfg *config.Config, sources Sources, queries *extract.Cache) (*Engine, error) {
	eng := &Engine{sources: sources}
	for _, r := range cfg.Rules {
		cr := compiledRule{spec: r}

		for _, m := range r.Match {
			cm := compiledMatch{label: m.Label, op: m.Op, value: m.Value}
			if m.Op == config.OpRegex || m.Op == config.OpNotRegex {
				re, err := config.MatchRegex(m.Value)
				if err != nil {
					return nil, fmt.Errorf("rule %q: %w", r.Name, err)
				}
				cm.regex = re
			}
			cr.matches = append(cr.matches, cm)
		}

		for _, a := range r.Actions {
			if a.Set == nil {
				continue
			}
			cs := compiledSet{spec: *a.Set}
			if a.Set.From != nil {
				q, err := queries.Compile(a.Set.From.Jq, a.Set.From.Regex)
				if err != nil {
					return nil, fmt.Errorf("rule %q: %w", r.Name, err)
				}
				cs.query = q
			}
			cr.sets = append(cr.sets, cs)
		}

		eng.rules = append(eng.rules, cr)
	}
	return eng, nil
}

// Apply runs every rule against a in declared order, mutating a's labels in
// place. It returns per-rule results for metrics/logging. If any
// `required: true` rule fails, it returns immediately with a
// *RequiredFailure — the caller must not forward the batch.
func (e *Engine) Apply(ctx context.Context, a alert.Alert) ([]Result, error) {
	labels, err := a.Labels()
	if err != nil {
		return nil, fmt.Errorf("read labels: %w", err)
	}
	annotations, err := a.Annotations()
	if err != nil {
		return nil, fmt.Errorf("read annotations: %w", err)
	}

	var results []Result
	for _, rule := range e.rules {
		if !matches(rule.matches, labels) {
			results = append(results, Result{Rule: rule.spec.Name, Skipped: true})
			continue
		}

		res := Result{Rule: rule.spec.Name, DryRun: rule.spec.DryRun}

		for _, action := range rule.spec.Actions {
			if action.Drop == nil {
				continue
			}
			m, name, isAnnotation := target(labels, annotations, action.Drop.Label, action.Drop.Annotation)
			if _, exists := m[name]; exists {
				if !rule.spec.DryRun {
					delete(m, name)
				}
				if isAnnotation {
					res.AnnotationsDropped = append(res.AnnotationsDropped, name)
				} else {
					res.Dropped = append(res.Dropped, name)
				}
			}
		}

		for _, set := range rule.sets {
			value, ok, err := e.resolveValue(ctx, set, labels, annotations)
			if err != nil || !ok {
				if rule.spec.Required {
					res.RequiredFailed = true
					results = append(results, res)
					kind, targetName := "label", set.spec.Label
					if set.spec.Annotation != "" {
						kind, targetName = "annotation", set.spec.Annotation
					}
					return results, &RequiredFailure{Rule: rule.spec.Name, Kind: kind, Target: targetName, Reason: err}
				}
				continue
			}

			m, name, isAnnotation := target(labels, annotations, set.spec.Label, set.spec.Annotation)
			_, exists := m[name]
			if exists && !set.spec.Overwrite {
				continue
			}

			if !rule.spec.DryRun {
				m[name] = value
			}
			switch {
			case isAnnotation && exists:
				res.AnnotationsOverwritten = append(res.AnnotationsOverwritten, name)
			case isAnnotation:
				res.AnnotationsAdded = append(res.AnnotationsAdded, name)
			case exists:
				res.Overwritten = append(res.Overwritten, name)
			default:
				res.Added = append(res.Added, name)
			}
		}

		results = append(results, res)
	}
	return results, nil
}

// target resolves which map (labels or annotations) a set/drop action
// addresses, and the action's name within that map. config.Validate
// enforces that exactly one of label/annotation is set, so annotation
// being non-empty alone decides it.
func target(labels, annotations map[string]string, label, annotation string) (m map[string]string, name string, isAnnotation bool) {
	if annotation != "" {
		return annotations, annotation, true
	}
	return labels, label, false
}

// resolveValue computes a set action's value. ok=false means "no value
// available"; for a from-source action with no `default`, that propagates
// as the rule's required-failure trigger. err is only set on lookup/eval
// errors, never on a clean "not found".
func (e *Engine) resolveValue(ctx context.Context, set compiledSet, labels, annotations map[string]string) (string, bool, error) {
	spec := set.spec
	switch {
	case spec.Value != "":
		return spec.Value, true, nil

	case spec.Template != "":
		v, err := tmpl.Render(setName(spec), spec.Template, tmpl.Data{Labels: labels, Annotations: annotations})
		if err != nil {
			return "", false, err
		}
		return v, true, nil

	case spec.From != nil:
		src, ok := e.sources.Get(spec.From.Source)
		if !ok {
			return "", false, fmt.Errorf("source %q is not registered", spec.From.Source)
		}
		start := time.Now()
		result, err := src.Lookup(ctx, source.LookupInput{Labels: labels, Annotations: annotations})
		metrics.SourceLookupDuration.WithLabelValues(spec.From.Source).Observe(time.Since(start).Seconds())
		switch {
		case err != nil:
			metrics.SourceLookupsTotal.WithLabelValues(spec.From.Source, "error").Inc()
		case result == nil:
			metrics.SourceLookupsTotal.WithLabelValues(spec.From.Source, "miss").Inc()
		default:
			metrics.SourceLookupsTotal.WithLabelValues(spec.From.Source, "hit").Inc()
		}
		if err != nil {
			if spec.Default != "" {
				return spec.Default, true, nil
			}
			return "", false, err
		}

		value, found, err := set.query.Run(ctx, result, labels, annotations)
		if err != nil {
			if spec.Default != "" {
				return spec.Default, true, nil
			}
			return "", false, err
		}
		if !found {
			if spec.Default != "" {
				return spec.Default, true, nil
			}
			return "", false, nil
		}
		return value, true, nil

	default:
		return "", false, fmt.Errorf("set action for %q has no value source", setName(spec))
	}
}

// setName returns whichever of Label/Annotation a SetAction targets, for
// error messages and template naming.
func setName(spec config.SetAction) string {
	if spec.Annotation != "" {
		return spec.Annotation
	}
	return spec.Label
}

func matches(ms []compiledMatch, labels map[string]string) bool {
	for _, m := range ms {
		v, exists := labels[m.label]
		switch m.op {
		case config.OpExists:
			if !exists {
				return false
			}
		case config.OpAbsent:
			if exists {
				return false
			}
		case config.OpEq:
			if !exists || v != m.value {
				return false
			}
		case config.OpNe:
			if exists && v == m.value {
				return false
			}
		case config.OpRegex:
			if !exists || !m.regex.MatchString(v) {
				return false
			}
		case config.OpNotRegex:
			if exists && m.regex.MatchString(v) {
				return false
			}
		}
	}
	return true
}
