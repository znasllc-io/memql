package memql

// package_gates.go runs the engine's own Init-grade DSL validation over a
// CANDIDATE package tree, from inside a process that is also serving.
//
// It exists because LintUnifiedTree, the pass that already does this, cannot
// be called here (memql#4794). That pass reaches its answer by emptying the
// global concept registry and reloading it -- correct in cmd/memqllint, which
// owns its process, and a security hole in a node that does not: for the width
// of the pass every concept's declared row-authz tier is ABSENT from the
// registry every live read consults, and an absent tier admits everybody. The
// window is short, which is the property that would have kept it out of a test
// and in production.
//
// So this pass builds the candidate tree's concepts into a registry of its own
// (memoryNodes.NewRegistry) and hands THAT to a throwaway engine's Init. The
// default registry is never read, never written, and never momentarily empty.
//
// Every candidate is validated against a private filesystem view: embedded
// core plus the registered product domains, with this package's domains
// replaced by its candidate bytes. Neither the live DSL registry nor serving
// bindings (providers, secret resolvers, shopper routes or cache metrics) are
// published by this pass. Concurrent requests continue reading the live tree.

import (
	"io"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/component"
	"github.com/znasllc-io/memql/core/dslfs"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// PackageDSLResult is what one candidate tree's Init-grade validation found.
type PackageDSLResult struct {
	// Mounted names the product domains the pass actually validated.
	Mounted []string
	// SkippedCore names domains that were NOT looked at because a core
	// embedded domain owns the namespace. The engine's own runtime mount
	// skips these silently -- correct at boot, where the embedded tree must
	// win, and wrong at analysis time, where the author believes they shipped
	// that domain. The caller turns each into a dsl_domain_reserved refusal.
	SkippedCore []string
	// Diagnostics is every problem strict boot would print, construct by
	// construct. Empty means this tree mounts clean.
	Diagnostics []LintDiagnostic
	// UnreadRootManifest is the diagnostic for a memql.toml at the root of the
	// tree (language_line_unread), or nil. It is NOT in Diagnostics: no mount
	// reads a root file -- each domain declares its line in its own
	// <domain>/memql.toml -- so boot refuses nothing for it, and a deploy must
	// neither be refused for it nor be told it "would refuse boot".
	UnreadRootManifest *LintDiagnostic
}

// AnalyzePackageDSL validates the product-DSL half of a candidate package.
//
// root is the package's dsl/ directory: its top-level entries are product
// namespace directories, exactly as MEMQL_DSL_PATH expects them. A tree with
// no dsl/ directory at all is not an error -- an SPAs-only package is an
// ordinary package (D6) -- so the caller decides whether to call this.
func AnalyzePackageDSL(logger *slog.Logger, root fs.FS) (PackageDSLResult, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	}

	tree, mounted, skippedCore, err := memqldsl.CandidateTree(root)
	if err != nil {
		return PackageDSLResult{}, err
	}

	result := PackageDSLResult{Mounted: mounted, SkippedCore: skippedCore}
	if msg, unread := memqldsl.UnreadRootManifest(root); unread {
		// A warning of its own: boot refuses nothing for it (lint_parity.go).
		result.UnreadRootManifest = &LintDiagnostic{File: dslfs.ManifestFile, Message: msg, Severity: LintSeverityWarning}
	}

	concepts, conceptSkips, err := BuildUnifiedConcepts(logger, tree)

	// A concept the loader could not BUILD leaves no trace in the load report,
	// which covers construct-phase skips only. Reporting it is the difference
	// between "your property is mistyped" and a bundle that lints clean and
	// silently loses a concept at boot, taking every query, mutation and shape
	// bound to it (memql#2909).
	for _, cs := range conceptSkips {
		result.Diagnostics = append(result.Diagnostics, LintDiagnostic{File: cs.File, Message: cs.String(), Severity: LintSeverityError})
	}
	if err != nil {
		result.Diagnostics = append(result.Diagnostics, LintDiagnostic{Message: err.Error(), Severity: LintSeverityError})
		sortDiagnostics(result.Diagnostics)
		return result, nil
	}

	eng, engErr := newEngine(nil, false, (&component.Component{}).WithLoggerWriter(io.Discard))
	if engErr != nil {
		return result, engErr
	}
	eng.Logger = logger

	initErr := eng.initFromTree(memoryNodes.NewRegistry(concepts), tree, false)

	if eng.loadReport != nil {
		for _, s := range eng.loadReport.Skipped {
			result.Diagnostics = append(result.Diagnostics, LintDiagnostic{File: s.File, Message: skipDiagnostic(s), Severity: LintSeverityError})
		}
		for _, d := range eng.loadReport.Duplicates {
			result.Diagnostics = append(result.Diagnostics, LintDiagnostic{Message: "duplicate construct: " + d.String(), Severity: LintSeverityError})
		}
		// Deliberately NO warnings here. Every diagnostic a package deploy
		// reads means "this would refuse boot"; a deprecated form inside its
		// window would not, and mixing the two would block a deploy on a
		// courtesy (memql#5390).
	}

	// A hard Init error that is NOT the strict-boot aggregate is its own
	// diagnostic: an invalid @relationship type, a CQS or dependency-tree
	// violation, a duplicate or empty concept. Those abort Init before the
	// report gate, so they surface only through the returned error.
	if initErr != nil && !strings.Contains(initErr.Error(), "strict DSL boot refused") {
		result.Diagnostics = append(result.Diagnostics, LintDiagnostic{Message: initErr.Error(), Severity: LintSeverityError})
	}

	sortDiagnostics(result.Diagnostics)
	// The concept build and Init both refuse a domain whose language line the
	// engine will not read (memql#5357); the package author reads it once.
	result.Diagnostics = dedupeDiagnostics(result.Diagnostics)
	return result, nil
}

// sortDiagnostics orders diagnostics by file, then position, then message. An
// error carries no position of its own, so errors keep their file-then-message
// order and a file's positioned warnings follow them in source order.
func sortDiagnostics(d []LintDiagnostic) {
	sort.SliceStable(d, func(i, j int) bool {
		if d[i].File != d[j].File {
			return d[i].File < d[j].File
		}
		if d[i].Line != d[j].Line {
			return d[i].Line < d[j].Line
		}
		if d[i].Column != d[j].Column {
			return d[i].Column < d[j].Column
		}
		return d[i].Message < d[j].Message
	})
}
