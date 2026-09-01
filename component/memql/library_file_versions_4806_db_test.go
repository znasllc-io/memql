package memql

import (
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// library_file_versions_4806_db_test.go -- the file version chain, end to end
// against a real store (epic memql#4806).
//
// Postgres-gated like its neighbours: sharedReadMergeEngine skips when no
// database is reachable, and CI's db-tests lane runs this package with
// MEMQL_REQUIRE_DB=1 so a skip there is a failure rather than a green.
//
// Four claims, and every one of them is unmeasurable without a store:
//
//  1. THE SUPERSEDE PAIR MAKES A DISTINCT ROW AND MOVES THE HEAD. The whole
//     design rests on "hypertable row history is not client-readable", so the
//     version has to be its own row that an ORDINARY OWNED QUERY returns. A
//     stub can only show that two statements were rendered.
//  2. THE HEAD'S sha256 IS BLANKED, NOT INHERITED. `update{}` is a read-merge
//     IN THE ENGINE (memql#1628), so whether `sha256: args.sha256 ?? ""`
//     writes an empty string or leaves the previous version's hash in place is
//     a fact about the executor, not about the call. It is also the difference
//     between "not measured yet" and a hash describing bytes that are gone --
//     a false integrity claim on the one field that exists to be checked.
//  3. A SECOND USER SEES NO VERSION -- through the named reads AND through a
//     raw query string, which is the path with no declared binding for the
//     tier to be resolved from and therefore the one the per-row gate exists
//     for. History must not become a way around a file's own admission.
//  4. THE QUOTA READ SUMS ACROSS VERSIONS, owner-scoped. Superseding destroys
//     nothing, so those bytes are as real as the head's.
//
// Both version mutations are @serverOnly, so every write here runs under
// auth.ContextWithInternalOrigin -- the release-cut db test's shape, and the
// same stamp component/server/fileversion applies in production.

func versionedFileArgs(fileId, owner string) map[string]any {
	return map[string]any{
		"fileId":   fileId,
		"name":     "q3.pdf",
		"mimeType": "application/pdf",
		"size":     1000,
		"sha256":   strings.Repeat("aa", 32),
		"blobUrl":  "library/" + owner + "/" + fileId + "/q3.pdf",
		"source":   "uploaded",
		"format":   "pdf",
	}
}

// TestFileVersionChainIsReadableAndOwned covers claims 1, 3 and 4.
func TestFileVersionChainIsReadableAndOwned(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)

	suffix := uniqueSuffix("libversions4806")
	userA := "user-a-" + suffix
	userB := "user-b-" + suffix
	fileId := "libfile-" + suffix

	ctxA := rowAuthzCallerCtx(userA)
	ctxB := rowAuthzCallerCtx(userB)
	// The @serverOnly pair needs internal origin; the READS deliberately do
	// not get it, because row admission under the caller's own actor is the
	// whole authorization.
	writeA := auth.ContextWithInternalOrigin(ctxA)

	runMutation(t, ctxA, eng, "createLibraryFile", versionedFileArgs(fileId, userA))

	// --- the supersede: freeze v1, move the head to v2 ---
	runMutation(t, writeA, eng, "createLibraryFileVersion", map[string]any{
		"versionId":     fileId + "-v1",
		"fileId":        fileId,
		"versionNumber": 1,
		"name":          "q3.pdf",
		"mimeType":      "application/pdf",
		"size":          1000,
		"sha256":        strings.Repeat("aa", 32),
		"blobUrl":       "library/" + userA + "/" + fileId + "/q3.pdf",
		"format":        "pdf",
		"summary":       "the first draft",
		"uploadedAt":    "2026-08-01T10:00:00Z",
	})
	runMutation(t, writeA, eng, "supersedeLibraryFileHead", map[string]any{
		"fileId":        fileId,
		"versionNumber": 2,
		"name":          "q3-final.pdf",
		"mimeType":      "application/pdf",
		"size":          2500,
		"sha256":        strings.Repeat("bb", 32),
		"blobUrl":       "library/" + userA + "/" + fileId + "/k-9/q3-final.pdf",
		"format":        "pdf",
	})

	// --- A reads the history and the head ---
	res, err := eng.Execute(ctxA, fmt.Sprintf(`libraryFileVersionsForFile(fileId: "%s")`, fileId))
	if err != nil {
		t.Fatalf("A reading the version history: %v", err)
	}
	blob := resultBlob(t, res)
	if !strings.Contains(blob, "the first draft") {
		t.Fatalf("the superseded version is not readable through libraryFileVersionsForFile -- "+
			"the whole design rests on a version being its own row an ordinary owned query "+
			"returns, because hypertable history is not client-readable.\n  saw: %s", blob)
	}
	if strings.Contains(blob, "q3-final.pdf") {
		t.Fatalf("the HEAD appeared in the history read; the newest version is the file row, and "+
			"a client folding the two would show it twice.\n  saw: %s", blob)
	}

	// The by-number read is what ?version={n} resolves through: an int
	// comparison pushed down to SQL, which only a store can answer.
	res, err = eng.Execute(ctxA, fmt.Sprintf(`libraryFileVersionByNumber(fileId: "%s", versionNumber: 1)`, fileId))
	if err != nil {
		t.Fatalf("A reading version 1 by number: %v", err)
	}
	if blob := resultBlob(t, res); !strings.Contains(blob, "the first draft") {
		t.Fatalf("libraryFileVersionByNumber did not resolve (file, 1): %s", blob)
	}
	res, err = eng.Execute(ctxA, fmt.Sprintf(`libraryFileVersionByNumber(fileId: "%s", versionNumber: 9)`, fileId))
	if err != nil {
		t.Fatalf("A reading a version that does not exist: %v", err)
	}
	if blob := resultBlob(t, res); strings.Contains(blob, fileId+"-v1") {
		t.Fatalf("asking for version 9 returned version 1 -- the number narrows nothing: %s", blob)
	}

	// --- claim 4: the quota read sees the superseded bytes ---
	res, err = eng.Execute(ctxA, `libraryFileVersionSizesForOwner()`)
	if err != nil {
		t.Fatalf("A reading the version quota: %v", err)
	}
	if blob := resultBlob(t, res); !strings.Contains(blob, "1000") {
		t.Fatalf("the superseded version's bytes are absent from the quota read -- superseding "+
			"destroys nothing, and a quota that ignored history would refuse a person using "+
			"numbers they cannot see anywhere.\n  saw: %s", blob)
	}

	// --- claim 3: B sees none of it, named and raw ---
	for name, q := range map[string]string{
		"named history":   fmt.Sprintf(`libraryFileVersionsForFile(fileId: "%s")`, fileId),
		"named by number": fmt.Sprintf(`libraryFileVersionByNumber(fileId: "%s", versionNumber: 1)`, fileId),
		"named quota":     `libraryFileVersionSizesForOwner()`,
		"raw by id":       fmt.Sprintf(`row.id=="v1:library:fileVersion:%s-v1"`, fileId),
		"raw by concept":  `concept=="v1:library:fileVersion"`,
	} {
		res, err := eng.Execute(ctxB, q)
		if err != nil {
			// A refusal is an acceptable outcome; being shown the row is not.
			t.Logf("B reading %s refused: %v", name, err)
			continue
		}
		if blob := resultBlob(t, res); strings.Contains(blob, "the first draft") {
			t.Fatalf("caller B was returned caller A's file version through %s. "+
				"v1:library:fileVersion declares @rowAuthz(owner=\"ownerUserId\", clusterOwner) "+
				"and B is neither -- history must not become a way around a file's own "+
				"admission.\n  B saw: %s", name, blob)
		}
	}
}

// TestSupersedeBlanksTheHashItCannotInherit is claim 2, on its own because it
// is the one an author would most plausibly "simplify" away.
//
// A chunked upload's handler never holds the whole file, so the head is
// superseded with NO hash and the analysis pass stamps one later. update{} is
// a read-merge, so omitting the argument would leave the PREVIOUS version's
// hash sitting on bytes it does not describe -- not a missing fact but a false
// one. The mutation writes `sha256: args.sha256 ?? ""` for exactly that
// reason, and whether the engine then persists the blank is a fact about the
// executor rather than about the call string.
func TestSupersedeBlanksTheHashItCannotInherit(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)

	suffix := uniqueSuffix("libversionhash4806")
	userA := "user-a-" + suffix
	fileId := "libfile-" + suffix
	ctxA := rowAuthzCallerCtx(userA)
	writeA := auth.ContextWithInternalOrigin(ctxA)

	storedId := runMutation(t, ctxA, eng, "createLibraryFile", versionedFileArgs(fileId, userA))

	// The head starts at version 1 with a measured hash, both STAMPED rather
	// than accepted -- a caller-chosen version number would let an upload
	// claim to be version 40 of a file with no history.
	before := latestPayload(t, ctx, db, "v1:library:file", storedId)
	if got := before["versionNumber"]; fmt.Sprint(got) != "1" {
		t.Fatalf("a new file's versionNumber is %v, want 1", got)
	}
	if fmt.Sprint(before["versionUploadedAt"]) == "" {
		t.Fatal("a new file has no versionUploadedAt -- the history's 'uploaded' reading has " +
			"nothing to render, and createdAt is the LAST WRITE, which an analysis transition moves")
	}

	runMutation(t, writeA, eng, "createLibraryFileVersion", map[string]any{
		"versionId": fileId + "-v1", "fileId": fileId, "versionNumber": 1,
		"name": "q3.pdf", "mimeType": "application/pdf", "size": 1000,
		"sha256": strings.Repeat("aa", 32), "format": "pdf",
		"blobUrl":                "library/" + userA + "/" + fileId + "/q3.pdf",
		"uploadedFromWorkerId":   "wrk-1",
		"uploadedFromWorkerName": "MacBook-Pro",
		"uploadedAt":             "2026-08-01T10:00:00Z",
	})
	// The chunked shape: no hash, and no machine either -- this version came
	// from a browser.
	runMutation(t, writeA, eng, "supersedeLibraryFileHead", map[string]any{
		"fileId": fileId, "versionNumber": 2,
		"name": "q3-final.pdf", "mimeType": "application/pdf", "size": 2500,
		"blobUrl": "library/" + userA + "/" + fileId + "/k-9/q3-final.pdf",
		"format":  "pdf",
	})

	after := latestPayload(t, ctx, db, "v1:library:file", storedId)
	if got := fmt.Sprint(after["sha256"]); got != "" {
		t.Fatalf("the head kept sha256 %q after a supersede that measured none. update{} is a "+
			"read-merge, so an omitted argument inherits -- and an inherited hash describes "+
			"bytes that are gone, which is a FALSE integrity claim rather than a missing one "+
			"on the one field that exists to be checked (design D5).", got)
	}
	// PROVENANCE IS PER VERSION AND NEVER INHERITED, by the same mechanism: a
	// file pushed from a laptop and then replaced from a browser must not go
	// on naming the laptop.
	for _, field := range []string{"uploadedFromWorkerId", "uploadedFromWorkerName", "uploadedFromPath"} {
		if got := fmt.Sprint(after[field]); got != "" {
			t.Errorf("the head kept %s=%q after a supersede that named no machine -- a browser "+
				"upload would render as 'uploaded from' somebody's laptop", field, got)
		}
	}
	// The status is 'analyzing', NEVER 'stored': indexFileOnCreate filters on
	// status=="stored" and graph.node.created fires on every write, so a head
	// re-entering that state would re-run promotion through createArtifact's
	// bare insert{} and wipe the artifact's labels (design D4).
	if got := fmt.Sprint(after["status"]); got != "analyzing" {
		t.Errorf("the head's status after a supersede is %q, want \"analyzing\" -- \"stored\" "+
			"would re-fire indexFileOnCreate and wipe the artifact's labels", got)
	}
	if got := fmt.Sprint(after["versionNumber"]); got != "2" {
		t.Errorf("the head's versionNumber is %s, want 2", got)
	}
	// The read-merge KEEPS what the supersede does not name: ownership survives
	// a new version, which is half of what "one row in the list" means.
	//
	// Containment rather than equality, and the difference is a fact about the
	// engine worth stating: ownerUserId carries an @relationship to
	// v1:identity:user, so the stored value is the CANONICAL id
	// (`v1:identity:user:<id>`) while the caller named the bare one. The
	// identifier conventions call that shape out -- canonical internally, bare
	// at every wire seam -- and an equality assertion here would be asserting
	// the wire spelling against the stored one.
	if got := fmt.Sprint(after["ownerUserId"]); !strings.Contains(got, userA) {
		t.Errorf("the head's ownerUserId became %q after a supersede; it must still name %s", got, userA)
	}

	// And the frozen version still holds its own facts, untouched.
	res, err := eng.Execute(ctxA, fmt.Sprintf(`libraryFileVersionByNumber(fileId: "%s", versionNumber: 1)`, fileId))
	if err != nil {
		t.Fatalf("reading the frozen version: %v", err)
	}
	blob := resultBlob(t, res)
	for _, want := range []string{strings.Repeat("aa", 32), "MacBook-Pro", "2026-08-01T10:00:00Z"} {
		if !strings.Contains(blob, want) {
			t.Errorf("the frozen version lost %q -- a version's own facts are frozen with it, "+
				"and its provenance is the version's, never the file's current one.\n  saw: %s",
				want, blob)
		}
	}
}
