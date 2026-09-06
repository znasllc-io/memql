// Package scorecard is the committed artifact and the page generated from it.
//
// It is PURE -- standard library and the two sibling pure packages -- because
// the page is a function of the JSON and nothing else. That is what makes
// `--check` meaningful: a stale page is a diff, not a judgement call.
//
// The JSON is the SOURCE and the markdown is DERIVED. Every consumer -- the
// page, the OS surface, the CI comment, the claims gate -- reads the JSON, so
// there is exactly one place a number lives and exactly one place it is
// formatted (figure.Figure.Render).
package scorecard

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/proving/figure"
)

// Scorecard is one dated publication.
type Scorecard struct {
	// Version is the artifact's schema version. Bumped when the shape
	// changes, so a reader that cannot understand a file says so instead of
	// reading half of it.
	Version int `json:"version"`
	// Date is YYYY-MM-DD and MUST equal the filename's stem. The gate checks
	// it, because a dated artifact whose name and content disagree is one
	// nobody can order.
	Date string `json:"date"`
	// Commit is the short SHA the suite ran at.
	Commit string `json:"commit"`
	// CorpusFingerprint identifies what was measured. Two scorecards with
	// different fingerprints measured different things, and joining them into
	// a trend is the mistake this field exists to make visible.
	CorpusFingerprint string `json:"corpusFingerprint"`
	// Tiers records what each tier last did, including "it has not run".
	Tiers map[figure.Tier]TierState `json:"tiers"`
	// Entries are the published figures, sorted by metric then scenario then
	// arm so a diff between two scorecards reads as changed numbers rather
	// than as a reordering.
	Entries []Entry `json:"entries"`
	// Governance are the pass-or-fail properties. Separate from Entries
	// because there is no governance score and folding them in would invite
	// one.
	Governance []Property `json:"governance"`
}

// TierState says what a tier last did. `LastRun` empty means it has never run,
// which is a fact the page prints rather than an absence it hides.
type TierState struct {
	LastRun string `json:"lastRun,omitempty"`
	// Armed reports whether the lane runs on its own. The live tier ships
	// disarmed (design P3), and a scorecard whose live figures are all
	// `tierNotRun` is only honest if it also says why.
	Armed bool `json:"armed"`
	// Note is the human sentence explaining the state.
	Note string `json:"note,omitempty"`
}

// Entry is one published figure, with the scenario and arm it came from.
type Entry struct {
	Scenario string        `json:"scenario"`
	Family   figure.Family `json:"family"`
	Arm      figure.Arm    `json:"arm"`
	Figure   figure.Figure `json:"figure"`
	// Control marks a figure produced by a NEGATIVE CONTROL scenario -- one
	// that exists to prove a counter can rise, and therefore deliberately
	// produces the opposite of the headline.
	//
	// It is carried so the claims gate can EXCLUDE it. Without this, every
	// zero-claim is unclaimable: the control for
	// `compileCallsOnCatalogHit` measures the catalog-MISS path on the same
	// arm, so "every scenario measuring this metric reports zero" is false by
	// construction and the gate would refuse a claim that is true. The
	// control still appears on the page and still blocks the suite when it
	// reads zero -- it is only claims it must not govern.
	Control bool `json:"control,omitempty"`
}

// Property is one governance property: pass or fail, and what it was asked of.
type Property struct {
	Name     string `json:"name"`
	Scenario string `json:"scenario"`
	Passed   bool   `json:"passed"`
	// Detail is required when Passed is false -- a failed property that does
	// not say what failed is a red light with no next step.
	Detail string `json:"detail,omitempty"`
}

// CurrentVersion is the schema version this package writes.
const CurrentVersion = 1

// Validate refuses a scorecard that cannot be published. It is called by the
// writer and by the gate, so a hand-edited file is caught on the next read.
func (s Scorecard) Validate() error {
	var problems []string
	bad := func(f string, a ...any) { problems = append(problems, fmt.Sprintf(f, a...)) }

	if s.Version != CurrentVersion {
		bad("version is %d, want %d", s.Version, CurrentVersion)
	}
	if !isDate(s.Date) {
		bad("date %q is not YYYY-MM-DD", s.Date)
	}
	if strings.TrimSpace(s.Commit) == "" {
		bad("no commit")
	}
	if strings.TrimSpace(s.CorpusFingerprint) == "" {
		bad("no corpus fingerprint; two scorecards could be joined into a trend without anyone noticing they measured different corpora")
	}
	for _, t := range []figure.Tier{figure.TierCI, figure.TierLive} {
		st, ok := s.Tiers[t]
		if !ok {
			bad("tier %q has no state; a tier that has not run must SAY so rather than be absent", t)
			continue
		}
		if st.LastRun == "" && strings.TrimSpace(st.Note) == "" {
			bad("tier %q has never run and gives no reason; the page would show empty figures with no explanation", t)
		}
	}
	if len(s.Entries) == 0 {
		bad("no entries")
	}
	for i, e := range s.Entries {
		if strings.TrimSpace(e.Scenario) == "" {
			bad("entry %d has no scenario", i)
		}
		if !e.Arm.Valid() {
			bad("entry %d has arm %q, which is not one of platform/baseline", i, e.Arm)
		}
		if !e.Family.Valid() {
			bad("entry %d has family %q", i, e.Family)
		}
		if err := e.Figure.Validate(); err != nil {
			bad("entry %d (%s/%s): %v", i, e.Scenario, e.Figure.Metric, err)
		}
	}
	for i, p := range s.Governance {
		if strings.TrimSpace(p.Name) == "" {
			bad("governance %d has no name", i)
		}
		if !p.Passed && strings.TrimSpace(p.Detail) == "" {
			bad("governance property %q failed and gives no detail; a red light with no next step is one people learn to ignore", p.Name)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("scorecard %s:\n  - %s", s.Date, strings.Join(problems, "\n  - "))
	}
	return nil
}

func isDate(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i, r := range s {
		if i == 4 || i == 7 {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Sort orders the entries canonically so two scorecards diff as changed
// numbers rather than as a reordering.
func (s *Scorecard) Sort() {
	sort.SliceStable(s.Entries, func(i, j int) bool {
		a, b := s.Entries[i], s.Entries[j]
		if a.Figure.Metric != b.Figure.Metric {
			return a.Figure.Metric < b.Figure.Metric
		}
		if a.Scenario != b.Scenario {
			return a.Scenario < b.Scenario
		}
		return a.Arm < b.Arm
	})
	sort.SliceStable(s.Governance, func(i, j int) bool {
		if s.Governance[i].Name != s.Governance[j].Name {
			return s.Governance[i].Name < s.Governance[j].Name
		}
		return s.Governance[i].Scenario < s.Governance[j].Scenario
	})
}

// Find returns the entry for one metric on one arm, across every scenario,
// EXCLUDING negative controls.
//
// It returns EVERY remaining match rather than the first, because a metric
// measured by two scenarios has two numbers and picking one silently is how a
// claim ends up resting on whichever scenario sorted first.
func (s Scorecard) Find(m figure.Metric, arm figure.Arm) []Entry {
	var out []Entry
	for _, e := range s.Entries {
		if e.Control {
			continue
		}
		if e.Figure.Metric == m && e.Arm == arm {
			out = append(out, e)
		}
	}
	return out
}

// Marshal writes the scorecard as indented JSON with a trailing newline, which
// is what a committed artifact needs to diff sanely.
func (s Scorecard) Marshal() ([]byte, error) {
	s.Sort()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("proving/scorecard: %w", err)
	}
	return append(b, '\n'), nil
}

// Unmarshal reads one back and validates it.
func Unmarshal(b []byte) (Scorecard, error) {
	var s Scorecard
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Scorecard{}, fmt.Errorf("proving/scorecard: %w", err)
	}
	if err := s.Validate(); err != nil {
		return Scorecard{}, err
	}
	return s, nil
}

// Newest returns the most recent scorecard under dir, by filename. Dates sort
// lexically, which is why the filename is the date and why Validate insists
// the two agree: an ordering that depended on mtime would reorder on a fresh
// checkout.
//
// ok is false when the directory holds no scorecards. That is a legitimate
// state -- before the first run -- and callers must render it as "no scorecard
// yet" rather than as an error or, worse, as zeroes.
func Newest(fsys fs.FS, dir string) (s Scorecard, name string, ok bool, err error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return Scorecard{}, "", false, fmt.Errorf("proving/scorecard: reading %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if !isDate(strings.TrimSuffix(e.Name(), ".json")) {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return Scorecard{}, "", false, nil
	}
	sort.Strings(names)
	newest := names[len(names)-1]
	raw, err := fs.ReadFile(fsys, path.Join(dir, newest))
	if err != nil {
		return Scorecard{}, "", false, fmt.Errorf("proving/scorecard: %w", err)
	}
	s, err = Unmarshal(raw)
	if err != nil {
		return Scorecard{}, newest, false, err
	}
	if s.Date != strings.TrimSuffix(newest, ".json") {
		return Scorecard{}, newest, false, fmt.Errorf("proving/scorecard: %s carries date %q; the name and the content must agree or the ordering is a guess", newest, s.Date)
	}
	return s, newest, true, nil
}
