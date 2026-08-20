// Package proxy implements the HTTP server that receives alert batches
// from Prometheus, enriches them via internal/engine, and fans them out to
// every configured Alertmanager target.
package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

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

	// ForwardClient is used to forward batches to Alertmanager targets; it
	// carries any TLS trust/identity built from Cfg.Forward.TLS. Nil means
	// "use the plain default client" (no forward.tls configured).
	ForwardClient *http.Client

	// ServerTLS, if set, is the tls.Config the listener's TLS handshake
	// should use going forward. Read by the http.Server's
	// GetConfigForClient callback (wired up by cmd/enricher) so a config
	// reload can rotate the server certificate/client CA without a
	// restart; it cannot turn TLS on or off for an already-running
	// listener.
	ServerTLS *tls.Config
}

// Server is the enricher's HTTP server: it decodes alert batches, runs
// them through the engine, and fans the result out to Alertmanager.
type Server struct {
	state       atomic.Pointer[State]
	log         *slog.Logger
	defaultHTTP *http.Client
}

// New creates a Server with no state; call SetState before serving traffic.
func New(log *slog.Logger) *Server {
	return &Server{
		log:         log,
		defaultHTTP: &http.Client{},
	}
}

// SetState atomically installs the config/engine/readiness a Server uses
// to handle requests, for both initial startup and hot reload.
func (s *Server) SetState(st *State) {
	s.state.Store(st)
}

// State returns the currently installed State, or nil before the first
// SetState call.
func (s *Server) State() *State {
	return s.state.Load()
}

func (s *Server) forwardClient(st *State) *http.Client {
	if st.ForwardClient != nil {
		return st.ForwardClient
	}
	return s.defaultHTTP
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
	st := s.State()
	if st == nil || (st.Synced != nil && !st.Synced()) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	st := s.State()
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

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(cfg.Enrichment.Timeout))
	defer cancel()

	if err := s.enrich(ctx, st.Engine, alerts, cfg.Enrichment.MaxConcurrency); err != nil {
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

	status, err := s.forward(r.Context(), cfg, s.forwardClient(st), r.URL.Path, out, r.Header)
	if err != nil {
		metrics.AlertsForwardedTotal.WithLabelValues("forward_failed").Inc()
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	metrics.AlertsForwardedTotal.WithLabelValues("ok").Inc()
	w.WriteHeader(status)
}

// enrich runs the engine over every alert concurrently, bounded by
// maxConcurrency (each alert's lookups/jq evaluation are independent, and
// the engine and its compiled queries are safe for concurrent use). A
// per-alert enrichment error (that is not a RequiredFailure) is logged and
// swallowed so one bad alert does not block the rest of the batch; a
// RequiredFailure cancels the remaining in-flight alerts and, once every
// goroutine has returned, aborts the whole batch — forwarding without the
// required label is unsafe.
func (s *Server) enrich(ctx context.Context, eng *engine.Engine, alerts []alert.Alert, maxConcurrency int) error {
	g, gctx := errgroup.WithContext(ctx)
	if maxConcurrency > 0 {
		g.SetLimit(maxConcurrency)
	}

	for _, a := range alerts {
		g.Go(func() error {
			results, err := eng.Apply(gctx, a)
			recordResults(results)
			if err != nil {
				var reqFail *engine.RequiredFailure
				if errors.As(err, &reqFail) {
					return err
				}
				s.log.Warn("alert enrichment failed", "error", err.Error())
			}
			return nil
		})
	}
	return g.Wait()
}

func recordResults(results []engine.Result) {
	for _, res := range results {
		switch {
		case res.Skipped:
			metrics.RuleEvaluationsTotal.WithLabelValues(res.Rule, "skipped").Inc()
			continue
		case res.RequiredFailed:
			metrics.RuleEvaluationsTotal.WithLabelValues(res.Rule, "required_failed").Inc()
		default:
			metrics.RuleEvaluationsTotal.WithLabelValues(res.Rule, "matched").Inc()
		}
		for _, l := range res.Added {
			metrics.LabelsAddedTotal.WithLabelValues(res.Rule, l).Inc()
		}
		for _, l := range res.Overwritten {
			metrics.LabelsOverwrittenTotal.WithLabelValues(res.Rule, l).Inc()
		}
		for _, l := range res.Dropped {
			metrics.LabelsDroppedTotal.WithLabelValues(res.Rule, l).Inc()
		}
		for _, an := range res.AnnotationsAdded {
			metrics.AnnotationsAddedTotal.WithLabelValues(res.Rule, an).Inc()
		}
		for _, an := range res.AnnotationsOverwritten {
			metrics.AnnotationsOverwrittenTotal.WithLabelValues(res.Rule, an).Inc()
		}
		for _, an := range res.AnnotationsDropped {
			metrics.AnnotationsDroppedTotal.WithLabelValues(res.Rule, an).Inc()
		}
	}
}

// forward sends the enriched batch to every target concurrently, and
// succeeds once minSuccess targets accept it — the rest are left to finish
// in the background so one slow Alertmanager replica doesn't hold up the
// response to Prometheus.
func (s *Server) forward(ctx context.Context, cfg *config.Config, client *http.Client, path string, body []byte, hdr http.Header) (int, error) {
	type result struct {
		status int
		err    error
	}

	// Detached from ctx's cancellation (but not its values): once
	// minSuccess is reached below, forward returns and the caller's HTTP
	// handler finishes, which cancels the request context. Stragglers past
	// minSuccess must keep running — bounded by their own per-attempt
	// timeout in forwardOne/forwardAttempt — rather than being killed the
	// instant the response to Prometheus is written.
	forwardCtx := context.WithoutCancel(ctx)

	results := make(chan result, len(cfg.Targets))
	var wg sync.WaitGroup
	for _, t := range cfg.Targets {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			status, err := s.forwardOne(forwardCtx, client, cfg.Forward, target, path, body, hdr)
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

// retryBackoff is the fixed delay between forward attempts. Kept short and
// unconfigurable on purpose: Alertmanager's own default notify timeout is
// 10s, so retries need to fit inside that budget rather than back off
// aggressively.
const retryBackoff = 200 * time.Millisecond

// forwardOne sends body to target, retrying up to cfg.Retries additional
// times (so cfg.Retries=0, the default, is a single attempt) on any
// failure — a non-2xx response or a transport-level error. It stops early
// if ctx is done between attempts.
func (s *Server) forwardOne(ctx context.Context, client *http.Client, cfg config.ForwardConfig, target, path string, body []byte, hdr http.Header) (int, error) {
	var status int
	var err error
	for attempt := 0; attempt <= cfg.Retries; attempt++ {
		if attempt > 0 {
			metrics.ForwardRetriesTotal.WithLabelValues(target).Inc()
			select {
			case <-ctx.Done():
				return status, err
			case <-time.After(retryBackoff):
			}
		}
		status, err = s.forwardAttempt(ctx, client, time.Duration(cfg.Timeout), target, path, body, hdr)
		if err == nil {
			return status, nil
		}
	}
	return status, err
}

func (s *Server) forwardAttempt(ctx context.Context, client *http.Client, timeout time.Duration, target, path string, body []byte, hdr http.Header) (int, error) {
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

	resp, err := client.Do(req)
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
