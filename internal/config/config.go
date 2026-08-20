// Package config defines the enricher's YAML configuration shape and loads
// it from disk.
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"sigs.k8s.io/yaml"
)

// Config is the enricher's top-level configuration.
type Config struct {
	Server     ServerConfig     `json:"server"`
	Targets    []TargetConfig   `json:"targets"`
	Forward    ForwardConfig    `json:"forward"`
	Enrichment EnrichmentConfig `json:"enrichment"`
	Sources    []SourceConfig   `json:"sources"`
	Rules      []RuleConfig     `json:"rules"`
}

// ServerConfig configures the enricher's own HTTP listener.
type ServerConfig struct {
	Listen       string         `json:"listen"`
	MaxBodyBytes int64          `json:"maxBodyBytes"`
	TLS          *ServerTLSSpec `json:"tls,omitempty"`
}

// ServerTLSSpec configures TLS for the enricher's own listener — typically
// used to authenticate the Prometheus instances allowed to POST alerts.
// Whether the listener serves TLS at all is decided once at startup; a
// config reload can rotate the certificate/key/client CA but cannot toggle
// TLS on or off without a restart.
type ServerTLSSpec struct {
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
	// ClientCAFile, if set, requires and verifies a client certificate on
	// every connection (mutual TLS).
	ClientCAFile string `json:"clientCAFile,omitempty"`
}

// TargetConfig is one Alertmanager instance to forward alerts to.
type TargetConfig struct {
	URL string `json:"url"`
}

// ForwardConfig controls how a batch is fanned out to Targets.
type ForwardConfig struct {
	MinSuccess int      `json:"minSuccess"`
	Timeout    Duration `json:"timeout"`
	// Retries is the number of additional attempts made against a target
	// after an initial failure (0, the default, means a single attempt).
	// Each retry gets its own Timeout and is separated by a short fixed
	// backoff.
	Retries int            `json:"retries"`
	TLS     *ClientTLSSpec `json:"tls,omitempty"`
}

// ClientTLSSpec configures TLS trust/identity for outbound connections to
// Alertmanager targets. Applies to every Target: Alertmanager replicas in
// one cluster normally share the same server certificate setup, so this is
// deliberately one shared block rather than per-target.
type ClientTLSSpec struct {
	// CAFile trusts an additional CA (e.g. self-signed or internal)
	// alongside the system pool.
	CAFile string `json:"caFile,omitempty"`
	// CertFile/KeyFile present a client certificate for mutual TLS.
	CertFile string `json:"certFile,omitempty"`
	KeyFile  string `json:"keyFile,omitempty"`
	// InsecureSkipVerify disables certificate verification entirely. An
	// explicit, discouraged escape hatch — prefer CAFile.
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

// EnrichmentConfig bounds how long rule evaluation may take per batch.
type EnrichmentConfig struct {
	Timeout        Duration `json:"timeout"`
	MaxConcurrency int      `json:"maxConcurrency"`
}

// SourceConfig declares one named lookup source.
type SourceConfig struct {
	Name       string                `json:"name"`
	Type       string                `json:"type"` // kubernetes | http | file
	Kubernetes *KubernetesSourceSpec `json:"kubernetes,omitempty"`
	HTTP       *HTTPSourceSpec       `json:"http,omitempty"`
	File       *FileSourceSpec       `json:"file,omitempty"`
}

// KubernetesSourceSpec configures a source backed by a Kubernetes GVR.
type KubernetesSourceSpec struct {
	Group     string `json:"group"`
	Version   string `json:"version"`
	Resource  string `json:"resource"`
	Namespace string `json:"namespace,omitempty"` // templated; omit for cluster-scoped
	Name      string `json:"name"`                // templated
}

// HTTPCacheSpec configures response caching for an HTTP source.
type HTTPCacheSpec struct {
	TTL         Duration `json:"ttl"`
	NegativeTTL Duration `json:"negativeTTL"`
	MaxEntries  int      `json:"maxEntries"`
}

// HTTPSourceSpec configures a source backed by an HTTP endpoint.
type HTTPSourceSpec struct {
	Method           string            `json:"method"`
	URL              string            `json:"url"` // templated
	Headers          map[string]string `json:"headers,omitempty"`
	AllowedHosts     []string          `json:"allowedHosts"`
	Timeout          Duration          `json:"timeout"`
	MaxResponseBytes int64             `json:"maxResponseBytes"`
	Cache            HTTPCacheSpec     `json:"cache"`
}

// FileSourceSpec configures a source backed by a watched local file.
type FileSourceSpec struct {
	Path string `json:"path"`
}

// MatchOp is a matcher comparison operator.
type MatchOp string

// Matcher operators usable in a RuleConfig's Match list.
const (
	OpExists   MatchOp = "exists"
	OpAbsent   MatchOp = "absent"
	OpEq       MatchOp = "eq"
	OpNe       MatchOp = "ne"
	OpRegex    MatchOp = "regex"
	OpNotRegex MatchOp = "notregex"
)

// MatchConfig is one label matcher; a rule's matchers are ANDed together.
type MatchConfig struct {
	Label string  `json:"label"`
	Op    MatchOp `json:"op"`
	Value string  `json:"value,omitempty"`
}

// FromConfig resolves a label value via a source lookup and jq query.
type FromConfig struct {
	Source string `json:"source"`
	Jq     string `json:"jq"`
	Regex  string `json:"regex,omitempty"`
}

// SetAction sets a label to a literal value, a template, or a source lookup.
type SetAction struct {
	Label     string      `json:"label"`
	Value     string      `json:"value,omitempty"`
	Template  string      `json:"template,omitempty"`
	From      *FromConfig `json:"from,omitempty"`
	Default   string      `json:"default,omitempty"`
	Overwrite bool        `json:"overwrite,omitempty"`
}

// DropAction removes a label.
type DropAction struct {
	Label string `json:"label"`
}

// ActionConfig is exactly one of Set or Drop.
type ActionConfig struct {
	Set  *SetAction  `json:"set,omitempty"`
	Drop *DropAction `json:"drop,omitempty"`
}

// RuleConfig is one enrichment rule: match, then apply Actions in order.
type RuleConfig struct {
	Name     string         `json:"name"`
	Match    []MatchConfig  `json:"match,omitempty"`
	Required bool           `json:"required,omitempty"`
	DryRun   bool           `json:"dryRun,omitempty"`
	Actions  []ActionConfig `json:"actions"`
}

// Load reads, expands ${ENV} references, and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw)
}

// Parse expands and validates already-read config bytes.
func Parse(raw []byte) (*Config, error) {
	expanded := expandEnv(raw)

	var cfg Config
	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	applyDefaults(&cfg)

	if err := Validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${VAR} with the environment variable's value, left
// untouched if unset, so a typo'd variable name fails validation loudly
// (as an unresolved ${...}) rather than silently becoming an empty string.
func expandEnv(raw []byte) []byte {
	return envVarPattern.ReplaceAllFunc(raw, func(match []byte) []byte {
		name := envVarPattern.FindSubmatch(match)[1]
		if v, ok := os.LookupEnv(string(name)); ok {
			return []byte(v)
		}
		return match
	})
}

// applyDefaults fills in zero-valued fields with the enricher's defaults.
func applyDefaults(cfg *Config) {
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = ":9099"
	}
	if cfg.Server.MaxBodyBytes == 0 {
		cfg.Server.MaxBodyBytes = 8 << 20
	}
	if cfg.Forward.MinSuccess == 0 {
		cfg.Forward.MinSuccess = 1
	}
	if cfg.Forward.Timeout == 0 {
		cfg.Forward.Timeout = Duration(5 * time.Second)
	}
	if cfg.Enrichment.Timeout == 0 {
		cfg.Enrichment.Timeout = Duration(3 * time.Second)
	}
	if cfg.Enrichment.MaxConcurrency == 0 {
		cfg.Enrichment.MaxConcurrency = 32
	}
	for i := range cfg.Sources {
		s := &cfg.Sources[i]
		if s.Type == "http" && s.HTTP != nil {
			if s.HTTP.Method == "" {
				s.HTTP.Method = "GET"
			}
			if s.HTTP.Timeout == 0 {
				s.HTTP.Timeout = Duration(2 * time.Second)
			}
			if s.HTTP.MaxResponseBytes == 0 {
				s.HTTP.MaxResponseBytes = 1 << 20
			}
			if s.HTTP.Cache.TTL == 0 {
				s.HTTP.Cache.TTL = Duration(10 * time.Minute)
			}
			if s.HTTP.Cache.NegativeTTL == 0 {
				s.HTTP.Cache.NegativeTTL = Duration(30 * time.Second)
			}
			if s.HTTP.Cache.MaxEntries == 0 {
				s.HTTP.Cache.MaxEntries = 10000
			}
		}
	}
}
