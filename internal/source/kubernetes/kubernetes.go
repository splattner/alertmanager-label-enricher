// Package kubernetes implements a lookup source backed by a
// watch-and-cache informer over an arbitrary Kubernetes resource (GVR),
// so a lookup costs a local map read rather than an API call.
package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"text/template"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"github.com/splattner/alertmanager-label-enricher/internal/source"
	"github.com/splattner/alertmanager-label-enricher/internal/tmpl"
)

// Config configures a Kubernetes-backed lookup source.
type Config struct {
	GVR       schema.GroupVersionResource
	Namespace string // template text; empty for cluster-scoped resources
	Name      string // template text
}

// resyncPeriod is defense-in-depth against a missed watch event via a
// periodic full relist, not the primary freshness mechanism (the watch is).
const resyncPeriod = 10 * time.Minute

// Source is a lookup source backed by a watch-and-cache informer over one
// Kubernetes resource (GVR).
type Source struct {
	name string
	cfg  Config

	nsTmpl   *template.Template
	nameTmpl *template.Template

	informer cache.SharedIndexInformer
	lister   cache.GenericLister
}

// New builds the source but does not start its informer; call Start to
// begin watching.
func New(name string, client dynamic.Interface, cfg Config) (*Source, error) {
	nameTmpl, err := tmpl.Compile(name+"-name", cfg.Name)
	if err != nil {
		return nil, fmt.Errorf("kubernetes source %q: %w", name, err)
	}
	var nsTmpl *template.Template
	if cfg.Namespace != "" {
		nsTmpl, err = tmpl.Compile(name+"-namespace", cfg.Namespace)
		if err != nil {
			return nil, fmt.Errorf("kubernetes source %q: %w", name, err)
		}
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, resyncPeriod, metav1.NamespaceAll, nil)
	gvrInformer := factory.ForResource(cfg.GVR)

	return &Source{
		name:     name,
		cfg:      cfg,
		nsTmpl:   nsTmpl,
		nameTmpl: nameTmpl,
		informer: gvrInformer.Informer(),
		lister:   gvrInformer.Lister(),
	}, nil
}

// Name returns the source's configured name.
func (s *Source) Name() string { return s.name }

// HasSynced reports whether the informer has completed its initial list.
func (s *Source) HasSynced() bool { return s.informer.HasSynced() }

// Start begins the informer's watch and blocks until the initial list has
// synced or ctx is cancelled.
func (s *Source) Start(ctx context.Context) error {
	go s.informer.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), s.informer.HasSynced) {
		return fmt.Errorf("kubernetes source %q: cache sync interrupted for %s", s.name, s.cfg.GVR)
	}
	return nil
}

// Lookup renders the source's name (and namespace, if namespaced) template
// against in, then serves the result from the informer's local cache.
func (s *Source) Lookup(_ context.Context, in source.LookupInput) (any, error) {
	name, err := render(s.nameTmpl, in)
	if err != nil {
		return nil, fmt.Errorf("kubernetes source %q: render name: %w", s.name, err)
	}
	if name == "" {
		return nil, nil
	}

	var (
		found any
		gerr  error
	)
	if s.nsTmpl != nil {
		ns, err := render(s.nsTmpl, in)
		if err != nil {
			return nil, fmt.Errorf("kubernetes source %q: render namespace: %w", s.name, err)
		}
		if ns == "" {
			return nil, nil
		}
		found, gerr = s.lister.ByNamespace(ns).Get(name)
	} else {
		found, gerr = s.lister.Get(name)
	}

	if gerr != nil {
		if apierrors.IsNotFound(gerr) {
			return nil, nil
		}
		return nil, fmt.Errorf("kubernetes source %q: lookup %s: %w", s.name, name, gerr)
	}

	return toJSON(found)
}

func render(t *template.Template, in source.LookupInput) (string, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, tmpl.Data{Labels: in.Labels, Annotations: in.Annotations}); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// toJSON round-trips the object through encoding/json so the result is
// normalized to the plain nil/bool/float64/string/map/slice shape that
// extract.Query (gojq) expects, rather than *unstructured.Unstructured's
// internal representation.
func toJSON(obj any) (any, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal object: %w", err)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("unmarshal object: %w", err)
	}
	return v, nil
}
