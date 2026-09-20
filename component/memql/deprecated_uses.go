package memql

// deprecated_uses.go -- the load's account of the deprecated language forms the
// DSL it read still spells (memql#5390; D22 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// A deprecated form keeps loading while its window runs, and a load that finds
// one says so four ways, all from one scan of the sources Init loaded:
//
//   - the load report carries a Warning per use (its code the form's rule, its
//     message the form's warning), which the offline passes hand to the author
//     -- memqllint prints it -- and which a strict boot ignores;
//   - one WARN log line per use names the file, line and column;
//   - memql_dsl_deprecated_uses_total{rule} rises by the uses found, so
//     refusing a form is a decision taken on evidence rather than on a guess
//     about who still writes it;
//   - MemQLEngine.DeprecatedUses lists them, for a surface that shows where the
//     cluster's DSL still spells one.
//
// Once the form's window is spent the same use is a strict-boot problem
// instead: a coded Skip carrying the refusal the parser makes of the spelling,
// so a load and a parse say one thing about it. The load records it here rather
// than trusting each construct's loader to: a concept's parse failure drops the
// concept without a skip of its own (ExtractConceptDecls), and `array(T)` is
// written in concepts.
//
// WHAT IS SCANNED is what Init reads its constructs from, the merged tree
// baseloader.ReadAll returns: the embedded core tree, every registered pack and
// every domain mounted from MEMQL_DSL_PATH or as a lint overlay, each file
// through its edition's front end. A durably promoted authored construct is not
// in that tree -- it is recompiled from its stored row after Init -- and is not
// scanned.
//
// # Which release the window is read at
//
// The whole decision is deprecation.Form.RefusesAt(release), and this is the
// component that knows which release the binary is: core/buildinfo's link-time
// stamp, which is EMPTY in every build that was not cut from a release. So a
// dev build, a test binary and the language server read "", which window.go
// cannot parse and therefore fails OPEN on -- they warn and never refuse --
// while a cluster cut from the release that closes a window refuses it with no
// flag for anyone to remember to flip. init() rather than a call inside Init:
// the parser consults the same answer, and a parse can happen before any engine
// is built.
//
// Setting it HERE, in the engine, is also what keeps the escape hatch open
// after the door shuts: cmd/memqlmigrate does not import this package, so its
// Current() stays empty and `--rewrite=slice-syntax` still reads a form the
// engine has stopped loading. A migration tool that refused the spelling it
// exists to remove would leave a bundle with no way across.

import (
	"fmt"
	"log/slog"
	"sort"

	"github.com/znasllc-io/memql/component/language/deprecation"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
	"github.com/znasllc-io/memql/component/metrics"
	"github.com/znasllc-io/memql/core/buildinfo"
)

func init() { deprecation.SetCurrent(buildinfo.Release()) }

// deprecatedFormsComponent is the load-report scope of a deprecated-form
// finding.
const deprecatedFormsComponent = "memql.deprecatedForms"

// DeprecatedUseRecord is one use of a deprecated form in a source the last Init
// loaded.
type DeprecatedUseRecord struct {
	// Rule is the form's rule (deprecation.Form.Rule); File the source's path
	// in the DSL tree; Text the spelling as written.
	Rule, File, Text string
	// Line and Column are where the spelling starts in File, 1-based, the
	// column counting runes.
	Line, Column int
}

// DeprecatedUses returns every use of a deprecated form -- inside its window or
// past it -- in the sources the last Init loaded, sorted by file, then line,
// then column. Nil before Init and when nothing loaded spells one. The slice is
// the caller's.
func (e *MemQLEngine) DeprecatedUses() []DeprecatedUseRecord {
	r := e.loadReport
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.deprecatedUses) == 0 {
		return nil
	}
	return append([]DeprecatedUseRecord(nil), r.deprecatedUses...)
}

// recordDeprecatedUses scans files for uses of a deprecated form and records
// what each one is on report: a Warning while the form is inside its window, a
// coded Skip once the window is spent. It logs each use on logger, which may be
// nil, and adds the uses to the metric -- for EVERY registered form, so a form
// nothing uses has its series at zero from this load on. Every node type loads
// the tree in its engine phase, before its transport phase serves /metrics, so
// no scrape sees a registered form's series missing: an alert over a series
// that does not exist evaluates to no data, which reads the same as "no use".
//
// release is the release the window is read at, which is what makes the whole
// path testable a release early: pass one past the window and this records the
// refusals a cluster cut from it will record.
func recordDeprecatedUses(report *LoadReport, files []baseloader.RawFile, logger *slog.Logger, release string) {
	var records []DeprecatedUseRecord
	for _, f := range files {
		for _, u := range languageParser.ScanDeprecatedUses(f.Content) {
			records = append(records, DeprecatedUseRecord{Rule: u.Rule, File: f.Path, Text: u.Text, Line: u.Line, Column: u.Column})
		}
	}
	sort.SliceStable(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Column < b.Column
	})

	// One Tracker per registered form, each over its own window at release:
	// the counting and the decision then come from the same object, so a use
	// cannot be counted under one reading of the window and judged under
	// another (component/language/deprecation).
	trackers := deprecation.Trackers(release)
	for _, rec := range records {
		form, ok := deprecation.Lookup(rec.Rule)
		if !ok {
			continue // unreachable while every rule the scanner reports is registered (parser tests hold it)
		}
		tracker := trackers[rec.Rule]
		tracker.Record(rec.Rule)
		at := fmt.Sprintf("%s:%d:%d", rec.File, rec.Line, rec.Column)
		if tracker.Refuses(rec.Rule) {
			use := languageParser.DeprecatedUse{Rule: rec.Rule, Line: rec.Line, Column: rec.Column, Text: rec.Text}
			report.AddSkip(baseloader.SkipFor(deprecatedFormsComponent, "deprecated form", rec.Text, rec.File, "parse",
				languageParser.DeprecatedUseRefusal(use, form)))
			if logger != nil {
				logger.Error("DSL uses a deprecated form whose window has closed: refused",
					"component", deprecatedFormsComponent, "rule", form.Rule, "at", at, "detail", form.Refusal())
			}
			continue
		}
		report.AddWarning(baseloader.Warning{
			Component: deprecatedFormsComponent,
			File:      rec.File,
			Line:      rec.Line,
			Column:    rec.Column,
			Code:      form.Rule,
			Message:   form.Warning(),
		})
		if logger != nil {
			logger.Warn("DSL uses a deprecated form",
				"component", deprecatedFormsComponent, "rule", form.Rule, "at", at, "detail", form.Warning())
		}
	}
	for rule, tracker := range trackers {
		metrics.DSLDeprecatedUse(rule, tracker.Counts()[rule])
	}

	report.mu.Lock()
	report.deprecatedUses = records
	report.mu.Unlock()
}
