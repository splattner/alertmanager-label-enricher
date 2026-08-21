package config

import (
	"fmt"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/splattner/alertmanager-label-enricher/internal/extract"
	"github.com/splattner/alertmanager-label-enricher/internal/tmpl"
)

// Validate checks a Config for internal consistency: required fields,
// duplicate names, dangling source references, and compilable jq/regex
// expressions. Call it after defaults have been applied.
func Validate(cfg *Config) error {
	if len(cfg.Targets) == 0 {
		return fmt.Errorf("targets: at least one target is required")
	}
	for i, t := range cfg.Targets {
		if t.URL == "" {
			return fmt.Errorf("targets[%d]: url is required", i)
		}
	}
	if cfg.Forward.MinSuccess > len(cfg.Targets) {
		return fmt.Errorf("forward.minSuccess (%d) exceeds the number of targets (%d)", cfg.Forward.MinSuccess, len(cfg.Targets))
	}
	if cfg.Forward.Retries < 0 {
		return fmt.Errorf("forward.retries (%d) must not be negative", cfg.Forward.Retries)
	}
	if cfg.Enrichment.MaxConcurrency < 0 {
		return fmt.Errorf("enrichment.maxConcurrency (%d) must not be negative", cfg.Enrichment.MaxConcurrency)
	}
	if cfg.CRD.MaxRules < 0 {
		return fmt.Errorf("crd.maxRules (%d) must not be negative", cfg.CRD.MaxRules)
	}

	if cfg.Server.TLS != nil {
		if cfg.Server.TLS.CertFile == "" || cfg.Server.TLS.KeyFile == "" {
			return fmt.Errorf("server.tls: certFile and keyFile are both required")
		}
	}
	if cfg.Forward.TLS != nil {
		t := cfg.Forward.TLS
		if (t.CertFile == "") != (t.KeyFile == "") {
			return fmt.Errorf("forward.tls: certFile and keyFile must both be set, or both empty")
		}
	}

	sourceNames := make(map[string]bool, len(cfg.Sources))
	for i, s := range cfg.Sources {
		if s.Name == "" {
			return fmt.Errorf("sources[%d]: name is required", i)
		}
		if sourceNames[s.Name] {
			return fmt.Errorf("sources[%d]: duplicate source name %q", i, s.Name)
		}
		sourceNames[s.Name] = true

		switch s.Type {
		case "kubernetes":
			if s.Kubernetes == nil {
				return fmt.Errorf("sources[%q]: type kubernetes requires a kubernetes block", s.Name)
			}
			if s.Kubernetes.Resource == "" {
				return fmt.Errorf("sources[%q]: kubernetes.resource is required", s.Name)
			}
			if s.Kubernetes.Name == "" {
				return fmt.Errorf("sources[%q]: kubernetes.name is required", s.Name)
			}
		case "http":
			if s.HTTP == nil {
				return fmt.Errorf("sources[%q]: type http requires an http block", s.Name)
			}
			if s.HTTP.URL == "" {
				return fmt.Errorf("sources[%q]: http.url is required", s.Name)
			}
			if len(s.HTTP.AllowedHosts) == 0 {
				return fmt.Errorf("sources[%q]: http.allowedHosts must list at least one host", s.Name)
			}
		case "file":
			if s.File == nil {
				return fmt.Errorf("sources[%q]: type file requires a file block", s.Name)
			}
			if s.File.Path == "" {
				return fmt.Errorf("sources[%q]: file.path is required", s.Name)
			}
		default:
			return fmt.Errorf("sources[%q]: unknown type %q (want kubernetes, http or file)", s.Name, s.Type)
		}
	}

	ruleNames := make(map[string]bool, len(cfg.Rules))
	for i, r := range cfg.Rules {
		if r.Name == "" {
			return fmt.Errorf("rules[%d]: name is required", i)
		}
		if ruleNames[r.Name] {
			return fmt.Errorf("rules[%q]: duplicate rule name", r.Name)
		}
		// "/" is reserved: rules sourced from an EnrichmentRule CR compile
		// to "<namespace>/<name>", so allowing it here would let a file
		// rule silently share a name - and therefore a metric series - with
		// a tenant's rule. Reserving the separator makes the collision
		// impossible rather than something to detect after the fact.
		if strings.Contains(r.Name, "/") {
			return fmt.Errorf("rules[%q]: name must not contain %q, which is reserved for CR-sourced rules (\"<namespace>/<name>\")", r.Name, "/")
		}
		ruleNames[r.Name] = true

		if err := ValidateRule(r, sourceNames); err != nil {
			return fmt.Errorf("rules[%q]: %w", r.Name, err)
		}
	}

	if err := validateEnforcement(&cfg.Enforcement, sourceNames); err != nil {
		return fmt.Errorf("enforcement: %w", err)
	}

	return nil
}

// ValidateRule checks one rule's match/actions for internal consistency:
// unknown matcher ops, uncompilable regex, at least one action, and (via
// validateAction) exactly-one-of set/drop, exactly-one-of label/
// annotation, undeclared source references, and uncompilable jq/regex.
// r.Name is assumed already set and validated by the caller - this is
// shared by file-config rules (Validate) and CR-sourced rules
// (internal/enforce), which get identical semantic validation.
func ValidateRule(r RuleConfig, sourceNames map[string]bool) error {
	for j, m := range r.Match {
		if err := validateMatch(m); err != nil {
			return fmt.Errorf("match[%d]: %w", j, err)
		}
	}

	if len(r.Actions) == 0 {
		return fmt.Errorf("at least one action is required")
	}
	for j, a := range r.Actions {
		if err := validateAction(a, sourceNames); err != nil {
			return fmt.Errorf("actions[%d]: %w", j, err)
		}
	}
	return nil
}

// MatchRegex compiles a matcher's regex the way the engine will: fully
// anchored, matching Prometheus's own matcher semantics. Exported so
// validation and engine.Compile share one implementation - if they ever
// diverged, a rule could validate cleanly and then fail to compile, which
// for a CR-sourced rule means a tenant's mistake breaking the reload.
func MatchRegex(value string) (*regexp.Regexp, error) {
	return regexp.Compile("^(?:" + value + ")$")
}

func validateMatch(m MatchConfig) error {
	if m.Label == "" {
		return fmt.Errorf("label is required")
	}
	switch m.Op {
	case OpExists, OpAbsent, OpEq, OpNe:
	case OpRegex, OpNotRegex:
		if _, err := MatchRegex(m.Value); err != nil {
			return fmt.Errorf("invalid regex %q: %w", m.Value, err)
		}
	default:
		return fmt.Errorf("unknown op %q", m.Op)
	}
	return nil
}

// validateEnforcement checks the shape of the enforcement policy itself
// (namespace selectors compile, matchers are well-formed, allowedSources
// reference declared sources). It does not evaluate any CR against it -
// that happens per-CR in internal/enforce.
func validateEnforcement(e *EnforcementConfig, sourceNames map[string]bool) error {
	for i, r := range e.Rules {
		if _, err := metav1.LabelSelectorAsSelector(&r.NamespaceSelector); err != nil {
			return fmt.Errorf("rules[%d].namespaceSelector: %w", i, err)
		}
		for j, m := range r.Match {
			if err := validateMatch(m); err != nil {
				return fmt.Errorf("rules[%d].match[%d]: %w", i, j, err)
			}
		}
		for _, s := range r.AllowedSources {
			if !sourceNames[s] {
				return fmt.Errorf("rules[%d].allowedSources: %q is not a declared source", i, s)
			}
		}
		if r.MaxRulesPerNamespace < 0 {
			return fmt.Errorf("rules[%d].maxRulesPerNamespace must not be negative", i)
		}
	}
	return nil
}

func validateAction(a ActionConfig, sourceNames map[string]bool) error {
	switch {
	case a.Set != nil && a.Drop != nil:
		return fmt.Errorf("exactly one of set or drop must be set")
	case a.Set != nil:
		return validateSet(a.Set, sourceNames)
	case a.Drop != nil:
		return validateDrop(a.Drop)
	default:
		return fmt.Errorf("exactly one of set or drop must be set")
	}
}

// validateDrop checks a drop action: exactly one of Label/Annotation is
// required. Force applies only to a reserved label - annotations carry no
// fingerprint risk, so a stray force there is rejected as meaningless
// rather than silently ignored.
func validateDrop(d *DropAction) error {
	switch {
	case d.Label != "" && d.Annotation != "":
		return fmt.Errorf("drop: exactly one of label or annotation is required")
	case d.Label != "":
		if ReservedLabels[d.Label] && !d.Force {
			return fmt.Errorf("drop.label %q is reserved; set force: true to acknowledge dropping it", d.Label)
		}
		return nil
	case d.Annotation != "":
		if d.Force {
			return fmt.Errorf("drop.annotation %q: force is not applicable to annotations (no fingerprint risk)", d.Annotation)
		}
		return nil
	default:
		return fmt.Errorf("drop: exactly one of label or annotation is required")
	}
}

func validateSet(s *SetAction, sourceNames map[string]bool) error {
	if s.Label != "" && s.Annotation != "" {
		return fmt.Errorf("set: exactly one of label or annotation is required")
	}
	if s.Label == "" && s.Annotation == "" {
		return fmt.Errorf("set: exactly one of label or annotation is required")
	}
	kind, name := "label", s.Label
	if s.Annotation != "" {
		kind, name = "annotation", s.Annotation
	}

	modes := 0
	if s.Value != "" {
		modes++
	}
	if s.Template != "" {
		modes++
	}
	if s.Template != "" {
		// Templates were previously unchecked: an unbalanced {{ passed
		// `enricher check` and failed per-alert at runtime instead, which
		// for a required rule means every batch 503s. Compile it here, the
		// same way engine.Compile will.
		if _, err := tmpl.Compile(name, s.Template); err != nil {
			return fmt.Errorf("set.%s %q: %w", kind, name, err)
		}
	}
	if s.From != nil {
		modes++
	}
	if modes != 1 {
		return fmt.Errorf("set.%s %q: exactly one of value, template or from is required", kind, name)
	}
	if s.From != nil {
		if !sourceNames[s.From.Source] {
			return fmt.Errorf("set.%s %q: from.source %q is not a declared source", kind, name, s.From.Source)
		}
		if s.From.Jq == "" {
			return fmt.Errorf("set.%s %q: from.jq is required", kind, name)
		}
		// Compile through the same function the engine's query cache uses,
		// rather than reimplementing it: parsing alone accepts expressions
		// that fail to compile (an undefined function, an unbound
		// variable), and any drift between the two would let a rule
		// validate cleanly and then fail engine.Compile - which for a
		// CR-sourced rule means a tenant's mistake breaking the reload for
		// everyone.
		if _, err := extract.Compile(s.From.Jq, s.From.Regex); err != nil {
			return fmt.Errorf("set.%s %q: %w", kind, name, err)
		}
	}
	if strings.TrimSpace(name) != name {
		return fmt.Errorf("set.%s %q: must not have leading/trailing whitespace", kind, name)
	}

	if kind == "annotation" {
		if s.Force {
			return fmt.Errorf("set.annotation %q: force is not applicable to annotations (no fingerprint risk)", name)
		}
		return nil
	}
	if ReservedLabels[name] && s.Overwrite && !s.Force {
		return fmt.Errorf("set.label %q is reserved; set force: true to acknowledge overwriting it", name)
	}
	return nil
}
