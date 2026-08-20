// Package alert decodes and re-encodes Prometheus alert batches while
// preserving every field, known or not, except the labels/annotations maps
// it exists to mutate. An alert with no annotations at all gains an empty
// "annotations":{} once Annotations() is called on it (mirroring Labels()),
// which Alertmanager treats identically to the field being absent.
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

// Annotations returns the alert's annotation map, creating it if absent.
// The returned map aliases the alert's own storage, so mutations are
// visible immediately - mirroring Labels().
func (a Alert) Annotations() (map[string]string, error) {
	raw, ok := a["annotations"]
	if !ok || raw == nil {
		annotations := map[string]string{}
		a["annotations"] = annotations
		return annotations, nil
	}

	switch v := raw.(type) {
	case map[string]string:
		return v, nil
	case map[string]any:
		annotations := make(map[string]string, len(v))
		for k, val := range v {
			s, ok := val.(string)
			if !ok {
				return nil, fmt.Errorf("annotation %q has non-string value %T", k, val)
			}
			annotations[k] = s
		}
		a["annotations"] = annotations
		return annotations, nil
	default:
		return nil, fmt.Errorf("annotations field has unexpected type %T", raw)
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
