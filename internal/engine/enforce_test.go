package engine

import (
	"context"
	"testing"

	"github.com/splattner/alertmanager-label-enricher/internal/config"
	"github.com/splattner/alertmanager-label-enricher/internal/enforce"
)

// TestEnforcedRulesFromDifferentNamespacesStayIsolated is the end-to-end
// proof (not just an assertion about matcher lists, but the real engine
// evaluating them) of this project's core multi-tenancy claim: two
// EnrichmentRule CRs from two different namespaces, compiled into the same
// engine as file-config rules would be, can never affect each other's
// alerts - even when tenant B's rule is deliberately crafted to target
// tenant A's namespace.
func TestEnforcedRulesFromDifferentNamespacesStayIsolated(t *testing.T) {
	enforcement := config.EnforcementConfig{
		NamespaceMatcherLabel: "namespace",
		Rules:                 []config.EnforcementRuleConfig{{}}, // catch-all: enforcement is active for every namespace
	}

	ruleA := config.RuleConfig{
		Name:    "mark-team",
		Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "team", Value: "team-a"}}},
	}
	enforcedA, err := enforce.Rule(ruleA, "team-a-namespace", nil, nil, enforcement)
	if err != nil {
		t.Fatalf("enforce.Rule for team A: %v", err)
	}

	// Tenant B's rule has no matchers of its own at all - if enforcement
	// were broken, this alone would already be enough to mutate every
	// alert in the stream (threat #1 from the plan).
	ruleB := config.RuleConfig{
		Name:    "mark-team",
		Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "team", Value: "team-b", Overwrite: true}}},
	}
	enforcedB, err := enforce.Rule(ruleB, "team-b-namespace", nil, nil, enforcement)
	if err != nil {
		t.Fatalf("enforce.Rule for team B: %v", err)
	}

	cfg := &config.Config{
		Targets: []config.TargetConfig{{URL: "http://x"}},
		Rules:   []config.RuleConfig{enforcedA, enforcedB},
	}
	eng := compile(t, cfg, fakeRegistry{})

	teamAAlert := newAlert(map[string]string{"alertname": "Test", "namespace": "team-a-namespace"})
	if _, err := eng.Apply(context.Background(), teamAAlert); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	labels, _ := teamAAlert.Labels()
	if labels["team"] != "team-a" {
		t.Fatalf("team-a-namespace alert got team=%q, want team-a (team B's rule must not have fired)", labels["team"])
	}

	teamBAlert := newAlert(map[string]string{"alertname": "Test", "namespace": "team-b-namespace"})
	if _, err := eng.Apply(context.Background(), teamBAlert); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	labels, _ = teamBAlert.Labels()
	if labels["team"] != "team-b" {
		t.Fatalf("team-b-namespace alert got team=%q, want team-b (team A's rule must not have fired)", labels["team"])
	}
}

// TestEnforcedRuleClaimingAnotherNamespaceMatchesNothing proves the
// specific AND-semantics mechanism: a tenant rule that explicitly names a
// namespace it isn't in never fires anywhere, rather than the injected
// matcher silently overriding the tenant's claim.
func TestEnforcedRuleClaimingAnotherNamespaceMatchesNothing(t *testing.T) {
	enforcement := config.EnforcementConfig{
		NamespaceMatcherLabel: "namespace",
		Rules:                 []config.EnforcementRuleConfig{{}},
	}

	rule := config.RuleConfig{
		Name:  "impersonate",
		Match: []config.MatchConfig{{Label: "namespace", Op: config.OpEq, Value: "victim-namespace"}},
		Actions: []config.ActionConfig{{Set: &config.SetAction{
			Annotation: "pwned", Value: "true",
		}}},
	}
	enforced, err := enforce.Rule(rule, "attacker-namespace", nil, nil, enforcement)
	if err != nil {
		t.Fatalf("enforce.Rule: %v", err)
	}

	cfg := &config.Config{
		Targets: []config.TargetConfig{{URL: "http://x"}},
		Rules:   []config.RuleConfig{enforced},
	}
	eng := compile(t, cfg, fakeRegistry{})

	victimAlert := newAlert(map[string]string{"alertname": "Test", "namespace": "victim-namespace"})
	if _, err := eng.Apply(context.Background(), victimAlert); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	annotations, err := victimAlert.Annotations()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := annotations["pwned"]; exists {
		t.Fatal("rule injected into attacker-namespace must not fire against victim-namespace's alert, even though it explicitly matches namespace=victim-namespace")
	}

	attackerAlert := newAlert(map[string]string{"alertname": "Test", "namespace": "attacker-namespace"})
	if _, err := eng.Apply(context.Background(), attackerAlert); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	annotations, err = attackerAlert.Annotations()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := annotations["pwned"]; exists {
		t.Fatal("rule must not fire even against the attacker's own namespace, since its own explicit matcher (namespace=victim-namespace) contradicts the injected one (namespace=attacker-namespace) - both must be true, and can't be")
	}
}
