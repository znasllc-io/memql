package memql

// load_report.go is the single structured account of a full DSL load
// pass (epic #2351 / S2, memql#2357). It is the fail-loud successor to
// the old "every loader skips a bad construct and boots anyway" posture:
//
//   - S1 (#2356) raised the per-slice parse/register failures from Debug
//     to WARN so a dropped construct is visible in logs.
//   - S5 (#2360) added the load-time per-kind uniqueness gate that ERROR-
//     logs same-registry name collisions.
//   - S2 (this file) folds BOTH signals -- every loader's skipped[]
//     constructs and the uniqueness gate's duplicates -- into one report
//     threaded through engine.Init, prints a boot summary, and makes
//     engine.Init REFUSE to boot the embedded core tree + registered
//     packs when the report has problems, unless an operator sets the
//     break-glass MEMQL_DSL_ALLOW_SKIPS.
//
// A half-loaded pack is worse than one that fails: a multi-node fleet
// boots green with arbitrary construct subsets (the copresent-rot
// mechanism, bff#153). Strict boot turns that silent, per-node drift
// into a loud, uniform boot failure.
//
// The durable-bundle re-hydration quarantines (authoring_promote_*.go)
// are recorded here too, but on a SEPARATE axis: they do NOT count
// toward the strict-boot decision. Stored authored bundles can rot when
// the grammar moves (a construct authored under an older grammar no
// longer recompiles); a rotted stored row must NOT brick a fleet the way
// a broken IN-TREE construct does. So a re-hydration failure is
// quarantined (skipped + ERROR-logged + reported) and boot continues.
// The grammar-version stamp (#2361) completes that picture by letting
// the engine recognise which stored rows predate a grammar move.

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// allowSkipsEnvVar is the operator break-glass: when set truthy, a DSL
// load pass with skipped constructs and/or duplicate registrations boots
// anyway (loudly), instead of engine.Init returning an error. Registered
// in scripts/secrets/manifest.yaml so the env-registry drift gate
// (cmd/envscan) and the cockpit Configuration screen both see it.
const allowSkipsEnvVar = "MEMQL_DSL_ALLOW_SKIPS"

// QuarantinedConstruct is a durably-promoted authored construct whose
// STORED source failed to recompile at boot re-hydration (grammar drift,
// a deleted concept dependency, ...). It is skipped + ERROR-logged +
// recorded here, but does NOT fail boot -- stored authored rows must not
// brick a fleet. Distinct from a baseloader.Skip, which is an IN-TREE
// (embedded core / pack) construct drop and DOES fail strict boot.
type QuarantinedConstruct struct {
	Kind     string
	Name     string
	BundleId string
	Owner    string
	Err      string
}

// LoadReport accumulates the outcome of a full DSL load pass. One report
// is built per engine.Init and stored on the engine so the post-Init
// re-hydration path can record quarantines onto the same ledger.
//
// Concurrency: Skipped / Duplicates / Registered are written only during
// the single-threaded engine.Init load sequence. Quarantined can be
// appended later from the live authoring-promote propagation subscriber
// (a runtime goroutine), so every mutator takes mu.
type LoadReport struct {
	mu          sync.Mutex
	Registered  map[string]int         // group ("shapes", "functions", ...) -> count registered
	Skipped     []baseloader.Skip      // parse/register drops from every unified loader
	Duplicates  []DuplicateConstruct   // same-registry name collisions (S5 / #2360)
	Quarantined []QuarantinedConstruct // durable-bundle re-hydration failures (does NOT fail boot)
}

// newLoadReport builds an empty report ready for the load sequence.
func newLoadReport() *LoadReport {
	return &LoadReport{Registered: map[string]int{}}
}

// newBaseloaderSink returns a fresh *baseloader.Report for a single
// baseloader-backed loader call; fold its skips back in with FoldSink.
func newBaseloaderSink() *baseloader.Report { return &baseloader.Report{} }

// firstReport picks the first non-nil report from a variadic list, or nil
// when none was supplied. The LoadUnified* functions take a trailing
// `report ...*LoadReport` so the ~dozens of existing test callers keep
// compiling while engine.Init opts in by passing one.
func firstReport(reports []*LoadReport) *LoadReport {
	for _, r := range reports {
		if r != nil {
			return r
		}
	}
	return nil
}

// FoldSink records the registered count for group and merges every skip
// the baseloader sink collected into this report. Convenience for the
// LoadUnified* wrappers that delegate to baseloader.LoadOne / LoadMany.
func (r *LoadReport) FoldSink(group string, registered int, sink *baseloader.Report) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Registered[group] += registered
	if sink != nil {
		r.Skipped = append(r.Skipped, sink.Skips...)
	}
}

// AddSkip records one in-tree construct drop.
func (r *LoadReport) AddSkip(s baseloader.Skip) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Skipped = append(r.Skipped, s)
}

// AddRegistered accumulates the registered count for a loader group.
func (r *LoadReport) AddRegistered(group string, n int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Registered[group] += n
}

// SetDuplicates records the S5 uniqueness-gate detections.
func (r *LoadReport) SetDuplicates(dups []DuplicateConstruct) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Duplicates = dups
}

// AddQuarantine records a durable-bundle re-hydration failure. Does NOT
// affect the strict-boot decision (see HasProblems).
func (r *LoadReport) AddQuarantine(q QuarantinedConstruct) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Quarantined = append(r.Quarantined, q)
}

// HasProblems reports whether the report has anything that should fail a
// strict boot: an in-tree construct that was skipped, or a same-registry
// duplicate. Quarantined constructs are deliberately excluded -- they are
// stored authored rows, not in-tree DSL, and must not brick a fleet.
func (r *LoadReport) HasProblems() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.Skipped) > 0 || len(r.Duplicates) > 0
}

// ProblemCount is the total strict-boot-blocking problem count.
func (r *LoadReport) ProblemCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.Skipped) + len(r.Duplicates)
}

// registeredTotal sums every group's registered count. Caller must hold
// mu (or accept a racy snapshot -- only used for the summary log).
func (r *LoadReport) registeredTotal() int {
	total := 0
	for _, n := range r.Registered {
		total += n
	}
	return total
}

// Detail renders a multi-line, human-readable account of every skipped
// construct + duplicate, each named with its file + error. Used in the
// strict-boot error and the detailed startup log.
func (r *LoadReport) Detail() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	if len(r.Skipped) > 0 {
		fmt.Fprintf(&b, "%d skipped construct(s):\n", len(r.Skipped))
		skips := append([]baseloader.Skip(nil), r.Skipped...)
		sort.Slice(skips, func(i, j int) bool {
			if skips[i].File != skips[j].File {
				return skips[i].File < skips[j].File
			}
			return skips[i].Name < skips[j].Name
		})
		for _, s := range skips {
			fmt.Fprintf(&b, "  - [%s] %s %q in %s (%s): %s\n",
				s.Component, s.Keyword, s.Name, s.File, s.Phase, s.Err)
		}
	}
	if len(r.Duplicates) > 0 {
		fmt.Fprintf(&b, "%d duplicate construct name(s):\n", len(r.Duplicates))
		for _, d := range r.Duplicates {
			b.WriteString("  - " + d.String() + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// logSummary emits the boot summary: one healthy Info line when the tree
// loaded clean, or an Error line plus the per-problem detail when it did
// not. Called at the tail of engine.Init after the strict-boot decision.
func (r *LoadReport) logSummary(logger *slog.Logger) {
	if logger == nil || r == nil {
		return
	}
	r.mu.Lock()
	registered := r.registeredTotal()
	skipped := len(r.Skipped)
	dups := len(r.Duplicates)
	quarantined := len(r.Quarantined)
	r.mu.Unlock()

	if skipped == 0 && dups == 0 {
		logger.Info("DSL load report: healthy",
			"component", ComponentName,
			"registered", registered,
			"skipped", 0,
			"duplicates", 0,
			"quarantined", quarantined)
		return
	}
	logger.Error("DSL load report: DEGRADED -- constructs were dropped at load",
		"component", ComponentName,
		"registered", registered,
		"skipped", skipped,
		"duplicates", dups,
		"quarantined", quarantined,
		"detail", r.Detail())
}

// dslAllowSkips reports whether the operator has set the break-glass
// MEMQL_DSL_ALLOW_SKIPS to a truthy value. Read directly off the process
// env (like MEMQL_DSL_PATH) -- the engine bootstrap runs before the
// config snapshot exists. A malformed value is treated as false (strict).
func dslAllowSkips() bool {
	v := strings.TrimSpace(os.Getenv(allowSkipsEnvVar))
	if v == "" {
		return false
	}
	if b, err := strconv.ParseBool(v); err == nil {
		return b
	}
	switch strings.ToLower(v) {
	case "1", "yes", "on":
		return true
	}
	return false
}
