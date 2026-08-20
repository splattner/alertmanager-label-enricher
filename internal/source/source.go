// Package source defines the lookup-source abstraction shared by the
// kubernetes, http and file source implementations.
package source

import "context"

// LookupInput carries the alert's labels/annotations into a source, for
// sources that need to template a request (an HTTP URL, a K8s object name).
type LookupInput struct {
	Labels      map[string]string
	Annotations map[string]string
}

// Source resolves a lookup input to a JSON-shaped value (map[string]any,
// []any, string, float64, bool, or nil) that extract.Query can run a jq
// expression against.
type Source interface {
	Name() string

	// Start begins any background work (informer sync, file watch). It must
	// return once startup is complete or ctx is done; ongoing work continues
	// in the background. No-op for sources with nothing to start.
	Start(ctx context.Context) error

	// HasSynced reports whether the source is ready to serve lookups.
	// Sources with no warm-up state (http) always return true.
	HasSynced() bool

	// Lookup resolves the input to a value. A "not found" result is
	// returned as (nil, nil), not an error — only transport/API failures
	// are errors.
	Lookup(ctx context.Context, in LookupInput) (any, error)
}
