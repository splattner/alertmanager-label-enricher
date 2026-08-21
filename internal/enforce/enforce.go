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
func Rule(r config.RuleConfig, ns string, nsLabels map[string]string, declaredSources map[string]bool, cfg config.EnforcementConfig) (config.RuleConfig, error) {
	// A CR's rule name is only unique within its own namespace, and a CR
	// namespace can otherwise be chosen to collide with a file-config rule
	// name. Compiling to "<namespace>/<name>" keeps ale_rule_evaluations_total
	// unambiguous across every rule source without the caller having to
	// remember to do it. Error messages below still report the tenant's
	// own r.Name, since that's what they'll recognize from their own CR.
	compiledName := ns + "/" + r.Name

	// Well-formedness is checked before, and independently of, any policy:
	// a rule that cannot compile would fail engine.Compile, and that fails
	// the recompile for every tenant, not just its author. This runs even
	// in single-tenant mode, where there is no policy to apply - "we trust
	// whoever writes these" is a statement about intent, not about syntax.
	if err := config.ValidateRule(r, declaredSources); err != nil {
		return config.RuleConfig{}, fmt.Errorf("rule %q: %w", r.Name, err)
	}

	if len(cfg.Rules) == 0 {
		enforced := r
		enforced.Name = compiledName
		return enforced, nil
	}

	policy, ok := SelectPolicy(cfg, nsLabels)
	if !ok {
		return config.RuleConfig{}, fmt.Errorf("namespace %q matches no enforcement policy", ns)
	}

	for _, a := range r.Actions {
		if a.Set == nil || a.Set.From == nil {
			continue
		}
		if !slices.Contains(policy.AllowedSources, a.Set.From.Source) {
			return config.RuleConfig{}, fmt.Errorf("rule %q: source %q is not permitted in namespace %q", r.Name, a.Set.From.Source, ns)
		}
	}

	if r.Required && !policy.AllowRequired {
		return config.RuleConfig{}, fmt.Errorf("rule %q: required: true is not permitted in namespace %q", r.Name, ns)
	}

	protected := protectedLabels(cfg.NamespaceMatcherLabel, policy.Match)
	for i, a := range r.Actions {
		if err := checkAction(a, protected, policy); err != nil {
			return config.RuleConfig{}, fmt.Errorf("rule %q: actions[%d]: %w", r.Name, i, err)
		}
	}

	enforced := r
	enforced.Name = compiledName
	enforced.Match = append(append([]config.MatchConfig{}, r.Match...), authoritativeMatchers(cfg.NamespaceMatcherLabel, ns, policy.Match)...)
	return enforced, nil
}

// protectedLabels is every label this policy asserts as authoritative: the
// namespace-scoping label plus each label named in the policy's own match
// list. All of them must be unwritable by the rule they scope.
//
// Matchers are evaluated before actions, so a rule can otherwise satisfy an
// authoritative matcher and then overwrite the very label that matched it -
// firing only on `cluster=prod` alerts and relabelling them `cluster=staging`,
// which defeats the scoping the matcher exists to provide.
func protectedLabels(namespaceMatcherLabel string, policyMatch []config.MatchConfig) []string {
	var out []string
	if namespaceMatcherLabel != "" {
		out = append(out, namespaceMatcherLabel)
	}
	for _, m := range policyMatch {
		if m.Label != "" && !slices.Contains(out, m.Label) {
			out = append(out, m.Label)
		}
	}
	return out
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

func checkAction(a config.ActionConfig, protected []string, policy config.EnforcementRuleConfig) error {
	switch {
	case a.Set != nil:
		return checkTarget(a.Set.Label, a.Set.Annotation, protected, policy)
	case a.Drop != nil:
		return checkTarget(a.Drop.Label, a.Drop.Annotation, protected, policy)
	}
	return nil
}

func checkTarget(label, annotation string, protected []string, policy config.EnforcementRuleConfig) error {
	if annotation != "" {
		// Annotations are never authoritative - nothing is scoped by one -
		// so the protected set does not apply to them.
		return checkPolicy("annotation", annotation, policy.Annotations, nil)
	}
	return checkPolicy("label", label, policy.Labels, protected)
}

// checkPolicy enforces one LabelPolicyConfig against one target name.
// protected is checked first and unconditionally: those labels stay
// unwritable regardless of allow/deny, because the policy relies on them
// to scope the rule.
func checkPolicy(kind, name string, policy config.LabelPolicyConfig, protected []string) error {
	if slices.Contains(protected, name) {
		return fmt.Errorf("%s %q is asserted by this namespace's enforcement policy and cannot be targeted by a namespaced rule", kind, name)
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
