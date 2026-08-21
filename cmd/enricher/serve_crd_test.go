package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/splattner/alertmanager-label-enricher/internal/config"
	"github.com/splattner/alertmanager-label-enricher/internal/crd"
	"github.com/splattner/alertmanager-label-enricher/internal/engine"
)

// These tests exercise the wiring runServe assembles - buildGeneration,
// the CRD watcher's debounced OnChange, and compileAndSwap - against a
// fake dynamic client instead of a real cluster. internal/crd's own tests
// already prove the watcher's enforcement/listing logic in isolation;
// what's missing there is proof that cmd/enricher actually starts the
// watcher, picks up its rules on the initial config load, and recompiles
// the engine automatically when a CR changes after that.

var namespaceGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
var eventGVR = schema.GroupVersionResource{Version: "v1", Resource: "events"}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func enrichmentRuleObj(ns, name string, spec map[string]any) *unstructured.Unstructured {
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

func namespaceObj(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": name},
	}}
}

func setAnnotationSpec(annotation, value string) map[string]any {
	return map[string]any{
		"actions": []any{
			map[string]any{"set": map[string]any{"annotation": annotation, "value": value}},
		},
	}
}

func newFakeDynamicClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		crd.GVR:      "EnrichmentRuleList",
		namespaceGVR: "NamespaceList",
		eventGVR:     "EventList",
	}, objs...)
}

// writeCRDConfig writes a minimal config.yaml with the CRD watch enabled
// (single-tenant: no enforcement.rules, so every CR passes through
// unrestricted) and forward.targets pointing at target.
func writeCRDConfig(t *testing.T, targetURL string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf(`
server:
  listen: ":0"
  maxBodyBytes: 1048576
targets:
  - url: %s
forward:
  minSuccess: 1
  timeout: 2s
enrichment:
  timeout: 2s
crd:
  enabled: true
`, targetURL)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func postBatch(t *testing.T, s *server) {
	t.Helper()
	body := `[{"labels":{"alertname":"Test"},"generatorURL":"http://prom/g"}]`
	req := httptest.NewRequest(http.MethodPost, "/api/v2/alerts", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.proxy.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestApplyConfigStartsCRDWatchAndCompilesRules(t *testing.T) {
	var received []byte
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	client := newFakeDynamicClient(
		namespaceObj("default"),
		enrichmentRuleObj("default", "add-note", setAnnotationSpec("note", "hello")),
	)

	s := newServer(writeCRDConfig(t, target.URL), discardLogger())
	s.kubeClient = func() (dynamic.Interface, error) { return client, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.applyConfig(ctx); err != nil {
		t.Fatalf("applyConfig: %v", err)
	}

	st := s.proxy.State()
	if st == nil || !st.Synced() {
		t.Fatal("expected Synced() to be true once the CRD watcher's informers have synced")
	}

	postBatch(t, s)

	var got []map[string]any
	if err := json.Unmarshal(received, &got); err != nil {
		t.Fatalf("target received invalid JSON: %v (%s)", err, received)
	}
	annotations, _ := got[0]["annotations"].(map[string]any)
	if annotations["note"] != "hello" {
		t.Fatalf("expected the CR-sourced rule to set annotations.note=hello, got %v", got[0])
	}

	cond := awaitReadyCondition(ctx, t, client, "default", "add-note")
	if cond["type"] != "Ready" || cond["status"] != "True" || cond["reason"] != "Compiled" {
		t.Fatalf("status.conditions[0] = %+v, want Ready/True/Compiled", cond)
	}
}

// awaitReadyCondition polls for the CR's Ready condition. Status writes are
// deliberately asynchronous - the engine swap must never wait on the API
// server - so a test that reads immediately after applyConfig would be
// racing the background writer.
func awaitReadyCondition(ctx context.Context, t *testing.T, client dynamic.Interface, ns, name string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		cr, err := client.Resource(crd.GVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get EnrichmentRule %s/%s: %v", ns, name, err)
		}
		conditions, found, err := unstructured.NestedSlice(cr.Object, "status", "conditions")
		if err != nil {
			t.Fatalf("read status.conditions: %v", err)
		}
		if found && len(conditions) == 1 {
			cond, _ := conditions[0].(map[string]any)
			return cond
		}
		if time.Now().After(deadline) {
			t.Fatalf("no Ready condition written for %s/%s within 5s (conditions=%+v)", ns, name, conditions)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestCRChangeTriggersDebouncedRecompile(t *testing.T) {
	var received []byte
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	client := newFakeDynamicClient(namespaceObj("default"))

	s := newServer(writeCRDConfig(t, target.URL), discardLogger())
	s.kubeClient = func() (dynamic.Interface, error) { return client, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.applyConfig(ctx); err != nil {
		t.Fatalf("applyConfig: %v", err)
	}

	postBatch(t, s)
	var before []map[string]any
	if err := json.Unmarshal(received, &before); err != nil {
		t.Fatalf("target received invalid JSON: %v (%s)", err, received)
	}
	if annotations, _ := before[0]["annotations"].(map[string]any); len(annotations) != 0 {
		t.Fatalf("expected no annotations before the CR exists, got %v", before[0])
	}

	rule := enrichmentRuleObj("default", "add-note", setAnnotationSpec("note", "hello"))
	if _, err := client.Resource(crd.GVR).Namespace("default").Create(ctx, rule, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create EnrichmentRule: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		postBatch(t, s)
		var got []map[string]any
		if err := json.Unmarshal(received, &got); err != nil {
			t.Fatalf("target received invalid JSON: %v (%s)", err, received)
		}
		annotations, _ := got[0]["annotations"].(map[string]any)
		if annotations["note"] == "hello" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("engine was not recompiled with the new EnrichmentRule within 5s of it being created")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A reload that fails must leave the running configuration completely
// intact. Before ALE-04 was fixed, buildGeneration stored the new
// generation and cancelled the old one's context - stopping its informers -
// before the compile could fail, leaving the proxy serving an engine whose
// sources were dead while /readyz stayed green.
//
// The failure is injected through the buildEngine seam rather than through
// a deliberately broken config: what's under test is the ordering of
// build/compile/publish/cancel, which must hold however the compile fails.
func TestFailedReloadLeavesRunningGenerationIntact(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	client := newFakeDynamicClient(namespaceObj("default"))
	s := newServer(writeCRDConfig(t, target.URL), discardLogger())
	s.kubeClient = func() (dynamic.Interface, error) { return client, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.applyConfig(ctx); err != nil {
		t.Fatalf("initial applyConfig: %v", err)
	}
	live := s.currentGen.Load()
	liveState := s.proxy.State()

	s.buildEngine = func(*config.Config, engine.Sources) (*engine.Engine, error) {
		return nil, errors.New("compile blew up")
	}
	if err := s.applyConfig(ctx); err == nil {
		t.Fatal("expected the reload to fail at the compile stage")
	}

	if got := s.currentGen.Load(); got != live {
		t.Error("currentGen was replaced by a reload that failed; the running generation must survive untouched")
	}
	if got := s.proxy.State(); got != liveState {
		t.Error("proxy state was replaced by a reload that failed")
	}
	if !live.watcher.HasSynced() {
		t.Error("the surviving generation's CRD watcher is no longer synced")
	}

	// The decisive check. A cancelled informer keeps reporting HasSynced,
	// so instead assert that the surviving generation still *observes*
	// cluster changes - something a torn-down informer cannot do.
	if _, err := client.Resource(crd.GVR).Namespace("default").Create(ctx,
		enrichmentRuleObj("default", "add-note", setAnnotationSpec("note", "hello")), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create EnrichmentRule: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, r := range live.watcher.Rules() {
			if r.Name == "default/add-note" {
				return // informer still live and watching: the failed reload really was a no-op
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the surviving generation's informer stopped observing changes - the failed reload tore it down")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A CRD watcher whose generation has been superseded must not be able to
// publish its (stale) engine over the live one.
func TestSupersededGenerationCannotPublish(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	client := newFakeDynamicClient(namespaceObj("default"))
	s := newServer(writeCRDConfig(t, target.URL), discardLogger())
	s.kubeClient = func() (dynamic.Interface, error) { return client, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.applyConfig(ctx); err != nil {
		t.Fatalf("first applyConfig: %v", err)
	}
	stale := s.currentGen.Load()

	if err := s.applyConfig(ctx); err != nil {
		t.Fatalf("second applyConfig: %v", err)
	}
	fresh := s.proxy.State()
	if s.currentGen.Load() == stale {
		t.Fatal("second applyConfig did not install a new generation")
	}

	// Simulate the stale watcher's debounce timer firing after the swap.
	if err := s.compileAndSwap(ctx, stale); err != nil {
		t.Fatalf("compileAndSwap on a stale generation returned an error: %v", err)
	}
	if s.proxy.State() != fresh {
		t.Error("a superseded generation republished its engine over the live one")
	}
}

// ALE-05. A CR whose jq engine.Compile would reject used to fail the whole
// compile: on the startup path that propagated out of runServe and the
// process never booted, so any namespace able to create an EnrichmentRule
// could keep the enricher down. It must now be rejected individually,
// leaving every other tenant's rules working.
func TestMalformedCRDoesNotBlockStartup(t *testing.T) {
	var received []byte
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	client := newFakeDynamicClient(
		namespaceObj("attacker"),
		namespaceObj("victim"),
		// Schema-valid, but the jq neither parses nor compiles.
		enrichmentRuleObj("attacker", "poison", map[string]any{
			"actions": []any{map[string]any{"set": map[string]any{
				"label": "x",
				"from":  map[string]any{"source": "nope", "jq": "..[[["},
			}}},
		}),
		enrichmentRuleObj("victim", "add-note", setAnnotationSpec("note", "hello")),
	)

	s := newServer(writeCRDConfig(t, target.URL), discardLogger())
	s.kubeClient = func() (dynamic.Interface, error) { return client, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.applyConfig(ctx); err != nil {
		t.Fatalf("startup failed because of one malformed tenant CR: %v", err)
	}

	// The innocent tenant's rule must still be compiled and working.
	postBatch(t, s)
	var got []map[string]any
	if err := json.Unmarshal(received, &got); err != nil {
		t.Fatalf("target received invalid JSON: %v (%s)", err, received)
	}
	annotations, _ := got[0]["annotations"].(map[string]any)
	if annotations["note"] != "hello" {
		t.Errorf("the valid tenant's rule did not apply: %v", got[0])
	}

	// And the poison CR must be reported to its own author, not silently lost.
	cond := awaitReadyCondition(ctx, t, client, "attacker", "poison")
	if cond["status"] != "False" || cond["reason"] != "PolicyViolation" {
		t.Errorf("condition = %+v, want the rejection reported as False/PolicyViolation", cond)
	}
}
