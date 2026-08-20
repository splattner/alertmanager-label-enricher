package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
