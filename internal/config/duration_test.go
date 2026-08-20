package config

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDurationUnmarshalsHumanReadableString(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`"1m30s"`), &d); err != nil {
		t.Fatal(err)
	}
	if time.Duration(d) != 90*time.Second {
		t.Errorf("got %v, want 90s", time.Duration(d))
	}
}

func TestDurationUnmarshalsNanosecondNumber(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`5000000000`), &d); err != nil {
		t.Fatal(err)
	}
	if time.Duration(d) != 5*time.Second {
		t.Errorf("got %v, want 5s", time.Duration(d))
	}
}

func TestDurationRejectsInvalidString(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`"not-a-duration"`), &d); err == nil {
		t.Fatal("expected an error")
	}
}

func TestDurationRejectsWrongType(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`true`), &d); err == nil {
		t.Fatal("expected an error for a boolean")
	}
}

func TestDurationMarshalRoundTrip(t *testing.T) {
	orig := Duration(90 * time.Second)
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `"1m30s"` {
		t.Errorf("MarshalJSON = %s, want \"1m30s\"", data)
	}

	var got Duration
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != orig {
		t.Errorf("round trip = %v, want %v", time.Duration(got), time.Duration(orig))
	}
}
