package crd

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/splattner/alertmanager-label-enricher/internal/config"
	"github.com/splattner/alertmanager-label-enricher/internal/metrics"
)

func getConditions(t *testing.T, w *Watcher, ns, name string) []metav1.Condition {
	t.Helper()
	obj, err := w.client.Resource(GVR).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s/%s: %v", ns, name, err)
	}
	conditions, err := readConditions(obj)
	if err != nil {
		t.Fatalf("readConditions: %v", err)
	}
	return conditions
}

func listEvents(t *testing.T, w *Watcher, ns string) []map[string]any {
	t.Helper()
	list, err := w.client.Resource(eventGVR).Namespace(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	out := make([]map[string]any, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, item.Object)
	}
	return out
}

func TestReconcileSetsReadyConditionTrueOnAcceptedRule(t *testing.T) {
	client := newFakeClient(t,
		namespaceObj("team-a", nil),
		enrichmentRule("team-a", "mark-team", setLabelSpec("team", "team-a")),
	)
	w := startedWatcher(t, client, catchAllEnforcement())

	w.Reconcile(context.Background())
	w.awaitStatusWrites()

	conditions := getConditions(t, w, "team-a", "mark-team")
	if len(conditions) != 1 {
		t.Fatalf("conditions = %+v, want exactly 1", conditions)
	}
	c := conditions[0]
	if c.Type != readyCondition || c.Status != metav1.ConditionTrue || c.Reason != "Compiled" {
		t.Fatalf("condition = %+v, want Ready/True/Compiled", c)
	}

	events := listEvents(t, w, "team-a")
	if len(events) != 1 {
		t.Fatalf("events = %+v, want exactly 1", events)
	}
	if events[0]["reason"] != "Compiled" || events[0]["type"] != "Normal" {
		t.Fatalf("event = %+v, want reason=Compiled type=Normal", events[0])
	}
}

func TestReconcileSetsReadyConditionFalseOnRejectedRule(t *testing.T) {
	client := newFakeClient(t,
		namespaceObj("team-a", nil),
		enrichmentRule("team-a", "escalate", setLabelSpec("severity", "critical")),
	)
	enforcement := catchAllEnforcement()
	enforcement.Rules[0].Labels = config.LabelPolicyConfig{Deny: []string{"severity"}}
	w := startedWatcher(t, client, enforcement)

	w.Reconcile(context.Background())
	w.awaitStatusWrites()

	conditions := getConditions(t, w, "team-a", "escalate")
	if len(conditions) != 1 {
		t.Fatalf("conditions = %+v, want exactly 1", conditions)
	}
	c := conditions[0]
	if c.Type != readyCondition || c.Status != metav1.ConditionFalse || c.Reason != "PolicyViolation" || c.Message == "" {
		t.Fatalf("condition = %+v, want Ready/False/PolicyViolation with a message", c)
	}

	events := listEvents(t, w, "team-a")
	if len(events) != 1 || events[0]["reason"] != "PolicyViolation" || events[0]["type"] != "Warning" {
		t.Fatalf("events = %+v, want exactly 1 reason=PolicyViolation type=Warning", events)
	}
}

func TestReconcileIsNoOpWhenUnchanged(t *testing.T) {
	client := newFakeClient(t,
		namespaceObj("team-a", nil),
		enrichmentRule("team-a", "mark-team", setLabelSpec("team", "team-a")),
	)
	w := startedWatcher(t, client, catchAllEnforcement())

	w.Reconcile(context.Background())
	w.awaitStatusWrites()
	w.Reconcile(context.Background())
	w.awaitStatusWrites()

	events := listEvents(t, w, "team-a")
	if len(events) != 1 {
		t.Fatalf("events = %+v, want exactly 1 - the second Reconcile should not have written anything new", events)
	}
	if got := testutil.ToFloat64(metrics.CRDStatusUpdatesTotal.WithLabelValues("ok")); got < 1 {
		t.Fatalf("ale_crd_status_updates_total{result=\"ok\"} = %v, want at least 1", got)
	}
}

func TestReconcileToleratesStatusUpdateConflict(t *testing.T) {
	client := newFakeClient(t,
		namespaceObj("team-a", nil),
		enrichmentRule("team-a", "mark-team", setLabelSpec("team", "team-a")),
	)
	client.PrependReactor("update", "enrichmentrules", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(GVR.GroupResource(), "mark-team", errors.New("resourceVersion mismatch"))
	})

	before := testutil.ToFloat64(metrics.CRDStatusUpdatesTotal.WithLabelValues("conflict"))

	w := startedWatcher(t, client, catchAllEnforcement())
	rules := w.Reconcile(context.Background())
	w.awaitStatusWrites()

	if len(rules) != 1 {
		t.Fatalf("Reconcile() rules = %+v, want the rule still compiled despite the status write conflict", rules)
	}
	if after := testutil.ToFloat64(metrics.CRDStatusUpdatesTotal.WithLabelValues("conflict")); after != before+1 {
		t.Fatalf("ale_crd_status_updates_total{result=\"conflict\"} = %v, want %v", after, before+1)
	}

	events := listEvents(t, w, "team-a")
	if len(events) != 0 {
		t.Fatalf("events = %+v, want none - no event should follow a failed status write", events)
	}
}

// ALE-13. Status writes used to run inline in Reconcile, which
// cmd/enricher calls from the compile path - so on startup the listener
// did not open until every CR's status had been written, one API round
// trip at a time. With a slow API server that scaled with tenant count.
func TestReconcileDoesNotWaitOnStatusWrites(t *testing.T) {
	objs := []runtime.Object{namespaceObj("ns", nil)}
	for i := 0; i < 20; i++ {
		objs = append(objs, enrichmentRule("ns", fmt.Sprintf("rule-%02d", i), setLabelSpec("team", "x")))
	}
	client := newFakeClient(t, objs...)

	// Every status write blocks for far longer than Reconcile may take.
	const writeDelay = 100 * time.Millisecond
	client.PrependReactor("update", "enrichmentrules", func(k8stesting.Action) (bool, runtime.Object, error) {
		time.Sleep(writeDelay)
		return false, nil, nil // fall through to the tracker
	})

	w := startedWatcher(t, client, catchAllEnforcement())

	start := time.Now()
	rules := w.Reconcile(context.Background())
	elapsed := time.Since(start)

	if len(rules) != 20 {
		t.Fatalf("Reconcile returned %d rules, want 20", len(rules))
	}
	// Inline writes would cost at least 20 * 100ms = 2s.
	if elapsed > time.Second {
		t.Errorf("Reconcile took %v; it is still blocking on status writes, which gates the engine swap and startup", elapsed)
	}

	// The writes must still happen, just not on the caller's path.
	w.awaitStatusWrites()
	conditions := getConditions(t, w, "ns", "rule-00")
	if len(conditions) != 1 || conditions[0].Status != metav1.ConditionTrue {
		t.Errorf("conditions = %+v, want the background writer to have set Ready=True", conditions)
	}
}

// Repeated reconciles must coalesce rather than queue: a status is a report
// on the current state, so a batch not yet written is superseded by a newer
// one instead of both being written.
func TestStatusWritesCoalesce(t *testing.T) {
	client := newFakeClient(t,
		namespaceObj("ns", nil),
		enrichmentRule("ns", "r", setLabelSpec("team", "x")),
	)
	var writes int32
	client.PrependReactor("update", "enrichmentrules", func(k8stesting.Action) (bool, runtime.Object, error) {
		atomic.AddInt32(&writes, 1)
		time.Sleep(50 * time.Millisecond)
		return false, nil, nil
	})

	w := startedWatcher(t, client, catchAllEnforcement())

	for i := 0; i < 10; i++ {
		w.Reconcile(context.Background())
	}
	w.awaitStatusWrites()

	// The condition only actually changes once, so at most a couple of
	// writes should reach the API server - certainly not one per call.
	if got := atomic.LoadInt32(&writes); got > 3 {
		t.Errorf("%d status writes for 10 reconciles of unchanged state; they are queueing rather than coalescing", got)
	}
}
