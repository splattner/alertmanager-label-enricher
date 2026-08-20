// Package wiring constructs the source registry and rule engine from a
// loaded configuration — the assembly step shared by the serve, check and
// test subcommands.
package wiring

import (
	"fmt"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/splattner/alertmanager-label-enricher/internal/config"
	"github.com/splattner/alertmanager-label-enricher/internal/engine"
	"github.com/splattner/alertmanager-label-enricher/internal/extract"
	fileSource "github.com/splattner/alertmanager-label-enricher/internal/source"
	filesrc "github.com/splattner/alertmanager-label-enricher/internal/source/file"
	httpsrc "github.com/splattner/alertmanager-label-enricher/internal/source/http"
	k8ssrc "github.com/splattner/alertmanager-label-enricher/internal/source/kubernetes"
)

// KubeClient resolves a dynamic client from the in-cluster service account,
// falling back to the local kubeconfig (KUBECONFIG or ~/.kube/config) for
// out-of-cluster development.
func KubeClient() (dynamic.Interface, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		restCfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(),
			&clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("no in-cluster config and no usable kubeconfig: %w", err)
		}
	}
	client, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}
	return client, nil
}

// BuildSources constructs every configured source. kubeClient may be nil if
// no source of type kubernetes is configured; passing nil while one is
// configured is an error.
func BuildSources(cfg *config.Config, kubeClient dynamic.Interface, logf func(format string, args ...any)) (*fileSource.Registry, error) {
	reg := fileSource.NewRegistry()

	for _, s := range cfg.Sources {
		switch s.Type {
		case "kubernetes":
			if kubeClient == nil {
				return nil, fmt.Errorf("source %q: kubernetes client unavailable", s.Name)
			}
			src, err := k8ssrc.New(s.Name, kubeClient, k8ssrc.Config{
				GVR:       s.Kubernetes.GVR(),
				Namespace: s.Kubernetes.Namespace,
				Name:      s.Kubernetes.Name,
			})
			if err != nil {
				return nil, err
			}
			if err := reg.Add(src); err != nil {
				return nil, err
			}

		case "http":
			src, err := httpsrc.New(s.Name, httpsrc.Config{
				Method:           s.HTTP.Method,
				URL:              s.HTTP.URL,
				Headers:          s.HTTP.Headers,
				AllowedHosts:     s.HTTP.AllowedHosts,
				Timeout:          s.HTTP.Timeout,
				MaxResponseBytes: s.HTTP.MaxResponseBytes,
				TTL:              s.HTTP.Cache.TTL,
				NegativeTTL:      s.HTTP.Cache.NegativeTTL,
				MaxEntries:       s.HTTP.Cache.MaxEntries,
			})
			if err != nil {
				return nil, err
			}
			if err := reg.Add(src); err != nil {
				return nil, err
			}

		case "file":
			src, err := filesrc.New(s.Name, s.File.Path, logf)
			if err != nil {
				return nil, err
			}
			if err := reg.Add(src); err != nil {
				return nil, err
			}

		default:
			return nil, fmt.Errorf("source %q: unknown type %q", s.Name, s.Type)
		}
	}

	return reg, nil
}

// BuildEngine compiles the rule engine against the given sources.
func BuildEngine(cfg *config.Config, sources engine.Sources) (*engine.Engine, error) {
	return engine.Compile(cfg, sources, extract.NewCache())
}

// NeedsKubeClient reports whether cfg declares any kubernetes source.
func NeedsKubeClient(cfg *config.Config) bool {
	for _, s := range cfg.Sources {
		if s.Type == "kubernetes" {
			return true
		}
	}
	return false
}
