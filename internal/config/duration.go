package config

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration is a time.Duration that unmarshals from YAML/JSON as a
// human-readable string ("5s", "2m30s") — plain encoding/json (which
// sigs.k8s.io/yaml converts through) only understands time.Duration as a
// raw integer of nanoseconds, which is not how any of this project's
// config examples write it.
type Duration time.Duration

// MarshalJSON renders d as its human-readable string form (e.g. "1m30s").
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts either a human-readable duration string or a plain
// number of nanoseconds.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	switch v := raw.(type) {
	case string:
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", v, err)
		}
		*d = Duration(parsed)
		return nil
	case float64:
		*d = Duration(time.Duration(v))
		return nil
	default:
		return fmt.Errorf("invalid duration: %v", raw)
	}
}
