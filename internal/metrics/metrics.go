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
		Help:      "Errors forwarding a batch to one Alertmanager target.",
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
		SourceLookupsTotal,
		SourceLookupDuration,
		ForwardDuration,
		ForwardErrorsTotal,
		ConfigReloadsTotal,
		ConfigReloadSuccessTimestamp,
	)
	return reg
}
