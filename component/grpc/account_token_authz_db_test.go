package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity/accounttoken"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// THE ACCEPTANCE CRITERION OF memql#3322 THAT IS A SERVER-SIDE CLAIM:
// a caller who does not own an account can neither read it nor act on
// it -- and "act on it" includes minting a credential against it.
//
// # Why this is a database test and not a handler test with a fake
//
// The claim is about the ENGINE. v1:identity:account declares
// @rowAuthz(owner="ownerUserId"), and the enforcement that makes the
// claim true is filter injection in
// component/memql/rowauthz_enforce.go plus the write guard in
// rowauthz_write_guard.go. A test with a mocked engine would be
// asserting that this file's own if-statements behave, which is
// exactly the thing that cannot fail interestingly -- there ARE no
// ownership if-statements in account_token_handlers.go, on purpose,
// because a second copy of the tier is a copy that drifts.
//
// So the whole test drives real handlers over a real engine over a
// real Postgres, from two different authenticated sessions, and reads
// the stored rows back afterwards.
//
// # And why the UI is not the check
//
// The portal hides the token controls from a caller who cannot use
// them. That is a courtesy. Everything asserted below happens with no
// browser involved: the intruder's session sends exactly the envelopes
// the owner's session sends, and is refused by the cluster.
//
// Postgres-gated -- openWireTestDB skips when no database is
// reachable, and CI's db-tests lane runs with MEMQL_REQUIRE_DB=1 so a
// skip there is a failure rather than a green. The runnable half of
// the token claims (no plaintext to the engine, digest-only
// persistence, the shape's keyHash-free projection) lives in
// component/identity/accounttoken.
func TestAccountIsolationAndTokenLifecycleAgainstLiveDatabase(t *testing.T) {
	db := openWireTestDB(t)
	bg := context.Background()

	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memqlengine.New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(concept.DefaultRegistry()))

	suffix := fmt.Sprintf("acct3322-%d", time.Now().UnixNano())
	ownerId := "v1:identity:user:owner-" + suffix
	intruderId := "v1:identity:user:intruder-" + suffix
	accountShort := "account-" + suffix
	accountRowId := "v1:identity:account:" + accountShort

	seedUserRow(bg, t, db, ownerId)
	seedUserRow(bg, t, db, intruderId)
	t.Cleanup(func() {
		for _, id := range []string{ownerId, intruderId, accountRowId} {
			_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).
				Where("id = ?", id).Exec(context.Background())
		}
	})

	owner, ownerStream := accountTestSession(t, eng, ownerId)
	intruder, intruderStream := accountTestSession(t, eng, intruderId)

	// ---------------------------------------------------------------
	// 1. The owner creates the account. ownerUserId is @serverSet, so
	//    the row's owner is decided by the engine from the actor.
	// ---------------------------------------------------------------
	driveGateQuery(t, owner, "create-account", fmt.Sprintf(
		`mutation createAccount(accountId:%q,name:"Northwind Trading",description:"integration fixture")`,
		accountShort))
	waitForQueryResult(t, ownerStream, "create-account", 15*time.Second)

	// ---------------------------------------------------------------
	// 2. READ. The owner sees it; the intruder does not.
	// ---------------------------------------------------------------
	read := fmt.Sprintf(`query accountById(accountId:%q)`, accountShort)

	driveGateQuery(t, owner, "owner-read", read)
	if n := rowsInResult(waitForQueryResult(t, ownerStream, "owner-read", 15*time.Second)); n != 1 {
		t.Fatalf("the OWNER read back %d rows for their own account, want 1. Nothing "+
			"below this line means anything if the fixture never landed.", n)
	}

	driveGateQuery(t, intruder, "intruder-read", read)
	got, qerr := awaitAccountOutcome(t, intruderStream, "intruder-read", 15*time.Second)
	if qerr == nil && rowsInResult(got) != 0 {
		t.Errorf("a caller with NO relationship to this account read %d of its rows. "+
			"v1:identity:account declares @rowAuthz(owner=\"ownerUserId\"), so "+
			"enforceRowAuthzOnPlan should have ANDed ownerUserId==actor.userId into "+
			"this read regardless of what the query author wrote (memql#3172).",
			rowsInResult(got))
	}

	// The list read too -- accountById is guarded by id, and a listing is
	// the shape where a missing predicate returns everyone's rows rather
	// than one.
	driveGateQuery(t, intruder, "intruder-list", `query accounts()`)
	list, listErr := awaitAccountOutcome(t, intruderStream, "intruder-list", 15*time.Second)
	if listErr == nil {
		for _, name := range accountNamesIn(list) {
			if name == "Northwind Trading" {
				t.Errorf("the intruder's `accounts` listing contains another operator's " +
					"customer. This is the read that would be wrong if the owned tier " +
					"stopped being injected.")
			}
		}
	}

	// ---------------------------------------------------------------
	// 3. WRITE. The intruder cannot edit or archive it, and the row is
	//    byte-for-byte unchanged afterwards.
	// ---------------------------------------------------------------
	before := accountPayload(t, db, accountRowId)

	for _, tc := range []struct{ id, call string }{
		{"intruder-update", fmt.Sprintf(`mutation updateAccount(accountId:%q,name:"Taken Over")`, accountShort)},
		{"intruder-archive", fmt.Sprintf(`mutation archiveAccount(accountId:%q)`, accountShort)},
	} {
		driveGateQuery(t, intruder, tc.id, tc.call)
		_, err := awaitAccountOutcome(t, intruderStream, tc.id, 15*time.Second)
		if err == nil {
			t.Errorf("%s succeeded against another operator's account. updateAccount "+
				"re-stamps ownerUserId from the actor, so an admitted write is a "+
				"TAKEOVER rather than an edit -- the write guard "+
				"(component/memql/rowauthz_write_guard.go, memql#3174) must refuse it.", tc.id)
		}
	}

	after := accountPayload(t, db, accountRowId)
	for _, field := range []string{"ownerUserId", "name", "status"} {
		if before[field] != after[field] {
			t.Errorf("field %q changed on a refused write: %v -> %v", field, before[field], after[field])
		}
	}

	// ---------------------------------------------------------------
	// 4. MINT. The intruder cannot mint a credential against an account
	//    that is not theirs -- and the refusal comes from the same
	//    engine read, not from a comparison in the handler.
	// ---------------------------------------------------------------
	denied := mintAccountToken(t, intruder, intruderStream, "intruder-mint", accountShort, "stolen")
	if denied.GetSuccess() {
		t.Fatalf("the intruder minted an account token against another operator's " +
			"customer. The mint's ownership check is `query accountById` run AS THE " +
			"CALLER; if this passes, either the read is no longer caller-scoped or the " +
			"handler stopped consulting it.")
	}
	if denied.GetErrorCode() != "permission_denied" {
		t.Errorf("mint refusal code = %q, want permission_denied (got message %q)",
			denied.GetErrorCode(), denied.GetErrorMessage())
	}
	if denied.GetPlainToken() != "" {
		t.Errorf("a REFUSED mint returned a plaintext credential")
	}
	if denied.GetAuditEventId() == "" {
		t.Errorf("the refused mint emitted no audit event. A blocked credential " +
			"issuance is exactly the event an audit trail exists for.")
	}

	// ---------------------------------------------------------------
	// 5. The owner mints. Plaintext comes back once; only the digest is
	//    stored; the subject echoed is the USER, never the account.
	// ---------------------------------------------------------------
	minted := mintAccountToken(t, owner, ownerStream, "owner-mint", accountShort, "Nightly export")
	if !minted.GetSuccess() {
		t.Fatalf("the owner could not mint a token for their own account: %s / %s",
			minted.GetErrorCode(), minted.GetErrorMessage())
	}
	plain := minted.GetPlainToken()
	if !strings.HasPrefix(plain, accounttoken.TokenPrefix) {
		t.Fatalf("minted credential %q does not carry %q", plain, accounttoken.TokenPrefix)
	}
	if minted.GetSubjectUserId() == accountRowId || minted.GetSubjectUserId() == accountShort {
		t.Errorf("the reply names the ACCOUNT as the credential's subject (%q). Nothing "+
			"authenticates as an account -- the subject is the operator user "+
			"(docs/internal/design/account-isolation-model.md 3.3).",
			minted.GetSubjectUserId())
	}
	if memqlengine.BareShortId(minted.GetSubjectUserId()) != memqlengine.BareShortId(ownerId) {
		t.Errorf("subject_user_id = %q, want the minting operator %q",
			minted.GetSubjectUserId(), ownerId)
	}
	if minted.GetAuditEventId() == "" {
		t.Errorf("the successful mint emitted no audit event")
	}
	tokenRowId := minted.GetIdentityId()
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).
			Where("id = ?", tokenRowId).Exec(context.Background())
	})

	// THE PLAINTEXT IS NOWHERE IN THE DATABASE. Not "not on the row we
	// expect" -- nowhere, because a stray copy on an audit row or a
	// second write would be just as bad as one on the credential.
	var hits int
	require.NoError(t, db.NewSelect().Model((*concept.MemoryNode)(nil)).
		Where("payload::text LIKE ?", "%"+plain+"%").
		ColumnExpr("count(*)").Scan(context.Background(), &hits))
	if hits != 0 {
		t.Fatalf("the plaintext credential appears in %d stored row(s). It must exist "+
			"only in the mint reply.", hits)
	}
	// And the digest IS there, so the credential is genuinely recorded
	// rather than the previous assertion passing vacuously.
	stored := accountPayload(t, db, tokenRowId)
	creds, _ := stored["credentials"].(map[string]any)
	if creds == nil {
		t.Fatalf("the minted credential row has no credentials block: %v", stored)
	}
	if creds["keyHash"] != accounttoken.Hash(plain) {
		t.Errorf("stored keyHash = %v, want the SHA-256 of the returned plaintext", creds["keyHash"])
	}

	// ---------------------------------------------------------------
	// 6. REVOKE. Not the intruder's to revoke; the owner's to revoke.
	// ---------------------------------------------------------------
	stolenRevoke := revokeAccountToken(t, intruder, intruderStream, "intruder-revoke", tokenRowId)
	if stolenRevoke.GetSuccess() {
		t.Errorf("the intruder revoked a credential another operator issued")
	}
	if stolenRevoke.GetErrorCode() != "permission_denied" {
		t.Errorf("revoke refusal code = %q, want permission_denied", stolenRevoke.GetErrorCode())
	}

	ok := revokeAccountToken(t, owner, ownerStream, "owner-revoke", tokenRowId)
	if !ok.GetSuccess() {
		t.Fatalf("the owner could not revoke their own credential: %s / %s",
			ok.GetErrorCode(), ok.GetErrorMessage())
	}
	if ok.GetAuditEventId() == "" {
		t.Errorf("the revoke emitted no audit event")
	}
	if active, _ := accountPayload(t, db, tokenRowId)["active"].(bool); active {
		t.Errorf("the credential row is still active after a successful revoke. " +
			"Revocation has to be immediate -- a token that keeps working until a " +
			"cache expires is not revoked.")
	}
}

// accountTestSession builds an authenticated streamSession for one
// user. Role writer rather than owner deliberately: an owner role
// would make it impossible to tell row-authz enforcement apart from a
// role check that happened to admit everything.
func accountTestSession(t *testing.T, eng *memqlengine.MemQLEngine, userId string) (*streamSession, *captureStream) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	cs := newCaptureStream(t)
	cs.ctx = auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: userId})
	return &streamSession{
		service:      &service{engine: eng, logger: logger},
		stream:       cs,
		logger:       logger,
		access:       &auth.AccessContext{UserId: userId, Role: auth.RoleWriter},
		accessLoaded: true,
	}, cs
}

func mintAccountToken(t *testing.T, s *streamSession, cs *captureStream, messageId, accountId, label string) *memqlv1.CreateAccountTokenResult {
	t.Helper()
	env := &memqlv1.MemqlClientMessage{
		MessageId: messageId,
		Payload: &memqlv1.MemqlClientMessage_CreateAccountToken{
			CreateAccountToken: &memqlv1.CreateAccountTokenMsg{
				RequestId: messageId, AccountId: accountId, Label: label,
			},
		},
	}
	require.NoError(t, s.handleCreateAccountToken(env, env.GetCreateAccountToken()))
	for _, m := range cs.snapshot() {
		if r := m.GetCreateAccountTokenResult(); r != nil && r.GetRequestId() == messageId {
			return r
		}
	}
	t.Fatalf("no CreateAccountTokenResult for %q", messageId)
	return nil
}

func revokeAccountToken(t *testing.T, s *streamSession, cs *captureStream, messageId, identityId string) *memqlv1.RevokeAccountTokenResult {
	t.Helper()
	env := &memqlv1.MemqlClientMessage{
		MessageId: messageId,
		Payload: &memqlv1.MemqlClientMessage_RevokeAccountToken{
			RevokeAccountToken: &memqlv1.RevokeAccountTokenMsg{
				RequestId: messageId, IdentityId: identityId,
			},
		},
	}
	require.NoError(t, s.handleRevokeAccountToken(env, env.GetRevokeAccountToken()))
	for _, m := range cs.snapshot() {
		if r := m.GetRevokeAccountTokenResult(); r != nil && r.GetRequestId() == messageId {
			return r
		}
	}
	t.Fatalf("no RevokeAccountTokenResult for %q", messageId)
	return nil
}

// awaitAccountOutcome is waitForQueryResult's tolerant sibling: a
// REFUSED write is expected here, and waitForQueryResult treats a
// QueryError as a test failure. Exactly one of the two returns is
// non-nil.
func awaitAccountOutcome(t *testing.T, cs *captureStream, requestId string, timeout time.Duration) (*memqlv1.QueryResultChunk, *memqlv1.QueryError) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, m := range cs.snapshot() {
			if qr := m.GetQueryResult(); qr != nil && qr.GetRequestId() == requestId && qr.GetDone() {
				return qr, nil
			}
			if qe := m.GetQueryError(); qe != nil && qe.GetRequestId() == requestId {
				return nil, qe.GetError()
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for an outcome for %q", requestId)
	return nil, nil
}

// rowsInResult counts rows in either result shape -- a raw node bundle
// or a shape projection in Data.
func rowsInResult(res *memqlv1.QueryResultChunk) int {
	if res == nil || res.GetResult() == nil {
		return 0
	}
	if b := res.GetResult().GetBundle(); b != nil && len(b.GetNodes()) > 0 {
		return len(b.GetNodes())
	}
	return len(res.GetResult().GetData())
}

func accountNamesIn(res *memqlv1.QueryResultChunk) []string {
	if res == nil || res.GetResult() == nil {
		return nil
	}
	var out []string
	for _, v := range res.GetResult().GetData() {
		if sv := v.GetStructValue(); sv != nil {
			if name, ok := sv.GetFields()["name"]; ok {
				out = append(out, name.GetStringValue())
			}
		}
	}
	for _, n := range res.GetResult().GetBundle().GetNodes() {
		if name, ok := n.GetPayload().AsMap()["name"].(string); ok {
			out = append(out, name)
		}
	}
	return out
}

// accountPayload reads the latest stored version of a row straight out
// of Postgres, bypassing the engine entirely -- the refused-write
// assertions are about what LANDED, and asking the engine would route
// the answer back through the very enforcement under test.
//
// OrderExpr rather than Order for the reason
// rowauthz_insert_stamp_db_test.go records: bun's Order() quotes the
// argument as a column name, so a pre-quoted "createdAt" reaches
// Postgres as ""createdAt"" and the read fails with 42703.
func accountPayload(t *testing.T, db *bun.DB, id string) map[string]any {
	t.Helper()
	var stored concept.MemoryNode
	if err := db.NewSelect().Model(&stored).
		Where("id = ?", id).OrderExpr(`"createdAt" DESC`).Limit(1).
		Scan(context.Background()); err != nil {
		t.Fatalf("read stored row %q: %v", id, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(stored.Payload, &payload); err != nil {
		t.Fatalf("decode stored payload for %q: %v", id, err)
	}
	return payload
}

// snapshot copies the sent envelopes under the stream's lock so the
// scan loops above do not read the slice while a handler appends to
// it.
func (c *captureStream) snapshot() []*memqlv1.MemqlServerMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*memqlv1.MemqlServerMessage, len(c.sent))
	copy(out, c.sent)
	return out
}
