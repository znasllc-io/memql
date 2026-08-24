package library

// similarity_test.go -- librarySimilarArtifacts (memql#4342, design 3.2).
//
// The two claims worth testing are the two things this capability adds on
// top of the raw vector operator: the right artifact comes back FIRST,
// and another user's file never comes back AT ALL. The second is not a
// theoretical guard -- integrations/similarity applies no per-row
// authorization whatsoever, so every hit in this suite's candidate pool
// is genuinely reachable and the scoping is the only thing between one
// person's Library and another's. The fixtures therefore make the
// foreign file the BEST match, so a missing filter cannot pass by luck.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// swapExtractor lets one integration analyse several files with
// different bodies.
type swapExtractor struct{ text string }

func (s *swapExtractor) Extract(context.Context, string, []byte) (string, error) {
	return s.text, nil
}

// heronText shares almost nothing with the kingfisher query below, so it
// scores lower than birdsText and gives the ordering assertion something
// to be wrong about.
func heronText() string {
	var b strings.Builder
	for range 8 {
		b.WriteString("The grey heron is a patient wader. It stands in the shallows without moving " +
			"and waits for something to swim close enough. Nothing about the technique is quick.\n\n")
	}
	return b.String()
}

// twoAnalysedFiles runs the real pass over two files owned by user-a and
// returns their artifact ids.
func twoAnalysedFiles(t *testing.T, s *libStub) (kingfisherArtifact, heronArtifact string) {
	t.Helper()
	ex := &swapExtractor{}
	i := NewIntegration(s)
	i.SetExtractor(ex)
	i.SetArtifactPoll(1, 0)

	s.seedFile("file-birds", "user-a", "birds.txt", "text/plain", "text")
	kingfisherArtifact = s.seedPromotedArtifact("file-birds", "user-a", nil)
	ex.text = birdsText()
	analyzedFile(t, s, i, "file-birds", "user-a")

	s.seedFile("file-heron", "user-a", "heron.txt", "text/plain", "text")
	heronArtifact = s.seedPromotedArtifact("file-heron", "user-a", nil)
	ex.text = heronText()
	analyzedFile(t, s, i, "file-heron", "user-a")

	return kingfisherArtifact, heronArtifact
}

func decodeHits(t *testing.T, nodes []memorynodes.MemoryNode) []similarArtifactHit {
	t.Helper()
	out := make([]similarArtifactHit, 0, len(nodes))
	for _, n := range nodes {
		var hit similarArtifactHit
		if err := json.Unmarshal(n.Payload, &hit); err != nil {
			t.Fatalf("decode hit payload: %v", err)
		}
		out = append(out, hit)
	}
	return out
}

// TestSimilarArtifacts_RanksTheRightArtifactFirst.
func TestSimilarArtifacts_RanksTheRightArtifactFirst(t *testing.T) {
	s := newLibStub()
	kingfisher, heron := twoAnalysedFiles(t, s)
	i := NewIntegration(s)

	nodes, err := i.handleSimilarArtifacts(ownerContext("user-a"), map[string]any{
		"text": "kingfisher tunnels river bank",
	}, 0)
	if err != nil {
		t.Fatalf("handleSimilarArtifacts: %v", err)
	}
	hits := decodeHits(t, nodes)
	if len(hits) == 0 {
		t.Fatalf("no results -- the analysis pass embedded %d chunk(s) for these two files, so a "+
			"query drawn from one of them must match something", len(s.vectors))
	}
	if hits[0].ArtifactId != kingfisher {
		t.Fatalf("first hit is %q, want the kingfisher file's artifact %q (heron is %q).\n  hits: %+v",
			hits[0].ArtifactId, kingfisher, heron, hits)
	}
	if hits[0].Snippet == "" || hits[0].Title == "" {
		t.Fatalf("the hit carries no snippet/title -- the fold must re-read the artifact so the "+
			"caller can see WHAT matched: %+v", hits[0])
	}
	if hits[0].FileId != "file-birds" {
		t.Fatalf("hit.fileId = %q, want \"file-birds\"", hits[0].FileId)
	}
}

// TestSimilarArtifacts_FoldsChunksToOneRowPerArtifact. A relevant file
// matches on several of its chunks; the person searching wants the file
// once, at its best chunk's score.
func TestSimilarArtifacts_FoldsChunksToOneRowPerArtifact(t *testing.T) {
	s := newLibStub()
	kingfisher, _ := twoAnalysedFiles(t, s)
	i := NewIntegration(s)

	nodes, err := i.handleSimilarArtifacts(ownerContext("user-a"), map[string]any{
		"text":  "kingfisher heron shallows tunnels",
		"limit": 10,
	}, 0)
	if err != nil {
		t.Fatalf("handleSimilarArtifacts: %v", err)
	}
	seen := map[string]int{}
	for _, hit := range decodeHits(t, nodes) {
		seen[hit.ArtifactId]++
	}
	if seen[kingfisher] != 1 {
		t.Fatalf("the kingfisher artifact appears %d time(s); the fold must collapse its several "+
			"matching chunks onto one row.\n  seen: %v", seen[kingfisher], seen)
	}
}

// TestSimilarArtifacts_NeverReturnsAnotherUsersFile is the load-bearing
// one. The foreign chunk here is an EXACT match for the query, so it wins
// the vector ranking outright -- a capability that forgot to scope would
// return it first, not merely somewhere in the tail.
//
// Note what this test does and does not isolate. Two gates stand between
// the two Libraries -- the chunk-level owner compare and the owner-gated
// artifact re-read -- and either one alone makes this test pass
// (measured: deleting just the first leaves it green). That is the
// defence-in-depth working, but it means this test is evidence about the
// PAIR. TestFoldChunksToArtifacts below covers the first gate on its own.
func TestSimilarArtifacts_NeverReturnsAnotherUsersFile(t *testing.T) {
	s := newLibStub()
	kingfisher, _ := twoAnalysedFiles(t, s)
	s.seedForeignChunk("foreign-1", "user-b", "file-secret", "artifact:foreign",
		"kingfisher tunnels river bank")
	i := NewIntegration(s)

	nodes, err := i.handleSimilarArtifacts(ownerContext("user-a"), map[string]any{
		"text":  "kingfisher tunnels river bank",
		"limit": 10,
	}, 0)
	if err != nil {
		t.Fatalf("handleSimilarArtifacts: %v", err)
	}
	hits := decodeHits(t, nodes)
	for _, hit := range hits {
		if hit.ArtifactId == "artifact:foreign" || hit.FileId == "file-secret" {
			t.Fatalf("a file belonging to user-b came back to user-a. similarTo applies NO "+
				"per-row authorization -- the scoping in this capability is the only thing "+
				"between the two Libraries.\n  hits: %+v", hits)
		}
	}
	if len(hits) == 0 || hits[0].ArtifactId != kingfisher {
		t.Fatalf("user-a's own best match should still lead; got %+v", hits)
	}

	// And the mirror: user-b sees theirs and not user-a's.
	nodes, err = i.handleSimilarArtifacts(ownerContext("user-b"), map[string]any{
		"text": "kingfisher tunnels river bank",
	}, 0)
	if err != nil {
		t.Fatalf("handleSimilarArtifacts as user-b: %v", err)
	}
	hits = decodeHits(t, nodes)
	if len(hits) != 1 || hits[0].ArtifactId != "artifact:foreign" {
		t.Fatalf("user-b should see exactly their own file; got %+v", hits)
	}
}

// TestSimilarArtifacts_RefusesUnattributed. Every gate is "is this the
// caller's row?", so an unattributed call cannot be answered -- and
// answering it as an empty result would read to the user as "you have
// nothing like that".
func TestSimilarArtifacts_RefusesUnattributed(t *testing.T) {
	s := newLibStub()
	twoAnalysedFiles(t, s)
	i := NewIntegration(s)

	if _, err := i.handleSimilarArtifacts(context.Background(), map[string]any{
		"text": "kingfisher",
	}, 0); err == nil {
		t.Fatalf("handleSimilarArtifacts answered a request with no acting user")
	}
}

// TestSimilarArtifacts_SeedArtifactIsExcludedFromItsOwnResults --
// "more like this one" must not answer "this one".
func TestSimilarArtifacts_SeedArtifactIsExcludedFromItsOwnResults(t *testing.T) {
	s := newLibStub()
	kingfisher, heron := twoAnalysedFiles(t, s)
	// Give the seed a summary so it is the query text, exercising the
	// summary-first branch.
	s.artifacts[kingfisher]["summary"] = "kingfishers nest in tunnels dug into a river bank"
	i := NewIntegration(s)

	nodes, err := i.handleSimilarArtifacts(ownerContext("user-a"), map[string]any{
		"artifactId": kingfisher,
		"limit":      10,
	}, 0)
	if err != nil {
		t.Fatalf("handleSimilarArtifacts by artifactId: %v", err)
	}
	for _, hit := range decodeHits(t, nodes) {
		if hit.ArtifactId == kingfisher {
			t.Fatalf("the seed artifact came back in its own similar-to results: %+v", hit)
		}
		if hit.ArtifactId != heron {
			t.Fatalf("unexpected hit %q; only the other file should match", hit.ArtifactId)
		}
	}
}

// TestSimilarArtifacts_SeedArtifactMustBeTheCallers. "More like this"
// over somebody else's artifact must not silently become a search for
// nothing -- it is refused by name.
func TestSimilarArtifacts_SeedArtifactMustBeTheCallers(t *testing.T) {
	s := newLibStub()
	twoAnalysedFiles(t, s)
	s.seedForeignChunk("foreign-1", "user-b", "file-secret", "artifact:foreign", "anything")
	i := NewIntegration(s)

	if _, err := i.handleSimilarArtifacts(ownerContext("user-a"), map[string]any{
		"artifactId": "artifact:foreign",
	}, 0); err == nil {
		t.Fatalf("a user seeded a similarity search from another user's artifact")
	}
}

// TestSimilarArtifacts_NeedsSomethingToSearchBy.
func TestSimilarArtifacts_NeedsSomethingToSearchBy(t *testing.T) {
	i := NewIntegration(newLibStub())
	if _, err := i.handleSimilarArtifacts(ownerContext("user-a"), map[string]any{}, 0); err == nil {
		t.Fatalf("handleSimilarArtifacts accepted a call with neither text nor artifactId")
	}
}

// TestFoldChunksToArtifacts states the fold as a property, independently
// of the engine: nothing that is not the caller's survives, and each
// artifact keeps its BEST chunk's score rather than its last-seen one.
func TestFoldChunksToArtifacts(t *testing.T) {
	chunks := []map[string]any{
		{"ownerUserId": "user-a", "artifactId": "art-1", "fileId": "f1", "seq": 0, "text": "low", "_similarity": 0.2},
		{"ownerUserId": "user-a", "artifactId": "art-1", "fileId": "f1", "seq": 4, "text": "high", "_similarity": 0.9},
		{"ownerUserId": "user-a", "artifactId": "art-1", "fileId": "f1", "seq": 7, "text": "mid", "_similarity": 0.5},
		{"ownerUserId": "user-b", "artifactId": "art-2", "fileId": "f2", "seq": 0, "text": "theirs", "_similarity": 1.0},
		{"ownerUserId": "user-a", "artifactId": "art-3", "fileId": "f3", "seq": 0, "text": "other", "_similarity": 0.4},
	}
	hits := foldChunksToArtifacts(chunks, "user-a", "")
	sortHitsByScore(hits)

	if len(hits) != 2 {
		t.Fatalf("fold produced %d artifact(s), want 2 (art-1 and art-3; art-2 is user-b's): %+v",
			len(hits), hits)
	}
	if hits[0].ArtifactId != "art-1" || hits[0].Score != 0.9 {
		t.Fatalf("art-1 should lead at its BEST chunk's score 0.9, got %+v", hits[0])
	}
	if hits[0].Seq != 4 || hits[0].Snippet != "high" {
		t.Fatalf("the surviving hit must describe the chunk that actually scored best, got %+v", hits[0])
	}
	// The exclusion is what makes "more like this" not answer "this".
	if got := foldChunksToArtifacts(chunks, "user-a", "art-1"); len(got) != 1 || got[0].ArtifactId != "art-3" {
		t.Fatalf("excluding art-1 should leave only art-3, got %+v", got)
	}
}

// TestResolveLimitIsClamped -- each returned artifact costs a further
// owner-gated read, so an unbounded limit is an unbounded fan-out.
func TestResolveLimitIsClamped(t *testing.T) {
	if got := resolveLimit(nil, 0); got != defaultSimilarLimit {
		t.Errorf("resolveLimit(nil, 0) = %d, want %d", got, defaultSimilarLimit)
	}
	if got := resolveLimit(nil, 3); got != 3 {
		t.Errorf("resolveLimit(nil, 3) = %d, want the engine's target 3", got)
	}
	if got := resolveLimit(float64(9), 0); got != 9 {
		t.Errorf("resolveLimit(9.0, 0) = %d, want 9 -- a JSON round-trip produces float64", got)
	}
	if got := resolveLimit(100000, 0); got != maxSimilarLimit {
		t.Errorf("resolveLimit(100000, 0) = %d, want it clamped to %d", got, maxSimilarLimit)
	}
}
