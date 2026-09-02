package memql

import (
	"fmt"
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// library_restore_4844_db_test.go -- taking things back out of the Bin
// (memql#4844 over the memql#4784 mutations), end to end against a real store.
//
// Postgres-gated like its neighbours: sharedReadMergeEngine skips when no
// database is reachable, and CI's db-tests lane runs this package with
// MEMQL_REQUIRE_DB=1 so a skip there is a failure rather than a green.
//
// What is pinned here, and why it needs a database:
//
//  1. THE ROUND TRIP. archive -> restore -> visible to libraryArtifacts()
//     again -> archive again. The list read is `archived != true` compiled to
//     SQL over the append-only versions, and only Postgres answers whether a
//     re-version carrying archived=false actually re-admits the row.
//  2. THE PAIR, CALLED THE WAY THE BIN CALLS IT. The backing file is NOT
//     restored by an automation, deliberately (restoreArtifact's own header:
//     a node.updated mirror filtered on archived==false fires on essentially
//     every artifact update, and with archiveFileOnArtifactArchive already in
//     place the two close an event cycle). The client runs restoreArtifact +
//     restoreLibraryFile as a pair (clients/os/src/apps/bin/restore.ts), the
//     file half addressed by the canonical sourceConceptRef spelling -- so
//     that is the call shape asserted, exactly as the archive test asserts
//     archiveLibraryFile the way the automation calls it.
//  3. RESTORE IS A READ-MERGE. The row comes back with its title, its name
//     and its provenance -- nothing was destroyed by the archive, so nothing
//     may be destroyed by the restore either.
//  4. A SECOND USER CANNOT PULL SOMEBODY ELSE'S ROW OUT OF THE BIN. The
//     composite tier's write guard is the gate, and it is only observable
//     against the real engine.

// TestRestorePairReturnsTheArtifactAndFileToTheLibrary is claims 1-4 for a
// file-backed artifact.
func TestRestorePairReturnsTheArtifactAndFileToTheLibrary(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)

	suffix := uniqueSuffix("library4844restore")
	userA := "user-a-" + suffix
	userB := "user-b-" + suffix
	fileId := "libfile-" + suffix
	ctxA := rowAuthzCallerCtx(userA)
	ctxB := rowAuthzCallerCtx(userB)

	runMutation(t, ctxA, eng, "createLibraryFile", libraryFileArgs(fileId, userA))
	sourceRef := "v1:library:file:" + fileId
	artifactId := runMutation(t, ctxA, eng, "createArtifact", map[string]any{
		"sourceConceptRef": sourceRef,
		"ownerUserId":      userA,
		"lens":             "artifact",
		"kind":             "file",
		"source":           "uploaded",
		"title":            "notes.md",
		"format":           "markdown",
		"mimeType":         "text/markdown",
	})

	listBlob := func(label string) string {
		res, err := eng.Execute(ctxA, `libraryArtifacts()`)
		if err != nil {
			t.Fatalf("libraryArtifacts() %s: %v", label, err)
		}
		return resultBlob(t, res)
	}

	// Baseline, asserted rather than assumed: a restore test over a list that
	// never held the row would prove nothing.
	if blob := listBlob("before archive"); !strings.Contains(blob, artifactId) {
		t.Fatalf("artifact %s missing from libraryArtifacts() before any archive.\n  saw: %s",
			artifactId, blob)
	}

	// The archive, both halves as production runs them: the owner's mutation,
	// then the cascade mutation the automation drives (this engine runs no
	// automation scheduler), addressed by the canonical ref exactly as
	// archiveFileOnArtifactArchive passes it.
	runMutation(t, ctxA, eng, "archiveArtifact", map[string]any{"artifactId": artifactId})
	runMutation(t, ctxA, eng, "archiveLibraryFile", map[string]any{"fileId": sourceRef})
	if blob := listBlob("after archive"); strings.Contains(blob, artifactId) {
		t.Fatalf("archived artifact %s still in libraryArtifacts().\n  saw: %s", artifactId, blob)
	}

	// A SECOND USER cannot restore it. Either the write guard refuses, or --
	// if a refusal ever became a silent no-op -- the row must still be out of
	// A's default list. Both outcomes are checked so the assertion cannot rot
	// into vacuity if the refusal shape changes.
	if _, err := eng.Execute(ctxB, fmt.Sprintf(`mutation restoreArtifact(artifactId: %s)`, langparser.QuoteString(artifactId))); err == nil {
		if blob := listBlob("after B's restore attempt"); strings.Contains(blob, artifactId) {
			t.Fatalf("caller B pulled caller A's artifact out of the Bin -- restoreArtifact must be "+
				"owner-scoped by the composite tier's write guard.\n  A's list: %s", blob)
		}
	}

	// THE RESTORE PAIR, index first then file -- the order restore.ts fixes so
	// an interruption leaves a visible row rather than an invisible change.
	runMutation(t, ctxA, eng, "restoreArtifact", map[string]any{"artifactId": artifactId})
	runMutation(t, ctxA, eng, "restoreLibraryFile", map[string]any{"fileId": sourceRef})

	afterRestore := listBlob("after restore")
	if !strings.Contains(afterRestore, artifactId) {
		t.Fatalf("restored artifact %s is not back in libraryArtifacts() -- restore must re-admit "+
			"the row to the `archived != true` default read.\n  saw: %s", artifactId, afterRestore)
	}
	if !strings.Contains(afterRestore, "notes.md") {
		t.Fatalf("the restored artifact lost its title -- update{} is a read-merge and the archive/"+
			"restore cycle must destroy nothing.\n  saw: %s", afterRestore)
	}

	fileRes, err := eng.Execute(ctxA, fmt.Sprintf(`libraryFileById(fileId: "%s")`, fileId))
	if err != nil {
		t.Fatalf("libraryFileById after restore: %v", err)
	}
	fileBlob := resultBlob(t, fileRes)
	if !strings.Contains(fileBlob, `"archived":false`) {
		t.Fatalf("restoreLibraryFile did not un-archive the backing file row -- the pair's second "+
			"half, addressed by the canonical ref, must resolve to the same row the cascade "+
			"archived.\n  saw: %s", fileBlob)
	}
	if !strings.Contains(fileBlob, "notes.md") {
		t.Fatalf("the restored file lost its name across the archive/restore cycle.\n  saw: %s", fileBlob)
	}

	// The door swings both ways: archiving again after a restore hides the row
	// again, so archive -> restore -> archive is a clean round trip.
	runMutation(t, ctxA, eng, "archiveArtifact", map[string]any{"artifactId": artifactId})
	if blob := listBlob("after re-archive"); strings.Contains(blob, artifactId) {
		t.Fatalf("re-archiving the restored artifact left it in the default list.\n  saw: %s", blob)
	}
}

// TestRestoreFolderReappearsInLibraryFolders is the folder half: a folder
// archived out of libraryFolders() comes back with its name and its place in
// the tree.
func TestRestoreFolderReappearsInLibraryFolders(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)

	suffix := uniqueSuffix("library4844folder")
	userA := "user-a-" + suffix
	folderId := "fold-" + suffix
	ctxA := rowAuthzCallerCtx(userA)

	runMutation(t, ctxA, eng, "createLibraryFolder", map[string]any{
		"folderId": folderId,
		"name":     "Client work",
	})

	folders := func(label string) string {
		res, err := eng.Execute(ctxA, `libraryFolders()`)
		if err != nil {
			t.Fatalf("libraryFolders() %s: %v", label, err)
		}
		return resultBlob(t, res)
	}

	if blob := folders("before archive"); !strings.Contains(blob, folderId) {
		t.Fatalf("folder %s missing from libraryFolders() before any archive.\n  saw: %s", folderId, blob)
	}

	runMutation(t, ctxA, eng, "archiveLibraryFolder", map[string]any{"folderId": folderId})
	if blob := folders("after archive"); strings.Contains(blob, folderId) {
		t.Fatalf("archived folder %s still in libraryFolders().\n  saw: %s", folderId, blob)
	}

	runMutation(t, ctxA, eng, "restoreLibraryFolder", map[string]any{"folderId": folderId})
	afterRestore := folders("after restore")
	if !strings.Contains(afterRestore, folderId) {
		t.Fatalf("restored folder %s is not back in libraryFolders().\n  saw: %s", folderId, afterRestore)
	}
	if !strings.Contains(afterRestore, "Client work") {
		t.Fatalf("the restored folder lost its name -- update{} is a read-merge.\n  saw: %s", afterRestore)
	}
}

// TestMoveArtifactToFolderAcceptsAnExplicitBlankAsRoot pins the re-file to
// root the Bin's restore flow leans on (memql#4844): when the restored item's
// folder is still archived, the CLIENT re-files it to the root by calling
// moveArtifactToFolder with an EXPLICIT folderId of "". The mutation writes
// `folderId: args.folderId ?? ""`, and ?? is blank-coalescing -- so "" and
// absent both mean the root, which is exactly the wanted collapse here (no
// folder's id is ""). The 4781 suite covers the absent spelling; this pins the
// explicit one, because that is the argument a restore flow actually sends.
func TestMoveArtifactToFolderAcceptsAnExplicitBlankAsRoot(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)

	suffix := uniqueSuffix("library4844move")
	userA := "user-a-" + suffix
	ctxA := rowAuthzCallerCtx(userA)

	artifactId := runMutation(t, ctxA, eng, "createArtifact", map[string]any{
		"sourceConceptRef": "v1:notes:note:" + suffix,
		"ownerUserId":      userA,
		"lens":             "artifact",
		"kind":             "note",
		"source":           "user_created",
		"title":            "filed note",
		"folderId":         "fold-" + suffix,
	})
	// The engine canonicalizes the relationship-declared folderId on write
	// ("fold-x" -> "v1:library:folder:fold-x"), so the fixture check accepts
	// the canonical spelling -- what matters is that SOMETHING non-blank is
	// filed, or the blank-move assertion below proves nothing.
	if got, _ := latestPayload(t, ctx, db, "v1:library:artifact", artifactId)["folderId"].(string); !strings.HasSuffix(got, "fold-"+suffix) {
		t.Fatalf("fixture folderId = %q, want it to name fold-%s", got, suffix)
	}

	runMutation(t, ctxA, eng, "moveArtifactToFolder", map[string]any{
		"artifactId": artifactId,
		"folderId":   "",
	})

	payload := latestPayload(t, ctx, db, "v1:library:artifact", artifactId)
	if got, ok := payload["folderId"]; !ok || got != "" {
		t.Fatalf("after an explicit folderId:\"\" move, folderId = %v (present=%v), want the empty "+
			"string that renders at the root", got, ok)
	}
	// The read-merge must leave the rest of the row standing.
	if payload["title"] != "filed note" {
		t.Fatalf("the blank move lost the title -- update{} is a read-merge.\n  payload: %v", payload)
	}
}
