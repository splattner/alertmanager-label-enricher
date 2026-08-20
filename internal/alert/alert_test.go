package alert

import (
	"encoding/json"
	"testing"
)

func TestDecodeEncodePreservesUnknownFields(t *testing.T) {
	in := `[{"labels":{"alertname":"Test","namespace":"payments"},"annotations":{"summary":"x"},"startsAt":"2026-08-20T10:00:00Z","generatorURL":"http://prom/graph"}]`

	alerts, err := DecodeBatch([]byte(in))
	if err != nil {
		t.Fatalf("DecodeBatch: %v", err)
	}
	labels, err := alerts[0].Labels()
	if err != nil {
		t.Fatal(err)
	}
	labels["team"] = "platform"

	out, err := EncodeBatch(alerts)
	if err != nil {
		t.Fatalf("EncodeBatch: %v", err)
	}

	var roundTripped []map[string]any
	if err := json.Unmarshal(out, &roundTripped); err != nil {
		t.Fatal(err)
	}
	got := roundTripped[0]
	if got["generatorURL"] != "http://prom/graph" {
		t.Errorf("generatorURL not preserved: %v", got["generatorURL"])
	}
	if got["startsAt"] != "2026-08-20T10:00:00Z" {
		t.Errorf("startsAt not preserved: %v", got["startsAt"])
	}
	gotLabels := got["labels"].(map[string]any)
	if gotLabels["team"] != "platform" || gotLabels["alertname"] != "Test" {
		t.Errorf("labels = %v", gotLabels)
	}
	gotAnnotations := got["annotations"].(map[string]any)
	if gotAnnotations["summary"] != "x" {
		t.Errorf("annotations = %v", gotAnnotations)
	}
}

func TestLabelsCreatesMissingMap(t *testing.T) {
	a := Alert{}
	labels, err := a.Labels()
	if err != nil {
		t.Fatal(err)
	}
	labels["x"] = "y"
	if a["labels"].(map[string]string)["x"] != "y" {
		t.Fatal("mutation through Labels() did not alias the alert's storage")
	}
}

func TestAnnotationsCreatesMissingMap(t *testing.T) {
	a := Alert{}
	annotations, err := a.Annotations()
	if err != nil {
		t.Fatal(err)
	}
	if annotations == nil {
		t.Fatal("Annotations() returned nil, want an empty non-nil map")
	}
	annotations["x"] = "y"
	if a["annotations"].(map[string]string)["x"] != "y" {
		t.Fatal("mutation through Annotations() did not alias the alert's storage")
	}
}

func TestAnnotationsAliasesExistingMap(t *testing.T) {
	a := Alert{"annotations": map[string]any{"summary": "x"}}
	annotations, err := a.Annotations()
	if err != nil {
		t.Fatal(err)
	}
	annotations["runbook_url"] = "https://wiki/runbook"

	again, err := a.Annotations()
	if err != nil {
		t.Fatal(err)
	}
	if again["runbook_url"] != "https://wiki/runbook" {
		t.Fatal("a second Annotations() call did not observe a mutation made through the first - annotations must alias like labels do, or rule chaining silently breaks")
	}
}

func TestAnnotationsRejectsNonStringValue(t *testing.T) {
	a := Alert{"annotations": map[string]any{"count": 3}}
	if _, err := a.Annotations(); err == nil {
		t.Fatal("expected an error for a non-string annotation value")
	}
}
