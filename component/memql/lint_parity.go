package memql

// lint_parity.go gives an offline tool (cmd/memqllint) a lint-time equivalent
// of the engine's init-time DSL validation. The parse + import-graph pass in
// component/memql/dslimports catches referential-integrity problems, but a
// pack that passes it can still CrashLoop the node that mounts it: the engine
// runs a second tier of validators at boot (relationship-type canonicality,
// the per-function contract gates, CQS, the dependency tree, per-kind
// uniqueness, ...) that dslimports does not model. Two proven witness classes:
// a non-canonical @relationship type ("assignedTo"), and a mutation that
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
	"strings"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql/baseloader"
	"github.com/znasllc-io/memql/core/component"
	"github.com/znasllc-io/memql/core/dslfs"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// LintDiagnostic is one init-time finding the engine-parity lint pass made.
// File is the origin path in the DSL tree when the finding is attributable to
// a single construct (a loader parse/register skip) or a single use; it is
// empty for a whole-tree error (an invalid @relationship type, a CQS or
// dependency-tree violation, a duplicate concept) that aborts Init before the
// load report is assembled.
type LintDiagnostic struct {
	File    string
	Message string
	// Code is the refusal's stable rule id when it carries one (a lowering
	// refusal's `lower_*`, an annotation's `annotation_*`, a deprecated form's
	// rule), and empty otherwise. Message carries it too, in brackets at the
	// end.
	Code string
	// Severity is LintSeverityError for a finding boot refuses, and
	// LintSeverityWarning for one that loads: a use of a deprecated language
	// form still inside its window (memql#5390), whose Message is the form's
	// warning. A caller deciding whether a tree mounts reads the errors only;
	// an empty Severity is an error.
	Severity string
	// Line and Column place a warning in File, 1-based, the column counting
	// runes; zero when the finding carries no position of its own (an error's
	// Message says where it is).
	Line, Column int
}

// The two severities a LintDiagnostic carries.
const (
	LintSeverityError   = "error"
	LintSeverityWarning = "warning"
)

// IsWarning reports whether d is a finding that does not stop the tree
// loading.
func (d LintDiagnostic) IsWarning() bool { return d.Severity == LintSeverityWarning }

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
		// Report the skips gathered before the hard error too. A tree can
		// hold a build-skip in one file and a fatal id error in a later one,
		// and returning only the second sends the author back for a second
		// round over a problem already measured (memql#2909).
		var diags []LintDiagnostic
		for _, cs := range conceptSkips {
			diags = append(diags, LintDiagnostic{File: cs.File, Message: cs.String(), Severity: LintSeverityError})
		}
		return withUnreadRootManifest(diags, root), skippedCore, fmt.Errorf("loading concepts from merged tree: %w", err)
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
		diags = append(diags, LintDiagnostic{File: cs.File, Message: cs.String(), Severity: LintSeverityError})
	}

	// Per-construct skips + duplicate registrations from the load report.
	// These are collected even when Init succeeds -- they only fail Init via
	// the strict-boot gate at the very end -- so they carry precise
	// per-construct attribution (the declared-usage witness lands here as a
	// parse-phase skip).
	if eng.loadReport != nil {
		for _, s := range eng.loadReport.Skipped {
			diags = append(diags, LintDiagnostic{File: s.File, Message: skipDiagnostic(s), Code: s.Code, Severity: LintSeverityError})
		}
		for _, d := range eng.loadReport.Duplicates {
			diags = append(diags, LintDiagnostic{Message: "duplicate construct: " + d.String(), Severity: LintSeverityError})
		}
		// What loads but must still change: each use of a deprecated form
		// inside its window (memql#5390), positioned, its message the form's
		// warning word for word. Never a reason the tree does not mount.
		for _, w := range eng.loadReport.WarningsSnapshot() {
			diags = append(diags, LintDiagnostic{File: w.File, Line: w.Line, Column: w.Column, Message: w.Message, Code: w.Code, Severity: LintSeverityWarning})
		}
	}

	// A hard Init error that is NOT the strict-boot aggregate (already
	// represented by the per-construct skips above) is its own diagnostic: an
	// invalid @relationship type, a relationship structural violation, a CQS
	// or dependency-tree violation, a duplicate/empty concept. These abort
	// Init before the report gate, so they surface only through the returned
	// error.
	if initErr != nil && !strings.Contains(initErr.Error(), "strict DSL boot refused") {
		diags = append(diags, LintDiagnostic{Message: initErr.Error(), Severity: LintSeverityError})
	}
	diags = withUnreadRootManifest(diags, root)

	sortDiagnostics(diags)
	return dedupeDiagnostics(diags), skippedCore, nil
}

// withUnreadRootManifest adds the diagnostic for a memql.toml at the root of
// the mounted tree, which no mount reads (dsl.UnreadRootManifest, memql#5357).
// Boot never sees that file, so it is the offline passes that must say so,
// where an author reads rather than in a log they discard: memqllint in its
// diagnostics, a package deploy as a warning of its own
// (PackageDSLResult.UnreadRootManifest) -- boot refuses nothing for it, so a
// deploy must not either.
func withUnreadRootManifest(diags []LintDiagnostic, root fs.FS) []LintDiagnostic {
	if msg, unread := memqldsl.UnreadRootManifest(root); unread {
		diags = append(diags, LintDiagnostic{File: dslfs.ManifestFile, Message: msg, Severity: LintSeverityError})
	}
	return diags
}

// skipDiagnostic is how the offline passes print one load-report skip. A
// ConceptSkip that stands for a tree-level refusal (a refused language line,
// memql#5357) prints through here too, which is what makes the concept-phase
// copy of a refusal and Init's copy one diagnostic rather than two.
func skipDiagnostic(s baseloader.Skip) string {
	return fmt.Sprintf("%s %q (%s): %s", s.Keyword, s.Name, s.Phase, s.Err)
}

// dedupeDiagnostics drops a diagnostic identical to the one before it, in a
// list already sorted by file then message. The concept build and Init both
// refuse a domain whose language line the engine will not read (memql#5357),
// so a pass that collects both holds that refusal twice; an author must read
// it once.
func dedupeDiagnostics(diags []LintDiagnostic) []LintDiagnostic {
	out := make([]LintDiagnostic, 0, len(diags))
	for i, d := range diags {
		if i > 0 && d == diags[i-1] {
			continue
		}
		out = append(out, d)
	}
	return out
}
