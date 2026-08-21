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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
var eventGVR = schema.GroupVersionResource{Version: "v1", Resource: "events"}

// readyCondition is the one status.conditions[].type this package writes -
// mirrors the "why wasn't my rule accepted" question a tenant asks.
const readyCondition = "Ready"

// defaultDebounce coalesces a burst of CR/Namespace change events (e.g.
// the informers' initial list, or several CRs applied together) into one
// OnChange call rather than rebuilding the engine once per object.
const defaultDebounce = time.Second

// defaultSyncTimeout bounds the initial list. Without it, a missing CRD or
// a ServiceAccount that cannot list enrichmentrules leaves the reflector
// retrying forever and Start blocking forever - which in cmd/enricher
// means startup never reaches ListenAndServe: no listener, no error, no
// crash, just a pod that never becomes ready. Failing with a diagnosable
// error is strictly better than hanging.
const defaultSyncTimeout = 60 * time.Second

// Config configures a Watcher.
type Config struct {
	Enforcement config.EnforcementConfig
	// DeclaredSources names the sources the file config declares. A
	// CR-sourced rule referencing anything else is rejected: enforcement
	// decides which of these a namespace may *use*, but a rule naming a
	// source that does not exist at all is simply malformed.
	DeclaredSources []string
	// MaxRules caps compiled CR-sourced rules across every namespace.
	// 0 means unlimited.
	MaxRules int
	// Debounce defaults to 1s if zero.
	Debounce time.Duration
	// SyncTimeout bounds how long Start waits for the initial list of
	// EnrichmentRules and Namespaces. Defaults to 60s if zero.
	SyncTimeout time.Duration
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
	client          dynamic.Interface
	enforcement     config.EnforcementConfig
	declaredSources map[string]bool
	maxRules        int
	debounce        time.Duration
	syncTimeout     time.Duration
	onChange        func()
	logf            func(format string, args ...any)

	ruleInformer cache.SharedIndexInformer
	ruleLister   cache.GenericLister
	nsInformer   cache.SharedIndexInformer
	nsLister     cache.GenericLister

	mu    sync.Mutex
	timer *time.Timer

	metricsMu      sync.Mutex
	seenNamespaces map[string]bool

	// Status writes run on a single background goroutine, coalescing:
	// statusPending holds the most recent batch not yet written.
	statusMu      sync.Mutex
	statusPending []decision
	statusRunning bool
	statusIdle    chan struct{} // signalled when the writer goes idle
}

// New builds a Watcher but does not start it; call Start to begin
// watching.
func New(client dynamic.Interface, cfg Config) *Watcher {
	debounce := cfg.Debounce
	if debounce <= 0 {
		debounce = defaultDebounce
	}
	syncTimeout := cfg.SyncTimeout
	if syncTimeout <= 0 {
		syncTimeout = defaultSyncTimeout
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

	declaredSources := make(map[string]bool, len(cfg.DeclaredSources))
	for _, name := range cfg.DeclaredSources {
		declaredSources[name] = true
	}

	return &Watcher{
		client:          client,
		enforcement:     cfg.Enforcement,
		declaredSources: declaredSources,
		maxRules:        cfg.MaxRules,
		debounce:        debounce,
		syncTimeout:     syncTimeout,
		onChange:        onChange,
		logf:            logf,
		ruleInformer:    ruleInformer.Informer(),
		ruleLister:      ruleInformer.Lister(),
		nsInformer:      nsInformer.Informer(),
		nsLister:        nsInformer.Lister(),
		seenNamespaces:  map[string]bool{},
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

	syncCtx, cancelSync := context.WithTimeout(ctx, w.syncTimeout)
	defer cancelSync()
	if !cache.WaitForCacheSync(syncCtx.Done(), w.ruleInformer.HasSynced, w.nsInformer.HasSynced) {
		if ctx.Err() != nil {
			return fmt.Errorf("crd: cache sync interrupted: %w", ctx.Err())
		}
		return fmt.Errorf("crd: timed out after %s waiting for the initial list of %s and namespaces. "+
			"Check that the EnrichmentRule CRD is installed (Helm value crd.install) and that this "+
			"ServiceAccount may list and watch both enrichmentrules and namespaces (Helm value crd.enabled "+
			"adds that RBAC)", w.syncTimeout, GVR.Resource)
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

// decision records what evaluate() decided about one EnrichmentRule CR, so
// Reconcile can turn it into a status.conditions write and (on transition)
// an Event, without re-running the enforcement logic.
type decision struct {
	obj      *unstructured.Unstructured
	accepted bool
	reason   string // Compiled | DecodeError | NamespaceUnreadable | PolicyViolation | MaxRulesExceeded
	message  string
}

// Rules returns every currently known EnrichmentRule CR that passes
// enforcement, enforced (authoritative matchers injected) and ready to
// append to config.Config.Rules for engine.Compile. A CR that fails to
// decode, whose namespace can't be read, or that enforcement rejects, is
// skipped - logged and counted, not returned - so one tenant's broken or
// disallowed CR never blocks any other tenant's rules, let alone the
// whole batch. Deterministic order: (spec.order, namespace, name).
//
// Rules does no I/O beyond the informers' local cache. Reconcile wraps it
// with status/Event writes; use Rules directly wherever those writes
// aren't wanted (e.g. tests asserting only on the compiled rule list).
func (w *Watcher) Rules() []config.RuleConfig {
	rules, _ := w.evaluate()
	return rules
}

// Reconcile is Rules plus its side effects: every processed CR's
// status.conditions[type=Ready] is set to reflect evaluate()'s decision,
// and an Event is emitted on each acceptance/rejection transition. No
// leader election guards this against other replicas doing the same work
// concurrently - see reconcileStatus and emitEvent for how that's made
// safe rather than exclusive.
//
// The rules are returned immediately and the status writes happen in the
// background. They are a courtesy to the CR's author, whereas the returned
// rules gate the engine swap and, on the first call, the listener starting
// at all - so an enricher must never wait on the API server to begin
// forwarding alerts. With a few hundred CRs and a slow API server, doing
// these inline delayed startup in proportion to how many tenants existed.
func (w *Watcher) Reconcile(ctx context.Context) []config.RuleConfig {
	rules, decisions := w.evaluate()
	w.queueStatusWrites(ctx, decisions)
	return rules
}

// queueStatusWrites hands decisions to the background writer, replacing any
// batch not yet written. A status is a report on the current state, so a
// newer batch always supersedes an older one - writing a snapshot that is
// already stale gains nothing and costs API calls. At most one writer runs
// at a time, which also keeps the request rate bounded no matter how often
// CRs change.
func (w *Watcher) queueStatusWrites(ctx context.Context, decisions []decision) {
	if len(decisions) == 0 {
		return
	}

	w.statusMu.Lock()
	w.statusPending = decisions
	if w.statusRunning {
		w.statusMu.Unlock()
		return
	}
	w.statusRunning = true
	w.statusMu.Unlock()

	go w.writeStatuses(ctx)
}

func (w *Watcher) writeStatuses(ctx context.Context) {
	for {
		w.statusMu.Lock()
		pending := w.statusPending
		w.statusPending = nil
		if pending == nil {
			w.statusRunning = false
			if w.statusIdle != nil {
				close(w.statusIdle)
				w.statusIdle = nil
			}
			w.statusMu.Unlock()
			return
		}
		w.statusMu.Unlock()

		for _, d := range pending {
			if ctx.Err() != nil {
				break // shut down; the next reconcile after restart converges
			}
			status := metav1.ConditionFalse
			if d.accepted {
				status = metav1.ConditionTrue
			}
			w.reconcileStatus(ctx, d.obj, metav1.Condition{
				Type:               readyCondition,
				Status:             status,
				Reason:             d.reason,
				Message:            d.message,
				ObservedGeneration: d.obj.GetGeneration(),
			})
		}
	}
}

// awaitStatusWrites blocks until every queued status write has been
// attempted. Only tests need this: production never waits on status, which
// is the whole point of writing it in the background.
func (w *Watcher) awaitStatusWrites() {
	for {
		w.statusMu.Lock()
		if !w.statusRunning && w.statusPending == nil {
			w.statusMu.Unlock()
			return
		}
		if w.statusIdle == nil {
			w.statusIdle = make(chan struct{})
		}
		idle := w.statusIdle
		w.statusMu.Unlock()
		<-idle
	}
}

func (w *Watcher) evaluate() ([]config.RuleConfig, []decision) {
	objs, err := w.ruleLister.List(labels.Everything())
	if err != nil {
		w.logf("crd: list enrichmentrules: %v", err)
		return nil, nil
	}

	type candidate struct {
		order int
		ns    string
		name  string
		obj   *unstructured.Unstructured
		spec  crSpec
	}
	candidates := make([]candidate, 0, len(objs))
	var decisions []decision
	for _, obj := range objs {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		spec, err := decodeSpec(u)
		if err != nil {
			msg := err.Error()
			w.recordRejected(u.GetNamespace(), "decode_error")
			w.logf("crd: reject EnrichmentRule %s/%s: %v", u.GetNamespace(), u.GetName(), err)
			decisions = append(decisions, decision{obj: u, accepted: false, reason: "DecodeError", message: msg})
			continue
		}
		candidates = append(candidates, candidate{order: spec.Order, ns: u.GetNamespace(), name: u.GetName(), obj: u, spec: spec})
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
				msg := fmt.Sprintf("read namespace: %v", err)
				w.recordRejected(c.ns, "namespace_unreadable")
				w.logf("crd: reject EnrichmentRule %s/%s: %v", c.ns, c.name, err)
				rejectedByNS[c.ns]++
				decisions = append(decisions, decision{obj: c.obj, accepted: false, reason: "NamespaceUnreadable", message: msg})
				continue
			}
			nsLabelsCache[c.ns] = nsLabels
		}

		r := c.spec.RuleConfig
		r.Name = c.name
		enforced, err := enforce.Rule(r, c.ns, nsLabels, w.declaredSources, w.enforcement)
		if err != nil {
			msg := err.Error()
			w.recordRejected(c.ns, "policy_violation")
			w.logf("crd: reject EnrichmentRule %s/%s: %v", c.ns, c.name, err)
			rejectedByNS[c.ns]++
			decisions = append(decisions, decision{obj: c.obj, accepted: false, reason: "PolicyViolation", message: msg})
			continue
		}

		if limit := w.maxRulesFor(nsLabels); limit > 0 && acceptedByNS[c.ns] >= limit {
			msg := fmt.Sprintf("namespace already has the maximum %d rule(s)", limit)
			w.recordRejected(c.ns, "max_rules_exceeded")
			w.logf("crd: reject EnrichmentRule %s/%s: %s", c.ns, c.name, msg)
			rejectedByNS[c.ns]++
			decisions = append(decisions, decision{obj: c.obj, accepted: false, reason: "MaxRulesExceeded", message: msg})
			continue
		}

		// The per-namespace cap bounds any one tenant; this bounds their
		// sum, which is what sets per-alert evaluation cost and the size of
		// the metric registry - rule names come from tenant-chosen CR names
		// and their series are never reclaimed. Ordering is deterministic,
		// so which rules make the cut is stable rather than a race.
		if w.maxRules > 0 && len(out) >= w.maxRules {
			msg := fmt.Sprintf("the cluster-wide limit of %d compiled EnrichmentRule(s) is already reached (crd.maxRules)", w.maxRules)
			w.recordRejected(c.ns, "global_max_rules_exceeded")
			w.logf("crd: reject EnrichmentRule %s/%s: %s", c.ns, c.name, msg)
			rejectedByNS[c.ns]++
			decisions = append(decisions, decision{obj: c.obj, accepted: false, reason: "MaxRulesExceeded", message: msg})
			continue
		}

		out = append(out, enforced)
		acceptedByNS[c.ns]++
		decisions = append(decisions, decision{obj: c.obj, accepted: true, reason: "Compiled", message: fmt.Sprintf("compiled as %q", enforced.Name)})
	}

	w.updateGauges(acceptedByNS, rejectedByNS)
	return out, decisions
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

// reconcileStatus writes cond into obj's status.conditions if it differs
// from what's already there, and emits an Event on that transition.
//
// Multiple enricher replicas may run this concurrently against the same
// CR with no coordination (no leader election - see the package doc for
// why). That's made safe, not exclusive: a Conflict means another replica
// already wrote an equivalent update moments ago, so it's dropped rather
// than retried - the next debounced reconcile (which runs on every
// replica, on every CR/Namespace change) converges regardless.
func (w *Watcher) reconcileStatus(ctx context.Context, obj *unstructured.Unstructured, cond metav1.Condition) {
	current, err := readConditions(obj)
	if err != nil {
		w.logf("crd: read status for %s/%s: %v", obj.GetNamespace(), obj.GetName(), err)
		return
	}

	updated := append([]metav1.Condition{}, current...)
	if !apimeta.SetStatusCondition(&updated, cond) {
		return
	}

	patch := obj.DeepCopy()
	if err := writeConditions(patch, updated); err != nil {
		w.logf("crd: encode status for %s/%s: %v", obj.GetNamespace(), obj.GetName(), err)
		return
	}

	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = w.client.Resource(GVR).Namespace(obj.GetNamespace()).UpdateStatus(writeCtx, patch, metav1.UpdateOptions{})
	switch {
	case err == nil:
		metrics.CRDStatusUpdatesTotal.WithLabelValues("ok").Inc()
		w.emitEvent(writeCtx, obj, eventTypeFor(cond.Status), cond.Reason, cond.Message)
	case apierrors.IsConflict(err):
		metrics.CRDStatusUpdatesTotal.WithLabelValues("conflict").Inc()
	default:
		metrics.CRDStatusUpdatesTotal.WithLabelValues("error").Inc()
		w.logf("crd: update status for %s/%s: %v", obj.GetNamespace(), obj.GetName(), err)
	}
}

// emitEvent records a courtesy Kubernetes Event explaining a status
// transition. Uses metadata.generateName rather than a deterministic
// name: with no leader election, two replicas can race to record the same
// transition, and generateName lets both succeed rather than one hitting
// AlreadyExists - the cost is an occasional duplicate Event, which is
// cosmetic (kubectl describe shows a list already).
func (w *Watcher) emitEvent(ctx context.Context, obj *unstructured.Unstructured, eventType, reason, message string) {
	now := time.Now().UTC().Format(time.RFC3339)
	event := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Event",
		"metadata": map[string]any{
			"generateName": obj.GetName() + "-",
			"namespace":    obj.GetNamespace(),
		},
		"involvedObject": map[string]any{
			"apiVersion":      GVR.Group + "/" + GVR.Version,
			"kind":            "EnrichmentRule",
			"name":            obj.GetName(),
			"namespace":       obj.GetNamespace(),
			"uid":             string(obj.GetUID()),
			"resourceVersion": obj.GetResourceVersion(),
		},
		"reason":         reason,
		"message":        message,
		"type":           eventType,
		"source":         map[string]any{"component": "alertmanager-label-enricher"},
		"firstTimestamp": now,
		"lastTimestamp":  now,
		"count":          int64(1),
	}}
	if _, err := w.client.Resource(eventGVR).Namespace(obj.GetNamespace()).Create(ctx, event, metav1.CreateOptions{}); err != nil {
		w.logf("crd: emit event for %s/%s reason=%s: %v", obj.GetNamespace(), obj.GetName(), reason, err)
	}
}

func eventTypeFor(status metav1.ConditionStatus) string {
	if status == metav1.ConditionTrue {
		return "Normal"
	}
	return "Warning"
}

func readConditions(obj *unstructured.Unstructured) ([]metav1.Condition, error) {
	raw, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil {
		return nil, fmt.Errorf("read status.conditions: %w", err)
	}
	if !found {
		return nil, nil
	}
	conditions := make([]metav1.Condition, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		var c metav1.Condition
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &c); err != nil {
			return nil, fmt.Errorf("decode status.conditions: %w", err)
		}
		conditions = append(conditions, c)
	}
	return conditions, nil
}

func writeConditions(obj *unstructured.Unstructured, conditions []metav1.Condition) error {
	raw := make([]any, 0, len(conditions))
	for _, c := range conditions {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&c)
		if err != nil {
			return fmt.Errorf("encode status.conditions: %w", err)
		}
		raw = append(raw, m)
	}
	return unstructured.SetNestedSlice(obj.Object, raw, "status", "conditions")
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
