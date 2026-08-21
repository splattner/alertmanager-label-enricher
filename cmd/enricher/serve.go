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

func runServe(configPath string) error {
	log := newLogger()
	srv := proxy.New(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var genCancel atomic.Pointer[context.CancelFunc]

	// generation holds everything that's expensive to rebuild and only
	// changes when the file config itself changes: sources (which starts
	// Kubernetes informers), TLS configs, and the CRD watcher (which
	// starts its own informers). compileAndSwap, below, reuses the
	// current generation's sources/watcher and only recompiles the
	// engine - so a CR-only change (the common case once the CRD watch is
	// enabled) never restarts a Kubernetes source informer or flaps
	// /readyz the way rebuilding everything would.
	type generation struct {
		cfg           *config.Config
		sources       *fileSource.Registry
		watcher       *crd.Watcher
		forwardClient *http.Client
		serverTLS     *tls.Config
	}
	var currentGen atomic.Pointer[generation]

	// compileAndSwap recompiles the engine from the current generation's
	// file-config rules plus (if the CRD watch is enabled) whatever
	// EnrichmentRule CRs internal/crd currently accepts, and atomically
	// swaps it into the running server. Declared as a var so the CRD
	// watcher's OnChange callback (built inside buildGeneration, below)
	// can reference it before it's assigned.
	var compileAndSwap func() error

	buildGeneration := func() error {
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}

		var kubeClient dynamic.Interface
		if wiring.NeedsKubeClient(cfg) {
			kubeClient, err = wiring.KubeClient()
			if err != nil {
				return fmt.Errorf("build kubernetes client: %w", err)
			}
		}

		sources, err := wiring.BuildSources(cfg, kubeClient, func(format string, args ...any) {
			log.Info(fmt.Sprintf(format, args...))
		})
		if err != nil {
			return err
		}

		var forwardClient *http.Client
		if cfg.Forward.TLS != nil {
			t := cfg.Forward.TLS
			tlsCfg, err := tlsutil.ClientConfig(t.CAFile, t.CertFile, t.KeyFile, t.InsecureSkipVerify)
			if err != nil {
				return fmt.Errorf("build forward tls config: %w", err)
			}
			forwardClient = &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
		}

		var serverTLS *tls.Config
		if cfg.Server.TLS != nil {
			serverTLS, err = tlsutil.ServerConfig(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile, cfg.Server.TLS.ClientCAFile)
			if err != nil {
				return fmt.Errorf("build server tls config: %w", err)
			}
		}

		genCtx, cancel := context.WithCancel(ctx)
		if err := sources.Start(genCtx); err != nil {
			cancel()
			return err
		}

		var watcher *crd.Watcher
		if cfg.CRD.Enabled {
			watcher = crd.New(kubeClient, crd.Config{
				Enforcement: cfg.Enforcement,
				Logf: func(format string, args ...any) {
					log.Info(fmt.Sprintf(format, args...))
				},
				OnChange: func() {
					if err := compileAndSwap(); err != nil {
						log.Error("recompile after EnrichmentRule change failed", "error", err.Error())
						return
					}
					log.Info("engine recompiled after EnrichmentRule change")
				},
			})
			if err := watcher.Start(genCtx); err != nil {
				cancel()
				return fmt.Errorf("start crd watcher: %w", err)
			}
		}

		currentGen.Store(&generation{
			cfg:           cfg,
			sources:       sources,
			watcher:       watcher,
			forwardClient: forwardClient,
			serverTLS:     serverTLS,
		})

		if prev := genCancel.Swap(&cancel); prev != nil {
			(*prev)()
		}
		return nil
	}

	compileAndSwap = func() error {
		gen := currentGen.Load()
		if gen == nil {
			return fmt.Errorf("no configuration generation built yet")
		}

		engineCfg := *gen.cfg
		if gen.watcher != nil {
			engineCfg.Rules = append(append([]config.RuleConfig{}, gen.cfg.Rules...), gen.watcher.Rules()...)
		}
		eng, err := wiring.BuildEngine(&engineCfg, gen.sources)
		if err != nil {
			return err
		}

		synced := gen.sources.HasSynced
		if gen.watcher != nil {
			w := gen.watcher
			synced = func() bool { return gen.sources.HasSynced() && w.HasSynced() }
		}

		srv.SetState(&proxy.State{
			Cfg:           gen.cfg,
			Engine:        eng,
			Synced:        synced,
			ForwardClient: gen.forwardClient,
			ServerTLS:     gen.serverTLS,
		})
		return nil
	}

	applyConfig := func() error {
		if err := buildGeneration(); err != nil {
			return err
		}
		return compileAndSwap()
	}

	reload := recordReload(applyConfig)

	if err := reload(); err != nil {
		return fmt.Errorf("initial config load: %w", err)
	}
	log.Info("config loaded", "path", configPath)

	go watchReload(ctx, configPath, log, reload)

	mux := http.NewServeMux()
	mux.Handle("/", srv.Handler())
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
				st := srv.State()
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
