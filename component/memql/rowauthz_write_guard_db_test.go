package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// Row-authz write enforcement, end to end against a real store
// (memql#3174).
//
// Postgres-gated like its neighbours -- readMergeTestEngine skips when
// no DB is reachable. CI's db-tests lane runs this package with
// MEMQL_REQUIRE_DB=1, so a skip there is a failure rather than a green.
//
// The claim that can only be made here: the write DOES NOT LAND. The
// unit tests in rowauthz_write_guard_test.go prove the guard answers
// correctly; only this one proves executeWrite consults it, that it
// consults it BEFORE the read-merge (which is what stops a mutation's
// `ownerUserId: actor.userId` re-stamp from deciding its own
// authorization), and that the victim's row is byte-for-byte untouched
// afterwards.

// rowAuthzOwnerRoleCtx is a cluster owner as the request path builds
// one: an AccessContext carrying RoleOwner (which is what
// actor.isClusterOwner resolves from) plus a TokenInfo (which is what
// the mutation executor attributes writes to).
func rowAuthzOwnerRoleCtx(userId string) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: userId,
		Role:   auth.RoleOwner,
	})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userId})
}

// THE BEHAVIOUR CHANGE, end to end: user A cannot edit user B's note,
// and B's row survives the attempt unchanged.
//
// `updateNote` is `update { id: args.noteId; ownerUserId: actor.userId;
// args.payload }` -- a caller-supplied target, a caller-supplied payload
// splat, and an owner re-stamp. Before this guard the write did not
// merely edit B's note, it REASSIGNED it to A. This is the shape
// memql#2991 parts 2 and 3 name for updateCalendarEvent /
// deleteCalendarEvent.
func TestWriteToAnotherCallersRowIsRefusedEndToEnd(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)

	suffix := uniqueSuffix("rowauthz3174")
	userA := "user-a-" + suffix
	userB := "user-b-" + suffix
	noteB := "note-b-" + suffix

	ctxA := rowAuthzCallerCtx(userA)
	ctxB := rowAuthzCallerCtx(userB)

	// createNote stamps ownerUserId from actor.userId, so the row is
	// genuinely B's. A create is unguarded by design -- no target row.
	storedId := runMutation(t, ctxB, eng, "createNote", map[string]any{
		"noteId": noteB, "title": "B's note", "body": "belongs to B",
	})

	before := latestPayload(t, ctxB, db, declaredOwnedConcept, storedId)

	// A names B's row.
	_, err := eng.Execute(ctxA, `mutation updateNote(noteId: "`+noteB+
		`", payload: {"title": "A took this", "body": "mine now"})`)
	if err == nil {
		t.Fatalf("caller A edited caller B's note on %s. The mutation re-stamps "+
			"ownerUserId from the actor, so this is a takeover, not just an edit "+
			"(memql#3174).", declaredOwnedConcept)
	}
	if !strings.Contains(err.Error(), "row-authz") {
		t.Fatalf("the write failed for the wrong reason: %v", err)
	}

	// And nothing landed: no new version, same owner, same content.
	after := latestPayload(t, ctxB, db, declaredOwnedConcept, storedId)
	for _, field := range []string{"ownerUserId", "title", "body"} {
		if before[field] != after[field] {
			t.Errorf("field %q changed on the refused write: %v -> %v",
				field, before[field], after[field])
		}
	}
	if after["ownerUserId"] != userB {
		t.Fatalf("the row's owner is now %v, not %s -- the refused write still "+
			"reassigned it", after["ownerUserId"], userB)
	}
}

// The other half: the legitimate owner's write lands and is durable.
// Without this the guard could be a blanket refusal on the write path
// and the test above would still be green.
func TestTheOwnersOwnWriteStillLandsEndToEnd(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)

	suffix := uniqueSuffix("rowauthz3174owner")
	userA := "user-a-" + suffix
	noteA := "note-a-" + suffix
	ctxA := rowAuthzCallerCtx(userA)

	storedId := runMutation(t, ctxA, eng, "createNote", map[string]any{
		"noteId": noteA, "title": "first", "body": "original",
	})
	runMutation(t, ctxA, eng, "updateNote", map[string]any{
		"noteId": noteA, "payload": map[string]any{"title": "second"},
	})

	after := latestPayload(t, ctxA, db, declaredOwnedConcept, storedId)
	if after["title"] != "second" {
		t.Fatalf("the owner's own update did not land: title is %v", after["title"])
	}
	if after["body"] != "original" {
		t.Fatalf("the read-merge broke: body is %v, expected the inherited value",
			after["body"])
	}
}

// CONSTRAINT 2 end to end: a cluster owner writes a row that is not
// theirs, because that is the one role-based escape this guard declares.
func TestClusterOwnerWritesAnotherCallersRowEndToEnd(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)

	suffix := uniqueSuffix("rowauthz3174co")
	userB := "user-b-" + suffix
	operator := "user-operator-" + suffix
	noteB := "note-b-" + suffix

	ctxB := rowAuthzCallerCtx(userB)
	ctxOwner := rowAuthzOwnerRoleCtx(operator)

	storedId := runMutation(t, ctxB, eng, "createNote", map[string]any{
		"noteId": noteB, "title": "B's note", "body": "belongs to B",
	})

	if _, err := eng.Execute(ctxOwner, `mutation updateNote(noteId: "`+noteB+
		`", payload: {"title": "operator edit"})`); err != nil {
		t.Fatalf("the cluster owner was refused: %v", err)
	}
	if got := latestPayload(t, ctxB, db, declaredOwnedConcept, storedId)["title"]; got != "operator edit" {
		t.Fatalf("the cluster owner's write did not land: title is %v", got)
	}
}

// CONSTRAINT 3 end to end: an update naming a row that does not exist is
// refused, on a DECLARED concept, before anything is written.
func TestUpdateOfAMissingRowIsRefusedEndToEnd(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)

	suffix := uniqueSuffix("rowauthz3174missing")
	ctxA := rowAuthzCallerCtx("user-a-" + suffix)

	_, err := eng.Execute(ctxA, `mutation updateNote(noteId: "no-such-note-`+suffix+
		`", payload: {"title": "ghost"})`)
	if err == nil {
		t.Fatal("an update naming a row that does not exist was authorized. There is no " +
			"owner to compare against, so it must refuse or report not-found -- never " +
			"fall through (memql#3174 constraint 3).")
	}
}
