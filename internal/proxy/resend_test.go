package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/splattner/alertmanager-label-enricher/internal/config"
	"github.com/splattner/alertmanager-label-enricher/internal/metrics"
	"github.com/splattner/alertmanager-label-enricher/internal/wiring"
)

var namespaceGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// TestResendCycleServesFromCacheNotBackend is the design's central
// correctness claim, exercised end-to-end: Prometheus resends every firing
// alert on its resend interval, so every enrichment rule with a source
// lookup runs again on every cycle. Both source types that talk to a real
// backend (kubernetes, http) must serve repeat lookups for the same input
// from their own cache/lister rather than re-fetching, or this design
// hammers the Kubernetes API server and any HTTP backend in direct
// proportion to firing-alert count and cluster size.
func TestResendCycleServesFromCacheNotBackend(t *testing.T) {
	scheme := runtime.NewScheme()
	kubeClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{namespaceGVR: "NamespaceList"},
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]any{
				"name":   "payments",
				"labels": map[string]any{"team": "platform"},
			},
		}},
	)

	var httpHits int64
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&httpHits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"tier": "tier-1"})
	}))
	defer httpSrv.Close()
	httpHost, err := url.Parse(httpSrv.URL)
	if err != nil {
		t.Fatal(err)
	}

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	cfg := &config.Config{
		Server:     config.ServerConfig{MaxBodyBytes: 1 << 20},
		Targets:    []config.TargetConfig{{URL: target.URL}},
		Forward:    config.ForwardConfig{MinSuccess: 1, Timeout: config.Duration(time.Second)},
		Enrichment: config.EnrichmentConfig{Timeout: config.Duration(time.Second), MaxConcurrency: 8},
		Sources: []config.SourceConfig{
			{Name: "ns", Type: "kubernetes", Kubernetes: &config.KubernetesSourceSpec{
				Version: "v1", Resource: "namespaces", Name: "{{ .Labels.namespace }}",
			}},
			{Name: "cmdb", Type: "http", HTTP: &config.HTTPSourceSpec{
				Method: "GET", URL: httpSrv.URL, AllowedHosts: []string{httpHost.Hostname()},
				Timeout: config.Duration(time.Second), MaxResponseBytes: 1 << 20,
				Cache: config.HTTPCacheSpec{TTL: config.Duration(time.Hour), NegativeTTL: config.Duration(time.Second), MaxEntries: 100},
			}},
		},
		Rules: []config.RuleConfig{
			{Name: "team", Actions: []config.ActionConfig{{Set: &config.SetAction{
				Label: "team", From: &config.FromConfig{Source: "ns", Jq: `.metadata.labels["team"]`},
			}}}},
			{Name: "tier", Actions: []config.ActionConfig{{Set: &config.SetAction{
				Label: "tier", From: &config.FromConfig{Source: "cmdb", Jq: `.tier`},
			}}}},
		},
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	sources, err := wiring.BuildSources(cfg, kubeClient, nil)
	if err != nil {
		t.Fatalf("BuildSources: %v", err)
	}
	eng, err := wiring.BuildEngine(cfg, sources)
	if err != nil {
		t.Fatalf("BuildEngine: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sources.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The informer's initial List+Watch (issued by Start) are legitimate,
	// one-time API calls. Only actions recorded from here on would prove
	// Lookup() is bypassing the lister and hitting the API directly.
	kubeClient.ClearActions()

	srv := New(discardLogger())
	srv.SetState(&State{Cfg: cfg, Engine: eng, Synced: sources.HasSynced})

	const resends = 5
	body := `[{"labels":{"alertname":"Test","namespace":"payments"}}]`
	for i := 0; i < resends; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v2/alerts", strings.NewReader(body))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("resend %d: status = %d, body = %s", i, rec.Code, rec.Body.String())
		}
	}

	for _, action := range kubeClient.Actions() {
		t.Errorf("unexpected Kubernetes API call after informer sync: %s %s", action.GetVerb(), action.GetResource().Resource)
	}
	if got := atomic.LoadInt64(&httpHits); got != 1 {
		t.Fatalf("HTTP backend received %d requests across %d resends, want 1 (the rest must be served from cache)", got, resends)
	}

	wantHits := float64(resends)
	if got := testutil.ToFloat64(metrics.SourceLookupsTotal.WithLabelValues("ns", "hit")); got != wantHits {
		t.Errorf(`ale_source_lookups_total{source="ns",result="hit"} = %v, want %v (every resend must still resolve a value, even when served from cache)`, got, wantHits)
	}
	if got := testutil.ToFloat64(metrics.SourceLookupsTotal.WithLabelValues("cmdb", "hit")); got != wantHits {
		t.Errorf(`ale_source_lookups_total{source="cmdb",result="hit"} = %v, want %v`, got, wantHits)
	}
}
