package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// discardLogger is the silent logger every authoring test runs under.
func authoringTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

// fakeNearMatcher is the fuzzy-tier seam stand-in: it returns a canned,
// similarity-ordered candidate list and records the MatchTexts it was asked
// about. nil matches + nil err is the "no near match" default.
type fakeNearMatcher struct {
	matches []memql.CatalogNearMatch
	err     error
	queries []string
}

func (f *fakeNearMatcher) CatalogNearMatches(_ context.Context, matchText string, _ int) ([]memql.CatalogNearMatch, error) {
	f.queries = append(f.queries, matchText)
	return f.matches, f.err
}

// designJSON builds the authoringDesign structured-output envelope a fake
// engine returns: an automation outline + a dependency list. Each dependency
// carries a real, parseable candidate source so DecideReuse can key it.
func designJSON(t *testing.T, deps []designDependency) string {
	t.Helper()
	b, err := json.Marshal(designResult{
		AutomationName:    "dailyDigest",
		AutomationPurpose: "Send a daily digest of new items.",
		Dependencies:      deps,
	})
	if err != nil {
		t.Fatalf("marshal design json: %v", err)
	}
	return string(b)
}

// A real, parseable spec source the catalog key + matcher can ingest.
const specCandidateSource = `@enabled
@description("Matches active digest items")
spec specDigestItemActive {
  payload.active == true
}`

// A real, parseable query source.
const queryCandidateSource = `use cognition.concepts.{ space }
use cognition.shapes.{ spaceCard }

@description("List the owner's active spaces")
query space queryOwnerActiveSpaces {
  filter  payload.ownerUserId == actor.userId
  shape   spaceCard
}`

// designEngine returns a fakeEngine whose authoringDesign InvokeAI yields the
// supplied JSON and whose queryCataloguedConstructsForOwner Execute yields the
// supplied catalog rows (shape-projected: a flat `output` envelope).
func designEngine(designOut string, catalogRows []map[string]any) *fakeEngine {
	return &fakeEngine{
		aiResponder: func(templateId string, _ map[string]any) (any, error) {
			if templateId == "authoringDesign" {
				return designOut, nil
			}
			return nil, nil
		},
		execResponder: func(query string) (any, error) {
			if strings.Contains(query, "queryCataloguedConstructsForOwner") {
				out := make([]any, 0, len(catalogRows))
				for _, r := range catalogRows {
					out = append(out, r)
				}
				return map[string]any{"output": out}, nil
			}
			return nil, nil
		},
	}
}

func newDesignLoop(fe *fakeEngine) *PlannerAgentLoop {
	return &PlannerAgentLoop{engine: fe, logger: authoringTestLogger()}
}

// TestRunDesignPass_AuthorsWhenCatalogEmpty: with no catalog, every emitted
// dependency is a genuine gap -> AUTHOR. This is the cold-start acceptance:
// net-new deps authored only when nothing exists (everything, here).
func TestRunDesignPass_AuthorsWhenCatalogEmpty(t *testing.T) {
	deps := []designDependency{
		{Kind: "spec", Name: "specDigestItemActive", Purpose: "active items", CandidateSource: specCandidateSource},
		{Kind: "query", Name: "queryOwnerActiveSpaces", Purpose: "active spaces", CandidateSource: queryCandidateSource},
	}
	fe := designEngine(designJSON(t, deps), nil)
	l := newDesignLoop(fe)

	plan, err := l.runDesignPass(context.Background(), "Send me a daily digest.", "user-1", nil)
	if err != nil {
		t.Fatalf("runDesignPass: %v", err)
	}
	if len(plan.Dependencies) != 2 {
		t.Fatalf("want 2 dependencies, got %d", len(plan.Dependencies))
	}
	if plan.authorCount() != 2 {
		t.Fatalf("empty catalog must author both deps, authorCount=%d", plan.authorCount())
	}
	for _, d := range plan.Dependencies {
		if d.Disposition != dispAuthor {
			t.Fatalf("dep %q: want author, got %s", d.Name, d.Disposition)
		}
	}
}

// TestRunDesignPass_ReusesExactCatalogMatch: when the catalog holds a
// construct with the SAME CatalogKey as a dependency's candidate source, the
// dependency resolves to reuseExact and adopts the cataloged construct's real
// name -- no hallucination, no author.
func TestRunDesignPass_ReusesExactCatalogMatch(t *testing.T) {
	// Key the spec candidate exactly as the catalog stores it.
	key, err := memql.CatalogKey("spec", specCandidateSource)
	if err != nil {
		t.Fatalf("CatalogKey: %v", err)
	}
	catalog := []map[string]any{
		{"name": "specItemIsActive", "kind": "spec", "catalogKey": key},
	}
	deps := []designDependency{
		// Model proposed a DIFFERENT name than the cataloged one; exact match
		// is name-independent, so it must still reuse + rename.
		{Kind: "spec", Name: "specDigestItemActive", Purpose: "active items", CandidateSource: specCandidateSource},
	}
	fe := designEngine(designJSON(t, deps), catalog)
	l := newDesignLoop(fe)

	plan, err := l.runDesignPass(context.Background(), "digest", "user-1", nil)
	if err != nil {
		t.Fatalf("runDesignPass: %v", err)
	}
	d := plan.Dependencies[0]
	if d.Disposition != dispReuseExact {
		t.Fatalf("want reuseExact, got %s", d.Disposition)
	}
	if d.ReuseName != "specItemIsActive" || d.Name != "specItemIsActive" {
		t.Fatalf("exact reuse must adopt the cataloged name, got Name=%q ReuseName=%q", d.Name, d.ReuseName)
	}
	if plan.authorCount() != 0 {
		t.Fatalf("an exact catalog hit must not be authored, authorCount=%d", plan.authorCount())
	}
}

// TestRunDesignPass_ReusesNearMatchAboveThreshold: no exact hit, but the fuzzy
// tier returns a same-kind candidate above the similarity threshold -> reuse.
func TestRunDesignPass_ReusesNearMatchAboveThreshold(t *testing.T) {
	deps := []designDependency{
		{Kind: "spec", Name: "specDigestItemActive", Purpose: "active", CandidateSource: specCandidateSource},
	}
	fe := designEngine(designJSON(t, deps), nil) // empty catalog -> no exact match
	near := &fakeNearMatcher{matches: []memql.CatalogNearMatch{
		{CatalogEntry: memql.CatalogEntry{Name: "specRowIsActive", Kind: "spec"}, Namespace: "v1:digest", Similarity: 0.91},
	}}
	l := newDesignLoop(fe)

	plan, err := l.runDesignPass(context.Background(), "digest", "user-1", near)
	if err != nil {
		t.Fatalf("runDesignPass: %v", err)
	}
	d := plan.Dependencies[0]
	if d.Disposition != dispReuseNear {
		t.Fatalf("want reuseNear, got %s", d.Disposition)
	}
	if d.ReuseName != "specRowIsActive" || d.Name != "specRowIsActive" {
		t.Fatalf("near reuse must adopt the cataloged name, got Name=%q", d.Name)
	}
	if d.Similarity != 0.91 {
		t.Fatalf("near reuse must carry the cosine score, got %v", d.Similarity)
	}
	if len(near.queries) != 1 {
		t.Fatalf("near matcher should be consulted exactly once, got %d", len(near.queries))
	}
}

// TestRunDesignPass_AuthorsWhenNearMatchBelowThreshold: a fuzzy candidate that
// is in the neighborhood but below the threshold is NOT a safe reuse -> author.
func TestRunDesignPass_AuthorsWhenNearMatchBelowThreshold(t *testing.T) {
	deps := []designDependency{
		{Kind: "spec", Name: "specDigestItemActive", Purpose: "active", CandidateSource: specCandidateSource},
	}
	fe := designEngine(designJSON(t, deps), nil)
	near := &fakeNearMatcher{matches: []memql.CatalogNearMatch{
		{CatalogEntry: memql.CatalogEntry{Name: "specSomethingElse", Kind: "spec"}, Similarity: 0.50},
	}}
	l := newDesignLoop(fe)

	plan, err := l.runDesignPass(context.Background(), "digest", "user-1", near)
	if err != nil {
		t.Fatalf("runDesignPass: %v", err)
	}
	if plan.Dependencies[0].Disposition != dispAuthor {
		t.Fatalf("below-threshold near match must author, got %s", plan.Dependencies[0].Disposition)
	}
}

// TestRunDesignPass_CrossKindNearMatchNotReused: a near match of a DIFFERENT
// kind is never a safe reuse even above the threshold -> author.
func TestRunDesignPass_CrossKindNearMatchNotReused(t *testing.T) {
	deps := []designDependency{
		{Kind: "spec", Name: "specDigestItemActive", Purpose: "active", CandidateSource: specCandidateSource},
	}
	fe := designEngine(designJSON(t, deps), nil)
	near := &fakeNearMatcher{matches: []memql.CatalogNearMatch{
		{CatalogEntry: memql.CatalogEntry{Name: "queryActiveThings", Kind: "query"}, Similarity: 0.99},
	}}
	l := newDesignLoop(fe)

	plan, err := l.runDesignPass(context.Background(), "digest", "user-1", near)
	if err != nil {
		t.Fatalf("runDesignPass: %v", err)
	}
	if plan.Dependencies[0].Disposition != dispAuthor {
		t.Fatalf("cross-kind near match must not reuse, got %s", plan.Dependencies[0].Disposition)
	}
}

// TestRunDesignPass_UnparseableCandidateAuthored: a candidate source that does
// not parse for its kind can't be keyed, so it is authored (the emit+repair
// pass produces a real construct) rather than failing the whole pass.
func TestRunDesignPass_UnparseableCandidateAuthored(t *testing.T) {
	deps := []designDependency{
		{Kind: "spec", Name: "specBroken", Purpose: "broken", CandidateSource: "this is not valid memql"},
	}
	fe := designEngine(designJSON(t, deps), nil)
	l := newDesignLoop(fe)

	plan, err := l.runDesignPass(context.Background(), "digest", "user-1", nil)
	if err != nil {
		t.Fatalf("an unparseable candidate must not fail the pass: %v", err)
	}
	if plan.Dependencies[0].Disposition != dispAuthor {
		t.Fatalf("unparseable candidate must be authored, got %s", plan.Dependencies[0].Disposition)
	}
}

// TestRunDesignPass_EmptyStatementRejected: the design pass refuses an empty
// Responsibility before making any model call.
func TestRunDesignPass_EmptyStatementRejected(t *testing.T) {
	fe := designEngine("", nil)
	l := newDesignLoop(fe)
	if _, err := l.runDesignPass(context.Background(), "   ", "user-1", nil); err == nil {
		t.Fatalf("empty statement must be rejected")
	}
	_, si, _ := fe.snapshot()
	if countContains(si, "authoringDesign") != 0 {
		t.Fatalf("empty statement must make ZERO model calls, got %d", countContains(si, "authoringDesign"))
	}
}

// TestRunDesignPass_NoDependenciesIsError: a design with an empty dependency
// list is a malformed result -- the pass errors rather than emit an empty
// bundle.
func TestRunDesignPass_NoDependenciesIsError(t *testing.T) {
	fe := designEngine(designJSON(t, nil), nil)
	l := newDesignLoop(fe)
	if _, err := l.runDesignPass(context.Background(), "digest", "user-1", nil); err == nil {
		t.Fatalf("a dependency-less design must error")
	}
}

// TestRunDesignPass_CatalogLoadFailureDegradesToAuthor: when the catalog read
// fails, the pass proceeds with an empty catalog (compose-nothing) instead of
// failing -- every dependency falls to AUTHOR.
func TestRunDesignPass_CatalogLoadFailureDegradesToAuthor(t *testing.T) {
	deps := []designDependency{
		{Kind: "spec", Name: "specDigestItemActive", Purpose: "active", CandidateSource: specCandidateSource},
	}
	fe := &fakeEngine{
		aiResponder: func(templateId string, _ map[string]any) (any, error) {
			if templateId == "authoringDesign" {
				return designJSON(t, deps), nil
			}
			return nil, nil
		},
		execResponder: func(query string) (any, error) {
			if strings.Contains(query, "queryCataloguedConstructsForOwner") {
				return nil, fmt.Errorf("db down")
			}
			return nil, nil
		},
	}
	l := newDesignLoop(fe)

	plan, err := l.runDesignPass(context.Background(), "digest", "user-1", nil)
	if err != nil {
		t.Fatalf("catalog load failure must degrade, not fail: %v", err)
	}
	if plan.Dependencies[0].Disposition != dispAuthor {
		t.Fatalf("compose-nothing degrade must author, got %s", plan.Dependencies[0].Disposition)
	}
}
