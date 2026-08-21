package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"k8s.io/client-go/dynamic"

	"github.com/splattner/alertmanager-label-enricher/internal/config"
	"github.com/splattner/alertmanager-label-enricher/internal/crd"
	"github.com/splattner/alertmanager-label-enricher/internal/engine"
	"github.com/splattner/alertmanager-label-enricher/internal/metrics"
	"github.com/splattner/alertmanager-label-enricher/internal/proxy"
	fileSource "github.com/splattner/alertmanager-label-enricher/internal/source"
	"github.com/splattner/alertmanager-label-enricher/internal/tlsutil"
	"github.com/splattner/alertmanager-label-enricher/internal/wiring"
)

func newServeCmd() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the enrichment proxy",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runServe(configPath)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "/etc/enricher/config.yaml", "path to the config file")
	return cmd
}

// generation holds everything that's expensive to rebuild and only changes
// when the file config itself changes: sources (which starts Kubernetes
// informers), TLS configs, and the CRD watcher (which starts its own
// informers). server.compileAndSwap, below, reuses one generation's
// sources/watcher and only recompiles the engine - so a CR-only change
// (the common case once the CRD watch is enabled) never restarts a
// Kubernetes source informer or flaps /readyz the way rebuilding
// everything would.
type generation struct {
	cfg           *config.Config
	sources       *fileSource.Registry
	watcher       *crd.Watcher
	forwardClient *http.Client
	serverTLS     *tls.Config
}

// server holds the mutable state a running `serve` invocation swaps as
// config reloads happen. It's a struct (rather than the closures-over-
// locals runServe used to build it from) so tests can drive
// applyConfig/compileAndSwap directly against an injected kubeClient
// factory - e.g. a fake dynamic client - instead of a real cluster.
type server struct {
	configPath string
	log        *slog.Logger
	proxy      *proxy.Server
	kubeClient func() (dynamic.Interface, error)
	// buildEngine is wiring.BuildEngine, indirected like kubeClient so
	// tests can force the compile stage to fail and assert that a failed
	// reload leaves the running generation untouched.
	buildEngine func(*config.Config, engine.Sources) (*engine.Engine, error)

	genCancel  atomic.Pointer[context.CancelFunc]
	currentGen atomic.Pointer[generation]
}

func newServer(configPath string, log *slog.Logger) *server {
	return &server{
		configPath:  configPath,
		log:         log,
		proxy:       proxy.New(log),
		kubeClient:  wiring.KubeClient,
		buildEngine: wiring.BuildEngine,
	}
}

// buildGeneration loads the file config and constructs everything that
// depends on it: sources, TLS configs, and (if enabled) the CRD watcher,
// all running under their own context. It deliberately does NOT publish
// anything - not the generation, not the engine, not the proxy state - so
// a caller that fails later can discard the whole thing by calling the
// returned cancel and leave the running configuration untouched.
//
// On error it has already torn down whatever it started, and the returned
// cancel is nil.
func (s *server) buildGeneration(ctx context.Context) (*generation, context.CancelFunc, error) {
	cfg, err := config.Load(s.configPath)
	if err != nil {
		return nil, nil, err
	}

	var kubeClient dynamic.Interface
	if wiring.NeedsKubeClient(cfg) {
		kubeClient, err = s.kubeClient()
		if err != nil {
			return nil, nil, fmt.Errorf("build kubernetes client: %w", err)
		}
	}

	sources, err := wiring.BuildSources(cfg, kubeClient, func(format string, args ...any) {
		s.log.Info(fmt.Sprintf(format, args...))
	})
	if err != nil {
		return nil, nil, err
	}

	var forwardClient *http.Client
	if cfg.Forward.TLS != nil {
		t := cfg.Forward.TLS
		tlsCfg, err := tlsutil.ClientConfig(t.CAFile, t.CertFile, t.KeyFile, t.InsecureSkipVerify)
		if err != nil {
			return nil, nil, fmt.Errorf("build forward tls config: %w", err)
		}
		forwardClient = &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	}

	var serverTLS *tls.Config
	if cfg.Server.TLS != nil {
		serverTLS, err = tlsutil.ServerConfig(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile, cfg.Server.TLS.ClientCAFile)
		if err != nil {
			return nil, nil, fmt.Errorf("build server tls config: %w", err)
		}
	}

	gen := &generation{
		cfg:           cfg,
		forwardClient: forwardClient,
		serverTLS:     serverTLS,
		sources:       sources,
	}

	genCtx, cancel := context.WithCancel(ctx)
	if err := sources.Start(genCtx); err != nil {
		cancel()
		return nil, nil, err
	}

	if cfg.CRD.Enabled {
		// OnChange closes over gen rather than reading s.currentGen, so a
		// watcher belonging to a superseded generation can never recompile
		// against a newer one's sources. compileAndSwap drops the result if
		// gen is no longer live.
		declaredSources := make([]string, 0, len(cfg.Sources))
		for _, src := range cfg.Sources {
			declaredSources = append(declaredSources, src.Name)
		}
		gen.watcher = crd.New(kubeClient, crd.Config{
			Enforcement:     cfg.Enforcement,
			DeclaredSources: declaredSources,
			Logf: func(format string, args ...any) {
				s.log.Info(fmt.Sprintf(format, args...))
			},
			OnChange: func() {
				if err := s.compileAndSwap(genCtx, gen); err != nil {
					s.log.Error("recompile after EnrichmentRule change failed", "error", err.Error())
					return
				}
				s.log.Info("engine recompiled after EnrichmentRule change")
			},
		})
		if err := gen.watcher.Start(genCtx); err != nil {
			cancel()
			return nil, nil, fmt.Errorf("start crd watcher: %w", err)
		}
	}

	return gen, cancel, nil
}

// compile builds the engine for gen from its file-config rules plus (if the
// CRD watch is enabled) whatever EnrichmentRule CRs internal/crd currently
// accepts. It has no side effects on the running server.
func (s *server) compile(ctx context.Context, gen *generation) (*proxy.State, error) {
	engineCfg := *gen.cfg
	if gen.watcher != nil {
		engineCfg.Rules = append(append([]config.RuleConfig{}, gen.cfg.Rules...), gen.watcher.Reconcile(ctx)...)
	}
	eng, err := s.buildEngine(&engineCfg, gen.sources)
	if err != nil {
		return nil, err
	}

	synced := gen.sources.HasSynced
	if w := gen.watcher; w != nil {
		synced = func() bool { return gen.sources.HasSynced() && w.HasSynced() }
	}

	return &proxy.State{
		Cfg:           gen.cfg,
		Engine:        eng,
		Synced:        synced,
		ForwardClient: gen.forwardClient,
		ServerTLS:     gen.serverTLS,
	}, nil
}

// compileAndSwap recompiles gen's engine and installs it, provided gen is
// still the live generation. It is the CRD watcher's path: sources and
// informers are untouched, only the rule set changes.
//
// The liveness check matters because a watcher outlives the instant its
// generation is replaced - its debounce timer may already be armed when a
// file-config reload swaps in a new generation. Publishing then would
// resurrect the superseded config.
func (s *server) compileAndSwap(ctx context.Context, gen *generation) error {
	state, err := s.compile(ctx, gen)
	if err != nil {
		return err
	}
	if s.currentGen.Load() != gen {
		s.log.Info("skipped recompile from a superseded configuration generation")
		return nil
	}
	s.proxy.SetState(state)
	return nil
}

// applyConfig swaps in a whole new configuration generation atomically:
// everything is built and the engine fully compiled before any of it
// becomes visible, and the previous generation keeps running - informers
// included - until the new one is known good.
//
// Ordering is the point. Publishing or cancelling before the compile can
// fail would leave the proxy serving an engine whose sources have already
// been shut down, with /readyz still green because a stopped informer
// keeps reporting HasSynced.
func (s *server) applyConfig(ctx context.Context) error {
	gen, cancel, err := s.buildGeneration(ctx)
	if err != nil {
		return err
	}

	state, err := s.compile(ctx, gen)
	if err != nil {
		cancel() // discard the half-built generation; the running one is untouched
		return err
	}

	s.currentGen.Store(gen)
	s.proxy.SetState(state)

	if prev := s.genCancel.Swap(&cancel); prev != nil {
		(*prev)()
	}
	return nil
}

func runServe(configPath string) error {
	log := newLogger()
	s := newServer(configPath, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	reload := recordReload(func() error { return s.applyConfig(ctx) })

	if err := reload(); err != nil {
		return fmt.Errorf("initial config load: %w", err)
	}
	log.Info("config loaded", "path", configPath)

	go watchReload(ctx, configPath, log, reload)

	mux := http.NewServeMux()
	mux.Handle("/", s.proxy.Handler())
	mux.Handle("/metrics", promhttp.HandlerFor(metrics.Registry(), promhttp.HandlerOpts{}))
	mux.HandleFunc("POST /-/reload", func(w http.ResponseWriter, _ *http.Request) {
		if err := reload(); err != nil {
			log.Error("reload failed", "error", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		log.Info("config reloaded via /-/reload")
		w.WriteHeader(http.StatusOK)
	})

	initialCfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{Addr: initialCfg.Server.Listen, Handler: mux}

	// Whether the listener serves TLS is fixed at startup from the initial
	// config; a reload can rotate the certificate/key/client CA (via
	// State.ServerTLS, read fresh on every handshake below) but cannot
	// turn TLS on or off for an already-running listener.
	useTLS := initialCfg.Server.TLS != nil
	if useTLS {
		httpSrv.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
				st := s.proxy.State()
				if st == nil || st.ServerTLS == nil {
					return nil, fmt.Errorf("server tls not configured")
				}
				return st.ServerTLS, nil
			},
		}
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Info("listening", "addr", initialCfg.Server.Listen, "tls", useTLS)
	if useTLS {
		err = httpSrv.ListenAndServeTLS("", "")
	} else {
		err = httpSrv.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// recordReload wraps fn (a config-apply attempt) so every reload trigger -
// initial load, SIGHUP/file watch, POST /-/reload - reports through the
// same ale_config_reloads_total/ale_config_reload_success_timestamp_seconds
// metrics regardless of what triggered it.
func recordReload(fn func() error) func() error {
	return func() error {
		if err := fn(); err != nil {
			metrics.ConfigReloadsTotal.WithLabelValues("error").Inc()
			return err
		}
		metrics.ConfigReloadsTotal.WithLabelValues("ok").Inc()
		metrics.ConfigReloadSuccessTimestamp.Set(float64(time.Now().Unix()))
		return nil
	}
}

// watchReload triggers reload on SIGHUP and on changes to the config
// file's parent directory (a ConfigMap volume update replaces the file via
// a symlink swap, which a watch on the file's own inode would miss).
func watchReload(ctx context.Context, configPath string, log *slog.Logger, reload func() error) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Warn("config file watch disabled", "error", err.Error())
		watcher = nil
	} else {
		defer func() { _ = watcher.Close() }()
		if err := watcher.Add(filepath.Dir(configPath)); err != nil {
			log.Warn("config file watch disabled", "error", err.Error())
		}
	}

	base := filepath.Base(configPath)
	trigger := func(source string) {
		if err := reload(); err != nil {
			log.Error("reload failed", "source", source, "error", err.Error())
			return
		}
		log.Info("config reloaded", "source", source)
	}

	var events <-chan fsnotify.Event
	if watcher != nil {
		events = watcher.Events
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			trigger("SIGHUP")
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if filepath.Base(ev.Name) == base {
				trigger("file watch")
			}
		}
	}
}
