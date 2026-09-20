package deprecation

// forms.go -- the forms currently in a window, and the release the window is
// read at (memql#5390; D22 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// window.go is the ARITHMETIC -- how long a window is, when it is spent, what
// the warning says. This file is the TABLE that arithmetic is applied to, and
// the one place a form is written down. Everything that has to agree about a
// deprecated form reads it: the parser's scan and its refusal, the engine's
// load report, the metric, Sense, the language server, and the table in
// docs/public/language/memql.md.
//
// # Deprecated is not retired
//
// A RETIRED form (component/language/parser/v1_refusals.go) refuses the moment
// it is retired, and the tree is migrated in the same change. A DEPRECATED form
// is the gentler promise a frozen edition makes: it keeps loading, every use
// warns naming what to write instead and the rewrite that writes it, every use
// is counted, and only once the window is spent does it refuse.
//
// # The refusal is a function of (form, release), not a flag
//
// Nothing in this table says "this form is refused now". Whether a form still
// loads is Form.RefusesAt(release) -- window.go's arithmetic over the form's
// DeprecatedIn and the release asking -- so the same table answers for the
// release that ships it and for any release a test cares to name. A flag would
// have to be flipped by hand in the release that refuses, which is a promise
// kept by somebody remembering.
//
// Current() is that release for the process: the release the binary was CUT
// FROM, which component/memql sets from core/buildinfo. It is empty in every
// build that is not a release -- a dev build, a test binary, the language
// server -- and an empty release is unreadable, so it warns and never refuses.
// That is window.go's fail-open posture, and it is the right one for an editor:
// the squiggle is a courtesy, the refusal belongs to the cluster.

import (
	"fmt"
	"sort"
	"sync"
)

// ArrayType is the rule of `array(T)`, the list type the Go-style `[]T`
// replaced and the first form through the window. It is a CONSTANT because it
// is what the scanner reports, what a diagnostic's code is, and what labels the
// metric: a misspelling should be a compile error, not a use that names no form.
const ArrayType = "deprecated_array_type"

// Form is one deprecated spelling and the two releases that bound its window.
type Form struct {
	// Rule is the stable id: the diagnostic code, the metric's label, and what
	// a docs row is keyed on. Wording may be revised; a rule, once shipped, is
	// not.
	Rule string
	// Spelling is what the author wrote, with a placeholder for the part that
	// varies: "array(T)".
	Spelling string
	// Replacement is what to write instead: "[]T".
	Replacement string
	// Migrator is the rewrite that carries a tree across.
	Migrator string
	// DeprecatedIn is the release whose load first warns, as window.go reads a
	// release: "MAJOR.MINOR" or "MAJOR.MINOR.PATCH".
	DeprecatedIn string
}

// shipped is every form this engine has in a window. Order is irrelevant --
// Forms sorts by rule.
//
// `array(T)` was deprecated in 0.23.0 and the arithmetic does the rest: the
// tree's VERSION is 0.22.8, so the next minor to ship is 0.23, and
// MinimumMinorReleases is 2, so Window.expiresAt puts the refusal at
// 0.23 + 2 = 0.25. Nothing here states 0.25; RefusedFrom derives it, which is
// what stops the table and the window disagreeing about the same form.
var shipped = []Form{
	{
		Rule:         ArrayType,
		Spelling:     "array(T)",
		Replacement:  "[]T",
		Migrator:     "memqlmigrate --rewrite=slice-syntax",
		DeprecatedIn: "0.23.0",
	},
}

var byRule = func() map[string]Form {
	out := make(map[string]Form, len(shipped))
	for _, f := range shipped {
		out[f.Rule] = f
	}
	return out
}()

// Forms returns every form in a window, sorted by rule. The slice is the
// caller's.
func Forms() []Form {
	out := make([]Form, 0, len(byRule))
	for _, f := range byRule {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rule < out[j].Rule })
	return out
}

// Lookup returns the form registered under rule.
func Lookup(rule string) (Form, bool) {
	f, ok := byRule[rule]
	return f, ok
}

// Window is the window f is read through at release current.
//
// The length is always MinimumMinorReleases: a form cannot be given a longer
// window without moving DeprecatedIn, which is also the published column. See
// the package comment's "Known limitation" for why that is not built for and
// what the fix would look like.
func (f Form) Window(current string) Window {
	return Window{MinorReleases: MinimumMinorReleases, DeprecatedAt: f.DeprecatedIn, Current: current}
}

// RefusedFrom is the first release the form stops loading at, as MAJOR.MINOR --
// the patch is not part of a window, so every 0.25.x refuses (window.go).
// Empty when DeprecatedIn cannot be read, which is the fail-open case: a form
// with no readable deprecation point has no readable expiry either.
func (f Form) RefusedFrom() string { return f.Window("").expiresAt() }

// RefusesAt reports whether f has spent its window by release current, so it no
// longer loads. THE decision, and the whole of it: a pure function of the form
// and a release, which is what lets a test see the refusal a release early
// rather than waiting for one.
func (f Form) RefusesAt(current string) bool { return New(f.Window(current)).Refuses(f.Rule) }

// Warning is what a load, a lint run and an editor say about a use of f while
// it still loads: window.go's sentence -- the spelling, the release it stops
// loading at, and the replacement -- plus the release it was deprecated in, the
// rewrite that does the work, and the rule in brackets.
//
// It takes no release, and that is not an oversight: the TEXT is a fact about
// the form alone (the window's end is DeprecatedIn + MinimumMinorReleases), and
// only the DECISION depends on who is asking. One sentence per form means the
// load report, the lint line and the squiggle are the same words, which is what
// lets the language server match an action to the diagnostic that asked for it.
func (f Form) Warning() string {
	return fmt.Sprintf("%s (deprecated in %s; `%s` rewrites it) [%s]",
		New(f.Window("")).Message("`"+f.Spelling+"`", "`"+f.Replacement+"`"),
		f.DeprecatedIn, f.Migrator, f.Rule)
}

// Refusal is what a parse and a load say about a use of f once the window is
// spent, in the same shape and naming the same three things: the window it had,
// what to write, and the rewrite that writes it.
func (f Form) Refusal() string {
	return fmt.Sprintf("`%s` was deprecated in %s and stopped loading in %s; write `%s` instead (`%s` rewrites it) [%s]",
		f.Spelling, f.DeprecatedIn, f.RefusedFrom(), f.Replacement, f.Migrator, f.Rule)
}

// Trackers returns one Tracker per registered form, keyed by rule, each over
// its OWN window at release current.
//
// One per form rather than one shared: a Tracker is a tracker of one window
// (window.go), and two forms deprecated in different releases expire in
// different releases. A single tracker would have to answer Refuses from
// somebody else's window.
func Trackers(current string) map[string]*Tracker {
	out := make(map[string]*Tracker, len(byRule))
	for rule, f := range byRule {
		out[rule] = New(f.Window(current))
	}
	return out
}

var (
	currentMu sync.RWMutex
	current   string
)

// Current is the release this process makes deprecation decisions at, or "" in
// a build that is not a release. See the file comment.
func Current() string {
	currentMu.RLock()
	defer currentMu.RUnlock()
	return current
}

// SetCurrent records the release deprecation decisions are made at and returns
// the function that puts it back.
//
// It is called ONCE in production, by the component that knows which release
// the binary is (component/memql, from core/buildinfo's link-time stamp), and
// otherwise only by a test moving the clock forward to see a window close. The
// release is a value rather than a constant for the same reason Window's three
// fields are: a window that cannot be read at both of its ends cannot be tested
// at either.
func SetCurrent(release string) (restore func()) {
	currentMu.Lock()
	defer currentMu.Unlock()
	prev := current
	current = release
	return func() {
		currentMu.Lock()
		defer currentMu.Unlock()
		current = prev
	}
}
