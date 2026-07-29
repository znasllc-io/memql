package memql

// lint_parity.go gives an offline tool (cmd/memqllint) a lint-time equivalent
// of the engine's init-time DSL validation. The parse + import-graph pass in
// component/memql/dslimports catches referential-integrity problems, but a
// pack that passes it can still CrashLoop the node that mounts it: the engine
// runs a second tier of validators at boot (relationship-type canonicality,
// the per-function contract gates, CQS, the dependency tree, per-kind
// uniqueness, ...) that dslimports does not model. Two proven witness classes:
// a non-canonical @relationship type ("references"), and a mutation that
// declares an arg it never references.
//
// Rather than re-implement those rules here (they would drift from the
// engine's), LintUnifiedTree runs the ENGINE'S OWN MemQLEngine.Init over the
// merged embedded-core + linted-overlay tree and reports every problem Init
// would reject or skip at boot. Init needs only a concept registry -- no
// database, no network -- so the whole boot-time validation tier runs offline.
// This is the same construction the strict-boot and deploy-pack load tests use.

import (
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// LintDiagnostic is one init-time problem the engine-parity lint pass found.
// File is the origin path in the DSL tree when the problem is attributable to
// a single construct (a loader parse/register skip); it is empty for a
// whole-tree error (an invalid @relationship type, a CQS or dependency-tree
// violation, a duplicate concept) that aborts Init before the load report is
// assembled.
type LintDiagnostic struct {
	File    string
	Message string
}

// LintUnifiedTree mounts every product-domain directory found in root as an
// overlay on the embedded core tree, then runs the engine's full init-time DSL
// validation over the merged tree and returns every problem the engine would
// reject or skip at boot. A pack that produces zero diagnostics here mounts
// clean; a diagnostic here is a latent CrashLoop.
//
// root is typically os.DirFS over a DSL root whose top-level entries are
// product namespace directories. Domains that collide with a core embedded
// domain are skipped (the embedded tree already owns them), so pointing this
// at the engine's own dsl/ tree validates the embedded tree as-is.
//
// Those skipped names are returned as skippedCoreDomains (memql#2782). The
// caller needs them to tell two very different outcomes apart: a domain that
// was validated and found clean, and a domain whose on-disk contents were
// never looked at because the embedded tree owns the namespace. Both otherwise
// produce zero diagnostics.
//
// Global state (the plugin-tree registrations and the additive concept
// registry) is restored before returning, so repeated in-process calls do not
// leak a mounted pack's concepts into the next run.
func LintUnifiedTree(logger *slog.Logger, root fs.FS) ([]LintDiagnostic, []string, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	}

	_, skippedCore, unmount := memqldsl.MountOverlayDomains(logger, root)
	defer func() {
		unmount()
		// LoadUnifiedConcepts is additive (MergeAll), and Init normalizes
		// concept relationships in place, so restore a clean embedded-only
		// registry: clear, then reload the now-overlay-free tree.
		concept.ReplaceAll(nil)
		_, _ = LoadUnifiedConcepts(logger)
	}()

	_, conceptSkips, err := LoadUnifiedConceptsWithSkips(logger)
	if err != nil {
		return nil, skippedCore, fmt.Errorf("loading concepts from merged tree: %w", err)
	}
	registry := concept.DefaultRegistry()

	// Route the constructor's own log line to io.Discard so the lint tool
	// stays quiet; Init-time logs go through eng.Logger, set below.
	eng, engErr := New(nil, (&component.Component{}).WithLoggerWriter(io.Discard))
	if engErr != nil {
		return nil, skippedCore, fmt.Errorf("constructing lint engine: %w", engErr)
	}
	eng.Logger = logger

	initErr := eng.Init(registry)

	var diags []LintDiagnostic

	// Concepts the unified loader could not build (memql#2909). These never
	// reach the registry, so they leave no trace in eng.loadReport -- that
	// report covers CONSTRUCT-phase skips. Without this the parity pass runs
	// the engine's own schema build and throws the result away, which is how
	// a bundle with a property typed `boolean` linted clean and dropped the
	// concept at boot. A dropped concept is not a dropped property: every
	// query, mutation and shape bound to it fails at runtime.
	for _, cs := range conceptSkips {
		diags = append(diags, LintDiagnostic{File: cs.File, Message: cs.String()})
	}

	// Per-construct skips + duplicate registrations from the load report.
	// These are collected even when Init succeeds -- they only fail Init via
	// the strict-boot gate at the very end -- so they carry precise
	// per-construct attribution (the declared-usage witness lands here as a
	// parse-phase skip).
	if eng.loadReport != nil {
		for _, s := range eng.loadReport.Skipped {
			diags = append(diags, LintDiagnostic{
				File:    s.File,
				Message: fmt.Sprintf("%s %q (%s): %s", s.Keyword, s.Name, s.Phase, s.Err),
			})
		}
		for _, d := range eng.loadReport.Duplicates {
			diags = append(diags, LintDiagnostic{Message: "duplicate construct: " + d.String()})
		}
	}

	// A hard Init error that is NOT the strict-boot aggregate (already
	// represented by the per-construct skips above) is its own diagnostic: an
	// invalid @relationship type, a relationship structural violation, a CQS
	// or dependency-tree violation, a duplicate/empty concept. These abort
	// Init before the report gate, so they surface only through the returned
	// error.
	if initErr != nil && !strings.Contains(initErr.Error(), "strict DSL boot refused") {
		diags = append(diags, LintDiagnostic{Message: initErr.Error()})
	}

	sort.SliceStable(diags, func(i, j int) bool {
		if diags[i].File != diags[j].File {
			return diags[i].File < diags[j].File
		}
		return diags[i].Message < diags[j].Message
	})
	return diags, skippedCore, nil
}
