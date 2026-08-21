package crd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/splattner/alertmanager-label-enricher/internal/config"
)

func enrichmentRule(ns, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "enricher.splattner.github.io/v1alpha1",
		"kind":       "EnrichmentRule",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
		},
		"spec": spec,
	}}
}

func namespaceObj(name string, labels map[string]string) *unstructured.Unstructured {
	l := map[string]any{}
	for k, v := range labels {
		l[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name":   name,
			"labels": l,
		},
	}}
}

func setLabelSpec(label, value string) map[string]any {
	return map[string]any{
		"actions": []any{
			map[string]any{"set": map[string]any{"label": label, "value": value}},
		},
	}
}

func newFakeClient(t *testing.T, objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		GVR:          "EnrichmentRuleList",
		namespaceGVR: "NamespaceList",
		eventGVR:     "EventList",
	}, objs...)
}

func startedWatcher(t *testing.T, client *dynamicfake.FakeDynamicClient, enforcement config.EnforcementConfig) *Watcher {
	t.Helper()
	w := New(client, Config{Enforcement: enforcement})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !w.HasSynced() {
		t.Fatal("expected HasSynced() to be true after Start")
	}
	return w
}

func catchAllEnforcement() config.EnforcementConfig {
	return config.EnforcementConfig{
		NamespaceMatcherLabel: "namespace",
		Rules:                 []config.EnforcementRuleConfig{{}},
	}
}

func TestWatcherListsAndEnforcesRulesAcrossNamespaces(t *testing.T) {
	client := newFakeClient(t,
		namespaceObj("team-a", nil),
		namespaceObj("team-b", nil),
		enrichmentRule("team-a", "mark-team", setLabelSpec("team", "team-a")),
		enrichmentRule("team-b", "mark-team", setLabelSpec("team", "team-b")),
	)
	w := startedWatcher(t, client, catchAllEnforcement())

	rules := w.Rules()
	if len(rules) != 2 {
		t.Fatalf("Rules() returned %d rules, want 2: %+v", len(rules), rules)
	}
	byName := map[string]config.RuleConfig{}
	for _, r := range rules {
		byName[r.Name] = r
	}
	a, ok := byName["team-a/mark-team"]
	if !ok {
		t.Fatalf("expected a rule named %q, got %v", "team-a/mark-team", byName)
	}
	if len(a.Match) != 1 || a.Match[0] != (config.MatchConfig{Label: "namespace", Op: config.OpEq, Value: "team-a"}) {
		t.Fatalf("team-a rule Match = %+v, want the injected namespace matcher", a.Match)
	}
	if _, ok := byName["team-b/mark-team"]; !ok {
		t.Fatalf("expected a rule named %q, got %v", "team-b/mark-team", byName)
	}
}

func TestWatcherRejectsRuleViolatingPolicy(t *testing.T) {
	client := newFakeClient(t,
		namespaceObj("team-a", nil),
		enrichmentRule("team-a", "escalate", setLabelSpec("severity", "critical")),
	)
	enforcement := catchAllEnforcement()
	enforcement.Rules[0].Labels = config.LabelPolicyConfig{Deny: []string{"severity"}}
	w := startedWatcher(t, client, enforcement)

	rules := w.Rules()
	if len(rules) != 0 {
		t.Fatalf("Rules() = %+v, want none (the only CR violates the label policy)", rules)
	}
}

func TestWatcherOrdersDeterministically(t *testing.T) {
	client := newFakeClient(t,
		namespaceObj("ns", nil),
		enrichmentRule("ns", "z-rule", mergeSpec(setLabelSpec("a", "1"), map[string]any{"order": int64(2)})),
		enrichmentRule("ns", "a-rule", mergeSpec(setLabelSpec("b", "1"), map[string]any{"order": int64(1)})),
		enrichmentRule("ns", "m-rule", mergeSpec(setLabelSpec("c", "1"), map[string]any{"order": int64(1)})),
	)
	w := startedWatcher(t, client, catchAllEnforcement())

	rules := w.Rules()
	if len(rules) != 3 {
		t.Fatalf("Rules() returned %d rules, want 3", len(rules))
	}
	var names []string
	for _, r := range rules {
		names = append(names, r.Name)
	}
	want := []string{"ns/a-rule", "ns/m-rule", "ns/z-rule"} // order 1 (a,m sorted by name), then order 2
	for i, n := range want {
		if names[i] != n {
			t.Fatalf("Rules() order = %v, want %v", names, want)
		}
	}
}

func mergeSpec(spec map[string]any, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range spec {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestWatcherEnforcesMaxRulesPerNamespace(t *testing.T) {
	client := newFakeClient(t,
		namespaceObj("ns", nil),
		enrichmentRule("ns", "a", setLabelSpec("a", "1")),
		enrichmentRule("ns", "b", setLabelSpec("b", "1")),
		enrichmentRule("ns", "c", setLabelSpec("c", "1")),
	)
	enforcement := catchAllEnforcement()
	enforcement.Rules[0].MaxRulesPerNamespace = 2
	w := startedWatcher(t, client, enforcement)

	rules := w.Rules()
	if len(rules) != 2 {
		t.Fatalf("Rules() returned %d rules, want exactly 2 (maxRulesPerNamespace)", len(rules))
	}
	// Deterministic ordering (by name, since no `order` set) means a and b
	// win, c is the one over the cap.
	names := map[string]bool{}
	for _, r := range rules {
		names[r.Name] = true
	}
	if !names["ns/a"] || !names["ns/b"] {
		t.Fatalf("Rules() = %v, want ns/a and ns/b to be the accepted ones", rules)
	}
}

func TestWatcherSingleTenantModePassesEverythingThrough(t *testing.T) {
	client := newFakeClient(t,
		namespaceObj("any-namespace", nil),
		enrichmentRule("any-namespace", "unrestricted", setLabelSpec("severity", "critical")),
	)
	w := startedWatcher(t, client, config.EnforcementConfig{}) // no enforcement.rules at all

	rules := w.Rules()
	if len(rules) != 1 {
		t.Fatalf("Rules() = %+v, want the single CR to pass through unrestricted", rules)
	}
	if len(rules[0].Match) != 0 {
		t.Fatalf("Match = %+v, want no injected matcher in single-tenant mode", rules[0].Match)
	}
}

func TestWatcherRejectsCRWithNoSpec(t *testing.T) {
	client := newFakeClient(t,
		namespaceObj("ns", nil),
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "enricher.splattner.github.io/v1alpha1",
			"kind":       "EnrichmentRule",
			"metadata":   map[string]any{"name": "broken", "namespace": "ns"},
		}},
	)
	w := startedWatcher(t, client, catchAllEnforcement())

	rules := w.Rules()
	if len(rules) != 0 {
		t.Fatalf("Rules() = %+v, want the spec-less CR rejected, not panicked on", rules)
	}
}

// A missing CRD or a ServiceAccount that cannot list enrichmentrules must
// fail with a diagnosable error rather than blocking forever. In
// cmd/enricher the context passed here is the process signal context,
// which nothing cancels during startup - so an unbounded wait means the
// pod never starts its listener and never crashes either (ALE-06).
func TestStartTimesOutWhenListIsRefused(t *testing.T) {
	client := newFakeClient(t)
	client.PrependReactor("list", "enrichmentrules", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: GVR.Group, Resource: GVR.Resource}, "",
			errors.New("cannot list resource"))
	})

	w := New(client, Config{
		Enforcement: config.EnforcementConfig{},
		SyncTimeout: 300 * time.Millisecond,
	})

	done := make(chan error, 1)
	go func() { done <- w.Start(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Start returned nil despite the initial list never succeeding")
		}
		for _, want := range []string{"timed out", "crd.install", "crd.enabled"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q - it must say how to fix the misconfiguration", err, want)
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start blocked well past its SyncTimeout; the sync wait is still unbounded")
	}
}

// Shutdown must be distinguishable from a misconfiguration.
func TestStartReportsCancellationDistinctly(t *testing.T) {
	client := newFakeClient(t)
	client.PrependReactor("list", "enrichmentrules", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: GVR.Group, Resource: GVR.Resource}, "", errors.New("nope"))
	})

	w := New(client, Config{SyncTimeout: 30 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- w.Start(ctx) }()
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "interrupted") {
			t.Fatalf("err = %v, want an 'interrupted' error naming the cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after its context was cancelled")
	}
}
