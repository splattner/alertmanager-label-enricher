package source

import (
	"context"
	"fmt"
)

// Registry holds the set of configured sources and starts/queries them
// together.
type Registry struct {
	byName map[string]Source
	order  []string
}

// NewRegistry returns an empty source registry.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Source)}
}

// Add registers a source under its own Name(). It is an error to add two
// sources with the same name.
func (r *Registry) Add(s Source) error {
	if _, exists := r.byName[s.Name()]; exists {
		return fmt.Errorf("duplicate source %q", s.Name())
	}
	r.byName[s.Name()] = s
	r.order = append(r.order, s.Name())
	return nil
}

// Get looks up a registered source by name.
func (r *Registry) Get(name string) (Source, bool) {
	s, ok := r.byName[name]
	return s, ok
}

// Start starts every source and waits for the initial sync of each,
// stopping at the first error.
func (r *Registry) Start(ctx context.Context) error {
	for _, name := range r.order {
		if err := r.byName[name].Start(ctx); err != nil {
			return fmt.Errorf("start source %q: %w", name, err)
		}
	}
	return nil
}

// HasSynced reports whether every source has completed its initial sync.
func (r *Registry) HasSynced() bool {
	for _, name := range r.order {
		if !r.byName[name].HasSynced() {
			return false
		}
	}
	return true
}
