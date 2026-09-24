package memql

// Real-engine tests for the connect-state write guard (memql#5623, Connect
// Shopify design record section 11, D13).
//
// v1:identity:githubConnectState declares no row tier, so the row-authz write
// guard passes every write to it, and the raw insert(...) literal is public
// language that never consults a mutation's @serverOnly. Before the guard, any
// signed-in caller could therefore plant a state row naming ANY user and ANY
// purpose -- including purpose app_setup naming a cluster owner, which the
// GitHub App setup callback's "still an active cluster owner" re-check accepts,
// because that check reads the user id off the planted row.
//
// Driven through Engine.Execute against a real Postgres (the shared read-merge
// engine, db-gated) because the hole is the WHOLE write path: parser, raw
// insert short-circuit, row-authz guard, schema validation and persistence. A
// test of the guard function alone would pass on an engine that never calls it.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

const connectStateConcept = "v1:identity:githubConnectState"

func connectStateRowCount(t *testing.T, db *bun.DB, id string) int {
	t.Helper()
	n, err := db.NewSelect().Model((*concept.MemoryNode)(nil)).
		Where("concept = ?", connectStateConcept).
		Where("id = ?", id).
		Count(context.Background())
	if err != nil {
		t.Fatalf("count %s rows at %s: %v", connectStateConcept, id, err)
	}
	return n
}

func uniqueConnectStateId(name string) string {
	return fmt.Sprintf("%s:%s-%d", connectStateConcept, name, time.Now().UnixNano())
}

// plantedAppSetupInsert is the forgery: an app_setup state naming the cluster
// owner, carrying a digest the attacker chose (so they hold the plaintext).
func plantedAppSetupInsert(id string) string {
	return fmt.Sprintf(
		`insert("%s", id="%s", payload={"userId":"v1:identity:user:the-owner","stateHash":"%s","expiresAt":"2099-01-01T00:00:00Z","purpose":"app_setup","returnPath":"/"})`,
		connectStateConcept, id, strings.Repeat("ab", 32))
}

// THE TEST THAT FAILS AGAINST MAIN: the raw insert lands a row.
func TestConnectStateRawInsertWithoutInternalOriginIsRefused(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)

	callers := []struct {
		name string
		ctx  context.Context
	}{
		{"writer", writerUserActorCtx()},
		// Not a rank question: the cluster owner is refused too, because
		// nothing but the identity store's Go ever needs to write one.
		{"owner", actorCtx(map[string]any{
			"sub": "v1:identity:user:an-owner", "email": "owner@example.com", "role": "owner",
		})},
		// Not an actor question either, unlike the credential guard: a system
		// actor without the origin stamp is still refused.
		{"system_actor", systemCredentialActorCtx()},
	}
	for _, c := range callers {
		t.Run(c.name, func(t *testing.T) {
			id := uniqueConnectStateId("planted-" + c.name)
			_, err := eng.Execute(c.ctx, plantedAppSetupInsert(id))
			if err == nil {
				t.Fatalf("a raw insert(...) planted a %s row as %s. Any signed-in caller can mint an "+
					"app_setup state naming the cluster owner, which the GitHub App setup callback "+
					"accepts (memql#5623).", connectStateConcept, c.name)
			}
			if !strings.Contains(err.Error(), "internal origin") {
				t.Errorf("refused, but not by the connect-state guard: %v", err)
			}
			if n := connectStateRowCount(t, db, id); n != 0 {
				t.Errorf("refused, yet %d row(s) landed at %s", n, id)
			}
		})
	}
}

// A raw insert naming an EXISTING row's id is a read-merge rewrite of that row:
// it could re-point a live state at another user, or clear consumedAt to revive
// a spent one. Refused, with the stored row unchanged.
func TestConnectStateRewriteOfAnExistingRowWithoutInternalOriginIsRefused(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)

	id := uniqueConnectStateId("existing")
	seed := fmt.Sprintf(
		`mutation createGithubConnectState(stateId: %s, userId: %s, stateHash: %s, expiresAt: %s)`,
		langparser.QuoteString(id), langparser.QuoteString("v1:identity:user:the-person"),
		langparser.QuoteString(strings.Repeat("cd", 32)), langparser.QuoteString("2099-01-01T00:00:00Z"))
	if _, err := eng.Execute(auth.ContextWithInternalOrigin(writerUserActorCtx()), seed); err != nil {
		t.Fatalf("seed the state the way the identity store does (internal origin): %v", err)
	}
	before := connectStateRowCount(t, db, id)

	rewrite := fmt.Sprintf(
		`insert("%s", id="%s", payload={"userId":"v1:identity:user:the-owner","purpose":"app_setup"})`,
		connectStateConcept, id)
	if _, err := eng.Execute(writerUserActorCtx(), rewrite); err == nil {
		t.Fatal("a raw insert(...) rewrote an existing connect state as a signed-in user")
	}
	// The named consume is refused by @serverOnly before the guard is reached,
	// so this leg holds the ROW's outcome rather than the guard: it fails only
	// if both gates go. The raw rewrite above is the guard's own evidence.
	consume := fmt.Sprintf(`mutation consumeGithubConnectState(stateId: %s, consumedAt: %s)`,
		langparser.QuoteString(id), langparser.QuoteString("2026-09-23T00:00:00Z"))
	if _, err := eng.Execute(writerUserActorCtx(), consume); err == nil {
		t.Fatal("consumeGithubConnectState ran for a signed-in user")
	}

	if after := connectStateRowCount(t, db, id); after != before {
		t.Errorf("refused, yet the row went from %d to %d version(s)", before, after)
	}
	if got := latestPayload(t, context.Background(), db, connectStateConcept, id)["userId"]; got != "v1:identity:user:the-person" {
		t.Errorf("stored userId = %v, want the person who began it", got)
	}
}

// The named create stays refused for a signed-in user. DEFENCE IN DEPTH, not
// the guard's evidence: @serverOnly refuses this before executeWrite runs, so
// the test fails only when BOTH gates are removed. The raw-insert tests above
// are what pin the guard.
func TestCreateGithubConnectStateIsRefusedForASignedInUser(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)

	id := uniqueConnectStateId("direct")
	q := fmt.Sprintf(
		`mutation createGithubConnectState(stateId: %s, userId: %s, stateHash: %s, expiresAt: %s, purpose: %s)`,
		langparser.QuoteString(id), langparser.QuoteString("v1:identity:user:the-owner"),
		langparser.QuoteString(strings.Repeat("ef", 32)), langparser.QuoteString("2099-01-01T00:00:00Z"),
		langparser.QuoteString("app_setup"))
	if _, err := eng.Execute(writerUserActorCtx(), q); err == nil {
		t.Fatal("createGithubConnectState ran for a signed-in user")
	}
	if n := connectStateRowCount(t, db, id); n != 0 {
		t.Errorf("refused, yet %d row(s) landed", n)
	}
}

// The positive control: the identity store's own writes -- create under the
// beginning user, consume under the identity service's actor, both stamping
// internal origin -- still land. Without this the refusals above would pass on
// a guard that refused everything.
func TestConnectStateServerWritesWithInternalOriginStillLand(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)

	id := uniqueConnectStateId("server")
	create := fmt.Sprintf(
		`mutation createGithubConnectState(stateId: %s, userId: %s, stateHash: %s, expiresAt: %s, returnPath: %s, sourceIP: %s, purpose: %s, organization: %s)`,
		langparser.QuoteString(id), langparser.QuoteString("v1:identity:user:the-person"),
		langparser.QuoteString(strings.Repeat("12", 32)), langparser.QuoteString("2099-01-01T00:00:00Z"),
		langparser.QuoteString("/"), langparser.QuoteString("203.0.113.9"),
		langparser.QuoteString(""), langparser.QuoteString(""))
	if _, err := eng.Execute(auth.ContextWithInternalOrigin(writerUserActorCtx()), create); err != nil {
		t.Fatalf("the identity store's create was refused: %v", err)
	}
	consume := fmt.Sprintf(`mutation consumeGithubConnectState(stateId: %s, consumedAt: %s, consumedFromIP: %s)`,
		langparser.QuoteString(id), langparser.QuoteString("2026-09-23T00:00:00Z"), langparser.QuoteString("203.0.113.9"))
	if _, err := eng.Execute(auth.ContextWithInternalOrigin(ownerSessionActorCtx()), consume); err != nil {
		t.Fatalf("the identity store's consume was refused: %v", err)
	}
	if got := latestPayload(t, context.Background(), db, connectStateConcept, id)["consumedAt"]; got == nil || got == "" {
		t.Errorf("consume landed no consumedAt: %v", got)
	}
}

// TestAConnectStateReadCarriesItsCreatedAt pins what consume's lifetime check
// depends on (component/identity, GithubConnectStateRow.serverShaped): the read
// the identity store makes returns the row's createdAt. Consume refuses a state
// whose lifetime it cannot measure, so without createdAt on this read every
// real state would look planted and GitHub Connect would stop working.
func TestAConnectStateReadCarriesItsCreatedAt(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)

	id := uniqueConnectStateId("createdat")
	hash := fmt.Sprintf("%064x", sha256.Sum256([]byte(id)))
	expires := time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)
	create := fmt.Sprintf(
		`mutation createGithubConnectState(stateId: %s, userId: %s, stateHash: %s, expiresAt: %s, returnPath: %s, sourceIP: %s, purpose: %s, organization: %s)`,
		langparser.QuoteString(id), langparser.QuoteString("v1:identity:user:the-person"),
		langparser.QuoteString(hash), langparser.QuoteString(expires),
		langparser.QuoteString("/"), langparser.QuoteString(""),
		langparser.QuoteString(""), langparser.QuoteString(""))
	if _, err := eng.Execute(auth.ContextWithInternalOrigin(writerUserActorCtx()), create); err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := eng.Execute(auth.ContextWithInternalOrigin(writerUserActorCtx()),
		fmt.Sprintf(`query githubConnectStateByHash(stateHash: %s)`, langparser.QuoteString(hash)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	nodes := res.Bundle.GetNodes()
	if len(nodes) != 1 {
		t.Fatalf("read returned %d rows, want 1", len(nodes))
	}
	// On the NODE, not in the payload: the shape's row.createdAt is an
	// intrinsic, and the payload of this read carries no createdAt key.
	if nodes[0].GetCreatedAt() == nil {
		t.Fatal("the state read carries no createdAt on its node")
	}
	created := nodes[0].GetCreatedAt().AsTime()
	if age := time.Since(created); age < -time.Minute || age > time.Hour {
		t.Fatalf("createdAt %s is not this write's time", created)
	}
}
