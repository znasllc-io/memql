package library

// train_test.go -- libraryTrainFile (memql#4342, design D7 + 3.6).
//
// Four claims: the chunks land in the chosen domain carrying the artifact
// sourceRef, the file records the domain, a domain the caller may not
// write to is refused, and a file that is not the caller's is not found.
//
// The domain gate is deny-by-default and that is the shape under test:
// with no authorizer wired, a plain user is refused. The reason it has to
// be that way is that integration.knowledge.ingest performs NO
// authorization of its own, so a permissive default here would make this
// the shortest path in the product to writing into anyone's corpus,
// wearing the name of a feature that says it checks.

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// trainableFile runs the real analysis pass so the file has real chunks
// to be trained FROM -- train reconstructs its text from them, so a
// fixture that hand-seeded chunk rows would not exercise the rejoin.
func trainableFile(t *testing.T, s *libStub) (fileId, artifactId string) {
	t.Helper()
	i := NewIntegration(s)
	i.SetExtractor(fixedExtractor{text: birdsText()})
	i.SetArtifactPoll(1, 0)
	s.seedFile("file-birds", "user-a", "birds.txt", "text/plain", "text")
	artifactId = s.seedPromotedArtifact("file-birds", "user-a", nil)
	analyzedFile(t, s, i, "file-birds", "user-a")
	return "file-birds", artifactId
}

// approvingAuthorizer stands in for the product's domain-write decision.
type recordingAuthorizer struct {
	allow bool
	err   error
	calls []string
}

func (a *recordingAuthorizer) MayWriteKnowledgeDomain(_ context.Context, userId, domainId string) (bool, error) {
	a.calls = append(a.calls, userId+"/"+domainId)
	return a.allow, a.err
}

// TestTrainFile_ChunksLandInTheDomainWithTheArtifactSourceRef.
func TestTrainFile_ChunksLandInTheDomainWithTheArtifactSourceRef(t *testing.T) {
	s := newLibStub()
	fileId, artifactId := trainableFile(t, s)
	auth := &recordingAuthorizer{allow: true}
	i := NewIntegration(s)
	i.SetDomainWriteAuthorizer(auth)

	if _, err := i.handleTrainFile(ownerContext("user-a"), map[string]any{
		"fileId": fileId, "domainId": "domain-birds",
	}, 0); err != nil {
		t.Fatalf("handleTrainFile: %v", err)
	}

	if len(s.ingests) != 1 {
		t.Fatalf("knowledge.ingest was called %d time(s), want 1", len(s.ingests))
	}
	ingest := s.ingests[0]
	if got := asString(ingest["domainId"]); got != "domain-birds" {
		t.Fatalf("ingest domainId = %q, want \"domain-birds\"", got)
	}
	wantRef := "artifact:" + artifactId
	if got := asString(ingest["sourceRef"]); got != wantRef {
		t.Fatalf("ingest sourceRef = %q, want %q -- every knowledge chunk has to point back at "+
			"the Library row it came from (design 3.6)", got, wantRef)
	}
	if got := asString(ingest["source"]); got != "fileUpload" {
		t.Fatalf("ingest source = %q, want \"fileUpload\" -- the provenance class the citation "+
			"registry reads", got)
	}
	text := asString(ingest["text"])
	if !strings.Contains(text, "kingfisher") || !strings.Contains(text, "Paragraph 11") {
		t.Fatalf("the ingested text does not span the whole file (len %d); train reconstructs it "+
			"from the file's chunks and must cover all of them", len(text))
	}

	// The decision is recorded ON THE FILE -- that list is the audit trail
	// a person can see (design D7).
	domains := stringSliceField(s.files[fileId], "trainedIntoDomainIds")
	if len(domains) != 1 || domains[0] != "domain-birds" {
		t.Fatalf("trainedIntoDomainIds = %v, want [domain-birds]", domains)
	}
	if len(s.auditEvents) != 1 {
		t.Fatalf("%d audit event(s) written, want 1", len(s.auditEvents))
	}
	if !strings.Contains(s.auditEvents[0], "library_file_trained") {
		t.Fatalf("the audit event does not name the action: %s", s.auditEvents[0])
	}
	if auth.calls[0] != "user-a/domain-birds" {
		t.Fatalf("the authorizer was asked %v, want [user-a/domain-birds]", auth.calls)
	}
}

// TestTrainFile_RepeatIsIdempotentOnTheDomainList. Training the same file
// into the same domain twice must not record the domain twice --
// appendLibraryFileTrainedDomain takes the FULL merged list, so the merge
// is this code's responsibility.
func TestTrainFile_RepeatIsIdempotentOnTheDomainList(t *testing.T) {
	s := newLibStub()
	fileId, _ := trainableFile(t, s)
	i := NewIntegration(s)
	i.SetDomainWriteAuthorizer(&recordingAuthorizer{allow: true})
	ctx := ownerContext("user-a")

	for range 2 {
		if _, err := i.handleTrainFile(ctx, map[string]any{
			"fileId": fileId, "domainId": "domain-birds",
		}, 0); err != nil {
			t.Fatalf("handleTrainFile: %v", err)
		}
	}
	domains := stringSliceField(s.files[fileId], "trainedIntoDomainIds")
	if len(domains) != 1 {
		t.Fatalf("trainedIntoDomainIds = %v after training twice into one domain, want one entry",
			domains)
	}
}

// TestTrainFile_SecondDomainIsAppendedNotReplaced.
func TestTrainFile_SecondDomainIsAppendedNotReplaced(t *testing.T) {
	s := newLibStub()
	fileId, _ := trainableFile(t, s)
	i := NewIntegration(s)
	i.SetDomainWriteAuthorizer(&recordingAuthorizer{allow: true})
	ctx := ownerContext("user-a")

	for _, domain := range []string{"domain-birds", "domain-rivers"} {
		if _, err := i.handleTrainFile(ctx, map[string]any{
			"fileId": fileId, "domainId": domain,
		}, 0); err != nil {
			t.Fatalf("handleTrainFile(%s): %v", domain, err)
		}
	}
	domains := stringSliceField(s.files[fileId], "trainedIntoDomainIds")
	if len(domains) != 2 || domains[0] != "domain-birds" || domains[1] != "domain-rivers" {
		t.Fatalf("trainedIntoDomainIds = %v, want both domains -- the merge appends, it does "+
			"not replace", domains)
	}
}

// TestTrainFile_UnauthorisedDomainIsRefused: the authorizer says no, and
// nothing is written -- not the knowledge chunks, not the domain list.
func TestTrainFile_UnauthorisedDomainIsRefused(t *testing.T) {
	s := newLibStub()
	fileId, _ := trainableFile(t, s)
	i := NewIntegration(s)
	i.SetDomainWriteAuthorizer(&recordingAuthorizer{allow: false})

	_, err := i.handleTrainFile(ownerContext("user-a"), map[string]any{
		"fileId": fileId, "domainId": "someone-elses-domain",
	}, 0)
	if err == nil {
		t.Fatalf("handleTrainFile trained into a domain the caller may not write to")
	}
	if !strings.Contains(err.Error(), "someone-elses-domain") {
		t.Fatalf("the refusal does not name the domain: %v", err)
	}
	if len(s.ingests) != 0 {
		t.Fatalf("%d ingest(s) ran despite the refusal", len(s.ingests))
	}
	if _, present := s.files[fileId]["trainedIntoDomainIds"]; present {
		t.Fatalf("the domain was recorded on the file despite the refusal: %v",
			s.files[fileId]["trainedIntoDomainIds"])
	}
}

// TestTrainFile_NoAuthorizerRefusesAPlainUser. Deny-by-default: a cluster
// that cannot answer "may they write here?" must not answer "yes".
func TestTrainFile_NoAuthorizerRefusesAPlainUser(t *testing.T) {
	s := newLibStub()
	fileId, _ := trainableFile(t, s)
	i := NewIntegration(s) // no authorizer wired

	if _, err := i.handleTrainFile(ownerContext("user-a"), map[string]any{
		"fileId": fileId, "domainId": "domain-birds",
	}, 0); err == nil {
		t.Fatalf("handleTrainFile trained with no domain authorizer wired; with nobody able to " +
			"vouch for the domain the call must be refused")
	}
	if len(s.ingests) != 0 {
		t.Fatalf("%d ingest(s) ran with no authorizer wired", len(s.ingests))
	}
}

// TestTrainFile_ClusterOwnerMayTrainWithoutAnAuthorizer -- the operator
// is the one caller the engine can decide about on its own, and is what
// keeps the capability exercisable on a fresh cluster.
func TestTrainFile_ClusterOwnerMayTrainWithoutAnAuthorizer(t *testing.T) {
	s := newLibStub()
	// The operator's own file: the file gate is unchanged for them, it is
	// only the DOMAIN gate the owner role opens.
	i := NewIntegration(s)
	i.SetExtractor(fixedExtractor{text: birdsText()})
	i.SetArtifactPoll(1, 0)
	s.seedFile("file-birds", "root", "birds.txt", "text/plain", "text")
	s.seedPromotedArtifact("file-birds", "root", nil)
	if err := i.AnalyzeFile(clusterOwnerContext("root"), AnalyzeFileParams{
		FileId: "file-birds", OwnerUserId: "root", Name: "birds.txt", MimeType: "text/plain",
	}); err != nil {
		t.Fatalf("AnalyzeFile: %v", err)
	}

	if _, err := i.handleTrainFile(clusterOwnerContext("root"), map[string]any{
		"fileId": "file-birds", "domainId": "domain-birds",
	}, 0); err != nil {
		t.Fatalf("a cluster owner was refused: %v", err)
	}
	if len(s.ingests) != 1 {
		t.Fatalf("%d ingest(s), want 1", len(s.ingests))
	}
}

// TestTrainFile_AnotherUsersFileIsNotFound. libraryFileById is gated on
// ownerUserId == actor.userId, so someone else's file does not come back
// -- and "not yours" reads exactly like "does not exist", which is the
// right answer for an id a caller can guess.
func TestTrainFile_AnotherUsersFileIsNotFound(t *testing.T) {
	s := newLibStub()
	fileId, _ := trainableFile(t, s)
	i := NewIntegration(s)
	i.SetDomainWriteAuthorizer(&recordingAuthorizer{allow: true})

	_, err := i.handleTrainFile(ownerContext("user-b"), map[string]any{
		"fileId": fileId, "domainId": "domain-birds",
	}, 0)
	if err == nil {
		t.Fatalf("user-b trained user-a's file")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("the refusal should be indistinguishable from a missing file, got: %v", err)
	}
	if len(s.ingests) != 0 {
		t.Fatalf("%d ingest(s) ran for another user's file", len(s.ingests))
	}
}

// TestTrainFile_FileWithNoTextIsRefused. An opaque file (design 3.4:
// ready, no chunks) has nothing to train on, and saying so beats sending
// an empty document into somebody's corpus.
func TestTrainFile_FileWithNoTextIsRefused(t *testing.T) {
	s := newLibStub()
	s.seedFile("file-zip", "user-a", "archive.zip", "application/zip", "other")
	s.seedPromotedArtifact("file-zip", "user-a", nil)
	s.files["file-zip"]["status"] = "ready"
	i := NewIntegration(s)
	i.SetDomainWriteAuthorizer(&recordingAuthorizer{allow: true})

	_, err := i.handleTrainFile(ownerContext("user-a"), map[string]any{
		"fileId": "file-zip", "domainId": "domain-birds",
	}, 0)
	if err == nil {
		t.Fatalf("handleTrainFile accepted a file with no extracted text")
	}
	if len(s.ingests) != 0 {
		t.Fatalf("an empty document was ingested")
	}
}

// TestTrainFile_AuthorizerErrorIsNotAYes. A check that could not be
// performed is not a check that passed.
func TestTrainFile_AuthorizerErrorIsNotAYes(t *testing.T) {
	s := newLibStub()
	fileId, _ := trainableFile(t, s)
	i := NewIntegration(s)
	i.SetDomainWriteAuthorizer(&recordingAuthorizer{allow: true, err: fmt.Errorf("directory down")})

	if _, err := i.handleTrainFile(ownerContext("user-a"), map[string]any{
		"fileId": fileId, "domainId": "domain-birds",
	}, 0); err == nil {
		t.Fatalf("an authorizer that ERRORED was treated as an approval")
	}
	if len(s.ingests) != 0 {
		t.Fatalf("%d ingest(s) ran after an authorizer error", len(s.ingests))
	}
}

// TestJoinChunkTexts_DropsTheOverlapSeam. Chunks overlap by design, so a
// naive join would repeat every seam -- and knowledge.ingest re-chunks
// whatever it is handed, so those repetitions would become duplicated
// knowledge chunks.
func TestJoinChunkTexts_DropsTheOverlapSeam(t *testing.T) {
	tail := strings.Repeat("shared seam text that is long enough to be believed. ", 2)
	first := "The opening of the document, several sentences long. " + tail
	second := tail + "And then the part that only the second chunk carries."

	got := joinChunkTexts([]map[string]any{
		{"text": first, "seq": 0},
		{"text": second, "seq": 1},
	})
	if strings.Count(got, tail) != 1 {
		t.Fatalf("the overlap appears %d time(s) in the rejoin; the seam must be dropped exactly "+
			"once.\n  got: %q", strings.Count(got, tail), got)
	}
	if !strings.Contains(got, "The opening of the document") ||
		!strings.Contains(got, "only the second chunk carries") {
		t.Fatalf("the rejoin lost content from one end: %q", got)
	}
}

// TestJoinChunkTexts_KeepsEverythingWhenThereIsNoSeam. The bias is
// one-directional on purpose: a missed seam repeats a little text, a
// wrongly-guessed one would DELETE some.
func TestJoinChunkTexts_KeepsEverythingWhenThereIsNoSeam(t *testing.T) {
	got := joinChunkTexts([]map[string]any{
		{"text": "First chunk with its own words.", "seq": 0},
		{"text": "Second chunk sharing nothing at all with it.", "seq": 1},
	})
	if !strings.Contains(got, "First chunk with its own words.") ||
		!strings.Contains(got, "Second chunk sharing nothing at all with it.") {
		t.Fatalf("a chunk was dropped when no seam was found: %q", got)
	}
}

// TestJoinChunkTexts_IgnoresACoincidentalShortMatch. Every pair of
// English chunks "overlaps" on a few characters; believing that would
// delete real text.
func TestJoinChunkTexts_IgnoresACoincidentalShortMatch(t *testing.T) {
	got := joinChunkTexts([]map[string]any{
		{"text": "The first chunk ends with the", "seq": 0},
		{"text": "the second chunk starts here", "seq": 1},
	})
	if !strings.Contains(got, "the second chunk starts here") {
		t.Fatalf("a coincidental short match ate the head of the next chunk: %q", got)
	}
}

// TestMergeDomainAdd covers the read-then-merge the mutation delegates.
func TestMergeDomainAdd(t *testing.T) {
	merged, changed := mergeDomainAdd([]string{"a"}, "b")
	if !changed || len(merged) != 2 || merged[1] != "b" {
		t.Fatalf("mergeDomainAdd([a], b) = %v, %v", merged, changed)
	}
	if _, changed := mergeDomainAdd([]string{"a", "b"}, "b"); changed {
		t.Fatalf("adding a domain already present reported a change")
	}
	// Trimmed compare: a value stored whitespace-padded through some other
	// path is still the same domain, not a second one.
	if _, changed := mergeDomainAdd([]string{" b "}, "b"); changed {
		t.Fatalf("a whitespace-padded stored value was treated as a different domain")
	}
}
