package main

import (
	"context"
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
	"github.com/splattner/alertmanager-label-enricher/internal/metrics"
	"github.com/splattner/alertmanager-label-enricher/internal/proxy"
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

	reload := func() error {
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
		eng, err := wiring.BuildEngine(cfg, sources)
		if err != nil {
			return err
		}

		genCtx, cancel := context.WithCancel(ctx)
		if err := sources.Start(genCtx); err != nil {
			cancel()
			return err
		}

		srv.SetState(&proxy.State{Cfg: cfg, Engine: eng, Synced: sources.HasSynced})

		if prev := genCancel.Swap(&cancel); prev != nil {
			(*prev)()
		}
		return nil
	}

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

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Info("listening", "addr", initialCfg.Server.Listen)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
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
