package config

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func validConfig() *Config {
	return &Config{
		Targets: []TargetConfig{{URL: "http://alertmanager:9093"}},
		Sources: []SourceConfig{
			{Name: "ns", Type: "kubernetes", Kubernetes: &KubernetesSourceSpec{Resource: "namespaces", Name: "{{ .Labels.namespace }}"}},
		},
		Rules: []RuleConfig{
			{
				Name: "team",
				Actions: []ActionConfig{{Set: &SetAction{
					Label: "team",
					From:  &FromConfig{Source: "ns", Jq: `.metadata.labels["team"]`},
				}}},
			},
		},
	}
}

func TestValidateAcceptsValidConfig(t *testing.T) {
	if err := Validate(validConfig()); err != nil {
		t.Fatalf("expected valid config to pass, got: %v", err)
	}
}

func TestValidateRejectsNoTargets(t *testing.T) {
	cfg := validConfig()
	cfg.Targets = nil
	assertRejects(t, cfg, "targets")
}

func TestValidateRejectsMinSuccessAboveTargetCount(t *testing.T) {
	cfg := validConfig()
	cfg.Forward.MinSuccess = 2
	assertRejects(t, cfg, "minSuccess")
}

func TestValidateRejectsNegativeRetries(t *testing.T) {
	cfg := validConfig()
	cfg.Forward.Retries = -1
	assertRejects(t, cfg, "retries")
}

func TestValidateRejectsNegativeMaxConcurrency(t *testing.T) {
	cfg := validConfig()
	cfg.Enrichment.MaxConcurrency = -1
	assertRejects(t, cfg, "maxConcurrency")
}

func TestValidateRejectsUnknownSourceType(t *testing.T) {
	cfg := validConfig()
	cfg.Sources[0].Type = "carrier-pigeon"
	assertRejects(t, cfg, "unknown type")
}

func TestValidateRejectsHTTPSourceWithoutAllowedHosts(t *testing.T) {
	cfg := validConfig()
	cfg.Sources = []SourceConfig{{Name: "cmdb", Type: "http", HTTP: &HTTPSourceSpec{URL: "http://cmdb"}}}
	cfg.Rules[0].Actions[0].Set.From.Source = "cmdb"
	assertRejects(t, cfg, "allowedHosts")
}

func TestValidateRejectsUndeclaredSourceReference(t *testing.T) {
	cfg := validConfig()
	cfg.Rules[0].Actions[0].Set.From.Source = "does-not-exist"
	assertRejects(t, cfg, "not a declared source")
}

func TestValidateRejectsInvalidJq(t *testing.T) {
	cfg := validConfig()
	cfg.Rules[0].Actions[0].Set.From.Jq = "not valid jq {{{"
	assertRejects(t, cfg, "invalid jq")
}

func TestValidateRejectsInvalidRegexMatcher(t *testing.T) {
	cfg := validConfig()
	cfg.Rules[0].Match = []MatchConfig{{Label: "cluster", Op: OpRegex, Value: "prod-("}}
	assertRejects(t, cfg, "invalid regex")
}

func TestValidateRejectsActionWithBothSetAndDrop(t *testing.T) {
	cfg := validConfig()
	cfg.Rules[0].Actions[0].Drop = &DropAction{Label: "pod"}
	assertRejects(t, cfg, "exactly one of set or drop")
}

func TestValidateRejectsSetWithMultipleValueSources(t *testing.T) {
	cfg := validConfig()
	cfg.Rules[0].Actions[0].Set.Value = "static"
	assertRejects(t, cfg, "exactly one of value, template or from")
}

func TestValidateRejectsDuplicateRuleNames(t *testing.T) {
	cfg := validConfig()
	cfg.Rules = append(cfg.Rules, cfg.Rules[0])
	assertRejects(t, cfg, "duplicate rule name")
}

func TestValidateRejectsDuplicateSourceNames(t *testing.T) {
	cfg := validConfig()
	cfg.Sources = append(cfg.Sources, cfg.Sources[0])
	assertRejects(t, cfg, "duplicate source name")
}

func TestValidateAcceptsServerTLSWithCertAndKey(t *testing.T) {
	cfg := validConfig()
	cfg.Server.TLS = &ServerTLSSpec{CertFile: "cert.pem", KeyFile: "key.pem"}
	if err := Validate(cfg); err != nil {
		t.Fatalf("expected valid config to pass, got: %v", err)
	}
}

func TestValidateRejectsServerTLSMissingKey(t *testing.T) {
	cfg := validConfig()
	cfg.Server.TLS = &ServerTLSSpec{CertFile: "cert.pem"}
	assertRejects(t, cfg, "server.tls")
}

func TestValidateRejectsServerTLSMissingCert(t *testing.T) {
	cfg := validConfig()
	cfg.Server.TLS = &ServerTLSSpec{KeyFile: "key.pem"}
	assertRejects(t, cfg, "server.tls")
}

func TestValidateRejectsDropOfReservedLabelWithoutForce(t *testing.T) {
	cfg := validConfig()
	cfg.Rules[0].Actions = []ActionConfig{{Drop: &DropAction{Label: "alertname"}}}
	assertRejects(t, cfg, "reserved")
}

func TestValidateAcceptsDropOfReservedLabelWithForce(t *testing.T) {
	cfg := validConfig()
	cfg.Rules[0].Actions = []ActionConfig{{Drop: &DropAction{Label: "alertname", Force: true}}}
	if err := Validate(cfg); err != nil {
		t.Fatalf("expected valid config to pass, got: %v", err)
	}
}

func TestValidateRejectsOverwriteOfReservedLabelWithoutForce(t *testing.T) {
	cfg := validConfig()
	cfg.Rules[0].Actions[0].Set.Label = "alertname"
	cfg.Rules[0].Actions[0].Set.Overwrite = true
	assertRejects(t, cfg, "reserved")
}

func TestValidateAcceptsOverwriteOfReservedLabelWithForce(t *testing.T) {
	cfg := validConfig()
	cfg.Rules[0].Actions[0].Set.Label = "alertname"
	cfg.Rules[0].Actions[0].Set.Overwrite = true
	cfg.Rules[0].Actions[0].Set.Force = true
	if err := Validate(cfg); err != nil {
		t.Fatalf("expected valid config to pass, got: %v", err)
	}
}

func TestValidateAcceptsAddingReservedLabelWithoutForce(t *testing.T) {
	cfg := validConfig()
	cfg.Rules[0].Actions[0].Set.Label = "alertname"
	if err := Validate(cfg); err != nil {
		t.Fatalf("expected valid config to pass, got: %v", err)
	}
}

func TestValidateAcceptsSetAnnotation(t *testing.T) {
	cfg := validConfig()
	cfg.Rules[0].Actions[0].Set = &SetAction{Annotation: "runbook_url", Value: "https://wiki/runbook"}
	if err := Validate(cfg); err != nil {
		t.Fatalf("expected valid config to pass, got: %v", err)
	}
}

func TestValidateAcceptsDropAnnotation(t *testing.T) {
	cfg := validConfig()
	cfg.Rules[0].Actions = []ActionConfig{{Drop: &DropAction{Annotation: "description"}}}
	if err := Validate(cfg); err != nil {
		t.Fatalf("expected valid config to pass, got: %v", err)
	}
}

func TestValidateRejectsSetWithBothLabelAndAnnotation(t *testing.T) {
	cfg := validConfig()
	cfg.Rules[0].Actions[0].Set = &SetAction{Label: "team", Annotation: "runbook_url", Value: "x"}
	assertRejects(t, cfg, "exactly one of label or annotation")
}

func TestValidateRejectsSetWithNeitherLabelNorAnnotation(t *testing.T) {
	cfg := validConfig()
	cfg.Rules[0].Actions[0].Set = &SetAction{Value: "x"}
	assertRejects(t, cfg, "exactly one of label or annotation")
}

func TestValidateRejectsDropWithBothLabelAndAnnotation(t *testing.T) {
	cfg := validConfig()
	cfg.Rules[0].Actions = []ActionConfig{{Drop: &DropAction{Label: "pod", Annotation: "description"}}}
	assertRejects(t, cfg, "exactly one of label or annotation")
}

func TestValidateRejectsDropWithNeitherLabelNorAnnotation(t *testing.T) {
	cfg := validConfig()
	cfg.Rules[0].Actions = []ActionConfig{{Drop: &DropAction{}}}
	assertRejects(t, cfg, "exactly one of label or annotation")
}

func TestValidateRejectsForceOnSetAnnotation(t *testing.T) {
	cfg := validConfig()
	cfg.Rules[0].Actions[0].Set = &SetAction{Annotation: "runbook_url", Value: "x", Force: true}
	assertRejects(t, cfg, "not applicable to annotations")
}

func TestValidateRejectsForceOnDropAnnotation(t *testing.T) {
	cfg := validConfig()
	cfg.Rules[0].Actions = []ActionConfig{{Drop: &DropAction{Annotation: "description", Force: true}}}
	assertRejects(t, cfg, "not applicable to annotations")
}

func TestValidateAcceptsSetAnnotationNamedAlertnameWithoutForce(t *testing.T) {
	// ReservedLabels/force exist only to guard the alert's fingerprint;
	// annotations never affect it, so the name "alertname" is unremarkable
	// as an annotation.
	cfg := validConfig()
	cfg.Rules[0].Actions[0].Set = &SetAction{Annotation: "alertname", Value: "x"}
	if err := Validate(cfg); err != nil {
		t.Fatalf("expected valid config to pass, got: %v", err)
	}
}

func TestValidateAcceptsForwardTLSCAOnly(t *testing.T) {
	cfg := validConfig()
	cfg.Forward.TLS = &ClientTLSSpec{CAFile: "ca.pem"}
	if err := Validate(cfg); err != nil {
		t.Fatalf("expected valid config to pass, got: %v", err)
	}
}

func TestValidateRejectsForwardTLSUnpairedClientCert(t *testing.T) {
	cfg := validConfig()
	cfg.Forward.TLS = &ClientTLSSpec{CertFile: "client.pem"}
	assertRejects(t, cfg, "forward.tls")
}

func TestValidateAcceptsWellFormedEnforcement(t *testing.T) {
	cfg := validConfig()
	cfg.CRD.Enabled = true
	cfg.Enforcement = EnforcementConfig{
		NamespaceMatcherLabel: "namespace",
		Rules: []EnforcementRuleConfig{{
			NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tenant-isolation": "enabled"}},
			Match:             []MatchConfig{{Label: "cluster", Op: OpEq, Value: "prod"}},
			Labels:            LabelPolicyConfig{Deny: []string{"severity", "team"}},
			AllowedSources:    []string{"ns"},
		}},
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("expected valid config to pass, got: %v", err)
	}
}

func TestValidateRejectsEnforcementInvalidNamespaceSelector(t *testing.T) {
	cfg := validConfig()
	cfg.Enforcement.Rules = []EnforcementRuleConfig{{
		NamespaceSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "team", Operator: "InvalidOp"},
		}},
	}}
	assertRejects(t, cfg, "namespaceSelector")
}

func TestValidateRejectsEnforcementInvalidMatchOp(t *testing.T) {
	cfg := validConfig()
	cfg.Enforcement.Rules = []EnforcementRuleConfig{{
		Match: []MatchConfig{{Label: "cluster", Op: "bogus"}},
	}}
	assertRejects(t, cfg, "unknown op")
}

func TestValidateRejectsEnforcementUndeclaredAllowedSource(t *testing.T) {
	cfg := validConfig()
	cfg.Enforcement.Rules = []EnforcementRuleConfig{{
		AllowedSources: []string{"does-not-exist"},
	}}
	assertRejects(t, cfg, "not a declared source")
}

func TestValidateRejectsEnforcementNegativeMaxRules(t *testing.T) {
	cfg := validConfig()
	cfg.Enforcement.Rules = []EnforcementRuleConfig{{
		MaxRulesPerNamespace: -1,
	}}
	assertRejects(t, cfg, "maxRulesPerNamespace")
}

func assertRejects(t *testing.T, cfg *Config, wantSubstr string) {
	t.Helper()
	err := Validate(cfg)
	if err == nil {
		t.Fatalf("expected validation to reject config, got nil error")
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("error = %q, want substring %q", err.Error(), wantSubstr)
	}
}

// gojq.Parse accepts expressions that gojq.Compile rejects - an undefined
// function, an unbound variable. Validation must catch those, or they pass
// `enricher check` and only fail when engine.Compile runs, which is during
// a reload: a failed configuration swap rather than a validation error.
func TestValidateRejectsJqThatParsesButDoesNotCompile(t *testing.T) {
	for _, jq := range []string{".x | no_such_function", "$nosuchvar"} {
		cfg := &Config{
			Targets: []TargetConfig{{URL: "http://am:9093"}},
			Sources: []SourceConfig{{Name: "s", Type: "file", File: &FileSourceSpec{Path: "/tmp/x"}}},
			Rules: []RuleConfig{{
				Name:    "r",
				Actions: []ActionConfig{{Set: &SetAction{Label: "a", From: &FromConfig{Source: "s", Jq: jq}}}},
			}},
		}
		applyDefaults(cfg)
		if err := Validate(cfg); err == nil {
			t.Errorf("Validate accepted jq %q, which engine.Compile would reject at reload time", jq)
		}
	}
}

// The variables the engine binds must still validate.
func TestValidateAcceptsJqUsingBoundVariables(t *testing.T) {
	cfg := &Config{
		Targets: []TargetConfig{{URL: "http://am:9093"}},
		Sources: []SourceConfig{{Name: "s", Type: "file", File: &FileSourceSpec{Path: "/tmp/x"}}},
		Rules: []RuleConfig{{
			Name:    "r",
			Actions: []ActionConfig{{Set: &SetAction{Label: "a", From: &FromConfig{Source: "s", Jq: `.[$labels.namespace] // $annotations.summary`}}}},
		}},
	}
	applyDefaults(cfg)
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate rejected jq using $labels/$annotations, which the engine does bind: %v", err)
	}
}
