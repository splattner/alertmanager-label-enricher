// Package alert decodes and re-encodes Prometheus alert batches while
// preserving every field, known or not, except the labels map it exists to
// mutate.
package alert

import (
	"encoding/json"
	"fmt"
)

// Alert is a single alert from a Prometheus POST body, kept as a generic map
// so fields this package does not know about (annotations, generatorURL,
// future additions) survive the round trip untouched.
type Alert map[string]any

// Labels returns the alert's label map, creating it if absent. The returned
// map aliases the alert's own storage, so mutations are visible immediately.
func (a Alert) Labels() (map[string]string, error) {
	raw, ok := a["labels"]
	if !ok || raw == nil {
		labels := map[string]string{}
		a["labels"] = labels
		return labels, nil
	}

	switch v := raw.(type) {
	case map[string]string:
		return v, nil
	case map[string]any:
		labels := make(map[string]string, len(v))
		for k, val := range v {
			s, ok := val.(string)
			if !ok {
				return nil, fmt.Errorf("label %q has non-string value %T", k, val)
			}
			labels[k] = s
		}
		a["labels"] = labels
		return labels, nil
	default:
		return nil, fmt.Errorf("labels field has unexpected type %T", raw)
	}
}

// Annotations returns the alert's annotation map, read-only for the purposes
// of this package (used only as templating/jq input).
func (a Alert) Annotations() map[string]string {
	raw, ok := a["annotations"]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case map[string]string:
		return v
	case map[string]any:
		out := make(map[string]string, len(v))
		for k, val := range v {
			if s, ok := val.(string); ok {
				out[k] = s
			}
		}
		return out
	default:
		return nil
	}
}

// DecodeBatch parses a Prometheus alert batch, preserving unknown fields on
// every alert.
func DecodeBatch(data []byte) ([]Alert, error) {
	var alerts []Alert
	if err := json.Unmarshal(data, &alerts); err != nil {
		return nil, fmt.Errorf("decode alert batch: %w", err)
	}
	return alerts, nil
}

// EncodeBatch re-serializes a batch after enrichment.
func EncodeBatch(alerts []Alert) ([]byte, error) {
	data, err := json.Marshal(alerts)
	if err != nil {
		return nil, fmt.Errorf("encode alert batch: %w", err)
	}
	return data, nil
}
