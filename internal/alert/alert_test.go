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

func TestAnnotationsReturnsNilWhenAbsent(t *testing.T) {
	a := Alert{}
	if got := a.Annotations(); got != nil {
		t.Fatalf("Annotations() = %v, want nil", got)
	}
}
