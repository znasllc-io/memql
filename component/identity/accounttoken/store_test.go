package accounttoken

import (
	"context"
	"strings"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// recordingEngine captures every call string the store issues. The
// store's whole job is to turn typed arguments into exactly the right
// named call, so the call string IS the unit under test.
type recordingEngine struct {
	calls  []string
	result *memqlengine.ExecuteResult
	err    error
}

func (e *recordingEngine) Execute(_ context.Context, query string) (*memqlengine.ExecuteResult, error) {
	e.calls = append(e.calls, query)
	if e.err != nil {
		return nil, e.err
	}
	return e.result, nil
}

// THE PROPERTY THAT MATTERS MOST: the plaintext credential cannot
// reach the engine.
//
// Not "does not" -- CANNOT. Store.Create has no parameter for it and
// createAccountTokenIdentity has no arg for it, so there is no
// expression a caller could write that would put it in a call string.
// This test mints a real token and then asserts the plaintext appears
// in none of the traffic, which is the observable form of that claim
// and the one that would fail if either signature grew a field.
func TestCreateNeverSendsThePlaintextToTheEngine(t *testing.T) {
	plain, hash, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	eng := &recordingEngine{}
	store := &Store{Engine: eng}

	if err := store.Create(context.Background(), "acct-tok-1", "v1:identity:account:acme",
		"Acme nightly export", hash, time.Time{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(eng.calls) != 1 {
		t.Fatalf("Create issued %d engine calls, want exactly 1: %v", len(eng.calls), eng.calls)
	}
	call := eng.calls[0]

	if strings.Contains(call, plain) {
		t.Fatalf("the PLAINTEXT credential appears in the engine call. It must exist "+
			"only in the mint reply: %s", call)
	}
	// And the body of the token, in case a future signature passes it
	// stripped of its prefix.
	if body := strings.TrimPrefix(plain, TokenPrefix); strings.Contains(call, body) {
		t.Fatalf("the credential body appears in the engine call: %s", call)
	}
	if !strings.Contains(call, hash) {
		t.Errorf("the digest is NOT in the engine call, so nothing would be persisted "+
			"and the credential could never be recognised as having existed: %s", call)
	}
	for _, want := range []string{
		"mutation createAccountTokenIdentity",
		`identityId:"acct-tok-1"`,
		`accountId:"v1:identity:account:acme"`,
		`label:"Acme nightly export"`,
	} {
		if !strings.Contains(call, want) {
			t.Errorf("call is missing %q: %s", want, call)
		}
	}
	// userId / mintedBy are stamped by the mutation from actor.userId
	// and are deliberately NOT args -- a caller cannot mint a credential
	// attributed to someone else.
	for _, forbidden := range []string{"userId:", "mintedBy:"} {
		if strings.Contains(call, forbidden) {
			t.Errorf("call passes %s, which the mutation stamps from actor.userId. "+
				"Passing it makes the credential's subject caller-controlled: %s",
				forbidden, call)
		}
	}
}

func TestCreateRejectsIncompleteArguments(t *testing.T) {
	eng := &recordingEngine{}
	store := &Store{Engine: eng}
	for _, tc := range []struct {
		name                                  string
		identityId, accountId, label, keyHash string
	}{
		{"no id", "", "acct", "label", "hash"},
		{"no account", "id", "", "label", "hash"},
		{"no label", "id", "acct", "", "hash"},
		{"no digest", "id", "acct", "label", ""},
	} {
		if err := store.Create(context.Background(), tc.identityId, tc.accountId,
			tc.label, tc.keyHash, time.Time{}); err == nil {
			t.Errorf("%s: Create succeeded on incomplete arguments", tc.name)
		}
	}
	if len(eng.calls) != 0 {
		t.Errorf("a rejected Create still reached the engine: %v", eng.calls)
	}
}

func TestRevokeTargetsTheBareSlug(t *testing.T) {
	eng := &recordingEngine{}
	store := &Store{Engine: eng}
	if err := store.Revoke(context.Background(), "v1:identity:identity:acct-tok-1"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	want := `mutation revokeAccountTokenIdentity(identityId:"acct-tok-1")`
	if len(eng.calls) != 1 || eng.calls[0] != want {
		t.Errorf("Revoke issued %v, want %q", eng.calls, want)
	}
}

// ByIdForCaller answers "no" identically for a row that does not exist
// and a row that is not the caller's, because accountTokenById's
// userId==actor.userId conjunct erases the difference before the store
// ever sees it. Asserting the nil here pins the handler's behaviour to
// that: it must not treat an empty result as an error to be reported
// differently from a refusal.
func TestByIdForCallerReturnsNilOnAnEmptyResult(t *testing.T) {
	eng := &recordingEngine{result: &memqlengine.ExecuteResult{}}
	store := &Store{Engine: eng}
	row, err := store.ByIdForCaller(context.Background(), "acct-tok-1")
	if err != nil {
		t.Fatalf("ByIdForCaller: %v", err)
	}
	if row != nil {
		t.Errorf("want nil row for an empty result, got %+v", row)
	}
	if len(eng.calls) != 1 || !strings.Contains(eng.calls[0], "query accountTokenById") {
		t.Errorf("unexpected traffic: %v", eng.calls)
	}
}

// The shape flattens credentials.accountId to `accountId`; the reader
// has to pick it up from there and must find no digest, because the
// shape projects none.
func TestRowFromNodeReadsTheFlattenedCredentialLeaves(t *testing.T) {
	node := &memqlv1.MemoryNode{
		Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
			"id":        structpb.NewStringValue("v1:identity:identity:acct-tok-1"),
			"userId":    structpb.NewStringValue("v1:identity:user:ada"),
			"label":     structpb.NewStringValue("Acme nightly export"),
			"active":    structpb.NewBoolValue(false),
			"accountId": structpb.NewStringValue("v1:identity:account:acme"),
			"mintedBy":  structpb.NewStringValue("v1:identity:user:ada"),
			"createdAt": structpb.NewStringValue("2026-08-08T10:00:00Z"),
		}},
	}
	row := rowFromNode(node)
	if row == nil {
		t.Fatal("rowFromNode returned nil for a well-formed node")
	}
	if row.AccountId != "v1:identity:account:acme" {
		t.Errorf("accountId = %q, want the flattened credentials.accountId", row.AccountId)
	}
	if row.MintedBy != "v1:identity:user:ada" {
		t.Errorf("mintedBy = %q", row.MintedBy)
	}
	if row.Active {
		t.Error("active=false on the wire must not read as active=true; a revoked " +
			"credential shown as live is the worst way for this to be wrong")
	}
	if row.Label != "Acme nightly export" {
		t.Errorf("label = %q", row.Label)
	}
}
