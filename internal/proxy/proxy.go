// Package proxy implements the HTTP server that receives alert batches
// from Prometheus, enriches them via internal/engine, and fans them out to
// every configured Alertmanager target.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/splattner/alertmanager-label-enricher/internal/alert"
	"github.com/splattner/alertmanager-label-enricher/internal/config"
	"github.com/splattner/alertmanager-label-enricher/internal/engine"
	"github.com/splattner/alertmanager-label-enricher/internal/metrics"
)

// State is the atomically-swappable set of everything a running server
// needs, so a config reload can replace it in one step.
type State struct {
	Cfg    *config.Config
	Engine *engine.Engine
	Synced func() bool // reports whether all sources have completed initial sync
}

// Server is the enricher's HTTP server: it decodes alert batches, runs
// them through the engine, and fans the result out to Alertmanager.
type Server struct {
	state atomic.Pointer[State]
	log   *slog.Logger
	http  *http.Client
}

// New creates a Server with no state; call SetState before serving traffic.
func New(log *slog.Logger) *Server {
	return &Server{
		log:  log,
		http: &http.Client{},
	}
}

// SetState atomically installs the config/engine/readiness a Server uses
// to handle requests, for both initial startup and hot reload.
func (s *Server) SetState(st *State) {
	s.state.Store(st)
}

func (s *Server) currentState() *State {
	return s.state.Load()
}

// Handler returns the Server's http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/alerts", s.handleAlerts)
	mux.HandleFunc("POST /api/v1/alerts", s.handleAlerts)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	st := s.currentState()
	if st == nil || (st.Synced != nil && !st.Synced()) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	st := s.currentState()
	if st == nil {
		http.Error(w, "not initialized", http.StatusServiceUnavailable)
		return
	}
	cfg := st.Cfg

	body, err := io.ReadAll(io.LimitReader(r.Body, cfg.Server.MaxBodyBytes+1))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if int64(len(body)) > cfg.Server.MaxBodyBytes {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}

	alerts, err := alert.DecodeBatch(body)
	if err != nil {
		metrics.AlertsForwardedTotal.WithLabelValues("decode_error").Inc()
		http.Error(w, "decode alerts: "+err.Error(), http.StatusBadRequest)
		return
	}
	metrics.AlertsReceivedTotal.Add(float64(len(alerts)))

	ctx, cancel := context.WithTimeout(r.Context(), cfg.Enrichment.Timeout)
	defer cancel()

	if err := s.enrich(ctx, st.Engine, alerts); err != nil {
		var reqFail *engine.RequiredFailure
		if errors.As(err, &reqFail) {
			s.log.Warn("required rule failed, batch not forwarded", "error", err.Error())
			metrics.AlertsForwardedTotal.WithLabelValues("required_failed").Inc()
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		s.log.Error("enrichment error", "error", err.Error())
	}

	out, err := alert.EncodeBatch(alerts)
	if err != nil {
		http.Error(w, "encode alerts: "+err.Error(), http.StatusInternalServerError)
		return
	}

	status, err := s.forward(r.Context(), cfg, r.URL.Path, out, r.Header)
	if err != nil {
		metrics.AlertsForwardedTotal.WithLabelValues("forward_failed").Inc()
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	metrics.AlertsForwardedTotal.WithLabelValues("ok").Inc()
	w.WriteHeader(status)
}

// enrich runs the engine over every alert. A per-alert enrichment error
// (that is not a RequiredFailure) is logged and swallowed so one bad alert
// does not block the rest of the batch; a RequiredFailure aborts the whole
// batch immediately since forwarding without the required label is unsafe.
func (s *Server) enrich(ctx context.Context, eng *engine.Engine, alerts []alert.Alert) error {
	for _, a := range alerts {
		results, err := eng.Apply(ctx, a)
		recordResults(results)
		if err != nil {
			var reqFail *engine.RequiredFailure
			if errors.As(err, &reqFail) {
				return err
			}
			s.log.Warn("alert enrichment failed", "error", err.Error())
		}
	}
	return nil
}

func recordResults(results []engine.Result) {
	for _, res := range results {
		if res.Skipped {
			metrics.RuleEvaluationsTotal.WithLabelValues(res.Rule, "skipped").Inc()
			continue
		}
		metrics.RuleEvaluationsTotal.WithLabelValues(res.Rule, "matched").Inc()
		for _, l := range res.Added {
			metrics.LabelsAddedTotal.WithLabelValues(res.Rule, l).Inc()
		}
		for _, l := range res.Overwritten {
			metrics.LabelsOverwrittenTotal.WithLabelValues(res.Rule, l).Inc()
		}
		for _, l := range res.Dropped {
			metrics.LabelsDroppedTotal.WithLabelValues(res.Rule, l).Inc()
		}
	}
}

// forward sends the enriched batch to every target concurrently, and
// succeeds once minSuccess targets accept it — the rest are left to finish
// in the background so one slow Alertmanager replica doesn't hold up the
// response to Prometheus.
func (s *Server) forward(ctx context.Context, cfg *config.Config, path string, body []byte, hdr http.Header) (int, error) {
	type result struct {
		status int
		err    error
	}

	results := make(chan result, len(cfg.Targets))
	var wg sync.WaitGroup
	for _, t := range cfg.Targets {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			status, err := s.forwardOne(ctx, cfg.Forward.Timeout, target, path, body, hdr)
			if err != nil {
				metrics.ForwardErrorsTotal.WithLabelValues(target).Inc()
			}
			results <- result{status: status, err: err}
		}(t.URL)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	successes := 0
	lastStatus := http.StatusBadGateway
	var lastErr error
	for r := range results {
		if r.err == nil {
			successes++
			lastStatus = r.status
		} else {
			lastErr = r.err
		}
		if successes >= cfg.Forward.MinSuccess {
			return lastStatus, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no targets configured")
	}
	return 0, fmt.Errorf("forward: only %d/%d targets required succeeded: %w", successes, cfg.Forward.MinSuccess, lastErr)
}

func (s *Server) forwardOne(ctx context.Context, timeout time.Duration, target, path string, body []byte, hdr http.Header) (int, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target+path, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request to %s: %w", target, err)
	}
	req.Header.Set("Content-Type", hdr.Get("Content-Type"))
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.http.Do(req)
	metrics.ForwardDuration.WithLabelValues(target).Observe(time.Since(start).Seconds())
	if err != nil {
		return 0, fmt.Errorf("request to %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("target %s returned %d", target, resp.StatusCode)
	}
	return resp.StatusCode, nil
}
