// Package http implements a lookup source that calls a templated HTTP
// endpoint and caches the decoded JSON response.
package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"text/template"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/splattner/alertmanager-label-enricher/internal/source"
	"github.com/splattner/alertmanager-label-enricher/internal/tmpl"
)

// Config configures an HTTP lookup source.
type Config struct {
	Method           string
	URL              string // template text, rendered against the alert
	Headers          map[string]string
	AllowedHosts     []string
	Timeout          time.Duration
	MaxResponseBytes int64
	TTL              time.Duration
	NegativeTTL      time.Duration
	MaxEntries       int
}

type cacheEntry struct {
	value     any
	err       error
	expiresAt time.Time
}

// Source calls a templated HTTP endpoint and caches the decoded response.
// A single alert batch commonly contains many alerts resolving to the same
// URL (e.g. several alerts for one namespace), so lookups are deduplicated
// with singleflight in addition to being cached.
type Source struct {
	name string
	cfg  Config

	urlTmpl *template.Template
	client  *http.Client
	group   singleflight.Group

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// New builds an HTTP lookup source from cfg.
func New(name string, cfg Config) (*Source, error) {
	urlTmpl, err := tmpl.Compile(name+"-url", cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("http source %q: %w", name, err)
	}
	return &Source{
		name:    name,
		cfg:     cfg,
		urlTmpl: urlTmpl,
		client:  &http.Client{Timeout: cfg.Timeout},
		cache:   make(map[string]cacheEntry),
	}, nil
}

// Name returns the source's configured name.
func (s *Source) Name() string { return s.name }

// HasSynced always returns true: an HTTP source has no warm-up state.
func (s *Source) HasSynced() bool { return true }

// Start is a no-op: an HTTP source does nothing until its first Lookup.
func (s *Source) Start(_ context.Context) error { return nil }

// Lookup renders the source's URL template against in, then fetches and
// caches the response (or reuses an unexpired cache entry).
func (s *Source) Lookup(ctx context.Context, in source.LookupInput) (any, error) {
	target, err := s.renderURL(in)
	if err != nil {
		return nil, err
	}
	if err := s.checkAllowedHost(target); err != nil {
		return nil, err
	}

	key := s.cfg.Method + " " + target
	if v, ok, err := s.cacheGet(key); ok {
		return v, err
	}

	res, err, _ := s.group.Do(key, func() (any, error) {
		v, ferr := s.fetch(ctx, target)
		s.cacheSet(key, v, ferr)
		return v, ferr
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Source) renderURL(in source.LookupInput) (string, error) {
	var buf strings.Builder
	if err := s.urlTmpl.Execute(&buf, tmpl.Data{Labels: in.Labels, Annotations: in.Annotations}); err != nil {
		return "", fmt.Errorf("http source %q: render url: %w", s.name, err)
	}
	return buf.String(), nil
}

func (s *Source) checkAllowedHost(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("http source %q: parse url %q: %w", s.name, rawURL, err)
	}
	if !slices.Contains(s.cfg.AllowedHosts, u.Hostname()) {
		return fmt.Errorf("http source %q: host %q is not in allowedHosts", s.name, u.Hostname())
	}
	return nil
}

// fetch performs the request without following redirects: a redirect could
// otherwise retarget the request to a host outside allowedHosts.
func (s *Source) fetch(ctx context.Context, target string) (any, error) {
	req, err := http.NewRequestWithContext(ctx, s.cfg.Method, target, nil)
	if err != nil {
		return nil, fmt.Errorf("http source %q: build request: %w", s.name, err)
	}
	for k, v := range s.cfg.Headers {
		req.Header.Set(k, v)
	}

	client := *s.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http source %q: request: %w", s.name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http source %q: unexpected status %d from %s", s.name, resp.StatusCode, target)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, s.cfg.MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("http source %q: read response: %w", s.name, err)
	}
	if int64(len(body)) > s.cfg.MaxResponseBytes {
		return nil, fmt.Errorf("http source %q: response exceeds maxResponseBytes (%d)", s.name, s.cfg.MaxResponseBytes)
	}

	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("http source %q: decode json response: %w", s.name, err)
	}
	return v, nil
}

func (s *Source) cacheGet(key string) (any, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.cache[key]
	if !ok || time.Now().After(e.expiresAt) {
		return nil, false, nil
	}
	return e.value, true, e.err
}

func (s *Source) cacheSet(key string, v any, err error) {
	ttl := s.cfg.TTL
	if err != nil {
		ttl = s.cfg.NegativeTTL
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.MaxEntries > 0 && len(s.cache) >= s.cfg.MaxEntries {
		// Cheapest possible eviction under load: drop an arbitrary entry
		// rather than track LRU order for a cache this size.
		for k := range s.cache {
			delete(s.cache, k)
			break
		}
	}
	s.cache[key] = cacheEntry{value: v, err: err, expiresAt: time.Now().Add(ttl)}
}
