package memql

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// library_upload_sessions_4782_db_test.go -- the chunked-upload session row
// against a real store (memql#4782, design C2).
//
// Postgres-gated like its 4340/4781 siblings. Three claims only the engine
// can answer:
//
//  1. The @serverOnly pair REFUSES a client-origin caller -- the annotation
//     is enforcement, not documentation. This is the negative control for
//     component/server/uploadsession's internal-origin stamp: strip the
//     stamp and its writes fail exactly like this.
//  2. Under internal origin WITH the caller's actor, the session lands
//     owned by that caller and reads back through uploadSessionById; the
//     complete write flips status while the read-merge keeps every
//     init-time fact.
//  3. A second user sees nothing, named read and raw path alike; and the
//     quota read openUploadSessionsForOwner counts exactly the OPEN
//     sessions of the caller alone.

func sessionArgs(uploadId, fileId, owner string, size int) map[string]any {
	return map[string]any{
		"uploadId":  uploadId,
		"name":      "big.mp4",
		"size":      size,
		"mimeType":  "video/mp4",
		"blobPath":  "library/" + owner + "/" + fileId + "/big.mp4",
		"fileId":    fileId,
		"chunkSize": 16 << 20,
	}
}

func TestUploadSessionWritesAreServerOnlyAndOwned(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)

	suffix := uniqueSuffix("library4782sess")
	userA := "user-a-" + suffix
	userB := "user-b-" + suffix
	uploadId := "upsess-" + suffix
	fileId := "libfile-" + suffix

	clientCtxA := rowAuthzCallerCtx(userA)
	internalCtxA := auth.ContextWithInternalOrigin(clientCtxA)
	clientCtxB := rowAuthzCallerCtx(userB)

	// --- claim 1: client origin is refused ---
	q, err := renderMutationCall(t, "createUploadSession", sessionArgs(uploadId, fileId, userA, 1000))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if _, err := eng.Execute(clientCtxA, q); err == nil {
		t.Fatalf("createUploadSession succeeded on a CLIENT-origin context -- @serverOnly is not " +
			"being enforced, and every client of the generated SDK can mint session rows with " +
			"arbitrary blobPaths")
	}

	// --- claim 2: internal origin + the caller's actor lands the row ---
	if _, err := eng.Execute(internalCtxA, q); err != nil {
		t.Fatalf("createUploadSession under internal origin: %v", err)
	}
	res, err := eng.Execute(clientCtxA, fmt.Sprintf(`uploadSessionById(uploadId: "%s")`, uploadId))
	if err != nil {
		t.Fatalf("uploadSessionById: %v", err)
	}
	blob := resultBlob(t, res)
	for _, want := range []string{uploadId, fileId, `"status":"open"`, "big.mp4"} {
		if !strings.Contains(blob, want) {
			t.Fatalf("the owner cannot read their session's %q back -- ownerUserId must stamp from "+
				"the ACTOR riding beside the internal origin.\n  saw: %s", want, blob)
		}
	}

	// --- claim 3: B sees nothing; the quota read counts A's open session ---
	for name, read := range map[string]string{
		"named": fmt.Sprintf(`uploadSessionById(uploadId: "%s")`, uploadId),
		"raw":   `concept=="v1:library:uploadSession"`,
	} {
		resB, err := eng.Execute(clientCtxB, read)
		if err != nil {
			t.Logf("B reading %s refused: %v", name, err)
			continue
		}
		if blobB := resultBlob(t, resB); strings.Contains(blobB, uploadId) {
			t.Fatalf("caller B was returned caller A's upload session through %s.\n  B saw: %s", name, blobB)
		}
	}
	open, err := eng.Execute(clientCtxA, `openUploadSessionsForOwner()`)
	if err != nil {
		t.Fatalf("openUploadSessionsForOwner: %v", err)
	}
	if blob := resultBlob(t, open); !strings.Contains(blob, uploadId) {
		t.Fatalf("the open-sessions quota read does not carry the open session -- init-time quota "+
			"enforcement counts these, and an invisible one fails the quota open.\n  saw: %s", blob)
	}

	// --- complete: @serverOnly too, flips status, keeps every fact ---
	completeQ, err := renderMutationCall(t, "completeUploadSession", map[string]any{"uploadId": uploadId})
	if err != nil {
		t.Fatalf("render complete: %v", err)
	}
	if _, err := eng.Execute(clientCtxA, completeQ); err == nil {
		t.Fatalf("completeUploadSession succeeded on a CLIENT-origin context")
	}
	if _, err := eng.Execute(internalCtxA, completeQ); err != nil {
		t.Fatalf("completeUploadSession under internal origin: %v", err)
	}
	done, err := eng.Execute(clientCtxA, fmt.Sprintf(`uploadSessionById(uploadId: "%s")`, uploadId))
	if err != nil {
		t.Fatalf("uploadSessionById after complete: %v", err)
	}
	doneBlob := resultBlob(t, done)
	if !strings.Contains(doneBlob, `"status":"completed"`) {
		t.Fatalf("complete did not flip status.\n  saw: %s", doneBlob)
	}
	if !strings.Contains(doneBlob, "big.mp4") || !strings.Contains(doneBlob, fileId) {
		t.Fatalf("complete lost init-time facts -- update{} is a read-merge and the minimal argument "+
			"list must leave every other field standing.\n  saw: %s", doneBlob)
	}
	// And the completed session leaves the open-sessions quota read.
	openAfter, err := eng.Execute(clientCtxA, `openUploadSessionsForOwner()`)
	if err != nil {
		t.Fatalf("openUploadSessionsForOwner after complete: %v", err)
	}
	if blob := resultBlob(t, openAfter); strings.Contains(blob, uploadId) {
		t.Fatalf("a COMPLETED session still counts as open -- the quota would double-charge every "+
			"finished upload forever.\n  saw: %s", blob)
	}
}

// renderMutationCall renders `mutation name(k: v, ...)` with JSON-encoded
// values, matching runMutation's rendering -- restated because runMutation
// require's success, and half this file's point is watching a call FAIL.
func renderMutationCall(t *testing.T, name string, args map[string]any) (string, error) {
	t.Helper()
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(args))
	for _, k := range keys {
		vb, err := json.Marshal(args[k])
		if err != nil {
			return "", err
		}
		parts = append(parts, k+": "+string(vb))
	}
	return fmt.Sprintf("mutation %s(%s)", name, strings.Join(parts, ", ")), nil
}
