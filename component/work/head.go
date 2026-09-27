package work

// head.go -- a run's HEAD and its steps' VERSIONS (epic memql#5414; design
// record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md,
// D18 and D19).
//
// EVERY VERSION IS KEPT, AND THE HEAD ONLY SAYS WHICH ONE IS CURRENT. A step
// re-run with a person's changes is a new version of the same step row -- the
// journal's runId-stepKey id, whose rows are append-only -- and every step
// after it runs again from the new answer, each as a new version too. Going
// back is not an undo: it moves the head to an earlier version, and "the
// previous version is still there" is a property of the store rather than a
// feature anybody had to build.
//
// THE BASIS IS WHAT MAKES GOING BACK CHEAP. A version records the head of
// every earlier step at the moment it started. Moving the head back to an
// earlier version of step k can then RESTORE each later step's version that
// was computed from that same upstream, instead of running it again -- the
// energy and the tokens spent once are not spent twice -- and only the first
// later step with no such version, and everything after it, runs again.
//
// THIS FILE DECIDES AND NEVER WRITES, for the reason ladder.go gives:
// integrations/work reads the versions, calls these, and writes what comes
// back, so every rule here is a table test with no engine.

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// HeadEntry names the current version of one step. RunId is set only when
// that version lives in ANOTHER run: a branch's shared prefix, served by
// reference from the run it forked.
type HeadEntry struct {
	Version int    `json:"version"`
	RunId   string `json:"runId,omitempty"`
}

// Head maps a top-level step key to its current version.
type Head map[string]HeadEntry

// Errors the head decisions return. Each is wrapped with the step it is about,
// because "a step was refused" is not a sentence anybody can act on.
var (
	// ErrStepNotInRun: the key is not one of the run's top-level steps.
	ErrStepNotInRun = errors.New("the step is not one of this run's steps")
	// ErrNestedStep: the key belongs to a list inside a step (a logic's
	// statement, a loop body). It is re-run with the step that holds it.
	ErrNestedStep = errors.New("the step is inside another step; re-run the step that holds it")
	// ErrVersionNotFound: the run records no such version of the step.
	ErrVersionNotFound = errors.New("the run records no such version of the step")
	// ErrVersionNotDone: the version did not finish, so it cannot be current.
	ErrVersionNotDone = errors.New("the version did not finish, so it cannot be made current")
)

// ParseHead decodes a stored head: a JSON object of {version, runId} entries,
// as it arrives off a row (numbers as float64). It is tolerant of a value it
// cannot read -- an entry that is not an object, or names no positive version,
// is skipped rather than guessed -- and answers nil for an absent head, which a
// reader treats as "each step's newest version is current".
func ParseHead(v any) Head {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	out := Head{}
	for key, raw := range m {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		version := intOf(entry["version"])
		if version <= 0 {
			continue
		}
		runId, _ := entry["runId"].(string)
		out[key] = HeadEntry{Version: version, RunId: strings.TrimSpace(runId)}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Object is the stored form of the head. Never nil: an empty head is written
// as {}, which is how a writer clears one.
func (h Head) Object() map[string]any {
	out := make(map[string]any, len(h))
	for key, e := range h {
		entry := map[string]any{"version": e.Version}
		if e.RunId != "" {
			entry["runId"] = e.RunId
		}
		out[key] = entry
	}
	return out
}

// Equal reports whether two heads name the same version of every step.
func (h Head) Equal(o Head) bool {
	if len(h) != len(o) {
		return false
	}
	for key, e := range h {
		if oe, ok := o[key]; !ok || oe != e {
			return false
		}
	}
	return true
}

// clone copies a head, so a decision never mutates the caller's.
func (h Head) clone() Head {
	out := make(Head, len(h))
	for k, v := range h {
		out[k] = v
	}
	return out
}

// BasisFor is the entries of head for the keys BEFORE key in order: the
// upstream a version of key runs against. A key the head does not name is
// left out rather than guessed.
func BasisFor(order []string, head Head, key string) Head {
	out := Head{}
	for _, k := range order {
		if k == key {
			break
		}
		if e, ok := head[k]; ok {
			out[k] = e
		}
	}
	return out
}

// PristineBasis is every earlier key at version 1 of this same run -- the
// basis of an ordinary run nobody has re-run, which the journal does not write
// (a basis on every step of every run would be O(n^2) for no reader).
func PristineBasis(order []string, key string) Head {
	out := Head{}
	for _, k := range order {
		if k == key {
			break
		}
		out[k] = HeadEntry{Version: 1}
	}
	return out
}

// IsPristine reports whether a basis is the pristine one, so the journal can
// omit it. A basis missing an earlier key is NOT pristine: writing it costs a
// few bytes, while omitting it would let a reader reconstruct an upstream the
// version never saw.
func IsPristine(basis Head, order []string, key string) bool {
	return basis.Equal(PristineBasis(order, key))
}

// StepVersion is one recorded version of one step, as the head decisions need
// it. A Basis read off a row where the journal omitted it (the pristine case)
// must be PristineBasis(order, key); VersionOf builds that.
type StepVersion struct {
	Key     string
	Version int
	Basis   Head
	Status  string
}

// VersionOf builds a StepVersion from what a row carries, reconstructing the
// pristine basis the journal omitted.
func VersionOf(order []string, key string, version int, storedBasis any, status string) StepVersion {
	basis := ParseHead(storedBasis)
	if basis == nil {
		basis = PristineBasis(order, key)
	}
	return StepVersion{Key: key, Version: version, Basis: basis, Status: status}
}

// finished reports whether a version can be current: it ran to a receipt.
// "skipped" counts, because a condition that skipped a step is an answer the
// run gave, and the steps after it were computed from it.
func (v StepVersion) finished() bool {
	return v.Status == "done" || v.Status == "skipped"
}

// NewestHead is the head of a run that stored none: each step's newest
// version, which is what every collapsed read already shows as current.
func NewestHead(order []string, versions map[string][]StepVersion) Head {
	out := Head{}
	for _, k := range order {
		if v := maxVersion(versions[k]); v > 0 {
			out[k] = HeadEntry{Version: v}
		}
	}
	return out
}

// RerunPlan is what running a step again executes.
type RerunPlan struct {
	// From is the first key that executes: the step being re-run.
	From string
	// Versions is the version each re-executed key runs as: one past the
	// highest it has recorded, so a re-run never reuses a version number --
	// and so never reuses the idempotency key a side effect ran under.
	Versions map[string]int
	// Stale is every key from From on, in order: each gets a new version.
	Stale []string
}

// PlanRerun decides what re-running key executes (D18): key itself and every
// step after it, each as a new version. The prefix is served from its current
// versions and never runs again.
func PlanRerun(order []string, versions map[string][]StepVersion, key string) (RerunPlan, error) {
	idx, err := indexOfTopLevel(order, key)
	if err != nil {
		return RerunPlan{}, err
	}
	plan := RerunPlan{From: key, Versions: map[string]int{}}
	for _, k := range order[idx:] {
		plan.Versions[k] = maxVersion(versions[k]) + 1
		plan.Stale = append(plan.Stale, k)
	}
	return plan, nil
}

// MoveHead makes version of key current (D18), and answers the new head and
// the steps that must run again.
//
// A version is made current WITH THE UPSTREAM IT WAS COMPUTED FROM: every
// earlier step its basis names takes that version too. Keeping today's
// upstream under an answer computed from another would draw a run that never
// happened -- the timeline would show step b reading an output of step a that b
// never saw.
//
// Then, for every step after key, in order, the NEWEST finished version whose
// basis equals the new head's entries for every earlier step becomes current --
// it was computed from exactly this upstream, so it is restored rather than
// run. The first later step with no such version is stale, and so is every
// step after it (their upstream is about to get a new version); their entries
// are left as they were until the re-run replaces them. Moving the head to a
// version whose whole downstream matches re-runs nothing.
func MoveHead(order []string, head Head, versions map[string][]StepVersion, key string, version int) (Head, []string, error) {
	idx, err := indexOfTopLevel(order, key)
	if err != nil {
		return nil, nil, err
	}
	target, ok := findVersion(versions[key], version)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s version %d", ErrVersionNotFound, key, version)
	}
	if !target.finished() {
		return nil, nil, fmt.Errorf("%w: %s version %d is %s", ErrVersionNotDone, key, version, target.Status)
	}
	next := head.clone()
	for _, k := range order[:idx] {
		if e, ok := target.Basis[k]; ok {
			next[k] = e
		}
	}
	next[key] = HeadEntry{Version: version}
	for i := idx + 1; i < len(order); i++ {
		k := order[i]
		want := BasisFor(order, next, k)
		match, found := newestMatching(versions[k], want)
		if !found {
			return next, append([]string(nil), order[i:]...), nil
		}
		next[k] = HeadEntry{Version: match.Version}
	}
	return next, nil, nil
}

// ForkAt is a branch's initial head (D19): every step before key, pointing at
// the source run's current version of it by reference. The source keeps its
// own runId on an entry that was already a reference, so a branch of a branch
// still points at the run that holds the version.
func ForkAt(order []string, source Head, sourceRunId, key string) (Head, error) {
	idx, err := indexOfTopLevel(order, key)
	if err != nil {
		return nil, err
	}
	out := Head{}
	for _, k := range order[:idx] {
		e, ok := source[k]
		if !ok {
			continue
		}
		if e.RunId == "" {
			e.RunId = sourceRunId
		}
		out[k] = e
	}
	return out, nil
}

// indexOfTopLevel finds key among the run's own steps.
func indexOfTopLevel(order []string, key string) (int, error) {
	if strings.Contains(key, "/") {
		return 0, fmt.Errorf("%w: %s", ErrNestedStep, key)
	}
	for i, k := range order {
		if k == key {
			return i, nil
		}
	}
	return 0, fmt.Errorf("%w: %s", ErrStepNotInRun, key)
}

func findVersion(vs []StepVersion, version int) (StepVersion, bool) {
	for _, v := range vs {
		if v.Version == version {
			return v, true
		}
	}
	return StepVersion{}, false
}

// newestMatching is the newest finished version whose basis is want.
func newestMatching(vs []StepVersion, want Head) (StepVersion, bool) {
	sorted := append([]StepVersion(nil), vs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Version > sorted[j].Version })
	for _, v := range sorted {
		if v.finished() && v.Basis.Equal(want) {
			return v, true
		}
	}
	return StepVersion{}, false
}

func maxVersion(vs []StepVersion) int {
	max := 0
	for _, v := range vs {
		if v.Version > max {
			max = v.Version
		}
	}
	return max
}

// intOf reads a JSON number the way a decoded row carries it. A value that is
// not a whole number inside int32's range answers 0, which every caller treats
// as absent.
//
// narrowing: GUARDED -- each arm checks the int32 bound (and, for float64,
// wholeness) before converting. component/work is a leaf module with no
// requires, so core/num is not reachable here; a version or a count past two
// billion is not a real value, and 0 is what every caller reads as absent.
func intOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		if n > math.MaxInt32 || n < math.MinInt32 {
			return 0
		}
		return int(n)
	case float64:
		if n != math.Trunc(n) || n > math.MaxInt32 || n < math.MinInt32 {
			return 0
		}
		return int(n)
	}
	return 0
}
