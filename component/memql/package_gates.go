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
// WHAT IS STILL GLOBAL, precisely. Init's construct loaders read the process
// tree (dsl.Tree()) rather than taking one, so the candidate's domains must be
// mounted there for the width of the pass. That leaves exactly one observable:
// for that width, dsl.Tree() lists the candidate's product domains alongside
// the cluster's own. Nothing rebuilds the live engine from it -- app/engine.go
// is the only runtime Init and it runs once, at boot -- so the live engine's
// constructs, specs, schemas and tiers are all fixed before this can run. The
// three runtime readers that remain are bounded: the capability-name set is a
// boot-warmed sync.Once, a prompt lookup is by exact name, and a namespace pin
// is per-domain and so cannot be changed by an unrelated domain arriving. A
// package CANNOT shadow a core domain in any of them: MountOverlayDomains
// skips a colliding name outright, which is also how this pass learns to
// refuse dsl_domain_reserved.
//
// packageGateMu serializes passes against each other, because two candidate
// trees mounted at once would validate each against the other's domains.

import (
	"io"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"sync"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/component"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

var packageGateMu sync.Mutex

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

	packageGateMu.Lock()
	defer packageGateMu.Unlock()

	mounted, skippedCore, unmount := memqldsl.MountOverlayDomains(logger, root)
	defer unmount()

	result := PackageDSLResult{Mounted: mounted, SkippedCore: skippedCore}

	concepts, conceptSkips, err := BuildUnifiedConcepts(logger, memqldsl.Tree())

	// A concept the loader could not BUILD leaves no trace in the load report,
	// which covers construct-phase skips only. Reporting it is the difference
	// between "your property is mistyped" and a bundle that lints clean and
	// silently loses a concept at boot, taking every query, mutation and shape
	// bound to it (memql#2909).
	for _, cs := range conceptSkips {
		result.Diagnostics = append(result.Diagnostics, LintDiagnostic{File: cs.File, Message: cs.String()})
	}
	if err != nil {
		result.Diagnostics = append(result.Diagnostics, LintDiagnostic{Message: err.Error()})
		sortDiagnostics(result.Diagnostics)
		return result, nil
	}

	eng, engErr := New(nil, (&component.Component{}).WithLoggerWriter(io.Discard))
	if engErr != nil {
		return result, engErr
	}
	eng.Logger = logger

	initErr := eng.Init(memoryNodes.NewRegistry(concepts))

	if eng.loadReport != nil {
		for _, s := range eng.loadReport.Skipped {
			result.Diagnostics = append(result.Diagnostics, LintDiagnostic{
				File:    s.File,
				Message: s.Keyword + " " + s.Name + " (" + s.Phase + "): " + s.Err,
			})
		}
		for _, d := range eng.loadReport.Duplicates {
			result.Diagnostics = append(result.Diagnostics, LintDiagnostic{Message: "duplicate construct: " + d.String()})
		}
	}

	// A hard Init error that is NOT the strict-boot aggregate is its own
	// diagnostic: an invalid @relationship type, a CQS or dependency-tree
	// violation, a duplicate or empty concept. Those abort Init before the
	// report gate, so they surface only through the returned error.
	if initErr != nil && !strings.Contains(initErr.Error(), "strict DSL boot refused") {
		result.Diagnostics = append(result.Diagnostics, LintDiagnostic{Message: initErr.Error()})
	}

	sortDiagnostics(result.Diagnostics)
	return result, nil
}

func sortDiagnostics(d []LintDiagnostic) {
	sort.SliceStable(d, func(i, j int) bool {
		if d[i].File != d[j].File {
			return d[i].File < d[j].File
		}
		return d[i].Message < d[j].Message
	})
}
