// Package config defines the enricher's YAML configuration shape and loads
// it from disk.
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// Config is the enricher's top-level configuration.
type Config struct {
	Server      ServerConfig      `json:"server"`
	Targets     []TargetConfig    `json:"targets"`
	Forward     ForwardConfig     `json:"forward"`
	Enrichment  EnrichmentConfig  `json:"enrichment"`
	Sources     []SourceConfig    `json:"sources"`
	Rules       []RuleConfig      `json:"rules"`
	CRD         CRDConfig         `json:"crd,omitempty"`
	Enforcement EnforcementConfig `json:"enforcement,omitempty"`
}

// CRDConfig controls whether the enricher watches EnrichmentRule custom
// resources (see internal/crd) in addition to the rules declared directly
// in this file.
type CRDConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// MaxRules caps how many CR-sourced rules are compiled in total,
	// across every namespace. enforcement's maxRulesPerNamespace bounds
	// any single tenant; this bounds their sum, which is what actually
	// determines evaluation cost per alert and the size of the metric
	// registry (rule names come from tenant-chosen CR names, and series
	// are never reclaimed). 0 means unlimited; defaults to 1000.
	MaxRules int `json:"maxRules,omitempty"`
}

// EnforcementConfig constrains what a namespaced EnrichmentRule CR is
// allowed to do, so a tenant with create/update on enrichmentrules in
// their own namespace cannot affect alerts belonging to another
// namespace. Applies only to CR-sourced rules; rules declared directly in
// this file are admin-authored and unrestricted by it.
type EnforcementConfig struct {
	// NamespaceMatcherLabel, if set, is injected as an authoritative
	// `<label> == <the CR's own namespace>` matcher on every CR-sourced
	// rule, and is implicitly added to every EnforcementRuleConfig's
	// label deny list - a tenant can never override the label that scopes
	// their own rules to their own namespace.
	NamespaceMatcherLabel string `json:"namespaceMatcherLabel,omitempty"`
	// Rules is evaluated against a CR's namespace's labels, first match
	// wins (mirrors how giantswarm/silence-operator scopes silences). A
	// namespace matching no entry means CRs in it are not compiled into
	// the engine at all - fail closed, not fail open.
	Rules []EnforcementRuleConfig `json:"rules,omitempty"`
}

// EnforcementRuleConfig is one namespace-selector-gated policy entry.
type EnforcementRuleConfig struct {
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector,omitempty"`
	// Match lists additional authoritative matchers appended (ANDed) to
	// every CR-sourced rule this entry governs, on top of
	// NamespaceMatcherLabel.
	Match []MatchConfig `json:"match,omitempty"`
	// Labels/Annotations independently constrain which label/annotation
	// names a CR-sourced rule's set/drop actions may target.
	Labels      LabelPolicyConfig `json:"labels,omitempty"`
	Annotations LabelPolicyConfig `json:"annotations,omitempty"`
	// AllowedSources lists which of the file config's declared sources a
	// CR-sourced rule's from.source may reference. Empty (the default)
	// means none: a source lookup can read anything the enricher's
	// ServiceAccount/credentials can reach, including secrets, so tenants
	// get no sources unless explicitly granted one.
	AllowedSources []string `json:"allowedSources,omitempty"`
	// AllowRequired permits a CR-sourced rule to set required: true. A
	// required rule that fails aborts the whole batch (503, not just the
	// tenant's own alerts), so this defaults to false.
	AllowRequired bool `json:"allowRequired,omitempty"`
	// MaxRulesPerNamespace caps how many EnrichmentRule CRs from one
	// namespace are compiled; 0 means unlimited.
	MaxRulesPerNamespace int `json:"maxRulesPerNamespace,omitempty"`
}

// LabelPolicyConfig is a deny-by-default allow/deny list for label or
// annotation names a CR-sourced rule may set/drop. If Allow is non-empty,
// only names in Allow are permitted and Deny is ignored. Otherwise every
// name is permitted except those listed in Deny (and, for labels,
// EnforcementConfig.NamespaceMatcherLabel, which is always denied).
type LabelPolicyConfig struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
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

// SetAction sets a label or annotation to a literal value, a template, or a
// source lookup. Exactly one of Label/Annotation is required. Annotations
// carry no fingerprint risk (unlike labels), so Force never applies to
// them - only to a reserved Label.
type SetAction struct {
	Label      string      `json:"label,omitempty"`
	Annotation string      `json:"annotation,omitempty"`
	Value      string      `json:"value,omitempty"`
	Template   string      `json:"template,omitempty"`
	From       *FromConfig `json:"from,omitempty"`
	Default    string      `json:"default,omitempty"`
	Overwrite  bool        `json:"overwrite,omitempty"`
	// Force must be set to touch a reserved label (see ReservedLabels);
	// required in addition to Overwrite, since overwriting alertname is a
	// distinct, more consequential choice than overwriting an ordinary
	// label and deserves its own explicit opt-in. Not applicable to
	// annotations.
	Force bool `json:"force,omitempty"`
}

// DropAction removes a label or annotation. Exactly one of Label/Annotation
// is required.
type DropAction struct {
	Label      string `json:"label,omitempty"`
	Annotation string `json:"annotation,omitempty"`
	// Force must be set to drop a reserved label (see ReservedLabels). Not
	// applicable to annotations.
	Force bool `json:"force,omitempty"`
}

// ReservedLabels are label names Alertmanager treats as special (currently
// just alertname, which every alert is expected to carry). Dropping or
// overwriting one changes how an alert is identified and displayed, so
// doing so requires the action's Force field to be set.
var ReservedLabels = map[string]bool{
	"alertname": true,
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

	// Strict: an unknown key is an error, not a silent no-op. A typo like
	// `enrichmnt:` or `retires: 3` would otherwise parse cleanly, leave the
	// default in place, and give `enricher check` no reason to complain -
	// a config change that appears to apply and doesn't is a bad failure
	// mode for the component alert delivery runs through.
	var cfg Config
	if err := yaml.UnmarshalStrict(expanded, &cfg); err != nil {
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
	if cfg.CRD.MaxRules == 0 {
		cfg.CRD.MaxRules = 1000
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
