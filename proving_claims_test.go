package main

import (
	"os"
	"os/exec"
	"path"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/proving/scorecard"
)

// A CLAIM MAY NOT OUTLIVE ITS NUMBER (epic memql#4993, design P4).
//
// # The defect this exists to catch
//
// docs/public/overview/why-memql-harness.md opens with "This page is the
// proof, not the pitch" and carries a table titled "Claims this page does NOT
// make yet". That discipline was maintained by hand, and by hand is exactly
// how it stops being true: a number written into README or site copy stays
// there when the measurement behind it moves, and nothing objects. The
// published sentence and the measured figure drift apart silently, and the
// direction they drift is always the flattering one.
//
// # Why the gate is over MARKED claims
//
// The alternative was scanning every number in README and docs/public. It was
// rejected: version numbers, ports, replica counts, timeouts and table values
// would all be flagged, the exemption list would become the real policy within
// a release, and a gate people exempt their way past protects nothing.
//
// So a published claim OPTS IN by naming the metric it rests on:
//
//	Replaying a run re-executes no completed step.
//	<!-- proving: metric=durability.resumedStepsReExecuted arm=platform value=0 -->
//
// Unmarked prose is untouched. The cost is that an unmarked claim is
// ungoverned, and that cost is deliberate: the marker is a promise the author
// makes, and a gate with no false positives is one people keep using.
//
// # Both directions
//
// The forward half fails when a marked claim names a metric the scorecard does
// not carry, reports as unmeasured, or measures differently. The MIRROR half
// (TestNoPendingClaimHasQuietlyBecomeTrue) fails when something listed as
// not-yet-claimed HAS become measurable -- a claim that is proven and still
// filed under "we do not claim this" is a different kind of stale, and the
// whole value of that table is that it is true.

const scorecardDir = "docs/public/overview/proving/scorecard"

// claimSources are the trees whose prose is governed. README is named
// explicitly because it is not under docs/.
var claimSources = []string{"README.md", "docs/public"}

func gitTrackedClaimFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, root := range claimSources {
		out, err := exec.Command("git", "ls-files", "-z", root).Output()
		if err != nil {
			t.Fatalf("git ls-files -z %s: %v", root, err)
		}
		for _, rel := range strings.Split(string(out), "\x00") {
			if rel == "" || path.Ext(rel) != ".md" {
				continue
			}
			files = append(files, rel)
		}
	}
	if len(files) == 0 {
		t.Fatal("git ls-files returned no markdown under README.md or docs/public -- this gate would verify nothing")
	}
	return files
}

func newestScorecard(t *testing.T) (scorecard.Scorecard, bool) {
	t.Helper()
	s, _, ok, err := scorecard.Newest(os.DirFS("."), scorecardDir)
	if err != nil {
		t.Fatalf("reading %s: %v", scorecardDir, err)
	}
	return s, ok
}

func TestPublishedClaimsRestOnAScorecardNumber(t *testing.T) {
	s, have := newestScorecard(t)

	var (
		all      []scorecard.Claim
		parseErr []error
	)
	for _, rel := range gitTrackedClaimFiles(t) {
		data, err := os.ReadFile(rel)
		if err != nil {
			// git-tracked but locally absent (a partial checkout) is not drift.
			continue
		}
		claims, errs := scorecard.ParseClaims(rel, string(data))
		all = append(all, claims...)
		parseErr = append(parseErr, errs...)
	}

	// A MALFORMED MARKER IS A FAILURE, not a skip. A marker nobody parses is
	// a claim nobody checks, and it looks exactly like a claim that passed.
	for _, err := range parseErr {
		t.Errorf("malformed proving marker: %v", err)
	}

	for _, f := range scorecard.CheckClaims(all, s, have) {
		t.Errorf("a published claim no longer rests on its number:\n%v", f)
	}

	if len(all) == 0 {
		t.Error("no published claim carries a proving marker.\n" +
			"The gate is over MARKED claims, so with none it verifies nothing. Either the markers were " +
			"removed, or the pages stopped making numeric claims -- and if it is the second, delete this " +
			"test rather than leaving a gate that guards an empty set.")
	}
}

func TestNoPendingClaimHasQuietlyBecomeTrue(t *testing.T) {
	// The mirror half. why-memql-harness.md's "Claims this page does NOT make
	// yet" table marks each pending claim with the metric that will settle it;
	// this fails when one of them starts being measured, so the table is
	// updated by the gate's insistence rather than by somebody remembering.
	s, have := newestScorecard(t)
	if !have {
		t.Skip("no scorecard is committed yet, so nothing can have become true")
	}
	const page = "docs/public/overview/why-memql-harness.md"
	data, err := os.ReadFile(page)
	if err != nil {
		t.Fatalf("reading %s: %v", page, err)
	}

	// The pending table's markers are spelled `proving-pending:` rather than
	// `proving:`, because the two say opposite things and one spelling for
	// both made the forward gate fail on every row of this table. The section
	// is still read on its own so a stray pending marker outside it is not
	// silently honoured.
	var pendingBlock strings.Builder
	inTable := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "## Claims this page does NOT make yet") {
			inTable = true
			continue
		}
		if inTable && strings.HasPrefix(line, "## ") {
			break
		}
		if inTable {
			pendingBlock.WriteString(line)
			pendingBlock.WriteString("\n")
		}
	}
	if pendingBlock.Len() == 0 {
		t.Fatalf("%s no longer carries a 'Claims this page does NOT make yet' section.\n"+
			"That section is the page's own honesty device -- it is what makes 'this page is the proof, "+
			"not the pitch' checkable. If every claim has been promoted, say so IN the section rather "+
			"than deleting it.", page)
	}

	pending, errs := scorecard.ParsePendingClaims(page, pendingBlock.String())
	for _, err := range errs {
		t.Errorf("malformed marker in the pending-claims table: %v", err)
	}
	for _, f := range scorecard.StillPending(pending, s, true) {
		t.Errorf("a claim listed as not-yet-made has become measurable:\n%v", f)
	}
}

func TestTheScorecardAndItsPageAgree(t *testing.T) {
	// The JSON is the SOURCE and the page is DERIVED, so a stale page is a
	// diff rather than a judgement call. `memql-bench --do=scorecard --check`
	// is the same assertion in CI; this one runs in `make test`, where a docs
	// change is far more likely to be made and far less likely to run the
	// proving lane.
	s, have := newestScorecard(t)
	if !have {
		t.Skip("no scorecard is committed yet")
	}
	const page = "docs/public/overview/proving-scorecard.md"
	got, err := os.ReadFile(page)
	if err != nil {
		t.Fatalf("%s is missing; regenerate it with `go run ./cmd/memql-bench --do=scorecard`", page)
	}
	if want := scorecard.RenderPage(s); string(got) != want {
		t.Errorf("%s is stale against the newest committed scorecard (%s).\n"+
			"Regenerate it with `go run ./cmd/memql-bench --do=scorecard`; the JSON is the source and "+
			"the page is derived, so hand-editing the page is always the wrong repair.", page, s.Date)
	}
}
