// Package crd watches namespaced EnrichmentRule custom resources and turns
// them into engine-ready rules, enforced against internal/enforce's
// tenancy policy so a rule sourced from one namespace can never affect
// alerts belonging to another.
package crd

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"github.com/splattner/alertmanager-label-enricher/internal/config"
	"github.com/splattner/alertmanager-label-enricher/internal/enforce"
	"github.com/splattner/alertmanager-label-enricher/internal/metrics"
)

// GVR is the EnrichmentRule custom resource's GroupVersionResource.
var GVR = schema.GroupVersionResource{
	Group:    "enricher.splattner.github.io",
	Version:  "v1alpha1",
	Resource: "enrichmentrules",
}

var namespaceGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// defaultDebounce coalesces a burst of CR/Namespace change events (e.g.
// the informers' initial list, or several CRs applied together) into one
// OnChange call rather than rebuilding the engine once per object.
const defaultDebounce = time.Second

// Config configures a Watcher.
type Config struct {
	Enforcement config.EnforcementConfig
	// Debounce defaults to 1s if zero.
	Debounce time.Duration
	// OnChange is called after Start, debounced, whenever a CR or
	// Namespace object changes. The caller should re-fetch Rules() and
	// recompile its engine; OnChange never learns what specifically
	// changed, only that something did.
	OnChange func()
	// Logf receives one-line diagnostics (rule accepted/rejected, sync
	// status). May be nil.
	Logf func(format string, args ...any)
}

// Watcher watches EnrichmentRule CRs and Namespace objects via dynamic
// informers (this project only ever uses a dynamic client, never a typed
// clientset - see internal/source/kubernetes for the same pattern), and
// produces the resulting enforced rule list on demand.
type Watcher struct {
	enforcement config.EnforcementConfig
	debounce    time.Duration
	onChange    func()
	logf        func(format string, args ...any)

	ruleInformer cache.SharedIndexInformer
	ruleLister   cache.GenericLister
	nsInformer   cache.SharedIndexInformer
	nsLister     cache.GenericLister

	mu    sync.Mutex
	timer *time.Timer

	metricsMu      sync.Mutex
	seenNamespaces map[string]bool
}

// New builds a Watcher but does not start it; call Start to begin
// watching.
func New(client dynamic.Interface, cfg Config) *Watcher {
	debounce := cfg.Debounce
	if debounce <= 0 {
		debounce = defaultDebounce
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	onChange := cfg.OnChange
	if onChange == nil {
		onChange = func() {}
	}

	factory := dynamicinformer.NewDynamicSharedInformerFactory(client, 10*time.Minute)
	ruleInformer := factory.ForResource(GVR)
	nsInformer := factory.ForResource(namespaceGVR)

	return &Watcher{
		enforcement:    cfg.Enforcement,
		debounce:       debounce,
		onChange:       onChange,
		logf:           logf,
		ruleInformer:   ruleInformer.Informer(),
		ruleLister:     ruleInformer.Lister(),
		nsInformer:     nsInformer.Informer(),
		nsLister:       nsInformer.Lister(),
		seenNamespaces: map[string]bool{},
	}
}

// Start begins both informers' watches and blocks until their initial
// list has synced or ctx is cancelled. After that, OnChange fires
// (debounced) on every subsequent CR or Namespace change until ctx is
// done.
func (w *Watcher) Start(ctx context.Context) error {
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { w.scheduleOnChange() },
		UpdateFunc: func(any, any) { w.scheduleOnChange() },
		DeleteFunc: func(any) { w.scheduleOnChange() },
	}
	if _, err := w.ruleInformer.AddEventHandler(handler); err != nil {
		return fmt.Errorf("watch enrichmentrules: %w", err)
	}
	if _, err := w.nsInformer.AddEventHandler(handler); err != nil {
		return fmt.Errorf("watch namespaces: %w", err)
	}

	go w.ruleInformer.Run(ctx.Done())
	go w.nsInformer.Run(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), w.ruleInformer.HasSynced, w.nsInformer.HasSynced) {
		return fmt.Errorf("crd: cache sync interrupted")
	}

	if len(w.enforcement.Rules) == 0 {
		w.logf("crd: WARNING enforcement.rules is empty - every EnrichmentRule CR is accepted unrestricted, from any namespace with create/update on enrichmentrules. This is single-tenant convenience mode; do not enable the CRD watch in a shared cluster without an enforcement policy.")
	}
	return nil
}

// HasSynced reports whether both informers have completed their initial
// list.
func (w *Watcher) HasSynced() bool {
	return w.ruleInformer.HasSynced() && w.nsInformer.HasSynced()
}

func (w *Watcher) scheduleOnChange() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(w.debounce, w.onChange)
}

// Rules returns every currently known EnrichmentRule CR that passes
// enforcement, enforced (authoritative matchers injected) and ready to
// append to config.Config.Rules for engine.Compile. A CR that fails to
// decode, whose namespace can't be read, or that enforcement rejects, is
// skipped - logged and counted, not returned - so one tenant's broken or
// disallowed CR never blocks any other tenant's rules, let alone the
// whole batch. Deterministic order: (spec.order, namespace, name).
func (w *Watcher) Rules() []config.RuleConfig {
	objs, err := w.ruleLister.List(labels.Everything())
	if err != nil {
		w.logf("crd: list enrichmentrules: %v", err)
		return nil
	}

	type candidate struct {
		order int
		ns    string
		name  string
		spec  crSpec
	}
	candidates := make([]candidate, 0, len(objs))
	for _, obj := range objs {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		spec, err := decodeSpec(u)
		if err != nil {
			w.recordRejected(u.GetNamespace(), "decode_error")
			w.logf("crd: reject EnrichmentRule %s/%s: %v", u.GetNamespace(), u.GetName(), err)
			continue
		}
		candidates = append(candidates, candidate{order: spec.Order, ns: u.GetNamespace(), name: u.GetName(), spec: spec})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].order != candidates[j].order {
			return candidates[i].order < candidates[j].order
		}
		if candidates[i].ns != candidates[j].ns {
			return candidates[i].ns < candidates[j].ns
		}
		return candidates[i].name < candidates[j].name
	})

	nsLabelsCache := map[string]map[string]string{}
	acceptedByNS := map[string]int{}
	rejectedByNS := map[string]int{}

	var out []config.RuleConfig
	for _, c := range candidates {
		nsLabels, ok := nsLabelsCache[c.ns]
		if !ok {
			var err error
			nsLabels, err = w.namespaceLabels(c.ns)
			if err != nil {
				w.recordRejected(c.ns, "namespace_unreadable")
				w.logf("crd: reject EnrichmentRule %s/%s: read namespace: %v", c.ns, c.name, err)
				rejectedByNS[c.ns]++
				continue
			}
			nsLabelsCache[c.ns] = nsLabels
		}

		r := c.spec.RuleConfig
		r.Name = c.name
		enforced, err := enforce.Rule(r, c.ns, nsLabels, w.enforcement)
		if err != nil {
			w.recordRejected(c.ns, "policy_violation")
			w.logf("crd: reject EnrichmentRule %s/%s: %v", c.ns, c.name, err)
			rejectedByNS[c.ns]++
			continue
		}

		if limit := w.maxRulesFor(nsLabels); limit > 0 && acceptedByNS[c.ns] >= limit {
			w.recordRejected(c.ns, "max_rules_exceeded")
			w.logf("crd: reject EnrichmentRule %s/%s: namespace already has the maximum %d rule(s)", c.ns, c.name, limit)
			rejectedByNS[c.ns]++
			continue
		}

		out = append(out, enforced)
		acceptedByNS[c.ns]++
	}

	w.updateGauges(acceptedByNS, rejectedByNS)
	return out
}

func (w *Watcher) maxRulesFor(nsLabels map[string]string) int {
	policy, ok := enforce.SelectPolicy(w.enforcement, nsLabels)
	if !ok {
		return 0
	}
	return policy.MaxRulesPerNamespace
}

func (w *Watcher) namespaceLabels(ns string) (map[string]string, error) {
	obj, err := w.nsLister.Get(ns)
	if err != nil {
		return nil, err
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("namespace %q: unexpected object type %T", ns, obj)
	}
	return u.GetLabels(), nil
}

func (w *Watcher) recordRejected(ns, reason string) {
	metrics.CRDRulesRejectedTotal.WithLabelValues(ns, reason).Inc()
}

// updateGauges sets ale_crd_rules{namespace,state} to the current
// snapshot, and zeroes out any namespace that had rules last time but has
// none this time (deleted CRs, or a namespace that no longer exists) so
// stale non-zero values don't linger.
func (w *Watcher) updateGauges(acceptedByNS, rejectedByNS map[string]int) {
	w.metricsMu.Lock()
	defer w.metricsMu.Unlock()

	current := make(map[string]bool, len(acceptedByNS)+len(rejectedByNS))
	for ns, n := range acceptedByNS {
		metrics.CRDRules.WithLabelValues(ns, "accepted").Set(float64(n))
		current[ns] = true
	}
	for ns, n := range rejectedByNS {
		metrics.CRDRules.WithLabelValues(ns, "rejected").Set(float64(n))
		current[ns] = true
	}
	for ns := range w.seenNamespaces {
		if current[ns] {
			continue
		}
		metrics.CRDRules.WithLabelValues(ns, "accepted").Set(0)
		metrics.CRDRules.WithLabelValues(ns, "rejected").Set(0)
	}
	w.seenNamespaces = current
}

// crSpec mirrors config.RuleConfig for decoding an EnrichmentRule CR's
// spec, plus Order - a CRD-only field for deterministic sequencing among
// a namespace's own rules. (File-config rules use their position in the
// YAML list for this instead, which a CR has no equivalent of.) Name is
// intentionally not read from spec even though config.RuleConfig carries
// a Name field - it always comes from the CR's own metadata.name, set by
// the caller after decoding, so a tenant putting `name:` in their spec by
// mistake has no effect.
type crSpec struct {
	config.RuleConfig
	Order int `json:"order,omitempty"`
}

func decodeSpec(obj *unstructured.Unstructured) (crSpec, error) {
	spec, found, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil {
		return crSpec{}, fmt.Errorf("read spec: %w", err)
	}
	if !found {
		return crSpec{}, fmt.Errorf("missing spec")
	}
	var s crSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(spec, &s); err != nil {
		return crSpec{}, fmt.Errorf("decode spec: %w", err)
	}
	return s, nil
}
