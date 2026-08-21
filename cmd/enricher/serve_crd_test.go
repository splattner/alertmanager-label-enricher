package main

import (
	"context"
	"encoding/json"
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

	"github.com/splattner/alertmanager-label-enricher/internal/crd"
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

	cr, err := client.Resource(crd.GVR).Namespace("default").Get(ctx, "add-note", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get EnrichmentRule: %v", err)
	}
	conditions, found, err := unstructured.NestedSlice(cr.Object, "status", "conditions")
	if err != nil || !found || len(conditions) != 1 {
		t.Fatalf("status.conditions = %+v (found=%v, err=%v), want exactly one condition", conditions, found, err)
	}
	cond, _ := conditions[0].(map[string]any)
	if cond["type"] != "Ready" || cond["status"] != "True" || cond["reason"] != "Compiled" {
		t.Fatalf("status.conditions[0] = %+v, want Ready/True/Compiled", cond)
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
