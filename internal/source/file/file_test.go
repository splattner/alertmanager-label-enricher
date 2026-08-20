package file

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/splattner/alertmanager-label-enricher/internal/source"
)

func TestLookupReturnsParsedContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "teams.yaml")
	if err := os.WriteFile(path, []byte("namespaces:\n  payments:\n    team: platform\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	src, err := New("teams", path, nil)
	if err != nil {
		t.Fatal(err)
	}
	v, err := src.Lookup(context.Background(), source.LookupInput{})
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	ns := m["namespaces"].(map[string]any)
	payments := ns["payments"].(map[string]any)
	if payments["team"] != "platform" {
		t.Fatalf("team = %v", payments["team"])
	}
}

func TestNewFailsOnMissingFile(t *testing.T) {
	if _, err := New("teams", "/nonexistent/path.yaml", nil); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestReloadOnWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "teams.yaml")
	if err := os.WriteFile(path, []byte("v: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	src, err := New("teams", path, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := src.Start(ctx); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("v: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		v, err := src.Lookup(context.Background(), source.LookupInput{})
		if err != nil {
			t.Fatal(err)
		}
		if v.(map[string]any)["v"] == float64(2) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("file source did not pick up the change within the deadline")
}
