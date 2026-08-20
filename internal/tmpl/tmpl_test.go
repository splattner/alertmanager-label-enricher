package tmpl

import "testing"

func TestRenderSubstitutesLabelsAndAnnotations(t *testing.T) {
	got, err := Render("t", "{{ .Labels.namespace }}/{{ .Annotations.summary }}", Data{
		Labels:      map[string]string{"namespace": "payments"},
		Annotations: map[string]string{"summary": "high load"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "payments/high load" {
		t.Fatalf("got %q", got)
	}
}

func TestRenderMissingKeyIsEmptyNotError(t *testing.T) {
	got, err := Render("t", "[{{ .Labels.missing }}]", Data{Labels: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if got != "[]" {
		t.Fatalf("got %q, want a missing key to render as empty string", got)
	}
}

func TestFuncMap(t *testing.T) {
	tests := []struct {
		tmpl string
		want string
	}{
		{`{{ urlquery .Labels.svc }}`, "checkout%2Fapi"},
		{`{{ lower .Labels.svc }}`, "checkout/api"},
		{`{{ upper .Labels.svc }}`, "CHECKOUT/API"},
		{`{{ default "fallback" .Labels.missing }}`, "fallback"},
		{`{{ default "fallback" .Labels.svc }}`, "checkout/api"},
	}
	for _, tt := range tests {
		got, err := Render("t", tt.tmpl, Data{Labels: map[string]string{"svc": "checkout/api"}})
		if err != nil {
			t.Fatalf("%s: %v", tt.tmpl, err)
		}
		if got != tt.want {
			t.Errorf("%s = %q, want %q", tt.tmpl, got, tt.want)
		}
	}
}
