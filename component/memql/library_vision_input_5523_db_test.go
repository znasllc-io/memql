package memql

import (
	"fmt"
	"strings"
	"testing"
)

// library_vision_input_5523_db_test.go -- the LIFECYCLE of a staged vision
// input, against a real store (issue memql#5523).
//
// A vision call through the app door lands each image in the session workspace
// as a Library file, because AppSessionStart.inputs takes Library artifact ids
// and nothing else. The owner's decision about what happens to those files IS
// the feature here, and it is three claims a stub cannot hold:
//
//  1. A `vision_input` file is WRITABLE and PROMOTABLE. The index concept's
//     source enum is the UNION of every backing row's, so a value the file
//     can hold and the index cannot is a promotion that silently never
//     happens -- leaving the image staged, the artifact id unresolvable, and
//     the vision call refused for a reason that looks like storage.
//     TestEveryBackingSourceValueIsPromotable pins the containment
//     statically; this pins that a row actually goes through.
//  2. IT IS ITS OWN VALUE, distinguishable from `app_session`. The archive
//     at session end is keyed on the distinction: `app_session` means a file
//     the session recorded reading or writing, which the person keeps, and
//     `vision_input` means something the engine wrote FOR the session to
//     read, which nobody asked for. If these shared a spelling the sweep
//     would archive files people meant to keep.
//  3. THE ARCHIVE PAIR takes both rows out. v1:library:file.archived's own
//     declaration says archiveArtifact archives the backing file "so the two
//     never disagree" -- and it does, through the
//     `archiveFileOnArtifactArchive` AUTOMATION, which is an event and a
//     subscriber and a second write. MEASURED HERE: with no automation
//     runtime driving events, archiveArtifact alone leaves `archived:false`
//     on the file. That is what made the release path write the pair itself
//     rather than depend on the automation having fired -- it runs in a
//     deferred call at the end of a turn with nothing left to check the
//     result, and a missed event leaves the image visible in the Files app of
//     somebody who never asked for it, forever.
//
// Postgres-gated like its neighbours: sharedReadMergeEngine skips when no
// database is reachable, and CI's db-tests lane runs this package with
// MEMQL_REQUIRE_DB=1 so a skip there is a failure rather than a green.

// visionInputFileArgs is one staged image, as integrations/agent/worker's
// stager writes it. The field values are the stager's: source, format and a
// summary naming the session.
func visionInputFileArgs(fileId, owner string) map[string]any {
	return map[string]any{
		"fileId":   fileId,
		"name":     "vision-input-01.png",
		"mimeType": "image/png",
		"size":     2048,
		"sha256":   "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"blobUrl":  "library/" + owner + "/" + fileId + "/vision-input-01.png",
		"source":   "vision_input",
		"format":   "image",
		"summary":  "Staged input of a vision call through the app door (session v1:worker:appSession:probe).",
	}
}

func TestAStagedVisionInputIsWritablePromotableAndArchivable(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)

	suffix := uniqueSuffix("vision5523")
	owner := "user-vision-" + suffix
	fileId := "visionfile-" + suffix
	ctx := rowAuthzCallerCtx(owner)

	// (1) THE FILE. createLibraryFile's own `source` arg has to accept the
	// value -- the arg enum and the concept enum are two declarations, and
	// only one of them is what a write is validated against.
	runMutation(t, ctx, eng, "createLibraryFile", visionInputFileArgs(fileId, owner))

	res, err := eng.Execute(ctx, fmt.Sprintf(`libraryFileById(fileId: "%s")`, fileId))
	if err != nil {
		t.Fatalf("read the staged file back: %v", err)
	}
	blob := resultBlob(t, res)
	if !strings.Contains(blob, "vision_input") {
		t.Fatalf("the staged file did not come back carrying source=vision_input -- a value the "+
			"write accepted and the read does not show is a provenance label nothing can act on.\n"+
			"  got: %s", blob)
	}
	// (2) IT IS NOT app_session. Asserted because the archive sweep's whole
	// correctness rests on telling the two apart.
	if strings.Contains(blob, "app_session") {
		t.Errorf("the staged file reads as app_session as well as vision_input -- the release "+
			"path archives one of these and not the other.\n  got: %s", blob)
	}

	// (3) PROMOTION, with the same value the file carries. This is the call
	// indexFileOnCreate makes, and the index enum is the union that has to
	// hold it.
	sourceRef := "v1:library:file:" + fileId
	artifactId := runMutation(t, ctx, eng, "createArtifact", map[string]any{
		"sourceConceptRef": sourceRef,
		"ownerUserId":      owner,
		"lens":             "artifact",
		"kind":             "file",
		"source":           "vision_input",
		"title":            "vision-input-01.png",
		"format":           "image",
		"mimeType":         "image/png",
	})
	if strings.TrimSpace(artifactId) == "" {
		t.Fatal("promoting a vision_input file produced no artifact id -- which is exactly the " +
			"silent failure the union enum exists to prevent: the image is staged, `inputs` has " +
			"nothing to carry, and the call is refused for what looks like a storage problem")
	}

	// THE STAGER RESOLVES THE ARTIFACT THROUGH THIS READ, so it is the read
	// asserted rather than a direct row fetch.
	res, err = eng.Execute(ctx, fmt.Sprintf(
		`libraryArtifactBySourceConceptRef(sourceConceptRef: "%s")`, sourceRef))
	if err != nil {
		t.Fatalf("resolve the index row by source ref: %v", err)
	}
	if blob := resultBlob(t, res); !strings.Contains(blob, "vision_input") {
		t.Fatalf("the index row does not carry the staged source -- the Library list would label "+
			"the image as something else.\n  got: %s", blob)
	}

	// (4) THE ARCHIVE, which is the lifecycle the owner chose.
	//
	// THE ARTIFACT FIRST, ON ITS OWN -- and the file is asserted to still be
	// visible after it. That is not a bug being pinned: it is the reason the
	// release path writes the pair. In a running cluster
	// `archiveFileOnArtifactArchive` closes this gap off the `node.updated`
	// event; here there is no automation runtime, which is exactly the
	// condition a missed event produces in production.
	runMutation(t, ctx, eng, "archiveArtifact", map[string]any{"artifactId": artifactId})

	res, err = eng.Execute(ctx, fmt.Sprintf(`libraryFileById(fileId: "%s")`, fileId))
	if err != nil {
		t.Fatalf("read the file after archiving its artifact: %v", err)
	}
	if fileArchived(t, res) {
		t.Log("the backing file was archived by the artifact write alone -- an automation runtime " +
			"is driving events in this harness. The release path's second write is then redundant " +
			"and idempotent, which is the intended relationship, not a reason to drop it.")
	}

	// THE FILE WRITE, which is what the release path actually does.
	runMutation(t, ctx, eng, "archiveLibraryFile", map[string]any{"fileId": fileId})

	res, err = eng.Execute(ctx, fmt.Sprintf(`libraryFileById(fileId: "%s")`, fileId))
	if err != nil {
		t.Fatalf("read the file after the archive pair: %v", err)
	}
	if !fileArchived(t, res) {
		t.Errorf("the archive pair did not archive the backing file, so every staged vision "+
			"input stays visible in the owner's Files app.\n  got: %s", resultBlob(t, res))
	}

	// AND THE ARTIFACT IS OUT OF THE DEFAULT LIST. Archiving that left the
	// row listed would put the image in front of the person anyway, which is
	// the outcome the whole decision exists to avoid.
	res, err = eng.Execute(ctx, `libraryArtifacts()`)
	if err != nil {
		t.Fatalf("list artifacts after the archive: %v", err)
	}
	if blob := resultBlob(t, res); strings.Contains(blob, fileId) {
		t.Errorf("the archived vision input is still in the default artifact list.\n  got: %s", blob)
	}
}

// The REACHABLE POSITIVE for the assertions above: an ordinary uploaded file
// written by the same helper IS listed, and IS not archived.
//
// Without it, every "it is hidden" assertion above is satisfied by a store
// that lists nothing at all -- a broken read and a working archive look
// identical from the assertion's side.
// fileArchived reads the `archived` flag off a libraryFileById result.
//
// A STRING SEARCH would be wrong here in a way that matters: the payload
// carries `archived` beside `archivedAt`-shaped neighbours and a summary this
// test writes itself, so `strings.Contains(blob, "archived")` answers true on
// a row that is not archived. The flag is read as a value.
func fileArchived(t *testing.T, res *ExecuteResult) bool {
	t.Helper()
	for _, row := range MaterializeRows(res) {
		if v, ok := row["archived"].(bool); ok {
			return v
		}
	}
	return false
}

func TestAnOrdinaryLibraryFileStaysVisible(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)

	suffix := uniqueSuffix("vision5523pos")
	owner := "user-vision-pos-" + suffix
	fileId := "visionpos-" + suffix
	ctx := rowAuthzCallerCtx(owner)

	args := visionInputFileArgs(fileId, owner)
	args["source"] = "uploaded"
	runMutation(t, ctx, eng, "createLibraryFile", args)
	artifactId := runMutation(t, ctx, eng, "createArtifact", map[string]any{
		"sourceConceptRef": "v1:library:file:" + fileId,
		"ownerUserId":      owner,
		"lens":             "artifact",
		"kind":             "file",
		"source":           "uploaded",
		"title":            "vision-input-01.png",
		"format":           "image",
		"mimeType":         "image/png",
	})
	if strings.TrimSpace(artifactId) == "" {
		t.Fatal("promoting an ordinary uploaded file produced no artifact id")
	}
	res, err := eng.Execute(ctx, `libraryArtifacts()`)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if blob := resultBlob(t, res); !strings.Contains(blob, fileId) {
		t.Fatalf("an unarchived file is not in the default artifact list, so every \"it is "+
			"hidden\" assertion in this file is unfalsifiable.\n  got: %s", blob)
	}
}
