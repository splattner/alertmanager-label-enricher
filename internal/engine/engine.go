// Package engine evaluates configured rules against an alert's labels,
// applying set/drop actions in declared order.
package engine

import (
	"context"
	"fmt"
	"regexp"

	"github.com/splattner/alertmanager-label-enricher/internal/alert"
	"github.com/splattner/alertmanager-label-enricher/internal/config"
	"github.com/splattner/alertmanager-label-enricher/internal/extract"
	"github.com/splattner/alertmanager-label-enricher/internal/source"
	"github.com/splattner/alertmanager-label-enricher/internal/tmpl"
)

// Sources looks up a named lookup source. Implemented by source.Registry.
type Sources interface {
	Get(name string) (source.Source, bool)
}

// Result records what happened evaluating one rule against one alert, for
// metrics and dry-run logging.
type Result struct {
	Rule        string
	Skipped     bool // matchers did not match
	DryRun      bool
	Added       []string
	Overwritten []string
	Dropped     []string
}

// RequiredFailure is returned when a `required: true` rule could not be
// satisfied; the caller (proxy handler) must fail the whole batch closed.
type RequiredFailure struct {
	Rule   string
	Label  string
	Reason error
}

func (e *RequiredFailure) Error() string {
	return fmt.Sprintf("rule %q: required label %q could not be set: %v", e.Rule, e.Label, e.Reason)
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
				re, err := regexp.Compile("^(?:" + m.Value + ")$")
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
	annotations := a.Annotations()

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
			if _, exists := labels[action.Drop.Label]; exists {
				if !rule.spec.DryRun {
					delete(labels, action.Drop.Label)
				}
				res.Dropped = append(res.Dropped, action.Drop.Label)
			}
		}

		for _, set := range rule.sets {
			value, ok, err := e.resolveValue(ctx, set, labels, annotations)
			if err != nil || !ok {
				if rule.spec.Required {
					return results, &RequiredFailure{Rule: rule.spec.Name, Label: set.spec.Label, Reason: err}
				}
				continue
			}

			_, exists := labels[set.spec.Label]
			if exists && !set.spec.Overwrite {
				continue
			}

			if !rule.spec.DryRun {
				labels[set.spec.Label] = value
			}
			if exists {
				res.Overwritten = append(res.Overwritten, set.spec.Label)
			} else {
				res.Added = append(res.Added, set.spec.Label)
			}
		}

		results = append(results, res)
	}
	return results, nil
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
		v, err := tmpl.Render(spec.Label, spec.Template, tmpl.Data{Labels: labels, Annotations: annotations})
		if err != nil {
			return "", false, err
		}
		return v, true, nil

	case spec.From != nil:
		src, ok := e.sources.Get(spec.From.Source)
		if !ok {
			return "", false, fmt.Errorf("source %q is not registered", spec.From.Source)
		}
		result, err := src.Lookup(ctx, source.LookupInput{Labels: labels, Annotations: annotations})
		if err != nil {
			if spec.Default != "" {
				return spec.Default, true, nil
			}
			return "", false, err
		}

		value, found, err := set.query.Run(result, labels, annotations)
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
		return "", false, fmt.Errorf("set action for label %q has no value source", spec.Label)
	}
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
