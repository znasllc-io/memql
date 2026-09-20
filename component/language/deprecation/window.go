// Package deprecation is the window a public language form goes through on its
// way out (memql#5390; D22 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// # What the window is for
//
// Before edition 2026 was frozen, retiring a form was one commit: the parser
// stopped accepting it, and the next person to load a bundle written against it
// got a refusal. That is the right trade while a language is being designed and
// the wrong one once bundles exist in repositories this one cannot see. The
// freeze is the moment the trade flips, which is why this package arrives with
// it rather than before it.
//
// So a form leaves in three stages, and D22 names all three:
//
//	deprecated  the form still loads; every use WARNS, naming the replacement
//	            and the release it stops loading at, and is COUNTED
//	expired     the window is spent; the form refuses
//	reserved    the name may never return with another meaning
//	            (component/language/reserved.json, enforced by cmd/memqlbreaking)
//
// # Why the count is not decoration
//
// The warning is a message to one author at one load. The count is the evidence
// the person deciding whether the form can actually go needs: "nobody has
// written this in six months" and "this fires four thousand times a day" are
// different answers, and without a counter both look identical from here. A
// deprecation with no count is a removal scheduled on a guess.
//
// # Why this is a leaf
//
// Standard library only, and no import of the parser or the registry. The
// loader consults it, so anything it imports it imports at load time, and a
// cycle here is a cluster that does not build. It also means the window is
// testable as a function over values: the two claims that matter -- a form
// inside the window loads, a form past it does not -- are decided by
// arithmetic over two version strings and nothing else.
//
// # Who consults it
//
// This file is the ARITHMETIC; forms.go is the table of forms it is applied to,
// and the four callers that make the window a mechanism rather than a library:
//
//	component/language/parser   ScanDeprecatedUses finds every use, and
//	                            parseTypeRef refuses one past its window
//	component/memql             the load warns, counts and (past the window)
//	                            refuses -- deprecated_uses.go
//	component/memql/sense       the editor's squiggle, carrying the rule
//	cmd/memql-lsp               the Deprecated tag and the quick fix
//
// component/memql is also the one caller of SetCurrent: it knows which release
// the binary was cut from, and every other consumer reads that one answer.
//
// # Fail-open, deliberately
//
// Every uncertainty resolves toward "keep loading". An unreadable version, an
// empty DeprecatedAt, a nil Tracker: none refuses. The asymmetry is the point.
// What this mechanism protects is a courtesy window; what a fail-closed bug
// here costs is a cluster that will not boot over a malformed version string.
package deprecation

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// MinimumMinorReleases is the floor D22 sets: "at least two minor releases".
// It is a floor rather than a default -- a caller asking for a shorter window
// gets this one, because a window shorter than the record's minimum is the
// thing the record exists to forbid, and a silently-honoured zero would let a
// form refuse in the release that deprecated it.
//
// The value belongs beside the depth cap and the budgets in
// component/language/tiers/limits.go, which names "the deprecation window" in
// its own list of manifest values; tiers imports nothing from here and this
// package imports nothing from tiers, so the constant is declared where it is
// READ and pinned to that list by TestDeprecationWindowMatchesTheManifest.
const MinimumMinorReleases = 2

// Window is the configuration of one tracker: how long the window is, when the
// form was deprecated, and where the engine is now. All three are VALUES, per
// the record's cross-cutting rule ("values, not constants"), which is also what
// makes the window testable at both of its ends.
type Window struct {
	// MinorReleases is the length of the window. Values below
	// MinimumMinorReleases are raised to it.
	MinorReleases int
	// DeprecatedAt is the release the form was deprecated in, as "MAJOR.MINOR"
	// or "MAJOR.MINOR.PATCH". Empty means nothing has been deprecated, and
	// nothing can then have expired.
	DeprecatedAt string
	// Current is the engine's own release, in the same form.
	Current string
}

// Tracker warns, counts and decides. The zero value and a nil pointer are both
// usable: the loader holds one whether or not anything configured it, and a
// panic on that path is a cluster that does not boot.
type Tracker struct {
	window Window

	mu     sync.Mutex
	counts map[string]int
}

// New returns a tracker over w, with the window floored at
// MinimumMinorReleases.
func New(w Window) *Tracker {
	if w.MinorReleases < MinimumMinorReleases {
		w.MinorReleases = MinimumMinorReleases
	}
	return &Tracker{window: w, counts: map[string]int{}}
}

// Record counts one use of form. Safe on a nil tracker and safe to call from
// several goroutines: a load walks the tree concurrently, and a counter that
// races is a counter whose number is not evidence of anything.
func (t *Tracker) Record(form string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.counts == nil {
		t.counts = map[string]int{}
	}
	t.counts[form]++
}

// Counts returns a COPY of the per-form use counts. A copy rather than the live
// map: a caller that ranges over the tracker's own map while a load is still
// recording is a data race, and it surfaces on somebody else's branch.
func (t *Tracker) Counts() map[string]int {
	if t == nil {
		return map[string]int{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]int, len(t.counts))
	for k, v := range t.counts {
		out[k] = v
	}
	return out
}

// Message is the load-time warning: what was written, what to write instead,
// and the release it stops loading at. All three, because each answers a
// question the author would otherwise have to go and look up, and the one most
// often left out -- the release -- is the one that turns "someday" into a date.
func (t *Tracker) Message(form, replacement string) string {
	w := Window{MinorReleases: MinimumMinorReleases}
	if t != nil {
		w = t.window
	}
	at := w.expiresAt()
	if at == "" {
		return fmt.Sprintf("%s is deprecated; write %s instead", form, replacement)
	}
	return fmt.Sprintf("%s is deprecated and stops loading in %s; write %s instead", form, at, replacement)
}

// Refuses reports whether the window has been spent, so the form no longer
// loads. Every uncertainty answers false; see the package comment.
func (t *Tracker) Refuses(form string) bool {
	if t == nil {
		return false
	}
	dep, ok := parseRelease(t.window.DeprecatedAt)
	if !ok {
		return false
	}
	cur, ok := parseRelease(t.window.Current)
	if !ok {
		return false
	}
	end := dep
	end.minor += t.window.MinorReleases
	return !cur.before(end)
}

// expiresAt renders the first release the form refuses at, or "" when there is
// no readable deprecation point to count from.
func (w Window) expiresAt() string {
	dep, ok := parseRelease(w.DeprecatedAt)
	if !ok {
		return ""
	}
	n := w.MinorReleases
	if n < MinimumMinorReleases {
		n = MinimumMinorReleases
	}
	return fmt.Sprintf("%d.%d", dep.major, dep.minor+n)
}

// release is a MAJOR.MINOR point. The patch is parsed and DISCARDED: the window
// is counted in minor releases, so 0.23.0 and 0.23.9 are the same point in it,
// and keeping the patch would make a window expire mid-minor for some clusters
// and not others.
type release struct{ major, minor int }

func parseRelease(s string) (release, bool) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "v"))
	if s == "" {
		return release{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return release{}, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return release{}, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return release{}, false
	}
	if major < 0 || minor < 0 {
		return release{}, false
	}
	return release{major: major, minor: minor}, true
}

func (r release) before(o release) bool {
	if r.major != o.major {
		return r.major < o.major
	}
	return r.minor < o.minor
}
