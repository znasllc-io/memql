package memql

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// The roaming desktop's engine half, against a real database (epic
// memql#4746). What only a database shows is that a desktop document
// survives the executor's write path and comes back through the read the
// shell actually calls.
//
// THE OTHER HALF -- does the text a real client sends PARSE -- is asserted in
// sdk/go/client/save_my_desktop_render_4746_test.go, and it is there rather
// than here because of a module boundary, not a preference. `sdk/go/client`
// belongs to the ROOT module; importing it from this leaf module drags the
// published root module in as an indirect dependency of all eighteen modules
// above it, which the module-boundaries lane refuses (and is right to). The
// lexer and parser live in component/language/parser, which both sides
// already depend on, so the parse question is answerable there with no engine
// at all -- and the hostile document is the same one.
//
// Green-by-skip warning: this file skips wherever no Postgres is reachable,
// so a plain `go test ./...` proves nothing here. To verify for real:
//
//	MEMQL_DATABASE_DSN=... MEMQL_REQUIRE_DB=1 go test -count=1 \
//	  -run TestDesktop ./component/memql/

// hostileDesktopDocument is a desktop document carrying every character
// class a person can actually put on their desk. THE POINT IS THE TEXT: a
// folder is named by its owner and a file is titled by whatever was
// uploaded, so quotes, backslashes, newlines, tabs and astral-plane
// characters are ordinary inputs on this path, not adversarial ones. The
// item ids are object KEYS, and `item-1` is not a bare identifier to the
// MemQL lexer -- an unquoted one lexes as `item`, `-`, `1`.
func hostileDesktopDocument() map[string]any {
	return map[string]any{
		"version": 1,
		"desks": []any{
			map[string]any{"id": "desk-1", "createdBy": "user"},
			map[string]any{"id": "desk-2", "createdBy": "auto"},
		},
		"activeDeskId": "desk-1",
		"surfaces": map[string]any{
			"desk-1": map[string]any{
				"items": map[string]any{
					"item-1": map[string]any{
						"kind": "folder", "id": "item-1",
						"name":     "Q3 \"final\"\\draft\n\ttaxes \u00e9\u00fc \U0001F5C2",
						"children": []any{},
					},
					"item-2": map[string]any{
						"kind": "file", "id": "item-2",
						"artifactId": "artifact-abc", "title": "notes: a/b \\ c \"quoted\"",
						"fileKind": "document", "source": "uploaded",
					},
				},
				"positions": map[string]any{
					"item-1": map[string]any{"col": 0, "row": 0},
					"item-2": map[string]any{"col": 3, "row": 1},
				},
			},
		},
		"dock":      map[string]any{"pinned": []any{"settings", "fleet"}},
		"themePack": "graphite",
	}
}

// TestDesktopDocumentSurvivesTheWritePath stores a desktop carrying every
// character class a person can actually put on their desk and reads it back.
// The engine is supposed to be OPAQUE to this payload; what this proves is
// that it really is -- through validation, the read-merge, JSONB and the
// shape projection.
func TestDesktopDocumentSurvivesTheWritePath(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)

	owner := "u-desktop-" + uniqueSuffix("4746render")
	ctx := rowAuthzCallerCtx(owner)
	doc := hostileDesktopDocument()

	storedID := runMutation(t, ctx, eng, "saveMyDesktop", map[string]any{
		"revision": 7,
		"document": doc,
	})

	p := latestPayload(t, ctx, db, "v1:os:desktop", storedID)
	require.Equal(t, canonicalUserId(owner), p["ownerUserId"],
		"ownerUserId must be STAMPED from the actor -- @rowAuthz(owner=...) is worthless if it is not")
	require.EqualValues(t, 7, p["revision"])

	// The document comes back BYTE-EQUAL. Comparing decoded JSON rather than
	// field-by-field is deliberate: the engine is supposed to be opaque to
	// this payload, and a spot-check of two fields would not notice it
	// dropping a third.
	wantJSON, err := json.Marshal(doc)
	require.NoError(t, err)
	gotJSON, err := json.Marshal(p["document"])
	require.NoError(t, err)
	require.JSONEq(t, string(wantJSON), string(gotJSON),
		"the stored document must equal the one the client sent, character for character")
}

// TestSaveMyDesktopIsOneRowPerPerson pins the reason the id is derived
// rather than supplied. Two saves by one person land on ONE row; a second
// person's save lands on a different one. Without the derivation this is
// the create-or-update dance routingPolicy has to do, and two tabs racing
// their first save mint two desktops with nothing to choose between them.
func TestSaveMyDesktopIsOneRowPerPerson(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)

	suffix := uniqueSuffix("4746oneRow")
	alice := rowAuthzCallerCtx("u-desktop-alice-" + suffix)
	bob := rowAuthzCallerCtx("u-desktop-bob-" + suffix)

	first := saveDesktop(t, alice, eng, 1, map[string]any{"version": 1, "themePack": "graphite"})
	second := saveDesktop(t, alice, eng, 2, map[string]any{"version": 1, "themePack": "midnight"})
	require.Equal(t, first, second,
		"a person's second save must overwrite their first -- insert{} at a derived id is create-or-upsert")

	other := saveDesktop(t, bob, eng, 1, map[string]any{"version": 1, "themePack": "graphite"})
	require.NotEqual(t, first, other, "two people must not share a desktop row")

	// The overwrite is a REPLACE of the document, not a merge of it. The
	// engine read-merges a partial payload onto the stored one, and that
	// merge is SHALLOW -- so `document` as a whole is replaced and a desk
	// the person deleted stays deleted. A deep merge here would resurrect
	// every item ever placed.
	p := latestPayload(t, alice, db, "v1:os:desktop", first)
	doc, ok := p["document"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "midnight", doc["themePack"], "the newest save's document must win whole")
	require.EqualValues(t, 2, p["revision"])
}

// TestMyDesktopReadsOnlyTheCallersRow is the owner tier, exercised through
// the read the shell calls rather than asserted off the annotation. The
// concept declares the PLAIN owner tier (no clusterOwner), so there is
// deliberately no operator counterpart to test.
func TestMyDesktopReadsOnlyTheCallersRow(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)

	suffix := uniqueSuffix("4746owner")
	alice := rowAuthzCallerCtx("u-desktop-a-" + suffix)
	bob := rowAuthzCallerCtx("u-desktop-b-" + suffix)

	aliceID := saveDesktop(t, alice, eng, 1, map[string]any{"version": 1, "themePack": "alice-only"})
	bobID := saveDesktop(t, bob, eng, 1, map[string]any{"version": 1, "themePack": "bob-only"})

	res, err := eng.Execute(alice, "query myDesktop()")
	require.NoError(t, err)
	blob := resultBlob(t, res)
	require.Contains(t, blob, aliceID, "a person must read their own desktop")
	require.NotContains(t, blob, bobID, "a person must not read anybody else's desktop")
	// The projection has to carry `revision`, or the shell's last-writer-wins
	// arithmetic starts from nothing and every save stamps 1.
	require.Contains(t, blob, "revision", "desktopDocument must project revision")

	res, err = eng.Execute(bob, "query myDesktop()")
	require.NoError(t, err)
	blob = resultBlob(t, res)
	require.Contains(t, blob, bobID)
	require.NotContains(t, blob, aliceID)
}

// saveDesktop runs saveMyDesktop for one caller and returns the stored row id.
func saveDesktop(t *testing.T, ctx context.Context, eng *MemQLEngine, revision int, doc map[string]any) string {
	t.Helper()
	return runMutation(t, ctx, eng, "saveMyDesktop", map[string]any{
		"revision": revision,
		"document": doc,
	})
}
