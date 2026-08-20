package extract

import "testing"

func TestQueryRun(t *testing.T) {
	tests := []struct {
		name        string
		jq          string
		regex       string
		input       any
		labels      map[string]string
		annotations map[string]string
		wantValue   string
		wantOK      bool
		wantErr     bool
	}{
		{
			name:      "simple field access",
			jq:        `.metadata.labels["team"]`,
			input:     map[string]any{"metadata": map[string]any{"labels": map[string]any{"team": "platform"}}},
			wantValue: "platform",
			wantOK:    true,
		},
		{
			name:   "missing field yields not-ok",
			jq:     `.metadata.labels["team"]`,
			input:  map[string]any{"metadata": map[string]any{"labels": map[string]any{}}},
			wantOK: false,
		},
		{
			name:      "labels variable is bound",
			jq:        `$labels.namespace`,
			input:     nil,
			labels:    map[string]string{"namespace": "payments"},
			wantValue: "payments",
			wantOK:    true,
		},
		{
			name:      "regex capture group",
			jq:        `.tier`,
			regex:     `^tier-(\d)$`,
			input:     map[string]any{"tier": "tier-2"},
			wantValue: "2",
			wantOK:    true,
		},
		{
			name:   "regex no match yields not-ok",
			jq:     `.tier`,
			regex:  `^tier-(\d)$`,
			input:  map[string]any{"tier": "nope"},
			wantOK: false,
		},
		{
			name:      "numeric result is stringified",
			jq:        `.count`,
			input:     map[string]any{"count": 3.0},
			wantValue: "3",
			wantOK:    true,
		},
		{
			name:    "non-scalar result is an error",
			jq:      `.metadata`,
			input:   map[string]any{"metadata": map[string]any{"a": "b"}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := Compile(tt.jq, tt.regex)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			value, ok, err := q.Run(tt.input, tt.labels, tt.annotations)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Run error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if ok != tt.wantOK {
				t.Fatalf("Run ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && value != tt.wantValue {
				t.Fatalf("Run value = %q, want %q", value, tt.wantValue)
			}
		})
	}
}

func TestCacheDedupesCompilation(t *testing.T) {
	c := NewCache()
	q1, err := c.Compile(".a", "")
	if err != nil {
		t.Fatal(err)
	}
	q2, err := c.Compile(".a", "")
	if err != nil {
		t.Fatal(err)
	}
	if q1 != q2 {
		t.Fatal("expected the same compiled query to be reused")
	}

	q3, err := c.Compile(".a", "^x$")
	if err != nil {
		t.Fatal(err)
	}
	if q1 == q3 {
		t.Fatal("expected a different regex to produce a different cache entry")
	}
}
