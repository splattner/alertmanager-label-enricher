// Package file implements a lookup source backed by a YAML or JSON file,
// reloaded when it changes on disk.
package file

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
	"sigs.k8s.io/yaml"

	"github.com/splattner/alertmanager-label-enricher/internal/source"
)

// Source is a lookup source backed by a YAML or JSON file.
type Source struct {
	name string
	path string

	data atomic.Pointer[any]

	logf func(format string, args ...any)
}

// New loads path once. logf may be nil.
func New(name, path string, logf func(format string, args ...any)) (*Source, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Source{name: name, path: path, logf: logf}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Name returns the source's configured name.
func (s *Source) Name() string { return s.name }

// HasSynced reports whether the file has been loaded at least once.
func (s *Source) HasSynced() bool { return s.data.Load() != nil }

// Start watches the file's parent directory (not the file itself: a
// ConfigMap volume update replaces the file via a symlink swap, which does
// not fire inotify events on a watch held on the old inode) and reloads on
// any write/create/rename affecting the file's basename.
func (s *Source) Start(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("file source %q: create watcher: %w", s.name, err)
	}

	dir := filepath.Dir(s.path)
	if err := watcher.Add(dir); err != nil {
		_ = watcher.Close()
		return fmt.Errorf("file source %q: watch %s: %w", s.name, dir, err)
	}

	base := filepath.Base(s.path)
	go func() {
		defer func() { _ = watcher.Close() }()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-watcher.Events:
				if !ok {
					return
				}
				if filepath.Base(ev.Name) != base {
					continue
				}
				if err := s.reload(); err != nil {
					s.logf("file source %q: reload %s failed: %v", s.name, s.path, err)
				} else {
					s.logf("file source %q: reloaded %s", s.name, s.path)
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				s.logf("file source %q: watch error: %v", s.name, err)
			}
		}
	}()
	return nil
}

func (s *Source) reload() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("read %s: %w", s.path, err)
	}
	var v any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}
	if v == nil {
		// An empty or all-null document unmarshals successfully to nil
		// rather than erroring; treated as a load failure so a torn read
		// (a non-atomic write caught mid-truncate) can never silently
		// replace good cached data with nothing.
		return fmt.Errorf("parse %s: empty or null document", s.path)
	}
	s.data.Store(&v)
	return nil
}

// Lookup returns the file's parsed content, ignoring the input — a file
// source is not templated per-alert.
func (s *Source) Lookup(_ context.Context, _ source.LookupInput) (any, error) {
	p := s.data.Load()
	if p == nil {
		return nil, fmt.Errorf("file source %q: not yet loaded", s.name)
	}
	return *p, nil
}
