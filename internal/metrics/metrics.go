// Package metrics defines the enricher's Prometheus instrumentation.
package metrics

import "github.com/prometheus/client_golang/prometheus"

const namespace = "ale"

// Metrics exposed on /metrics, all under the "ale" namespace.
var (
	AlertsReceivedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "alerts_received_total",
		Help:      "Alerts received from Prometheus, before enrichment.",
	})

	AlertsForwardedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "alerts_forwarded_total",
		Help:      "Alert batches forwarded to Alertmanager, by result.",
	}, []string{"result"}) // ok | required_failed | decode_error

	RuleEvaluationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "rule_evaluations_total",
		Help:      "Rule evaluations, by rule and result.",
	}, []string{"rule", "result"}) // matched | skipped | required_failed

	LabelsAddedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "labels_added_total",
		Help:      "Labels newly added to an alert, by rule and label.",
	}, []string{"rule", "label"})

	LabelsOverwrittenTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "labels_overwritten_total",
		Help:      "Existing labels overwritten on an alert, by rule and label. Overwriting changes the alert's fingerprint in Alertmanager.",
	}, []string{"rule", "label"})

	LabelsDroppedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "labels_dropped_total",
		Help:      "Labels dropped from an alert, by rule and label. Dropping changes the alert's fingerprint in Alertmanager.",
	}, []string{"rule", "label"})

	AnnotationsAddedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "annotations_added_total",
		Help:      "Annotations newly added to an alert, by rule and annotation. Informational only: unlike labels, annotations never change the alert's fingerprint.",
	}, []string{"rule", "annotation"})

	AnnotationsOverwrittenTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "annotations_overwritten_total",
		Help:      "Existing annotations overwritten on an alert, by rule and annotation. Informational only: unlike labels, annotations never change the alert's fingerprint.",
	}, []string{"rule", "annotation"})

	AnnotationsDroppedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "annotations_dropped_total",
		Help:      "Annotations dropped from an alert, by rule and annotation. Informational only: unlike labels, annotations never change the alert's fingerprint.",
	}, []string{"rule", "annotation"})

	SourceLookupsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "source_lookups_total",
		Help:      "Lookups issued against a source, by result.",
	}, []string{"source", "result"}) // hit | miss | error

	SourceLookupDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "source_lookup_duration_seconds",
		Help:      "Latency of source lookups.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"source"})

	ForwardDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "forward_duration_seconds",
		Help:      "Latency forwarding a batch to one Alertmanager target.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"target"})

	ForwardErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "forward_errors_total",
		Help:      "Errors forwarding a batch to one Alertmanager target, after exhausting forward.retries.",
	}, []string{"target"})

	ForwardRetriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "forward_retries_total",
		Help:      "Retry attempts made forwarding a batch to one Alertmanager target.",
	}, []string{"target"})

	ConfigReloadsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "config_reloads_total",
		Help:      "Configuration reload attempts, by result.",
	}, []string{"result"}) // ok | error

	ConfigReloadSuccessTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "config_reload_success_timestamp_seconds",
		Help:      "Unix timestamp of the last successful configuration reload.",
	})

	CRDRules = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "crd_rules",
		Help:      "EnrichmentRule CRs currently known, by namespace and state.",
	}, []string{"namespace", "state"}) // accepted | rejected

	CRDRulesRejectedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "crd_rules_rejected_total",
		Help:      "EnrichmentRule CRs rejected, by namespace and reason.",
	}, []string{"namespace", "reason"}) // decode_error | namespace_unreadable | policy_violation

	CRDStatusUpdatesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "crd_status_updates_total",
		Help:      "EnrichmentRule status.conditions writes attempted, by result. No leader election guards these across replicas, so a nonzero conflict rate is expected and benign - it's resolved by the next reconcile.",
	}, []string{"result"}) // ok | conflict | error
)

// Registry returns a registry with all enricher metrics registered.
func Registry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		AlertsReceivedTotal,
		AlertsForwardedTotal,
		RuleEvaluationsTotal,
		LabelsAddedTotal,
		LabelsOverwrittenTotal,
		LabelsDroppedTotal,
		AnnotationsAddedTotal,
		AnnotationsOverwrittenTotal,
		AnnotationsDroppedTotal,
		SourceLookupsTotal,
		SourceLookupDuration,
		ForwardDuration,
		ForwardErrorsTotal,
		ForwardRetriesTotal,
		ConfigReloadsTotal,
		ConfigReloadSuccessTimestamp,
		CRDRules,
		CRDRulesRejectedTotal,
		CRDStatusUpdatesTotal,
	)
	return reg
}
