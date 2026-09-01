package memql

import (
	"fmt"
	"strings"
	"testing"
)

// library_folders_4781_db_test.go -- the Files app's folder tree and upload
// provenance, end to end against a real store (memql#4781).
//
// Postgres-gated like library_file_4340_db_test.go, and shaped by the same
// philosophy: this engine runs no automation scheduler, so what is asserted
// for promotion is createArtifact CALLED THE WAY indexFileOnCreate CALLS IT
// -- same argument names, same coalesced blanks -- rather than the trigger
// firing, which strict boot and the automations suite own.
//
// Four claims:
//
//  1. The folder lifecycle is owned end to end: create / rename / move /
//     archive land under the acting user, a second user sees nothing, the
//     default tree read hides archived folders, and the by-id read answers
//     for an archived one (a caller asking about a specific id deserves the
//     honest answer).
//  2. moveArtifactToFolder is a READ-MERGE: labels and archived survive a
//     re-filing untouched, and an absent target files the row at root.
//  3. Promotion forwards the filing and the verified machine provenance:
//     createArtifact accepts folderId / producedByWorkerId /
//     producedByWorkerName and artifactFull projects all three -- a concept
//     field is not a readable field until a shape says so.
//  4. createLibraryFile accepts the filing + provenance quartet and
//     libraryFileFull projects it, uploadedFromPath included (the sync
//     epic reads the path off the FILE row, so the projection is
//     load-bearing, not cosmetic).

func TestLibraryFolderLifecycleIsOwned(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)

	suffix := uniqueSuffix("library4781fold")
	userA := "user-a-" + suffix
	userB := "user-b-" + suffix
	rootFolder := "fold-root-" + suffix
	childFolder := "fold-child-" + suffix
	ctxA := rowAuthzCallerCtx(userA)
	ctxB := rowAuthzCallerCtx(userB)

	// Create: ownerUserId is stamped from the actor, never an argument.
	runMutation(t, ctxA, eng, "createLibraryFolder", map[string]any{
		"folderId": rootFolder,
		"name":     "Client videos",
	})
	runMutation(t, ctxA, eng, "createLibraryFolder", map[string]any{
		"folderId":       childFolder,
		"name":           "Raw takes",
		"parentFolderId": rootFolder,
	})

	// A's tree read carries both, with the nesting intact.
	res, err := eng.Execute(ctxA, `libraryFolders()`)
	if err != nil {
		t.Fatalf("A reading libraryFolders(): %v", err)
	}
	blob := resultBlob(t, res)
	for _, want := range []string{rootFolder, childFolder, "Client videos", "Raw takes"} {
		if !strings.Contains(blob, want) {
			t.Fatalf("libraryFolders() is missing %q.\n  saw: %s", want, blob)
		}
	}
	if !strings.Contains(blob, fmt.Sprintf(`"parentFolderId":"%s"`, rootFolder)) &&
		!strings.Contains(blob, fmt.Sprintf(`"parentFolderId":"v1:library:folder:%s"`, rootFolder)) {
		t.Fatalf("the child folder's parentFolderId did not survive the write -- the tree fold "+
			"has nothing to nest on.\n  saw: %s", blob)
	}

	// B sees nothing, through the named read and the raw path.
	for name, q := range map[string]string{
		"named": `libraryFolders()`,
		"raw":   `concept=="v1:library:folder"`,
	} {
		resB, err := eng.Execute(ctxB, q)
		if err != nil {
			t.Logf("B reading %s refused: %v", name, err)
			continue
		}
		if blobB := resultBlob(t, resB); strings.Contains(blobB, rootFolder) {
			t.Fatalf("caller B was returned caller A's folder through %s -- v1:library:folder "+
				"declares @rowAuthz(owner=\"ownerUserId\", clusterOwner) and B is neither.\n  B saw: %s",
				name, blobB)
		}
	}

	// Rename touches one field of one row.
	runMutation(t, ctxA, eng, "renameLibraryFolder", map[string]any{
		"folderId": childFolder,
		"name":     "Selects",
	})
	// Move re-parents to root via the ABSENT argument -- the spelling the
	// OS uses, so it is the spelling this test uses.
	runMutation(t, ctxA, eng, "moveLibraryFolder", map[string]any{
		"folderId": childFolder,
	})
	moved, err := eng.Execute(ctxA, fmt.Sprintf(`libraryFolderById(folderId: "%s")`, childFolder))
	if err != nil {
		t.Fatalf("libraryFolderById after rename+move: %v", err)
	}
	movedBlob := resultBlob(t, moved)
	if !strings.Contains(movedBlob, "Selects") {
		t.Fatalf("rename did not land.\n  saw: %s", movedBlob)
	}
	if strings.Contains(movedBlob, rootFolder) {
		t.Fatalf("move-to-root left the old parent standing -- parentFolderId must be blanked, "+
			"not merely absent from the call (the ?? \"\" spelling writes it).\n  saw: %s", movedBlob)
	}

	// Archive hides it from the tree read and keeps it reachable by id.
	runMutation(t, ctxA, eng, "archiveLibraryFolder", map[string]any{"folderId": childFolder})
	tree, err := eng.Execute(ctxA, `libraryFolders()`)
	if err != nil {
		t.Fatalf("libraryFolders() after archive: %v", err)
	}
	treeBlob := resultBlob(t, tree)
	if strings.Contains(treeBlob, childFolder) {
		t.Fatalf("the archived folder is still in libraryFolders() -- `archived != true` is what "+
			"makes archiveLibraryFolder a soft delete.\n  saw: %s", treeBlob)
	}
	if !strings.Contains(treeBlob, rootFolder) {
		t.Fatalf("archiving the child hid the parent too.\n  saw: %s", treeBlob)
	}
	byId, err := eng.Execute(ctxA, fmt.Sprintf(`libraryFolderById(folderId: "%s")`, childFolder))
	if err != nil {
		t.Fatalf("libraryFolderById on the archived folder: %v", err)
	}
	if blob := resultBlob(t, byId); !strings.Contains(blob, childFolder) {
		t.Fatalf("the archived folder is unreachable by id -- archive is soft, and the by-id read "+
			"answers honestly with the archived field to say which kind of row it got.\n  saw: %s", blob)
	}
}

func TestMoveArtifactToFolderPreservesLabelsAndArchived(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)

	suffix := uniqueSuffix("library4781move")
	userA := "user-a-" + suffix
	folderA := "fold-a-" + suffix
	folderB := "fold-b-" + suffix
	fileId := "libfile-" + suffix
	ctxA := rowAuthzCallerCtx(userA)

	for id, name := range map[string]string{folderA: "Inbox", folderB: "Reports"} {
		runMutation(t, ctxA, eng, "createLibraryFolder", map[string]any{"folderId": id, "name": name})
	}

	// Promote an artifact carrying labels and an initial filing -- the state
	// a real row is in when somebody moves it.
	sourceRef := "v1:library:file:" + fileId
	artifactId := runMutation(t, ctxA, eng, "createArtifact", map[string]any{
		"sourceConceptRef": sourceRef,
		"ownerUserId":      userA,
		"lens":             "artifact",
		"kind":             "file",
		"source":           "uploaded",
		"title":            "q3.pdf",
		"format":           "pdf",
		"mimeType":         "application/pdf",
		"labels":           []string{"finance", "q3"},
		"folderId":         folderA,
	})

	runMutation(t, ctxA, eng, "moveArtifactToFolder", map[string]any{
		"artifactId": artifactId,
		"folderId":   folderB,
	})

	res, err := eng.Execute(ctxA, fmt.Sprintf(`libraryArtifactById(artifactId: "%s")`, artifactId))
	if err != nil {
		t.Fatalf("libraryArtifactById after move: %v", err)
	}
	blob := resultBlob(t, res)
	if !strings.Contains(blob, folderB) || strings.Contains(blob, folderA) {
		t.Fatalf("the move did not re-file the artifact from %s to %s.\n  saw: %s", folderA, folderB, blob)
	}
	for _, label := range []string{"finance", "q3"} {
		if !strings.Contains(blob, label) {
			t.Fatalf("label %q did not survive moveArtifactToFolder -- update{} is a read-merge and a "+
				"move must disturb nothing but the filing (issue #4781 acceptance).\n  saw: %s", label, blob)
		}
	}
	if strings.Contains(blob, `"archived":true`) {
		t.Fatalf("moveArtifactToFolder archived the row as a side effect.\n  saw: %s", blob)
	}

	// Move to root: the absent-argument spelling the OS uses.
	runMutation(t, ctxA, eng, "moveArtifactToFolder", map[string]any{"artifactId": artifactId})
	rooted, err := eng.Execute(ctxA, fmt.Sprintf(`libraryArtifactById(artifactId: "%s")`, artifactId))
	if err != nil {
		t.Fatalf("libraryArtifactById after move-to-root: %v", err)
	}
	if blob := resultBlob(t, rooted); strings.Contains(blob, folderB) {
		t.Fatalf("move-to-root left the artifact filed under %s.\n  saw: %s", folderB, blob)
	}
}

func TestPromotionForwardsFolderAndProvenance(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)

	suffix := uniqueSuffix("library4781prov")
	userA := "user-a-" + suffix
	fileId := "libfile-" + suffix
	folderId := "fold-" + suffix
	workerId := "wrk-" + suffix
	ctxA := rowAuthzCallerCtx(userA)

	// createArtifact, called the way indexFileOnCreate's step calls it after
	// memql#4781: filing plus the verified machine fields, blanks coalesced.
	artifactId := runMutation(t, ctxA, eng, "createArtifact", map[string]any{
		"sourceConceptRef":     "v1:library:file:" + fileId,
		"ownerUserId":          userA,
		"lens":                 "artifact",
		"kind":                 "file",
		"source":               "uploaded",
		"title":                "q3.pdf",
		"format":               "pdf",
		"mimeType":             "application/pdf",
		"folderId":             folderId,
		"producedByWorkerId":   workerId,
		"producedByWorkerName": "MacBook-Pro",
	})

	res, err := eng.Execute(ctxA, fmt.Sprintf(`libraryArtifactById(artifactId: "%s")`, artifactId))
	if err != nil {
		t.Fatalf("libraryArtifactById: %v", err)
	}
	blob := resultBlob(t, res)
	for field, want := range map[string]string{
		"folderId":             folderId,
		"producedByWorkerId":   workerId,
		"producedByWorkerName": "MacBook-Pro",
	} {
		if !strings.Contains(blob, want) {
			t.Fatalf("artifactFull does not carry %s=%q after promotion -- a concept field is not a "+
				"readable field until the shape projects it, and the Files app inspector reads this "+
				"one.\n  saw: %s", field, want, blob)
		}
	}
}

func TestLibraryFileCarriesFilingAndProvenance(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)

	suffix := uniqueSuffix("library4781file")
	userA := "user-a-" + suffix
	fileId := "libfile-" + suffix
	ctxA := rowAuthzCallerCtx(userA)

	args := libraryFileArgs(fileId, userA)
	args["folderId"] = "fold-" + suffix
	args["uploadedFromWorkerId"] = "wrk-" + suffix
	args["uploadedFromWorkerName"] = "MacBook-Pro"
	args["uploadedFromPath"] = "/Users/a/Reports/notes.md"
	runMutation(t, ctxA, eng, "createLibraryFile", args)

	res, err := eng.Execute(ctxA, fmt.Sprintf(`libraryFileById(fileId: "%s")`, fileId))
	if err != nil {
		t.Fatalf("libraryFileById: %v", err)
	}
	blob := resultBlob(t, res)
	for _, want := range []string{
		"fold-" + suffix,
		"wrk-" + suffix,
		"MacBook-Pro",
		"/Users/a/Reports/notes.md",
	} {
		if !strings.Contains(blob, want) {
			t.Fatalf("libraryFileFull does not carry %q -- the inspector and the future sync epic "+
				"both read these off the FILE row, so the projection is load-bearing.\n  saw: %s",
				want, blob)
		}
	}
}
