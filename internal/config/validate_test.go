package config

import (
	"strings"
	"testing"
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
