package config

import (
	"os"
	"testing"
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
