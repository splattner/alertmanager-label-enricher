package proxy

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/splattner/alertmanager-label-enricher/internal/alert"
	"github.com/splattner/alertmanager-label-enricher/internal/config"
	"github.com/splattner/alertmanager-label-enricher/internal/engine"
	"github.com/splattner/alertmanager-label-enricher/internal/extract"
	"github.com/splattner/alertmanager-label-enricher/internal/source"
	"github.com/splattner/alertmanager-label-enricher/internal/tlsutil"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type emptyRegistry struct{}

func (emptyRegistry) Get(string) (source.Source, bool) { return nil, false }

func mustEngine(t *testing.T, cfg *config.Config) *engine.Engine {
	t.Helper()
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	eng, err := engine.Compile(cfg, emptyRegistry{}, extract.NewCache())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return eng
}

func newTestServer(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	srv := New(discardLogger())
	srv.SetState(&State{Cfg: cfg, Engine: mustEngine(t, cfg), Synced: func() bool { return true }})
	return srv
}

func TestForwardsEnrichedBatchPreservingUnknownFields(t *testing.T) {
	var received []byte
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	cfg := &config.Config{
		Server:     config.ServerConfig{MaxBodyBytes: 1 << 20},
		Targets:    []config.TargetConfig{{URL: target.URL}},
		Forward:    config.ForwardConfig{MinSuccess: 1, Timeout: config.Duration(time.Second)},
		Enrichment: config.EnrichmentConfig{Timeout: config.Duration(time.Second)},
		Rules: []config.RuleConfig{{
			Name:    "mark",
			Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "team", Value: "platform"}}},
		}},
	}
	srv := newTestServer(t, cfg)

	body := `[{"labels":{"alertname":"Test"},"generatorURL":"http://prom/g"}]`
	req := httptest.NewRequest(http.MethodPost, "/api/v2/alerts", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got []map[string]any
	if err := json.Unmarshal(received, &got); err != nil {
		t.Fatalf("target received invalid JSON: %v (%s)", err, received)
	}
	if got[0]["generatorURL"] != "http://prom/g" {
		t.Errorf("generatorURL not preserved: %v", got[0])
	}
	labels := got[0]["labels"].(map[string]any)
	if labels["team"] != "platform" {
		t.Errorf("team label not applied: %v", labels)
	}
}

func TestForwardSucceedsWithOneOfTwoTargetsUp(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{MaxBodyBytes: 1 << 20},
		Targets: []config.TargetConfig{
			{URL: "http://127.0.0.1:1"}, // nothing listens on port 1: connection refused
			{URL: up.URL},
		},
		Forward:    config.ForwardConfig{MinSuccess: 1, Timeout: config.Duration(time.Second)},
		Enrichment: config.EnrichmentConfig{Timeout: config.Duration(time.Second)},
	}
	srv := newTestServer(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/api/v2/alerts", strings.NewReader(`[{"labels":{"alertname":"Test"}}]`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 (minSuccess=1 met by the up target)", rec.Code, rec.Body.String())
	}
}

func TestForwardFailsWhenMinSuccessNotMet(t *testing.T) {
	cfg := &config.Config{
		Server:     config.ServerConfig{MaxBodyBytes: 1 << 20},
		Targets:    []config.TargetConfig{{URL: "http://127.0.0.1:1"}},
		Forward:    config.ForwardConfig{MinSuccess: 1, Timeout: config.Duration(time.Second)},
		Enrichment: config.EnrichmentConfig{Timeout: config.Duration(time.Second)},
	}
	srv := newTestServer(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/api/v2/alerts", strings.NewReader(`[{"labels":{"alertname":"Test"}}]`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestRequiredRuleFailureReturns503AndDoesNotForward(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	cfg := &config.Config{
		Server:     config.ServerConfig{MaxBodyBytes: 1 << 20},
		Targets:    []config.TargetConfig{{URL: target.URL}},
		Forward:    config.ForwardConfig{MinSuccess: 1, Timeout: config.Duration(time.Second)},
		Enrichment: config.EnrichmentConfig{Timeout: config.Duration(time.Second)},
		Sources:    []config.SourceConfig{{Name: "cmdb", Type: "http", HTTP: &config.HTTPSourceSpec{URL: "http://cmdb", AllowedHosts: []string{"cmdb"}}}},
		Rules: []config.RuleConfig{{
			Name:     "required-tier",
			Required: true,
			Actions: []config.ActionConfig{{Set: &config.SetAction{
				Label: "tier",
				From:  &config.FromConfig{Source: "cmdb", Jq: ".tier"},
			}}},
		}},
	}
	// emptyRegistry.Get always returns not-found, which the engine treats
	// as "source not registered" — an error, triggering the required path.
	srv := newTestServer(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/api/v2/alerts", strings.NewReader(`[{"labels":{"alertname":"Test"}}]`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s, want 503", rec.Code, rec.Body.String())
	}
	if called {
		t.Fatal("target must not be called when a required rule fails")
	}
}

func TestReadyzReflectsSyncState(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{MaxBodyBytes: 1 << 20}, Targets: []config.TargetConfig{{URL: "http://x"}}}
	srv := New(discardLogger())
	srv.SetState(&State{Cfg: cfg, Engine: mustEngine(t, cfg), Synced: func() bool { return false }})

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503 while not synced", rec.Code)
	}
}

func TestHealthzAlwaysOK(t *testing.T) {
	srv := New(discardLogger())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", rec.Code)
	}
}

// tlsTargetCAFile starts an httptest TLS server and writes its certificate
// to a PEM file under t.TempDir(), for exercising forward.tls the same way
// a deployment would: a CA file on disk.
func tlsTargetCAFile(t *testing.T, target *httptest.Server) string {
	t.Helper()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: target.Certificate().Raw})
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestForwardOverTLSWithTrustedCA(t *testing.T) {
	var received []byte
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	caFile := tlsTargetCAFile(t, target)

	tlsCfg, err := tlsutil.ClientConfig(caFile, "", "", false)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Server:     config.ServerConfig{MaxBodyBytes: 1 << 20},
		Targets:    []config.TargetConfig{{URL: target.URL}},
		Forward:    config.ForwardConfig{MinSuccess: 1, Timeout: config.Duration(time.Second)},
		Enrichment: config.EnrichmentConfig{Timeout: config.Duration(time.Second)},
	}
	srv := New(discardLogger())
	srv.SetState(&State{
		Cfg:           cfg,
		Engine:        mustEngine(t, cfg),
		Synced:        func() bool { return true },
		ForwardClient: &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v2/alerts", strings.NewReader(`[{"labels":{"alertname":"Test"}}]`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(received) == 0 {
		t.Fatal("target received no body")
	}
}

func TestForwardOverTLSWithoutTrustedCAFails(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	// No forward.tls configured: the default client only trusts the
	// system pool, which does not include the test server's certificate.
	cfg := &config.Config{
		Server:     config.ServerConfig{MaxBodyBytes: 1 << 20},
		Targets:    []config.TargetConfig{{URL: target.URL}},
		Forward:    config.ForwardConfig{MinSuccess: 1, Timeout: config.Duration(time.Second)},
		Enrichment: config.EnrichmentConfig{Timeout: config.Duration(time.Second)},
	}
	srv := newTestServer(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/api/v2/alerts", strings.NewReader(`[{"labels":{"alertname":"Test"}}]`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (untrusted certificate)", rec.Code)
	}
}

func TestForwardRetriesUntilSuccess(t *testing.T) {
	var attempts int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	cfg := &config.Config{
		Server:     config.ServerConfig{MaxBodyBytes: 1 << 20},
		Targets:    []config.TargetConfig{{URL: target.URL}},
		Forward:    config.ForwardConfig{MinSuccess: 1, Timeout: config.Duration(time.Second), Retries: 2},
		Enrichment: config.EnrichmentConfig{Timeout: config.Duration(time.Second)},
	}
	srv := newTestServer(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/api/v2/alerts", strings.NewReader(`[{"labels":{"alertname":"Test"}}]`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 after retries", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt64(&attempts); got != 3 {
		t.Fatalf("target received %d attempts, want 3 (1 initial + 2 retries)", got)
	}
}

func TestForwardStopsAfterExhaustingRetries(t *testing.T) {
	var attempts int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&attempts, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer target.Close()

	cfg := &config.Config{
		Server:     config.ServerConfig{MaxBodyBytes: 1 << 20},
		Targets:    []config.TargetConfig{{URL: target.URL}},
		Forward:    config.ForwardConfig{MinSuccess: 1, Timeout: config.Duration(time.Second), Retries: 2},
		Enrichment: config.EnrichmentConfig{Timeout: config.Duration(time.Second)},
	}
	srv := newTestServer(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/api/v2/alerts", strings.NewReader(`[{"labels":{"alertname":"Test"}}]`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 once retries are exhausted", rec.Code)
	}
	if got := atomic.LoadInt64(&attempts); got != 3 {
		t.Fatalf("target received %d attempts, want exactly 3 (1 initial + 2 retries, no more)", got)
	}
}

func TestForwardDefaultIsNoRetries(t *testing.T) {
	var attempts int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&attempts, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer target.Close()

	cfg := &config.Config{
		Server:     config.ServerConfig{MaxBodyBytes: 1 << 20},
		Targets:    []config.TargetConfig{{URL: target.URL}},
		Forward:    config.ForwardConfig{MinSuccess: 1, Timeout: config.Duration(time.Second)}, // Retries: 0 (zero value)
		Enrichment: config.EnrichmentConfig{Timeout: config.Duration(time.Second)},
	}
	srv := newTestServer(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/api/v2/alerts", strings.NewReader(`[{"labels":{"alertname":"Test"}}]`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if got := atomic.LoadInt64(&attempts); got != 1 {
		t.Fatalf("target received %d attempts, want exactly 1 (retries default to 0)", got)
	}
}

// trackingSource records how many Lookup calls are in flight at once, and
// blocks until release is closed - used to prove enrich() actually runs
// alerts concurrently, bounded by maxConcurrency.
type trackingSource struct {
	release chan struct{}

	mu      sync.Mutex
	cur     int
	maxSeen int
}

func (s *trackingSource) Name() string                { return "track" }
func (s *trackingSource) Start(context.Context) error { return nil }
func (s *trackingSource) HasSynced() bool             { return true }
func (s *trackingSource) Lookup(_ context.Context, _ source.LookupInput) (any, error) {
	s.mu.Lock()
	s.cur++
	if s.cur > s.maxSeen {
		s.maxSeen = s.cur
	}
	s.mu.Unlock()

	<-s.release

	s.mu.Lock()
	s.cur--
	s.mu.Unlock()
	return "value", nil
}

type trackingRegistry struct{ src source.Source }

func (r trackingRegistry) Get(name string) (source.Source, bool) {
	if name == "track" {
		return r.src, true
	}
	return nil, false
}

func TestEnrichBoundsConcurrencyByMaxConcurrency(t *testing.T) {
	const maxConcurrency = 2
	const alertCount = 6

	track := &trackingSource{release: make(chan struct{})}
	cfg := &config.Config{
		Targets: []config.TargetConfig{{URL: "http://alertmanager"}},
		Sources: []config.SourceConfig{{Name: "track", Type: "file", File: &config.FileSourceSpec{Path: "/dev/null"}}},
		Rules: []config.RuleConfig{{
			Name: "lookup",
			Actions: []config.ActionConfig{{Set: &config.SetAction{
				Label: "team",
				From:  &config.FromConfig{Source: "track", Jq: "."},
			}}},
		}},
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	eng, err := engine.Compile(cfg, trackingRegistry{src: track}, extract.NewCache())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	var alerts []alert.Alert
	for i := 0; i < alertCount; i++ {
		batch, err := alert.DecodeBatch([]byte(fmt.Sprintf(`[{"labels":{"alertname":"Test%d"}}]`, i)))
		if err != nil {
			t.Fatal(err)
		}
		alerts = append(alerts, batch[0])
	}

	srv := New(discardLogger())
	done := make(chan error, 1)
	go func() {
		done <- srv.enrich(context.Background(), eng, alerts, maxConcurrency)
	}()

	deadline := time.After(time.Second)
	for {
		track.mu.Lock()
		cur := track.cur
		track.mu.Unlock()
		if cur == maxConcurrency {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("never observed %d concurrent lookups (currently %d)", maxConcurrency, cur)
		case <-time.After(5 * time.Millisecond):
		}
	}
	close(track.release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("enrich returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("enrich did not return after lookups were released")
	}

	track.mu.Lock()
	defer track.mu.Unlock()
	if track.maxSeen != maxConcurrency {
		t.Fatalf("max concurrent lookups seen = %d, want exactly %d (maxConcurrency should cap it, not just allow reaching it)", track.maxSeen, maxConcurrency)
	}
}

func TestForwardStragglerSurvivesRequestContextCancellation(t *testing.T) {
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer fast.Close()

	slowDone := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		close(slowDone)
	}))
	defer slow.Close()

	cfg := &config.Config{
		Server:     config.ServerConfig{MaxBodyBytes: 1 << 20},
		Targets:    []config.TargetConfig{{URL: fast.URL}, {URL: slow.URL}},
		Forward:    config.ForwardConfig{MinSuccess: 1, Timeout: config.Duration(2 * time.Second)},
		Enrichment: config.EnrichmentConfig{Timeout: config.Duration(time.Second)},
	}
	srv := newTestServer(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/v2/alerts", strings.NewReader(`[{"labels":{"alertname":"Test"}}]`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (minSuccess=1 met by the fast target)", rec.Code)
	}

	// Simulate what net/http.Server does once ServeHTTP returns: cancel the
	// request context. The still-in-flight forward to the slow target must
	// not be killed by that - it should be left to finish in the
	// background, bounded only by its own forward.timeout.
	cancel()

	select {
	case <-slowDone:
	case <-time.After(time.Second):
		t.Fatal("forward to the slow target was aborted by request context cancellation instead of finishing in the background")
	}
}
