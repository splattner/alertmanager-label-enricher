package enforce

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/splattner/alertmanager-label-enricher/internal/config"
)

func baseRule() config.RuleConfig {
	return config.RuleConfig{
		Name:    "runbook",
		Actions: []config.ActionConfig{{Set: &config.SetAction{Annotation: "runbook_url", Value: "https://wiki/x"}}},
	}
}

func policyFor(entries ...config.EnforcementRuleConfig) config.EnforcementConfig {
	return config.EnforcementConfig{NamespaceMatcherLabel: "namespace", Rules: entries}
}

func catchAll(overrides config.EnforcementRuleConfig) config.EnforcementRuleConfig {
	return overrides // zero-value NamespaceSelector matches every namespace
}

func assertRejects(t *testing.T, err error, wantSubstr string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("error = %q, want substring %q", err.Error(), wantSubstr)
	}
}

func TestRuleWithNoPolicyRulesPassesThroughUnrestricted(t *testing.T) {
	// Single-tenant convenience mode: enforcement.rules is entirely empty,
	// so CR-sourced rules are not touched at all.
	r := baseRule()
	r.Match = []config.MatchConfig{{Label: "severity", Op: config.OpExists}}

	got, err := Rule(r, "payments", map[string]string{}, config.EnforcementConfig{})
	if err != nil {
		t.Fatalf("Rule: %v", err)
	}
	if len(got.Match) != 1 {
		t.Fatalf("Match = %+v, want unchanged (no injected matcher in pass-through mode)", got.Match)
	}
}

func TestRuleInjectsNamespaceMatcher(t *testing.T) {
	cfg := policyFor(catchAll(config.EnforcementRuleConfig{}))
	r := baseRule()

	got, err := Rule(r, "payments", map[string]string{}, cfg)
	if err != nil {
		t.Fatalf("Rule: %v", err)
	}
	want := config.MatchConfig{Label: "namespace", Op: config.OpEq, Value: "payments"}
	if len(got.Match) != 1 || got.Match[0] != want {
		t.Fatalf("Match = %+v, want exactly [%+v]", got.Match, want)
	}
}

func TestRuleAppendsInjectedMatcherRatherThanReplacingTenantMatchers(t *testing.T) {
	cfg := policyFor(catchAll(config.EnforcementRuleConfig{}))
	r := baseRule()
	r.Match = []config.MatchConfig{{Label: "severity", Op: config.OpExists}}

	got, err := Rule(r, "payments", map[string]string{}, cfg)
	if err != nil {
		t.Fatalf("Rule: %v", err)
	}
	if len(got.Match) != 2 {
		t.Fatalf("Match = %+v, want the tenant's matcher plus the injected one", got.Match)
	}
	if got.Match[0].Label != "severity" {
		t.Fatalf("tenant's own matcher must be preserved, got %+v", got.Match[0])
	}
	if got.Match[1] != (config.MatchConfig{Label: "namespace", Op: config.OpEq, Value: "payments"}) {
		t.Fatalf("injected matcher = %+v", got.Match[1])
	}
}

func TestRuleWithContradictingTenantNamespaceMatcherGetsBothANDed(t *testing.T) {
	// This is the core security property: enforcement never overrides a
	// tenant matcher, it only adds to the AND chain. A tenant claiming
	// another namespace ends up with two matchers on the same label that
	// can never both be true, rather than one being silently replaced.
	cfg := policyFor(catchAll(config.EnforcementRuleConfig{}))
	r := baseRule()
	r.Match = []config.MatchConfig{{Label: "namespace", Op: config.OpEq, Value: "other-tenant"}}

	got, err := Rule(r, "payments", map[string]string{}, cfg)
	if err != nil {
		t.Fatalf("Rule: %v", err)
	}
	namespaceMatchers := 0
	for _, m := range got.Match {
		if m.Label == "namespace" {
			namespaceMatchers++
		}
	}
	if namespaceMatchers != 2 {
		t.Fatalf("Match = %+v, want both the tenant's namespace matcher and the injected one present (not merged/overridden)", got.Match)
	}
}

func TestRuleRejectsWhenNoNamespaceSelectorMatches(t *testing.T) {
	cfg := policyFor(config.EnforcementRuleConfig{
		NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tenant-isolation": "enabled"}},
	})
	_, err := Rule(baseRule(), "payments", map[string]string{}, cfg)
	assertRejects(t, err, "matches no enforcement policy")
}

func TestSelectPolicyFirstMatchWins(t *testing.T) {
	specific := config.EnforcementRuleConfig{NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"team": "payments"}}, AllowRequired: true}
	fallback := catchAll(config.EnforcementRuleConfig{AllowRequired: false})
	cfg := policyFor(specific, fallback)

	got, ok := SelectPolicy(cfg, map[string]string{"team": "payments"})
	if !ok || !got.AllowRequired {
		t.Fatalf("SelectPolicy = %+v, %v, want the specific (first) rule to win", got, ok)
	}

	got, ok = SelectPolicy(cfg, map[string]string{"team": "other"})
	if !ok || got.AllowRequired {
		t.Fatalf("SelectPolicy = %+v, %v, want the catch-all fallback for a non-matching namespace", got, ok)
	}
}

func TestSelectPolicyReturnsFalseWhenNothingMatches(t *testing.T) {
	cfg := policyFor(config.EnforcementRuleConfig{
		NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tenant-isolation": "enabled"}},
	})
	_, ok := SelectPolicy(cfg, map[string]string{})
	if ok {
		t.Fatal("expected no policy to match")
	}
}

func TestRuleRejectsSetOnDeniedLabel(t *testing.T) {
	cfg := policyFor(catchAll(config.EnforcementRuleConfig{Labels: config.LabelPolicyConfig{Deny: []string{"severity"}}}))
	r := config.RuleConfig{Name: "x", Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "severity", Value: "critical"}}}}

	_, err := Rule(r, "payments", map[string]string{}, cfg)
	assertRejects(t, err, `label "severity" is denied`)
}

func TestRuleRejectsSetOnNamespaceScopingLabelEvenWithoutDenyList(t *testing.T) {
	// The namespace-scoping label is always off-limits, regardless of
	// whether it's separately named in the deny list - this is threat #2
	// from the plan: a tenant must never be able to rewrite the label
	// enforcement itself relies on to scope them.
	cfg := policyFor(catchAll(config.EnforcementRuleConfig{}))
	r := config.RuleConfig{Name: "x", Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "namespace", Value: "other-tenant"}}}}

	_, err := Rule(r, "payments", map[string]string{}, cfg)
	assertRejects(t, err, "namespace-scoping label")
}

func TestRuleRejectsDropOfNamespaceScopingLabel(t *testing.T) {
	cfg := policyFor(catchAll(config.EnforcementRuleConfig{}))
	r := config.RuleConfig{Name: "x", Actions: []config.ActionConfig{{Drop: &config.DropAction{Label: "namespace"}}}}

	_, err := Rule(r, "payments", map[string]string{}, cfg)
	assertRejects(t, err, "namespace-scoping label")
}

func TestRuleAllowsAnnotationsUnrestrictedByDefault(t *testing.T) {
	cfg := policyFor(catchAll(config.EnforcementRuleConfig{}))
	if _, err := Rule(baseRule(), "payments", map[string]string{}, cfg); err != nil {
		t.Fatalf("annotations should be unrestricted by default, got: %v", err)
	}
}

func TestRuleRejectsAnnotationNotInAllowList(t *testing.T) {
	cfg := policyFor(catchAll(config.EnforcementRuleConfig{
		Annotations: config.LabelPolicyConfig{Allow: []string{"dashboard_url"}},
	}))
	_, err := Rule(baseRule(), "payments", map[string]string{}, cfg) // sets runbook_url
	assertRejects(t, err, `annotation "runbook_url" is not in this namespace's allow list`)
}

func TestRuleAllowListTakesPrecedenceOverDenyList(t *testing.T) {
	cfg := policyFor(catchAll(config.EnforcementRuleConfig{
		Labels: config.LabelPolicyConfig{Allow: []string{"team"}, Deny: []string{"team"}},
	}))
	r := config.RuleConfig{Name: "x", Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "team", Value: "platform"}}}}

	if _, err := Rule(r, "payments", map[string]string{}, cfg); err != nil {
		t.Fatalf("allow list should take precedence over deny, got: %v", err)
	}
}

func TestRuleRejectsSourceNotInAllowedSources(t *testing.T) {
	cfg := policyFor(catchAll(config.EnforcementRuleConfig{AllowedSources: []string{"ns"}}))
	r := config.RuleConfig{Name: "x", Actions: []config.ActionConfig{{Set: &config.SetAction{
		Label: "team", From: &config.FromConfig{Source: "secrets", Jq: ".team"},
	}}}}

	_, err := Rule(r, "payments", map[string]string{}, cfg)
	assertRejects(t, err, `"secrets" is not a declared source`)
}

func TestRuleAllowsSourceInAllowedSources(t *testing.T) {
	cfg := policyFor(catchAll(config.EnforcementRuleConfig{AllowedSources: []string{"ns"}}))
	r := config.RuleConfig{Name: "x", Actions: []config.ActionConfig{{Set: &config.SetAction{
		Label: "team", From: &config.FromConfig{Source: "ns", Jq: ".team"},
	}}}}

	if _, err := Rule(r, "payments", map[string]string{}, cfg); err != nil {
		t.Fatalf("source in allowedSources should be permitted, got: %v", err)
	}
}

func TestRuleRejectsRequiredUnlessPolicyAllows(t *testing.T) {
	cfg := policyFor(catchAll(config.EnforcementRuleConfig{}))
	r := baseRule()
	r.Required = true

	_, err := Rule(r, "payments", map[string]string{}, cfg)
	assertRejects(t, err, "required: true is not permitted")
}

func TestRuleAllowsRequiredWhenPolicyGrantsIt(t *testing.T) {
	cfg := policyFor(catchAll(config.EnforcementRuleConfig{AllowRequired: true}))
	r := baseRule()
	r.Required = true

	if _, err := Rule(r, "payments", map[string]string{}, cfg); err != nil {
		t.Fatalf("required: true should be permitted when the policy allows it, got: %v", err)
	}
}
