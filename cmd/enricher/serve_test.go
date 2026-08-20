package main

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/splattner/alertmanager-label-enricher/internal/metrics"
)

func TestRecordReloadCountsSuccessAndSetsTimestamp(t *testing.T) {
	before := testutil.ToFloat64(metrics.ConfigReloadsTotal.WithLabelValues("ok"))
	beforeTS := testutil.ToFloat64(metrics.ConfigReloadSuccessTimestamp)

	reload := recordReload(func() error { return nil })
	if err := reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if after := testutil.ToFloat64(metrics.ConfigReloadsTotal.WithLabelValues("ok")); after != before+1 {
		t.Fatalf("ale_config_reloads_total{result=\"ok\"} = %v, want %v", after, before+1)
	}
	// Same-second re-runs (e.g. `go test -count=2`) can legitimately produce
	// an unchanged whole-second timestamp, so only assert it never regresses.
	if after := testutil.ToFloat64(metrics.ConfigReloadSuccessTimestamp); after == 0 || after < beforeTS {
		t.Fatalf("ale_config_reload_success_timestamp_seconds not set correctly: before=%v after=%v", beforeTS, after)
	}
}

func TestRecordReloadCountsError(t *testing.T) {
	before := testutil.ToFloat64(metrics.ConfigReloadsTotal.WithLabelValues("error"))

	wantErr := errors.New("bad config")
	reload := recordReload(func() error { return wantErr })
	if err := reload(); !errors.Is(err, wantErr) {
		t.Fatalf("reload() = %v, want %v", err, wantErr)
	}

	if after := testutil.ToFloat64(metrics.ConfigReloadsTotal.WithLabelValues("error")); after != before+1 {
		t.Fatalf("ale_config_reloads_total{result=\"error\"} = %v, want %v", after, before+1)
	}
}
