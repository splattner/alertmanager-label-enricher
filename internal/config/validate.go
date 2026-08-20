package config

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/itchyny/gojq"
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
		ruleNames[r.Name] = true

		for j, m := range r.Match {
			if m.Label == "" {
				return fmt.Errorf("rules[%q].match[%d]: label is required", r.Name, j)
			}
			switch m.Op {
			case OpExists, OpAbsent, OpEq, OpNe:
			case OpRegex, OpNotRegex:
				if _, err := regexp.Compile("^(?:" + m.Value + ")$"); err != nil {
					return fmt.Errorf("rules[%q].match[%d]: invalid regex %q: %w", r.Name, j, m.Value, err)
				}
			default:
				return fmt.Errorf("rules[%q].match[%d]: unknown op %q", r.Name, j, m.Op)
			}
		}

		if len(r.Actions) == 0 {
			return fmt.Errorf("rules[%q]: at least one action is required", r.Name)
		}
		for j, a := range r.Actions {
			if err := validateAction(a, sourceNames); err != nil {
				return fmt.Errorf("rules[%q].actions[%d]: %w", r.Name, j, err)
			}
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
		if a.Drop.Label == "" {
			return fmt.Errorf("drop.label is required")
		}
		if ReservedLabels[a.Drop.Label] && !a.Drop.Force {
			return fmt.Errorf("drop.label %q is reserved; set force: true to acknowledge dropping it", a.Drop.Label)
		}
		return nil
	default:
		return fmt.Errorf("exactly one of set or drop must be set")
	}
}

func validateSet(s *SetAction, sourceNames map[string]bool) error {
	if s.Label == "" {
		return fmt.Errorf("set.label is required")
	}
	modes := 0
	if s.Value != "" {
		modes++
	}
	if s.Template != "" {
		modes++
	}
	if s.From != nil {
		modes++
	}
	if modes != 1 {
		return fmt.Errorf("set.label %q: exactly one of value, template or from is required", s.Label)
	}
	if s.From != nil {
		if !sourceNames[s.From.Source] {
			return fmt.Errorf("set.label %q: from.source %q is not a declared source", s.Label, s.From.Source)
		}
		if s.From.Jq == "" {
			return fmt.Errorf("set.label %q: from.jq is required", s.Label)
		}
		if _, err := gojq.Parse(s.From.Jq); err != nil {
			return fmt.Errorf("set.label %q: invalid jq %q: %w", s.Label, s.From.Jq, err)
		}
		if s.From.Regex != "" {
			if _, err := regexp.Compile(s.From.Regex); err != nil {
				return fmt.Errorf("set.label %q: invalid regex %q: %w", s.Label, s.From.Regex, err)
			}
		}
	}
	if strings.TrimSpace(s.Label) != s.Label {
		return fmt.Errorf("set.label %q: must not have leading/trailing whitespace", s.Label)
	}
	if ReservedLabels[s.Label] && s.Overwrite && !s.Force {
		return fmt.Errorf("set.label %q is reserved; set force: true to acknowledge overwriting it", s.Label)
	}
	return nil
}
