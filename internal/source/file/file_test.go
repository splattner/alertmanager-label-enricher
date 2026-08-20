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

func TestNewRejectsEmptyDocument(t *testing.T) {
	// An empty (or all-null) YAML document unmarshals successfully to nil
	// rather than erroring; it must still be rejected, otherwise a torn
	// read caught mid-write could silently replace good cached data with
	// nothing without ever surfacing an error.
	path := filepath.Join(t.TempDir(), "teams.yaml")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New("teams", path, nil); err == nil {
		t.Fatal("expected New to reject an empty document")
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

	// Write-then-rename, not a truncating in-place write: matches how a
	// ConfigMap volume actually updates (an atomic symlink swap) and
	// avoids a reload racing a torn read of a half-written file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte("v: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		v, err := src.Lookup(context.Background(), source.LookupInput{})
		if err != nil {
			t.Fatal(err)
		}
		if m, ok := v.(map[string]any); ok && m["v"] == float64(2) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("file source did not pick up the change within the deadline")
}
