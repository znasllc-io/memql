package webauthn

// Store-level tests for the passkey management operations (memql#3409).
//
// They assert the two things a caller cannot see for itself: WHAT query
// the store emits, and WHICH ACTOR it emits it under. Both are load
// bearing -- the first is where "soft-delete, not delete" actually lives,
// and the second is what the memql#2513 credential guard admits or
// rejects.

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// captureEngine records every query and the actor it ran under.
type captureEngine struct {
	queries []string
	actors  []string
}

func (e *captureEngine) Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error) {
	e.queries = append(e.queries, query)
	e.actors = append(e.actors, auth.ActorFromContext(ctx))
	return &memqlengine.ExecuteResult{}, nil
}

func (e *captureEngine) only(t *testing.T) (string, string) {
	t.Helper()
	if len(e.queries) != 1 {
		t.Fatalf("expected exactly one engine call, got %d: %v", len(e.queries), e.queries)
	}
	return e.queries[0], e.actors[0]
}

// TestRevokeIsASoftDeleteNotADelete is the acceptance criterion in query
// form. The concept keeps revoked credentials "around for audit but
// inactive", and for passkeys specifically the credential id must stay
// TAKEN -- register/finish refuses an id that already exists, revoked
// rows included, because revoking a row does not make the authenticator
// forget its private key. A hard delete would free the id and let a
// still-live authenticator be re-enrolled onto a fresh row.
func TestRevokeIsASoftDeleteNotADelete(t *testing.T) {
	engine := &captureEngine{}
	store := &Store{Engine: engine}

	if err := store.Revoke(context.Background(), "v1:identity:identity:pk-1"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	query, _ := engine.only(t)

	if !strings.Contains(query, "revokePasskeyIdentity") {
		t.Errorf("Revoke emitted %q, want the revokePasskeyIdentity mutation", query)
	}
	if strings.Contains(strings.ToLower(query), "delete") {
		t.Errorf("Revoke emitted a DELETE (%q). Revocation must flip active=false: the row is "+
			"audit history, and its credential id must stay taken so the same authenticator "+
			"cannot be re-enrolled onto a fresh row", query)
	}
	// The canonical prefix is stripped: the mutation takes a bare slug,
	// matching every sibling revoke in the identity stores.
	if !strings.Contains(query, `identityId:"pk-1"`) {
		t.Errorf("Revoke passed %q, want the bare slug pk-1", query)
	}
}

// TestManagementWritesRunUnderTheSystemCredentialActor pins the actor.
//
// A passkey row is in the memql#2513 machine-credential set, so the
// engine admits a write to it ONLY from a system actor. The handler that
// calls these methods runs under the signed-in user's session, and if
// that actor reached the engine every rename and every revoke would be
// rejected at the row-write guard. Asserted here rather than left to an
// integration test because the failure is silent at compile time and
// total at runtime.
func TestManagementWritesRunUnderTheSystemCredentialActor(t *testing.T) {
	// Start from a caller actor, which is what the request context
	// actually carries: the helper must REPLACE it, not defer to it.
	base := auth.ContextWithUserActor(context.Background(), "v1:identity:user:alice")

	t.Run("revoke", func(t *testing.T) {
		engine := &captureEngine{}
		store := &Store{Engine: engine}
		if err := store.Revoke(base, "pk-1"); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		_, actor := engine.only(t)
		if !strings.HasPrefix(actor, "system:") {
			t.Errorf("Revoke ran as %q; the memql#2513 guard admits a passkey write only from a "+
				"system actor, so this write would be rejected", actor)
		}
	})

	t.Run("rename", func(t *testing.T) {
		engine := &captureEngine{}
		store := &Store{Engine: engine}
		if err := store.Rename(base, "pk-1", "Work laptop"); err != nil {
			t.Fatalf("Rename: %v", err)
		}
		query, actor := engine.only(t)
		if !strings.HasPrefix(actor, "system:") {
			t.Errorf("Rename ran as %q, want a system: actor", actor)
		}
		if !strings.Contains(query, "renamePasskeyIdentity") || !strings.Contains(query, `label:"Work laptop"`) {
			t.Errorf("Rename emitted %q, want renamePasskeyIdentity carrying the new label", query)
		}
	})
}

// TestRenameRejectsEmptyInput keeps the store from emitting a mutation
// that would blank a passkey's only human-readable field.
func TestRenameRejectsEmptyInput(t *testing.T) {
	engine := &captureEngine{}
	store := &Store{Engine: engine}
	if err := store.Rename(context.Background(), "pk-1", "   "); err == nil {
		t.Error("Rename accepted a blank label; the label is the only thing that tells two " +
			"authenticators apart in the list")
	}
	if len(engine.queries) != 0 {
		t.Errorf("Rename hit the engine anyway: %v", engine.queries)
	}
}

// TestAuthenticatorNameIsDisplayOnlyAndToleratesUnknowns pins the two
// behaviours the AAGUID table's callers depend on: a known model
// resolves, and everything else resolves to the EMPTY string rather than
// to a placeholder that reads like a finding. An authenticator that
// withholds its model (every platform authenticator that chooses to) and
// a model newer than the table are both ordinary.
func TestAuthenticatorNameIsDisplayOnlyAndToleratesUnknowns(t *testing.T) {
	if got := AuthenticatorName("fbfc3007-154e-4ecc-8c0b-6e020557d7bd"); got != "iCloud Keychain" {
		t.Errorf("AuthenticatorName(iCloud aaguid) = %q, want %q", got, "iCloud Keychain")
	}
	// Case-insensitive: the value arrives as whatever uuid formatting the
	// attestation produced.
	if got := AuthenticatorName("FBFC3007-154E-4ECC-8C0B-6E020557D7BD"); got != "iCloud Keychain" {
		t.Errorf("AuthenticatorName is case-sensitive; got %q", got)
	}
	for _, absent := range []string{"", "   ", zeroAAGUID, "11111111-2222-3333-4444-555555555555"} {
		if got := AuthenticatorName(absent); got != "" {
			t.Errorf("AuthenticatorName(%q) = %q, want \"\" -- an unresolvable model is ordinary, "+
				"not an error to surface", absent, got)
		}
	}
}
