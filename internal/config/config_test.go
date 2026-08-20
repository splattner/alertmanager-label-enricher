package config

import (
	"os"
	"testing"
	"time"
)

const minimalYAML = `
targets:
  - url: http://alertmanager:9093
`

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Server.Listen != ":9099" {
		t.Errorf("Server.Listen = %q, want :9099", cfg.Server.Listen)
	}
	if cfg.Forward.MinSuccess != 1 {
		t.Errorf("Forward.MinSuccess = %d, want 1", cfg.Forward.MinSuccess)
	}
	if time.Duration(cfg.Forward.Timeout) != 5*time.Second {
		t.Errorf("Forward.Timeout = %v, want 5s", time.Duration(cfg.Forward.Timeout))
	}
}

func TestParseAcceptsHumanReadableDurations(t *testing.T) {
	raw := []byte(`
targets:
  - url: http://alertmanager:9093
forward:
  timeout: 2500ms
  retries: 3
enrichment:
  timeout: 1m30s
sources:
  - name: cmdb
    type: http
    http:
      url: http://cmdb
      allowedHosts: ["cmdb"]
      timeout: 750ms
      cache:
        ttl: 10m
        negativeTTL: 15s
`)
	cfg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	checks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"forward.timeout", time.Duration(cfg.Forward.Timeout), 2500 * time.Millisecond},
		{"enrichment.timeout", time.Duration(cfg.Enrichment.Timeout), 90 * time.Second},
		{"sources[0].http.timeout", time.Duration(cfg.Sources[0].HTTP.Timeout), 750 * time.Millisecond},
		{"sources[0].http.cache.ttl", time.Duration(cfg.Sources[0].HTTP.Cache.TTL), 10 * time.Minute},
		{"sources[0].http.cache.negativeTTL", time.Duration(cfg.Sources[0].HTTP.Cache.NegativeTTL), 15 * time.Second},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if cfg.Forward.Retries != 3 {
		t.Errorf("Forward.Retries = %d, want 3", cfg.Forward.Retries)
	}
}

func TestParseRejectsInvalidDuration(t *testing.T) {
	raw := []byte(`
targets:
  - url: http://alertmanager:9093
forward:
  timeout: not-a-duration
`)
	if _, err := Parse(raw); err == nil {
		t.Fatal("expected an error for an invalid duration string")
	}
}

func TestExpandEnvSubstitutesKnownVars(t *testing.T) {
	t.Setenv("CMDB_TOKEN", "secret-value")
	raw := `
targets:
  - url: http://alertmanager:9093
sources:
  - name: cmdb
    type: http
    http:
      url: http://cmdb
      allowedHosts: ["cmdb"]
      headers:
        Authorization: 'Bearer ${CMDB_TOKEN}'
`
	cfg, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := cfg.Sources[0].HTTP.Headers["Authorization"]
	if got != "Bearer secret-value" {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer secret-value")
	}
}

func TestExpandEnvLeavesUnsetVarUntouched(t *testing.T) {
	_ = os.Unsetenv("ALE_TEST_UNSET_VAR")
	raw := []byte(`targets: [{url: 'http://${ALE_TEST_UNSET_VAR}'}]`)
	cfg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Targets[0].URL != "http://${ALE_TEST_UNSET_VAR}" {
		t.Errorf("URL = %q, want the placeholder left intact", cfg.Targets[0].URL)
	}
}

func TestLoadPropagatesValidationError(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	if err := os.WriteFile(path, []byte("targets: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected Load to surface the validation error")
	}
}
