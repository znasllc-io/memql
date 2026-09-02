package memql

import (
	"fmt"
	"strings"
	"testing"
)

// library_folder_delete_db_test.go -- the empty-folder disposition, end to end
// against a real store.
//
// Archiving a folder that held no file anywhere beneath it used to leave a row
// in the Archive place and in the Bin. Archiving exists so a person can get
// something back, and an empty folder tree has nothing in it to get back, so
// that row answered no question anybody asked while sitting among the files
// that genuinely were waiting there. Those folders take `deleted` instead.
//
// The claim this pins is the one the client-side walk cannot check, because
// the walk only decides WHICH mutation to call: that `deleted` actually
// removes the folder from EVERY read at once -- the tree, the Bin, and the
// by-id resolve -- and that restoreLibraryFolder does not bring it back.
//
// The by-id read is the one worth stating out loud. It deliberately answers
// for an ARCHIVED folder (a caller naming a specific id deserves the honest
// answer, and the archived field says which kind of row it got), so it is the
// obvious place for a deleted one to reappear -- a stale desk shortcut or a
// breadcrumb resolving to a folder no other surface can show.
func TestDeletedFolderLeavesEveryFolderRead(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)

	suffix := uniqueSuffix("libraryfolddel")
	userA := "user-a-" + suffix
	keptFolder := "fold-kept-" + suffix
	// TWO deleted folders, because the two dispositions are independent fields
	// and one folder cannot make all three reads mean something. A folder that
	// only ever took `deleted` fails `archived == true` on its own, so it would
	// clear the Bin assertion for the wrong reason; a folder that took both
	// fails `archived != true` on its own, so it would clear the TREE assertion
	// for the wrong reason.
	//
	// The archived-then-deleted one is also the shape a real cluster already
	// holds: every empty folder somebody archived before this rule existed
	// carries `archived` and is sitting in the Bin right now.
	plainFolder := "fold-plain-" + suffix
	binnedFolder := "fold-binned-" + suffix
	ctxA := rowAuthzCallerCtx(userA)

	for id, name := range map[string]string{
		keptFolder:   "Client videos",
		plainFolder:  "Nothing here",
		binnedFolder: "Nothing here either",
	} {
		runMutation(t, ctxA, eng, "createLibraryFolder", map[string]any{"folderId": id, "name": name})
	}

	read := func(label, q string) string {
		res, err := eng.Execute(ctxA, q)
		if err != nil {
			t.Fatalf("%s (%s): %v", label, q, err)
		}
		return resultBlob(t, res)
	}

	before := read("tree before the delete", `libraryFolders()`)
	for _, id := range []string{plainFolder, binnedFolder} {
		if !strings.Contains(before, id) {
			t.Fatalf("folder %s is missing from libraryFolders() before anything happened to it -- "+
				"the rest of this test would pass vacuously.\n  saw: %s", id, before)
		}
	}

	runMutation(t, ctxA, eng, "archiveLibraryFolder", map[string]any{"folderId": binnedFolder})
	if blob := read("bin before the delete", `libraryArchivedFolders()`); !strings.Contains(blob, binnedFolder) {
		t.Fatalf("the archived folder never reached the Bin, so the delete below would clear the "+
			"Bin assertion without proving anything.\n  saw: %s", blob)
	}

	for _, id := range []string{plainFolder, binnedFolder} {
		runMutation(t, ctxA, eng, "deleteLibraryFolder", map[string]any{"folderId": id})
	}

	// The tree, the Bin and the by-id resolve. All three carry isNotDeleted,
	// and each is checked against the folder whose OTHER field does not
	// already exclude it.
	if blob := read("tree", `libraryFolders()`); strings.Contains(blob, plainFolder) {
		t.Fatalf("the deleted folder is still in libraryFolders().\n  saw: %s", blob)
	} else if !strings.Contains(blob, keptFolder) {
		t.Fatalf("deleting two folders took the sibling with them.\n  saw: %s", blob)
	}
	if blob := read("bin", `libraryArchivedFolders()`); strings.Contains(blob, binnedFolder) {
		t.Fatalf("the deleted folder is still in the Bin -- libraryArchivedFolders() carries "+
			"isNotDeleted precisely so an empty folder never sits among the things somebody "+
			"can restore.\n  saw: %s", blob)
	}
	for _, id := range []string{plainFolder, binnedFolder} {
		byId := read("by id", fmt.Sprintf(`libraryFolderById(folderId: "%s")`, id))
		if strings.Contains(byId, id) {
			t.Fatalf("deleted folder %s is still reachable by id. That read answers for an ARCHIVED "+
				"folder on purpose, which is exactly why it needs the deleted filter of its own: it "+
				"is where a stale desk shortcut would resurrect one.\n  saw: %s", id, byId)
		}
	}

	// Restore is not an inverse. It clears `archived` alone, and the folder
	// reads exclude these rows on the other field, so nothing brings them back.
	for _, id := range []string{plainFolder, binnedFolder} {
		runMutation(t, ctxA, eng, "restoreLibraryFolder", map[string]any{"folderId": id})
	}
	after := read("tree after restore", `libraryFolders()`)
	for _, id := range []string{plainFolder, binnedFolder} {
		if strings.Contains(after, id) {
			t.Fatalf("restoreLibraryFolder resurrected DELETED folder %s. It clears `archived` and "+
				"deliberately never touches `deleted` -- resurrecting an empty folder restores the "+
				"noise the disposition exists to remove.\n  saw: %s", id, after)
		}
	}
}

// TestArchivedFolderStillReachesTheBin is the control, and it is the half that
// would fail if somebody "simplified" the two dispositions into one field.
//
// The two are independent on purpose: an archived folder is somewhere a person
// can still go and look, a deleted one is gone from every surface. A test that
// only checked the delete would pass just as happily against a change that
// dropped every folder out of the Bin.
func TestArchivedFolderStillReachesTheBin(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)

	suffix := uniqueSuffix("libraryfoldarch")
	userA := "user-a-" + suffix
	folderId := "fold-" + suffix
	ctxA := rowAuthzCallerCtx(userA)

	runMutation(t, ctxA, eng, "createLibraryFolder", map[string]any{
		"folderId": folderId,
		"name":     "Client videos",
	})
	runMutation(t, ctxA, eng, "archiveLibraryFolder", map[string]any{"folderId": folderId})

	res, err := eng.Execute(ctxA, `libraryArchivedFolders()`)
	if err != nil {
		t.Fatalf("libraryArchivedFolders(): %v", err)
	}
	if blob := resultBlob(t, res); !strings.Contains(blob, folderId) {
		t.Fatalf("an archived folder is missing from the Bin. Adding isNotDeleted to that read "+
			"must not exclude rows that carry no `deleted` key at all -- which is every folder "+
			"written before the field existed, and the reason the trait spells it `!= true`.\n"+
			"  saw: %s", blob)
	}
}
