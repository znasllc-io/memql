package memql

// unified_rule_loader.go loads the `rule` corpus: struct-form
// `rule NAME { }` blocks with @when / @policy / @level / @precedence /
// @onUnavailable / @exclude / @locked, consumed by the AI router to decide
// which policy chain serves a call. Source: dsl/rules/rules.memql, plus any
// rule a mounted product bundle declares in its own domain.
//
// It mirrors unified_policy_loader.go -- the same walk, the same slice, the
// same langparser load-time path -- and differs in exactly two ways, both of
// which are the reason this file exists rather than a second call to the
// generic loader:
//
//  1. @locked IS REFUSED OUTSIDE THE EMBEDDED TREE. Only the loader knows
//     which tree a file came from; the parser reads text and cannot. Without
//     the check, a mounted bundle could declare a rule that evaluates before
//     every shipped one, which is precisely the authority @locked exists to
//     reserve.
//  2. REGISTRATION REFUSES A DUPLICATE rather than overwriting. Last-writer-
//     wins is what let a bundle silently replace a shipped construct: no
//     message, and a cluster routing by a rule nobody can find.

import (
	"fmt"
	"io/fs"
	"log/slog"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// LoadUnifiedRules walks the unified tree, extracts every `rule NAME { }`
// block through the langparser's load-time path, registers the resulting
// RuleConfig, and finalizes the evaluation order.
//
// Finalization is part of loading rather than a separate call the caller might
// forget: an unfinalized registry answers Ordered() with nil, and a router
// walking nil rules resolves every call through no rule at all.
func LoadUnifiedRules(logger *slog.Logger, registry *RuleRegistry, report ...*LoadReport) (int, error) {
	return loadUnifiedRulesFromTree(memqldsl.Tree(), logger, registry, report...)
}

func loadUnifiedRulesFromTree(tree fs.FS, logger *slog.Logger, registry *RuleRegistry, report ...*LoadReport) (int, error) {
	if registry == nil {
		return 0, fmt.Errorf("rule registry is nil")
	}
	rep := firstReport(report)
	core := coreDomainSet()
	total := 0

	for _, raw := range baseloader.ReadAllFrom(tree, logger) {
		for _, slice := range ExtractKeywordSlices(raw.Content, "rule") {
			decl, err := languageParser.ParseRuleDecl(slice.Source)
			if err != nil {
				logRuleSkip(logger, rep, raw.Path, slice.Name, "parse", err)
				continue
			}
			cfg, err := ruleDeclToRuleConfig(decl, raw.Path)
			if err != nil {
				logRuleSkip(logger, rep, raw.Path, slice.Name, "convert", err)
				continue
			}
			cfg.Name = slice.Name

			// @locked is the embedded tree's alone. A bundle that wants its
			// rule to win says so with @precedence, which is visible, ordered
			// against every other rule, and cannot outrank a shipped one.
			if cfg.Locked && !core[domainOf(raw.Path)] {
				logRuleSkip(logger, rep, raw.Path, slice.Name, "convert",
					fmt.Errorf("rule %q: @locked is accepted only in the embedded tree -- a mounted rule that "+
						"could lock itself would evaluate before every shipped rule, which is the authority the "+
						"annotation exists to reserve. Use @precedence instead", cfg.Name))
				continue
			}

			if err := registry.Register(cfg); err != nil {
				logRuleSkip(logger, rep, raw.Path, slice.Name, "register", err)
				continue
			}
			total++
		}
	}

	if err := registry.Finalize(); err != nil {
		rep.AddSkip(baseloader.Skip{
			Component: "memql.unifiedRuleLoader", Keyword: "rule", Name: "(order)",
			File: "dsl/rules/rules.memql", Phase: "finalize", Err: err.Error(),
		})
		if logger != nil {
			logger.Warn("unified rule loader: the evaluation order could not be computed",
				"component", "memql.unifiedRuleLoader", "error", err)
		}
		return total, err
	}

	rep.AddRegistered("rules", total)
	if logger != nil {
		logger.Info("unified rule loader: registered routing rules",
			"component", "memql.unifiedRuleLoader", "count", total)
	}
	return total, nil
}

func logRuleSkip(logger *slog.Logger, rep *LoadReport, path, name, phase string, err error) {
	if logger != nil {
		logger.Warn("unified rule loader: "+phase+" failed",
			"file", path, "rule", name, "error", err)
	}
	rep.AddSkip(baseloader.Skip{
		Component: "memql.unifiedRuleLoader", Keyword: "rule",
		Name: name, File: path, Phase: phase, Err: err.Error(),
	})
}

// domainOf returns the first path segment, which is the DSL domain a file
// belongs to. The merged tree keys mounts by domain, so the segment is what
// says whether a file was compiled in or mounted at runtime.
func domainOf(path string) string {
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return path
}

// coreDomainSet is dsl.CoreDomains as a lookup. A runtime mount colliding with
// a core domain is skipped by the mount path itself, so a file under a core
// domain came from the embedded tree.
func coreDomainSet() map[string]bool {
	names := memqldsl.CoreDomains()
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}
