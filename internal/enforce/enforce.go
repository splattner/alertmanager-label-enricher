// Package enforce is the tenancy security boundary for CR-sourced rules
// (see internal/crd): it decides whether a namespaced EnrichmentRule is
// permitted to exist as configured, and if so, augments it with the
// authoritative matchers that scope it to its own namespace.
//
// It never weakens a rule's own matchers - it only ANDs an additional
// authoritative matcher onto them, so a tenant matcher that contradicts it
// simply matches nothing rather than being overridden. And it denies
// (returns an error for) anything a rule's actions try to do that the
// policy disallows, rather than silently stripping the offending action -
// a rejected rule fails loudly instead of applying a neutered version of
// itself. This package has no I/O: every decision is a pure function of
// its inputs, which is deliberate given it is the security boundary.
package enforce

import (
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/splattner/alertmanager-label-enricher/internal/config"
)

// Rule applies cfg's enforcement policy to a rule sourced from a
// namespaced EnrichmentRule CR in namespace ns (whose labels are
// nsLabels), returning the enforced rule - ready for engine.Compile - or
// an error explaining why the rule was rejected.
//
// If cfg has no policy rules at all, enforcement is a deliberate no-op:
// CR-sourced rules pass through unrestricted, matching file-config rules.
// This is single-tenant convenience mode; the caller is responsible for
// warning about it at startup when the CRD watch is enabled without any
// enforcement policy configured.
func Rule(r config.RuleConfig, ns string, nsLabels map[string]string, cfg config.EnforcementConfig) (config.RuleConfig, error) {
	// A CR's rule name is only unique within its own namespace, and a CR
	// namespace can otherwise be chosen to collide with a file-config rule
	// name. Compiling to "<namespace>/<name>" keeps ale_rule_evaluations_total
	// unambiguous across every rule source without the caller having to
	// remember to do it. Error messages below still report the tenant's
	// own r.Name, since that's what they'll recognize from their own CR.
	compiledName := ns + "/" + r.Name

	if len(cfg.Rules) == 0 {
		enforced := r
		enforced.Name = compiledName
		return enforced, nil
	}

	policy, ok := SelectPolicy(cfg, nsLabels)
	if !ok {
		return config.RuleConfig{}, fmt.Errorf("namespace %q matches no enforcement policy", ns)
	}

	allowedSources := make(map[string]bool, len(policy.AllowedSources))
	for _, s := range policy.AllowedSources {
		allowedSources[s] = true
	}
	if err := config.ValidateRule(r, allowedSources); err != nil {
		return config.RuleConfig{}, fmt.Errorf("rule %q: %w", r.Name, err)
	}

	if r.Required && !policy.AllowRequired {
		return config.RuleConfig{}, fmt.Errorf("rule %q: required: true is not permitted in namespace %q", r.Name, ns)
	}

	for i, a := range r.Actions {
		if err := checkAction(a, cfg.NamespaceMatcherLabel, policy); err != nil {
			return config.RuleConfig{}, fmt.Errorf("rule %q: actions[%d]: %w", r.Name, i, err)
		}
	}

	enforced := r
	enforced.Name = compiledName
	enforced.Match = append(append([]config.MatchConfig{}, r.Match...), authoritativeMatchers(cfg.NamespaceMatcherLabel, ns, policy.Match)...)
	return enforced, nil
}

// SelectPolicy evaluates cfg.Rules against nsLabels, first match wins - a
// namespace-selector-gated policy list, mirroring how
// giantswarm/silence-operator scopes silence enforcement rules to
// namespaces. An entry with an empty NamespaceSelector matches every
// namespace, so it's usable as a trailing catch-all. Exported so callers
// that need the selected policy for reasons other than enforcing one rule
// (e.g. internal/crd capping MaxRulesPerNamespace) don't reimplement
// selection.
func SelectPolicy(cfg config.EnforcementConfig, nsLabels map[string]string) (config.EnforcementRuleConfig, bool) {
	for _, r := range cfg.Rules {
		sel, err := metav1.LabelSelectorAsSelector(&r.NamespaceSelector)
		if err != nil {
			// config.Validate (via validateEnforcement) already rejects an
			// uncompilable selector at config load time, so this is
			// unreachable on any config that made it into a running
			// Engine. Skip rather than panic on a live reconcile path.
			continue
		}
		if sel.Matches(labels.Set(nsLabels)) {
			return r, true
		}
	}
	return config.EnforcementRuleConfig{}, false
}

func authoritativeMatchers(namespaceMatcherLabel, ns string, extra []config.MatchConfig) []config.MatchConfig {
	var out []config.MatchConfig
	if namespaceMatcherLabel != "" {
		out = append(out, config.MatchConfig{Label: namespaceMatcherLabel, Op: config.OpEq, Value: ns})
	}
	return append(out, extra...)
}

func checkAction(a config.ActionConfig, namespaceMatcherLabel string, policy config.EnforcementRuleConfig) error {
	switch {
	case a.Set != nil:
		return checkTarget(a.Set.Label, a.Set.Annotation, namespaceMatcherLabel, policy)
	case a.Drop != nil:
		return checkTarget(a.Drop.Label, a.Drop.Annotation, namespaceMatcherLabel, policy)
	}
	return nil
}

func checkTarget(label, annotation, namespaceMatcherLabel string, policy config.EnforcementRuleConfig) error {
	if annotation != "" {
		return checkPolicy("annotation", annotation, policy.Annotations, "")
	}
	return checkPolicy("label", label, policy.Labels, namespaceMatcherLabel)
}

// checkPolicy enforces one LabelPolicyConfig against one target name.
// alwaysDenied, when set, is checked first and unconditionally - it's how
// the namespace-scoping label stays unwritable regardless of allow/deny.
func checkPolicy(kind, name string, policy config.LabelPolicyConfig, alwaysDenied string) error {
	if alwaysDenied != "" && name == alwaysDenied {
		return fmt.Errorf("%s %q is the namespace-scoping label and cannot be targeted by a namespaced rule", kind, name)
	}
	if len(policy.Allow) > 0 {
		if !slices.Contains(policy.Allow, name) {
			return fmt.Errorf("%s %q is not in this namespace's allow list", kind, name)
		}
		return nil
	}
	if slices.Contains(policy.Deny, name) {
		return fmt.Errorf("%s %q is denied for this namespace", kind, name)
	}
	return nil
}
