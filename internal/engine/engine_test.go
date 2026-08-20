package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/splattner/alertmanager-label-enricher/internal/alert"
	"github.com/splattner/alertmanager-label-enricher/internal/config"
	"github.com/splattner/alertmanager-label-enricher/internal/extract"
	"github.com/splattner/alertmanager-label-enricher/internal/metrics"
	"github.com/splattner/alertmanager-label-enricher/internal/source"
)

// fakeSource returns a fixed value (or error) regardless of input.
type fakeSource struct {
	name  string
	value any
	err   error
}

func (f *fakeSource) Name() string                { return f.name }
func (f *fakeSource) Start(context.Context) error { return nil }
func (f *fakeSource) HasSynced() bool             { return true }
func (f *fakeSource) Lookup(context.Context, source.LookupInput) (any, error) {
	return f.value, f.err
}

type fakeRegistry map[string]source.Source

func (r fakeRegistry) Get(name string) (source.Source, bool) {
	s, ok := r[name]
	return s, ok
}

func newAlert(labels map[string]string) alert.Alert {
	a := alert.Alert{}
	l := make(map[string]any, len(labels))
	for k, v := range labels {
		l[k] = v
	}
	a["labels"] = l
	return a
}

func compile(t *testing.T, cfg *config.Config, reg fakeRegistry) *Engine {
	t.Helper()
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	eng, err := Compile(cfg, reg, extract.NewCache())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return eng
}

func TestMatchOpsAnchored(t *testing.T) {
	// A regex matcher must be fully anchored: "prod" should not match
	// "preprod-1" or "prod-1-extra" via unanchored substring search.
	cfg := &config.Config{
		Targets: []config.TargetConfig{{URL: "http://x"}},
		Rules: []config.RuleConfig{{
			Name:    "r",
			Match:   []config.MatchConfig{{Label: "cluster", Op: config.OpRegex, Value: "prod-.*"}},
			Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "matched", Value: "yes"}}},
		}},
	}
	eng := compile(t, cfg, fakeRegistry{})

	for _, tt := range []struct {
		cluster string
		want    bool
	}{
		{"prod-eu1", true},
		{"preprod-1", false},
		{"staging", false},
	} {
		a := newAlert(map[string]string{"cluster": tt.cluster})
		if _, err := eng.Apply(context.Background(), a); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		labels, _ := a.Labels()
		_, matched := labels["matched"]
		if matched != tt.want {
			t.Errorf("cluster=%q: matched=%v, want %v", tt.cluster, matched, tt.want)
		}
	}
}

func TestRulesApplyInOrderCumulatively(t *testing.T) {
	// Rule 2 must see the label rule 1 added.
	cfg := &config.Config{
		Targets: []config.TargetConfig{{URL: "http://x"}},
		Rules: []config.RuleConfig{
			{
				Name:    "add-a",
				Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "a", Value: "1"}}},
			},
			{
				Name:    "add-b-if-a",
				Match:   []config.MatchConfig{{Label: "a", Op: config.OpEq, Value: "1"}},
				Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "b", Value: "2"}}},
			},
		},
	}
	eng := compile(t, cfg, fakeRegistry{})
	a := newAlert(nil)
	if _, err := eng.Apply(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	labels, _ := a.Labels()
	if labels["a"] != "1" || labels["b"] != "2" {
		t.Fatalf("labels = %v, want a=1 b=2", labels)
	}
}

func TestOverwriteRequiresOptIn(t *testing.T) {
	cfg := &config.Config{
		Targets: []config.TargetConfig{{URL: "http://x"}},
		Rules: []config.RuleConfig{{
			Name:    "try-overwrite",
			Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "team", Value: "new"}}},
		}},
	}
	eng := compile(t, cfg, fakeRegistry{})
	a := newAlert(map[string]string{"team": "original"})
	results, err := eng.Apply(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	labels, _ := a.Labels()
	if labels["team"] != "original" {
		t.Fatalf("team = %q, want unchanged %q", labels["team"], "original")
	}
	if len(results[0].Added) != 0 || len(results[0].Overwritten) != 0 {
		t.Fatalf("expected no added/overwritten without opt-in, got %+v", results[0])
	}

	cfg.Rules[0].Actions[0].Set.Overwrite = true
	eng = compile(t, cfg, fakeRegistry{})
	a = newAlert(map[string]string{"team": "original"})
	results, err = eng.Apply(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	labels, _ = a.Labels()
	if labels["team"] != "new" {
		t.Fatalf("team = %q, want %q", labels["team"], "new")
	}
	if len(results[0].Overwritten) != 1 || results[0].Overwritten[0] != "team" {
		t.Fatalf("expected team recorded as overwritten, got %+v", results[0])
	}
}

func TestDropRemovesLabel(t *testing.T) {
	cfg := &config.Config{
		Targets: []config.TargetConfig{{URL: "http://x"}},
		Rules: []config.RuleConfig{{
			Name:    "drop-pod",
			Actions: []config.ActionConfig{{Drop: &config.DropAction{Label: "pod"}}},
		}},
	}
	eng := compile(t, cfg, fakeRegistry{})
	a := newAlert(map[string]string{"pod": "x-123", "alertname": "Test"})
	results, err := eng.Apply(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	labels, _ := a.Labels()
	if _, exists := labels["pod"]; exists {
		t.Fatal("expected pod label to be dropped")
	}
	if results[0].Dropped[0] != "pod" {
		t.Fatalf("results = %+v", results[0])
	}
}

func TestDryRunDoesNotMutate(t *testing.T) {
	cfg := &config.Config{
		Targets: []config.TargetConfig{{URL: "http://x"}},
		Rules: []config.RuleConfig{{
			Name:    "dry",
			DryRun:  true,
			Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "team", Value: "platform"}}},
		}},
	}
	eng := compile(t, cfg, fakeRegistry{})
	a := newAlert(nil)
	results, err := eng.Apply(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	labels, _ := a.Labels()
	if _, exists := labels["team"]; exists {
		t.Fatal("dry-run rule must not mutate labels")
	}
	if len(results[0].Added) != 1 || results[0].Added[0] != "team" {
		t.Fatalf("dry-run should still report what it would have added, got %+v", results[0])
	}
}

func TestFromSourceLookupAndDefault(t *testing.T) {
	cfg := &config.Config{
		Targets: []config.TargetConfig{{URL: "http://x"}},
		Sources: []config.SourceConfig{{Name: "teams", Type: "file", File: &config.FileSourceSpec{Path: "unused"}}},
		Rules: []config.RuleConfig{{
			Name: "from-source",
			Actions: []config.ActionConfig{{Set: &config.SetAction{
				Label:   "team",
				From:    &config.FromConfig{Source: "teams", Jq: `.namespaces[$labels.namespace].team`},
				Default: "unassigned",
			}}},
		}},
	}

	t.Run("lookup hits", func(t *testing.T) {
		reg := fakeRegistry{"teams": &fakeSource{name: "teams", value: map[string]any{
			"namespaces": map[string]any{"payments": map[string]any{"team": "platform"}},
		}}}
		eng := compile(t, cfg, reg)
		a := newAlert(map[string]string{"namespace": "payments"})
		if _, err := eng.Apply(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		labels, _ := a.Labels()
		if labels["team"] != "platform" {
			t.Fatalf("team = %q, want platform", labels["team"])
		}
	})

	t.Run("lookup misses falls back to default", func(t *testing.T) {
		reg := fakeRegistry{"teams": &fakeSource{name: "teams", value: map[string]any{"namespaces": map[string]any{}}}}
		eng := compile(t, cfg, reg)
		a := newAlert(map[string]string{"namespace": "unknown-ns"})
		if _, err := eng.Apply(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		labels, _ := a.Labels()
		if labels["team"] != "unassigned" {
			t.Fatalf("team = %q, want unassigned", labels["team"])
		}
	})
}

func TestRequiredRuleFailsBatchClosed(t *testing.T) {
	cfg := &config.Config{
		Targets: []config.TargetConfig{{URL: "http://x"}},
		Sources: []config.SourceConfig{{Name: "cmdb", Type: "http", HTTP: &config.HTTPSourceSpec{URL: "http://x", AllowedHosts: []string{"x"}}}},
		Rules: []config.RuleConfig{{
			Name:     "required-tier",
			Required: true,
			Actions: []config.ActionConfig{{Set: &config.SetAction{
				Label: "tier",
				From:  &config.FromConfig{Source: "cmdb", Jq: `.tier`},
			}}},
		}},
	}
	reg := fakeRegistry{"cmdb": &fakeSource{name: "cmdb", err: errors.New("backend unreachable")}}
	eng := compile(t, cfg, reg)
	a := newAlert(nil)

	_, err := eng.Apply(context.Background(), a)
	var reqFail *RequiredFailure
	if !errors.As(err, &reqFail) {
		t.Fatalf("Apply err = %v, want a *RequiredFailure", err)
	}
	if reqFail.Rule != "required-tier" || reqFail.Kind != "label" || reqFail.Target != "tier" {
		t.Fatalf("RequiredFailure = %+v", reqFail)
	}
}

func TestOptionalRuleFailsOpenOnLookupError(t *testing.T) {
	cfg := &config.Config{
		Targets: []config.TargetConfig{{URL: "http://x"}},
		Sources: []config.SourceConfig{{Name: "cmdb", Type: "http", HTTP: &config.HTTPSourceSpec{URL: "http://x", AllowedHosts: []string{"x"}}}},
		Rules: []config.RuleConfig{{
			Name: "optional-tier",
			Actions: []config.ActionConfig{{Set: &config.SetAction{
				Label: "tier",
				From:  &config.FromConfig{Source: "cmdb", Jq: `.tier`},
			}}},
		}},
	}
	reg := fakeRegistry{"cmdb": &fakeSource{name: "cmdb", err: errors.New("backend unreachable")}}
	eng := compile(t, cfg, reg)
	a := newAlert(map[string]string{"alertname": "Test"})

	if _, err := eng.Apply(context.Background(), a); err != nil {
		t.Fatalf("optional rule must fail open, got error: %v", err)
	}
	labels, _ := a.Labels()
	if _, exists := labels["tier"]; exists {
		t.Fatal("tier should not be set when the lookup failed with no default")
	}
}

func TestApplyRecordsSourceLookupMetrics(t *testing.T) {
	ruleFor := func(sourceName string) *config.Config {
		return &config.Config{
			Targets: []config.TargetConfig{{URL: "http://x"}},
			Sources: []config.SourceConfig{{Name: sourceName, Type: "file", File: &config.FileSourceSpec{Path: "/dev/null"}}},
			Rules: []config.RuleConfig{{
				Name:    "lookup",
				Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "team", From: &config.FromConfig{Source: sourceName, Jq: "."}}}},
			}},
		}
	}

	tests := []struct {
		name       string
		src        *fakeSource
		wantResult string
	}{
		{"hit", &fakeSource{name: "src-hit", value: "found"}, "hit"},
		{"miss", &fakeSource{name: "src-miss", value: nil}, "miss"},
		{"error", &fakeSource{name: "src-error", err: errors.New("boom")}, "error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ruleFor(tt.src.name)
			reg := fakeRegistry{tt.src.name: tt.src}
			eng := compile(t, cfg, reg)

			before := testutil.ToFloat64(metrics.SourceLookupsTotal.WithLabelValues(tt.src.name, tt.wantResult))
			if _, err := eng.Apply(context.Background(), newAlert(map[string]string{"alertname": "Test"})); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			after := testutil.ToFloat64(metrics.SourceLookupsTotal.WithLabelValues(tt.src.name, tt.wantResult))

			if after != before+1 {
				t.Fatalf("ale_source_lookups_total{source=%q,result=%q} = %v, want %v", tt.src.name, tt.wantResult, after, before+1)
			}
		})
	}
}

func TestApplySetsAnnotation(t *testing.T) {
	cfg := &config.Config{
		Targets: []config.TargetConfig{{URL: "http://x"}},
		Rules: []config.RuleConfig{{
			Name:    "runbook",
			Actions: []config.ActionConfig{{Set: &config.SetAction{Annotation: "runbook_url", Value: "https://wiki/runbook"}}},
		}},
	}
	eng := compile(t, cfg, fakeRegistry{})
	a := newAlert(map[string]string{"alertname": "Test"})

	results, err := eng.Apply(context.Background(), a)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(results) != 1 || len(results[0].AnnotationsAdded) != 1 || results[0].AnnotationsAdded[0] != "runbook_url" {
		t.Fatalf("results = %+v", results)
	}
	if len(results[0].Added) != 0 {
		t.Fatalf("annotation set must not also count as a label add: %+v", results[0])
	}

	annotations, err := a.Annotations()
	if err != nil {
		t.Fatal(err)
	}
	if annotations["runbook_url"] != "https://wiki/runbook" {
		t.Fatalf("annotations = %v", annotations)
	}
}

func TestApplyOverwriteGuardAppliesToAnnotations(t *testing.T) {
	cfg := &config.Config{
		Targets: []config.TargetConfig{{URL: "http://x"}},
		Rules: []config.RuleConfig{{
			Name:    "runbook",
			Actions: []config.ActionConfig{{Set: &config.SetAction{Annotation: "runbook_url", Value: "https://wiki/new"}}},
		}},
	}
	eng := compile(t, cfg, fakeRegistry{})
	a := alert.Alert{
		"labels":      map[string]any{"alertname": "Test"},
		"annotations": map[string]any{"runbook_url": "https://wiki/old"},
	}

	results, err := eng.Apply(context.Background(), a)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(results[0].AnnotationsAdded) != 0 || len(results[0].AnnotationsOverwritten) != 0 {
		t.Fatalf("expected no-op without overwrite:true, got %+v", results[0])
	}
	annotations, _ := a.Annotations()
	if annotations["runbook_url"] != "https://wiki/old" {
		t.Fatalf("annotation must not change without overwrite:true, got %v", annotations["runbook_url"])
	}

	cfg.Rules[0].Actions[0].Set.Overwrite = true
	eng = compile(t, cfg, fakeRegistry{})
	results, err = eng.Apply(context.Background(), a)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(results[0].AnnotationsOverwritten) != 1 || results[0].AnnotationsOverwritten[0] != "runbook_url" {
		t.Fatalf("results = %+v", results[0])
	}
	annotations, _ = a.Annotations()
	if annotations["runbook_url"] != "https://wiki/new" {
		t.Fatalf("annotation = %v, want it overwritten", annotations["runbook_url"])
	}
}

func TestApplyDropsAnnotation(t *testing.T) {
	cfg := &config.Config{
		Targets: []config.TargetConfig{{URL: "http://x"}},
		Rules: []config.RuleConfig{{
			Name:    "strip-description",
			Actions: []config.ActionConfig{{Drop: &config.DropAction{Annotation: "description"}}},
		}},
	}
	eng := compile(t, cfg, fakeRegistry{})
	a := alert.Alert{
		"labels":      map[string]any{"alertname": "Test"},
		"annotations": map[string]any{"description": "a very long description"},
	}

	results, err := eng.Apply(context.Background(), a)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(results[0].AnnotationsDropped) != 1 || results[0].AnnotationsDropped[0] != "description" {
		t.Fatalf("results = %+v", results[0])
	}
	annotations, _ := a.Annotations()
	if _, exists := annotations["description"]; exists {
		t.Fatal("description annotation should have been dropped")
	}
}

func TestApplyChainedRulesSeeAnnotationsWrittenByEarlierRules(t *testing.T) {
	// This is the test that would catch a regression back to Annotations()
	// returning a detached copy: if rule 2's template read a stale copy
	// instead of the alert's real storage, it would never see what rule 1
	// wrote.
	cfg := &config.Config{
		Targets: []config.TargetConfig{{URL: "http://x"}},
		Rules: []config.RuleConfig{
			{Name: "set-owner", Actions: []config.ActionConfig{{Set: &config.SetAction{Annotation: "owner", Value: "platform-team"}}}},
			{Name: "set-summary", Actions: []config.ActionConfig{{Set: &config.SetAction{
				Annotation: "summary", Template: "owned by {{ .Annotations.owner }}",
			}}}},
		},
	}
	eng := compile(t, cfg, fakeRegistry{})
	a := newAlert(map[string]string{"alertname": "Test"})

	if _, err := eng.Apply(context.Background(), a); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	annotations, err := a.Annotations()
	if err != nil {
		t.Fatal(err)
	}
	if annotations["summary"] != "owned by platform-team" {
		t.Fatalf("summary = %q, want the second rule to see the first rule's annotation", annotations["summary"])
	}
}

func TestApplyRequiredFailureNamesAnnotation(t *testing.T) {
	cfg := &config.Config{
		Targets: []config.TargetConfig{{URL: "http://x"}},
		Sources: []config.SourceConfig{{Name: "cmdb", Type: "http", HTTP: &config.HTTPSourceSpec{URL: "http://x", AllowedHosts: []string{"x"}}}},
		Rules: []config.RuleConfig{{
			Name:     "required-runbook",
			Required: true,
			Actions: []config.ActionConfig{{Set: &config.SetAction{
				Annotation: "runbook_url",
				From:       &config.FromConfig{Source: "cmdb", Jq: `.runbook`},
			}}},
		}},
	}
	reg := fakeRegistry{"cmdb": &fakeSource{name: "cmdb", err: errors.New("backend unreachable")}}
	eng := compile(t, cfg, reg)

	_, err := eng.Apply(context.Background(), newAlert(nil))
	var reqFail *RequiredFailure
	if !errors.As(err, &reqFail) {
		t.Fatalf("Apply err = %v, want a *RequiredFailure", err)
	}
	if reqFail.Kind != "annotation" || reqFail.Target != "runbook_url" {
		t.Fatalf("RequiredFailure = %+v", reqFail)
	}
}
