package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/splattner/alertmanager-label-enricher/internal/source"
)

func hostOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}

func TestLookupDecodesJSONResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tier": "tier-2"})
	}))
	defer srv.Close()

	src, err := New("cmdb", Config{
		Method: "GET", URL: srv.URL, AllowedHosts: []string{hostOf(t, srv.URL)},
		Timeout: time.Second, MaxResponseBytes: 1 << 20, TTL: time.Minute, NegativeTTL: time.Second, MaxEntries: 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	v, err := src.Lookup(context.Background(), source.LookupInput{})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := v.(map[string]any)
	if !ok || m["tier"] != "tier-2" {
		t.Fatalf("Lookup result = %v", v)
	}
}

func TestLookupRejectsDisallowedHost(t *testing.T) {
	src, err := New("cmdb", Config{
		Method: "GET", URL: "http://127.0.0.1:1/x", AllowedHosts: []string{"cmdb.internal"},
		Timeout: time.Second, MaxResponseBytes: 1 << 20, TTL: time.Minute, NegativeTTL: time.Second, MaxEntries: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Lookup(context.Background(), source.LookupInput{}); err == nil {
		t.Fatal("expected an error for a host outside allowedHosts")
	}
}

func TestLookupDoesNotFollowRedirects(t *testing.T) {
	// A redirect could retarget the request outside allowedHosts, so it
	// must not be followed transparently.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://evil.example/steal", http.StatusFound)
	}))
	defer srv.Close()

	src, err := New("cmdb", Config{
		Method: "GET", URL: srv.URL, AllowedHosts: []string{hostOf(t, srv.URL)},
		Timeout: time.Second, MaxResponseBytes: 1 << 20, TTL: time.Minute, NegativeTTL: time.Second, MaxEntries: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Lookup(context.Background(), source.LookupInput{}); err == nil {
		t.Fatal("expected an error since the 302 response is not itself valid JSON/2xx")
	}
}

func TestLookupCachesWithinTTL(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"n": float64(atomic.LoadInt64(&hits))})
	}))
	defer srv.Close()

	src, err := New("cmdb", Config{
		Method: "GET", URL: srv.URL, AllowedHosts: []string{hostOf(t, srv.URL)},
		Timeout: time.Second, MaxResponseBytes: 1 << 20, TTL: time.Hour, NegativeTTL: time.Second, MaxEntries: 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		if _, err := src.Lookup(context.Background(), source.LookupInput{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("backend hit %d times, want 1 (cached)", got)
	}
}

func TestLookupRetriesAfterNegativeTTLExpires(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	src, err := New("cmdb", Config{
		Method: "GET", URL: srv.URL, AllowedHosts: []string{hostOf(t, srv.URL)},
		Timeout: time.Second, MaxResponseBytes: 1 << 20, TTL: time.Hour, NegativeTTL: 20 * time.Millisecond, MaxEntries: 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := src.Lookup(context.Background(), source.LookupInput{}); err == nil {
		t.Fatal("expected an error from the 500 response")
	}
	if _, err := src.Lookup(context.Background(), source.LookupInput{}); err == nil {
		t.Fatal("expected the cached error to still apply immediately")
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("backend hit %d times within negativeTTL, want 1", got)
	}

	time.Sleep(40 * time.Millisecond)
	if _, err := src.Lookup(context.Background(), source.LookupInput{}); err == nil {
		t.Fatal("expected another error once negativeTTL expired")
	}
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Fatalf("backend hit %d times, want 2 after negativeTTL expiry", got)
	}
}

func TestLookupTemplatesURLFromLabels(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	src, err := New("cmdb", Config{
		Method: "GET", URL: srv.URL + "/service/{{ .Labels.service }}", AllowedHosts: []string{hostOf(t, srv.URL)},
		Timeout: time.Second, MaxResponseBytes: 1 << 20, TTL: time.Minute, NegativeTTL: time.Second, MaxEntries: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Lookup(context.Background(), source.LookupInput{Labels: map[string]string{"service": "checkout"}}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/service/checkout" {
		t.Fatalf("requested path = %q, want /service/checkout", gotPath)
	}
}

// ALE-08. singleflight hands the leader's context to everyone who joins
// the flight. When the leader went away - its client disconnected, its
// enrichment deadline passed - the shared fetch was cancelled out from
// under healthy waiters, and the resulting error was then cached under
// negativeTTL, turning one abandoned request into a source-wide outage for
// the full TTL (30s by default).
func TestCancelledCallerDoesNotPoisonOthersOrTheCache(t *testing.T) {
	release := make(chan struct{})
	var hits int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"team":"payments"}`))
	}))
	defer backend.Close()

	s, err := New("cmdb", Config{
		Method: "GET", URL: backend.URL, AllowedHosts: []string{"127.0.0.1"},
		Timeout: 10 * time.Second, MaxResponseBytes: 1 << 20,
		TTL: 10 * time.Minute, NegativeTTL: 30 * time.Second, MaxEntries: 100,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctxA, cancelA := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	var errA, errB error
	var valB any

	wg.Add(1)
	go func() { defer wg.Done(); _, errA = s.Lookup(ctxA, source.LookupInput{}) }()
	waitForHits(t, &hits, 1) // A is the singleflight leader

	wg.Add(1)
	go func() { defer wg.Done(); valB, errB = s.Lookup(context.Background(), source.LookupInput{}) }()
	time.Sleep(100 * time.Millisecond) // let B join A's flight

	cancelA()      // A's caller goes away
	close(release) // the backend would have answered fine
	wg.Wait()

	// A asked for something it can no longer use, and is told so.
	if errA == nil {
		t.Error("the cancelled caller should see its own cancellation")
	}
	// B's context was never cancelled, so B must get the real answer.
	if errB != nil {
		t.Fatalf("a healthy waiter was poisoned by another caller's cancellation: %v", errB)
	}
	m, _ := valB.(map[string]any)
	if m["team"] != "payments" {
		t.Fatalf("healthy waiter got %v, want the fetched value", valB)
	}

	// And nothing negative was cached: the next lookup is served, not failed.
	v, err := s.Lookup(context.Background(), source.LookupInput{})
	if err != nil {
		t.Fatalf("a later lookup failed, so the cancellation was cached: %v", err)
	}
	if m, _ := v.(map[string]any); m["team"] != "payments" {
		t.Fatalf("later lookup got %v, want the cached value", v)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("backend was hit %d times, want 1 - the result should have been cached and shared", got)
	}
}

func waitForHits(t *testing.T, hits *int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(hits) < want {
		if time.Now().After(deadline) {
			t.Fatalf("backend saw %d requests, want %d", atomic.LoadInt32(hits), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A genuine upstream failure must still be cached under negativeTTL - the
// fix must not disable negative caching wholesale.
func TestUpstreamFailureIsStillNegativelyCached(t *testing.T) {
	var hits int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer backend.Close()

	s, err := New("cmdb", Config{
		Method: "GET", URL: backend.URL, AllowedHosts: []string{"127.0.0.1"},
		Timeout: 5 * time.Second, MaxResponseBytes: 1 << 20,
		TTL: 10 * time.Minute, NegativeTTL: 30 * time.Second, MaxEntries: 100,
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if _, err := s.Lookup(context.Background(), source.LookupInput{}); err == nil {
			t.Fatal("expected the upstream 500 to surface as an error")
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("backend was hit %d times, want 1 - a real upstream failure must still be negatively cached", got)
	}
}

// At capacity, expired entries are dead weight already treated as misses,
// so they should go before any live entry is discarded (ALE-17).
func TestCacheEvictsExpiredEntriesBeforeLiveOnes(t *testing.T) {
	s, err := New("cmdb", Config{
		Method: "GET", URL: "http://example.invalid", AllowedHosts: []string{"example.invalid"},
		Timeout: time.Second, MaxResponseBytes: 1 << 20,
		TTL: time.Minute, NegativeTTL: time.Minute, MaxEntries: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Two already-expired entries and one live one, at capacity.
	past := time.Now().Add(-time.Hour)
	s.cache["stale-1"] = cacheEntry{value: "old", expiresAt: past}
	s.cache["stale-2"] = cacheEntry{value: "old", expiresAt: past}
	s.cache["hot"] = cacheEntry{value: "keep-me", expiresAt: time.Now().Add(time.Hour)}

	s.cacheSet("new", "fresh", nil)

	if _, ok := s.cache["hot"]; !ok {
		t.Error("a live entry was evicted while expired entries were available to reclaim")
	}
	if _, ok := s.cache["stale-1"]; ok {
		t.Error("an expired entry survived eviction")
	}
	if _, ok := s.cache["stale-2"]; ok {
		t.Error("an expired entry survived eviction")
	}
	if _, ok := s.cache["new"]; !ok {
		t.Error("the new entry was not stored")
	}
}
