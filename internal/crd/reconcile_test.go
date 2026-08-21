package crd

import (
	"context"
	"errors"
	"testing"

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
	w.Reconcile(context.Background())

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
