package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

// config.ValidateRule is what decides whether a CR-sourced rule is allowed
// into the engine, and engine.Compile is what actually builds it. If the
// two ever disagree, a tenant rule can pass validation and then fail the
// compile - which fails the whole recompile, for every tenant. They share
// their regex and jq compilation (config.MatchRegex, extract.Compile)
// precisely so that cannot happen; this pins the invariant.
func TestAnythingValidateRuleAcceptsAlsoCompiles(t *testing.T) {
	sources := map[string]bool{"s": true}

	rules := []config.RuleConfig{
		{Name: "plain", Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "a", Value: "v"}}}},
		{Name: "tmpl", Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "a", Template: "{{ .Labels.ns }}"}}}},
		{Name: "jq", Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "a", From: &config.FromConfig{Source: "s", Jq: ".metadata.labels.team"}}}}},
		{Name: "jq-vars", Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "a", From: &config.FromConfig{Source: "s", Jq: `.[$labels.namespace] // $annotations.summary`}}}}},
		{Name: "jq-regex", Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "a", From: &config.FromConfig{Source: "s", Jq: ".x", Regex: `^team-(.+)$`}}}}},
		{Name: "jq-fancy", Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "a", From: &config.FromConfig{Source: "s", Jq: `[.items[]? | select(.ready) | .name] | join(",")`}}}}},
		{
			Name: "regex-matchers",
			Match: []config.MatchConfig{
				{Label: "ns", Op: config.OpRegex, Value: "prod-.*"},
				{Label: "job", Op: config.OpNotRegex, Value: "(a|b)+"},
			},
			Actions: []config.ActionConfig{{Drop: &config.DropAction{Label: "noisy"}}},
		},
		{Name: "annotation", Actions: []config.ActionConfig{{Set: &config.SetAction{Annotation: "runbook", Value: "https://x"}}}},
		// Rejected by validation - listed to prove the table exercises both
		// outcomes rather than only ever hitting the accept path.
		{Name: "bad-jq", Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "a", From: &config.FromConfig{Source: "s", Jq: "..[[["}}}}},
		{Name: "uncompilable-jq", Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "a", From: &config.FromConfig{Source: "s", Jq: ".x | no_such_function"}}}}},
		{Name: "bad-regex", Match: []config.MatchConfig{{Label: "ns", Op: config.OpRegex, Value: "prod-("}}, Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "a", Value: "v"}}}},
	}

	accepted, rejected := 0, 0
	for _, r := range rules {
		validErr := config.ValidateRule(r, sources)
		_, compileErr := Compile(&config.Config{Rules: []config.RuleConfig{r}}, fakeRegistry{}, extract.NewCache())

		switch {
		case validErr == nil && compileErr != nil:
			t.Errorf("rule %q passed ValidateRule but failed to compile: %v\n"+
				"validation and compilation have drifted apart - a tenant rule like this would break the recompile for everyone", r.Name, compileErr)
		case validErr != nil && compileErr == nil:
			// Safe direction (validation is stricter), but worth knowing.
			t.Logf("note: rule %q is rejected by validation yet would compile: %v", r.Name, validErr)
			rejected++
		case validErr == nil:
			accepted++
		default:
			rejected++
		}
	}

	if accepted == 0 || rejected == 0 {
		t.Fatalf("table exercised only one outcome (accepted=%d rejected=%d); it must cover both", accepted, rejected)
	}
}

// ALE-02. gojq only observes cancellation when it is handed a context.
// Without one, a pathological expression runs to completion regardless of
// enrichment.timeout, pinning a core and never releasing its concurrency
// slot - and jq from an EnrichmentRule CR is tenant-supplied.
func TestPathologicalJqIsBoundedByContext(t *testing.T) {
	cfg := &config.Config{Rules: []config.RuleConfig{{
		Name: "expensive",
		Actions: []config.ActionConfig{{Set: &config.SetAction{
			Label: "x",
			From:  &config.FromConfig{Source: "s", Jq: `reduce range(200000000) as $i (0; .+$i)`},
		}}},
	}}}
	eng, err := Compile(cfg, fakeRegistry{"s": &fakeSource{value: map[string]any{}}}, extract.NewCache())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		_, _ = eng.Apply(ctx, newAlert(map[string]string{"alertname": "T"}))
	}()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("Apply took %v for a 300ms deadline; jq is not being interrupted promptly", elapsed)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Apply never returned: enrichment.timeout does not bound jq evaluation, so this goroutine and its concurrency slot are leaked for the lifetime of the process")
	}
}

// A rule that is not required must fail open when its jq is cut short:
// the alert still goes out, just without that label.
func TestCancelledJqFailsOpenForNonRequiredRule(t *testing.T) {
	cfg := &config.Config{Rules: []config.RuleConfig{{
		Name: "expensive",
		Actions: []config.ActionConfig{{Set: &config.SetAction{
			Label: "x",
			From:  &config.FromConfig{Source: "s", Jq: `reduce range(200000000) as $i (0; .+$i)`},
		}}},
	}}}
	eng, err := Compile(cfg, fakeRegistry{"s": &fakeSource{value: map[string]any{}}}, extract.NewCache())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	a := newAlert(map[string]string{"alertname": "T"})
	if _, err := eng.Apply(ctx, a); err != nil {
		t.Fatalf("Apply returned an error for a non-required rule, want fail-open: %v", err)
	}
	labels, err := a.Labels()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := labels["x"]; exists {
		t.Error("label x was set despite the jq being cut short")
	}
	if labels["alertname"] != "T" {
		t.Error("the alert's own labels must survive intact")
	}
}

// ALE-03. Alertmanager rejects an alert with no labels, and rejects the
// entire POST with it - so emitting one strands every other alert in the
// batch. An alert's last label is load-bearing and must survive a drop.
func TestDropRefusesToRemoveTheLastLabel(t *testing.T) {
	cfg := &config.Config{Rules: []config.RuleConfig{{
		Name:    "strip",
		Actions: []config.ActionConfig{{Drop: &config.DropAction{Label: "alertname", Force: true}}},
	}}}
	eng, err := Compile(cfg, fakeRegistry{}, extract.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	a := newAlert(map[string]string{"alertname": "OnlyLabel"})
	results, err := eng.Apply(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}

	labels, err := a.Labels()
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 1 || labels["alertname"] != "OnlyLabel" {
		t.Fatalf("labels = %v, want the last label kept so the alert stays deliverable", labels)
	}
	if len(results) != 1 || len(results[0].DropsRefused) != 1 || results[0].DropsRefused[0] != "alertname" {
		t.Errorf("results = %+v, want the refusal recorded", results)
	}
	if len(results[0].Dropped) != 0 {
		t.Errorf("Dropped = %v, want empty - nothing was actually dropped", results[0].Dropped)
	}
}

// The guard is about the last label only: with more than one, dropping
// works normally, including down to exactly one.
func TestDropStillRemovesLabelsWhileOthersRemain(t *testing.T) {
	cfg := &config.Config{Rules: []config.RuleConfig{{
		Name: "strip",
		Actions: []config.ActionConfig{
			{Drop: &config.DropAction{Label: "team"}},
			{Drop: &config.DropAction{Label: "env"}},
		},
	}}}
	eng, err := Compile(cfg, fakeRegistry{}, extract.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	a := newAlert(map[string]string{"alertname": "X", "team": "a", "env": "prod"})
	if _, err := eng.Apply(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	labels, _ := a.Labels()
	if len(labels) != 1 || labels["alertname"] != "X" {
		t.Fatalf("labels = %v, want both droppable labels removed and alertname kept", labels)
	}
}

// Annotations carry no such constraint - an alert with zero annotations is
// perfectly deliverable.
func TestDropRemovesTheLastAnnotation(t *testing.T) {
	cfg := &config.Config{Rules: []config.RuleConfig{{
		Name:    "strip",
		Actions: []config.ActionConfig{{Drop: &config.DropAction{Annotation: "summary"}}},
	}}}
	eng, err := Compile(cfg, fakeRegistry{}, extract.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	a := alert.Alert{
		"labels":      map[string]any{"alertname": "X"},
		"annotations": map[string]any{"summary": "only one"},
	}
	if _, err := eng.Apply(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	annotations, _ := a.Annotations()
	if len(annotations) != 0 {
		t.Fatalf("annotations = %v, want the last annotation dropped normally", annotations)
	}
}

// ALE-11. Apply used to run every drop in a rule before any of its sets,
// so a rule read one way and behaved another. Actions now execute in the
// order they are written.
func TestActionsRunInDeclaredOrder(t *testing.T) {
	tests := []struct {
		name    string
		actions []config.ActionConfig
		want    map[string]string
	}{
		{
			name: "set then drop leaves the label gone",
			actions: []config.ActionConfig{
				{Set: &config.SetAction{Label: "tmp", Value: "v"}},
				{Drop: &config.DropAction{Label: "tmp"}},
			},
			want: map[string]string{"alertname": "T"},
		},
		{
			name: "drop then set leaves the label set",
			actions: []config.ActionConfig{
				{Drop: &config.DropAction{Label: "tmp"}},
				{Set: &config.SetAction{Label: "tmp", Value: "v"}},
			},
			want: map[string]string{"alertname": "T", "tmp": "v"},
		},
		{
			name: "a later set can read what an earlier one wrote",
			actions: []config.ActionConfig{
				{Set: &config.SetAction{Label: "team", Value: "payments"}},
				{Set: &config.SetAction{Label: "oncall", Template: "{{ .Labels.team }}-oncall"}},
			},
			want: map[string]string{"alertname": "T", "team": "payments", "oncall": "payments-oncall"},
		},
		{
			name: "a set after a drop of the same label sees it absent",
			actions: []config.ActionConfig{
				{Set: &config.SetAction{Label: "env", Value: "first"}},
				{Drop: &config.DropAction{Label: "env"}},
				{Set: &config.SetAction{Label: "env", Value: "second"}},
			},
			want: map[string]string{"alertname": "T", "env": "second"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{Rules: []config.RuleConfig{{Name: "r", Actions: tt.actions}}}
			eng, err := Compile(cfg, fakeRegistry{}, extract.NewCache())
			if err != nil {
				t.Fatal(err)
			}
			a := newAlert(map[string]string{"alertname": "T"})
			if _, err := eng.Apply(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			labels, _ := a.Labels()
			if len(labels) != len(tt.want) {
				t.Fatalf("labels = %v, want %v", labels, tt.want)
			}
			for k, v := range tt.want {
				if labels[k] != v {
					t.Errorf("labels[%q] = %q, want %q (full: %v)", k, labels[k], v, labels)
				}
			}
		})
	}
}

// ALE-12. The most common required-failure path is a clean miss: the
// lookup worked, the jq matched nothing, no default. err is nil there, so
// the message an on-call engineer saw during a live outage read
// "could not be set: <nil>".
func TestRequiredFailureExplainsACleanMiss(t *testing.T) {
	cfg := &config.Config{Rules: []config.RuleConfig{{
		Name:     "team-required",
		Required: true,
		Actions: []config.ActionConfig{{Set: &config.SetAction{
			Label: "team",
			From:  &config.FromConfig{Source: "ns", Jq: `.metadata.labels["team"]`},
		}}},
	}}}
	eng, err := Compile(cfg, fakeRegistry{"ns": &fakeSource{value: map[string]any{"metadata": map[string]any{}}}}, extract.NewCache())
	if err != nil {
		t.Fatal(err)
	}

	_, err = eng.Apply(context.Background(), newAlert(map[string]string{"alertname": "T"}))
	if err == nil {
		t.Fatal("expected a RequiredFailure")
	}
	msg := err.Error()
	if strings.Contains(msg, "<nil>") {
		t.Errorf("the 503 reason still reads %q - it must say what actually went wrong", msg)
	}
	for _, want := range []string{"team-required", `"team"`, "ns", "default"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
}
