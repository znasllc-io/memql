package memql

// unified_policy_loader.go covers the two policy systems that live in
// the new tree:
//
//   - SI Router routing policies: struct-form `policy NAME { }` blocks
//     with @primary / @fallback / @maxLatencyMs / @preferredRole
//     annotations. Consumed by Router to resolve a policy name to a
//     provider chain. Source: dsl/policies/routing.memql.
//
//   - Decision policy functions: procedural `func (Policy) NAME(ctx)
//     <type> { ... }` blocks with @tier / @frontend_visible / etc.
//     Consumed by engine.EvaluatePolicy for cross-cutting decisions.
//     Source: dsl/policies/<tier>.memql (one file per tier).
//
// Both loaders walk dsl.Tree() and feed the per-slice text through the
// existing single-declaration parsers (parsePolicyMemQL,
// parsePolicyFunctionFile) so all the legacy validation still runs.

import (
	"fmt"
	"log/slog"
	"strings"

	languageParser "github.com/visionarys-io/memql/component/language/parser"
	"github.com/visionarys-io/memql/component/memql/baseloader"
)

// LoadUnifiedPolicies walks the unified tree, extracts every
// `policy NAME { }` block, parses each via parsePolicyMemQL, and
// registers the resulting PolicyConfig in the supplied registry.
//
// Also rebuilds the role -> policy map so DefaultForRole resolves
// correctly. First @preferredRole wins (matches legacy behavior).
func LoadUnifiedPolicies(logger *slog.Logger, registry *PolicyRegistry) (int, error) {
	if registry == nil {
		return 0, fmt.Errorf("policy registry is nil")
	}
	total := 0
	for _, raw := range baseloader.ReadAll(logger) {
		for _, slice := range ExtractKeywordSlices(raw.Content, "policy") {
			cfg, err := parsePolicyMemQL("unified:"+raw.Path+":"+slice.Name, []byte(slice.Source))
			if err != nil {
				if logger != nil {
					logger.Debug("unified policy loader: parse failed",
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

// LoadUnifiedPolicyFunctions walks the unified tree, extracts every
// `func (Policy) NAME(...) { ... }` block, parses each via
// parsePolicyFunctionFile, and registers the resulting PolicyFunction
// in the supplied registry. The tier comes from the explicit
// @tier("core"|"bff") annotation on each policy.
//
// Cross-tier validation (core -> bff calls) runs once over the full
// registry after every file has been loaded.
func LoadUnifiedPolicyFunctions(logger *slog.Logger, registry *PolicyFunctionRegistry) (int, error) {
	if registry == nil {
		return 0, fmt.Errorf("policy function registry is nil")
	}
	total := 0
	for _, raw := range baseloader.ReadAll(logger) {
		for _, slice := range ExtractFunctionSlices(raw.Content) {
			if slice.Kind != languageParser.FunctionTypePolicy {
				continue
			}
			// Pass a synthetic origin so the filename-vs-function-name
			// check in policyFunctionFromDef passes against
			// <funcName>.memql. Tier is empty because the new
			// per-construct layout has no directory-based tier --
			// policyFunctionFromDef reads the @tier annotation.
			syntheticOrigin := slice.Name + ".memql"
			fn, err := parsePolicyFunctionFile(syntheticOrigin, "", []byte(slice.Source))
			if err != nil {
				if logger != nil {
					logger.Debug("unified policy-function loader: parse failed",
						"file", raw.Path, "policy", slice.Name, "error", err)
				}
				continue
			}
			fn.Origin = "unified:" + raw.Path
			registry.mu.Lock()
			registry.byName[fn.Name] = fn
			registry.mu.Unlock()
			total++
		}
	}

	// Cross-tier validation: core policies must not call bff policies.
	// Walk the registered set once; report each violating edge.
	for _, p := range registry.All() {
		violations := findCrossTierCallViolations(p, registry)
		for _, v := range violations {
			if logger != nil {
				logger.Warn("policy: cross-tier delegation rejected",
					"policy", p.Name,
					"tier", p.Tier,
					"target", v.target,
					"targetTier", v.targetTier,
				)
			}
		}
	}

	if logger != nil {
		logger.Info("unified policy-function loader: registered decision policies",
			"component", "memql.unifiedPolicyFunctionLoader", "count", total)
	}
	return total, nil
}

