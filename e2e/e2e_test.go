//go:build e2e

// Package e2e runs the enricher against a real Alertmanager container: it
// starts the enricher in-process, points it at the container, POSTs an
// alert batch, and asserts the enriched labels reached Alertmanager via its
// own GET /api/v2/alerts. This is what internal/proxy's httptest-based
// tests cannot cover — real container startup and the real Alertmanager API
// contract, across the versions in .github/workflows/e2e.yml's matrix.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/splattner/alertmanager-label-enricher/internal/config"
	"github.com/splattner/alertmanager-label-enricher/internal/engine"
	"github.com/splattner/alertmanager-label-enricher/internal/extract"
	"github.com/splattner/alertmanager-label-enricher/internal/proxy"
	"github.com/splattner/alertmanager-label-enricher/internal/source"
)

// defaultImage is the Alertmanager version compiled in for local, unmatrixed
// runs. .github/workflows/e2e.yml also runs older pinned versions; keep
// them in sync manually (see the comment on the customManager in
// renovate.json, which deliberately does not touch the matrix).
const defaultImage = "quay.io/prometheus/alertmanager:v0.34.0"

type emptyRegistry struct{}

func (emptyRegistry) Get(string) (source.Source, bool) { return nil, false }

func containerRuntime(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("CONTAINER_RUNTIME")
	if bin == "" {
		bin = "docker"
	}
	if _, err := exec.LookPath(bin); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("container runtime %q not found in CI: %v", bin, err)
		}
		t.Skipf("container runtime %q not found, skipping e2e test (set CI=1 to make this a failure)", bin)
	}
	return bin
}

// startAlertmanager runs a real Alertmanager container on a random host
// port and waits for it to report ready.
func startAlertmanager(t *testing.T) (baseURL string) {
	t.Helper()
	runtime := containerRuntime(t)

	image := os.Getenv("E2E_ALERTMANAGER_IMAGE")
	if image == "" {
		image = defaultImage
	}

	// Only stdout carries the container ID; a first-time pull writes
	// progress to stderr (docker) or, on podman, sometimes stdout ahead of
	// the ID, so pre-pulling keeps `run` reliably down to just the ID.
	if out, err := exec.Command(runtime, "pull", image).CombinedOutput(); err != nil {
		t.Fatalf("pull %s: %v\n%s", image, err, out)
	}

	cmd := exec.Command(runtime, "run", "--rm", "-d", "-P", image)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("start alertmanager container: %v\n%s", err, stderr.String())
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	containerID := lines[len(lines)-1]
	t.Cleanup(func() {
		_ = exec.Command(runtime, "rm", "-f", containerID).Run()
	})

	portOut, err := exec.Command(runtime, "port", containerID, "9093/tcp").CombinedOutput()
	if err != nil {
		t.Fatalf("resolve mapped port: %v\n%s", err, portOut)
	}
	// portOut looks like "0.0.0.0:32768\n"; take the last colon-delimited field.
	fields := strings.Fields(string(portOut))
	if len(fields) == 0 {
		t.Fatalf("unexpected `%s port` output: %q", runtime, portOut)
	}
	idx := strings.LastIndex(fields[0], ":")
	if idx == -1 {
		t.Fatalf("unexpected `%s port` output: %q", runtime, portOut)
	}
	port, err := strconv.Atoi(fields[0][idx+1:])
	if err != nil {
		t.Fatalf("parse mapped port from %q: %v", fields[0], err)
	}
	baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/-/ready")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return baseURL
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("alertmanager at %s did not become ready in time", baseURL)
	return ""
}

// startEnricher builds and runs the enricher in-process (via httptest),
// configured with a single rule and pointed at target.
func startEnricher(t *testing.T, target string) *httptest.Server {
	t.Helper()

	cfg := &config.Config{
		Server:     config.ServerConfig{MaxBodyBytes: 1 << 20},
		Targets:    []config.TargetConfig{{URL: target}},
		Forward:    config.ForwardConfig{MinSuccess: 1, Timeout: config.Duration(5 * time.Second)},
		Enrichment: config.EnrichmentConfig{Timeout: config.Duration(3 * time.Second)},
		Rules: []config.RuleConfig{{
			Name:    "mark-e2e",
			Actions: []config.ActionConfig{{Set: &config.SetAction{Label: "environment", Value: "e2e"}}},
		}},
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	eng, err := engine.Compile(cfg, emptyRegistry{}, extract.NewCache())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	srv := proxy.New(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	srv.SetState(&proxy.State{Cfg: cfg, Engine: eng, Synced: func() bool { return true }})

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestE2EEnrichedAlertReachesAlertmanager(t *testing.T) {
	amURL := startAlertmanager(t)
	enricher := startEnricher(t, amURL)

	alertName := fmt.Sprintf("E2ETest-%d", time.Now().UnixNano())
	body := fmt.Sprintf(`[{"labels":{"alertname":%q,"severity":"warning"},"annotations":{"summary":"e2e probe"},"startsAt":%q}]`,
		alertName, time.Now().UTC().Format(time.RFC3339))

	resp, err := http.Post(enricher.URL+"/api/v2/alerts", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST to enricher: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enricher returned %d, want 200", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	labels := waitForAlert(ctx, t, amURL, alertName)

	if labels["environment"] != "e2e" {
		t.Fatalf("alert as seen by Alertmanager has labels %v, want environment=e2e", labels)
	}
}

// waitForAlert polls Alertmanager's own API until it reports the named
// alert, and returns its labels as Alertmanager stored them — the real
// end-to-end assertion, not just "the enricher accepted the POST".
func waitForAlert(ctx context.Context, t *testing.T, amURL, alertName string) map[string]any {
	t.Helper()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("alert %q never appeared in Alertmanager within the deadline", alertName)
		default:
		}

		resp, err := http.Get(amURL + "/api/v2/alerts")
		if err == nil {
			var alerts []map[string]any
			if json.NewDecoder(resp.Body).Decode(&alerts) == nil {
				for _, a := range alerts {
					labels, _ := a["labels"].(map[string]any)
					if labels != nil && labels["alertname"] == alertName {
						_ = resp.Body.Close()
						return labels
					}
				}
			}
			_ = resp.Body.Close()
		}
		time.Sleep(300 * time.Millisecond)
	}
}
