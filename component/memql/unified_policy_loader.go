package memql

// unified_policy_loader.go loads the live policy system in the tree:
// SI Router routing policies — struct-form `policy NAME { }` blocks
// with @primary / @fallback / @maxLatencyMs / @preferredRole
// annotations, consumed by Router to resolve a policy name to a
// provider chain. Source: dsl/policies/policies.memql.
//
// (The decision-policy tier — procedural `func (Policy)` blocks
// evaluated via engine.EvaluatePolicy — was retired in #984.)
//
// The loader walks dsl.Tree() and feeds the per-slice text through the
// langparser load-time path so all validation still runs.

import (
	"fmt"
	"log/slog"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// LoadUnifiedPolicies walks the unified tree, extracts every
// `policy NAME { }` block through the langparser's load-time path,
// and registers the resulting PolicyConfig in the supplied
// registry. Also rebuilds the role -> policy map so DefaultForRole
// resolves correctly. First @preferredRole wins (matches legacy
// behaviour).
//
// memql#333 (sub-epic #329 / Stage 1C of #310) migrated the parsing
// half off the hand-rolled parsePolicyMemQL onto
// languageParser.ParsePolicyDecl + the in-package
// policyDeclToPolicyConfig converter. The hand-rolled parser is
// unreferenced from production after this child; tests in
// policy_parser_test.go still exercise it pending #329's cleanup PR.
func LoadUnifiedPolicies(logger *slog.Logger, registry *PolicyRegistry) (int, error) {
	if registry == nil {
		return 0, fmt.Errorf("policy registry is nil")
	}
	total := 0
	for _, raw := range baseloader.ReadAll(logger) {
		for _, slice := range ExtractKeywordSlices(raw.Content, "policy") {
			decl, err := languageParser.ParsePolicyDecl(slice.Source)
			if err != nil {
				if logger != nil {
					logger.Debug("unified policy loader: parse failed",
						"file", raw.Path, "policy", slice.Name, "error", err)
				}
				continue
			}
			cfg, err := policyDeclToPolicyConfig(decl)
			if err != nil {
				if logger != nil {
					logger.Debug("unified policy loader: convert failed",
						"file", raw.Path, "policy", slice.Name, "error", err)
				}
				continue
			}
			cfg.Name = slice.Name
			registry.mu.Lock()
			registry.byName[cfg.Name] = cfg
			for _, role := range cfg.PreferredRoles {
				role = strings.TrimSpace(role)
				if role == "" {
					continue
				}
				if _, exists := registry.byRole[role]; !exists {
					registry.byRole[role] = cfg.Name
				}
			}
			registry.mu.Unlock()
			total++
		}
	}
	if logger != nil {
		logger.Info("unified policy loader: registered routing policies",
			"component", "memql.unifiedPolicyLoader", "count", total)
	}
	return total, nil
}
