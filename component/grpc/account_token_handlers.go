package memql

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/accounttoken"
	"github.com/znasllc-io/memql/core/id"
)

// Account-token mint / revoke (memql#3322).
//
// # What is being minted, and what it is not
//
// An account token is a credential ISSUED TO A USER ON BEHALF OF AN
// ACCOUNT. Its authenticated subject is the operator's
// v1:identity:user; `credentials.accountId` is a binding for
// attribution and grouped revocation. Nothing authenticates AS an
// account, and no surface admits an mql_acct_ bearer today -- see the
// package comment on component/identity/accounttoken for why both the
// "authenticate as the account" and the "authenticate as the operator,
// narrowed to the account" designs are ruled out by
// docs/internal/design/account-isolation-model.md sections 3.3 and 5.2.
//
// # Why these are envelopes rather than DSL calls
//
// Everything an operator does to an account -- create, edit, archive,
// list its tokens -- is an ordinary named query or mutation the portal
// executes over the query path. Only two operations need a handler:
//
//   - the mint, because the plaintext exists in exactly one place (the
//     reply) and must never reach the engine, a log line or a payload;
//   - the revoke, because it is audited and the audit id comes back.
//
// # Authorization: the engine decides, this file only asks
//
// The ownership check is `query accountById` executed AS THE CALLER.
// v1:identity:account declares @rowAuthz(owner="ownerUserId"), so the
// engine ANDs `ownerUserId == actor.userId` into that read
// (component/memql/rowauthz_enforce.go) whatever this file believes.
// A caller who does not own the account gets zero rows, and zero rows
// is the refusal. That is deliberately NOT a comparison written here:
// a hand-written `if account.OwnerUserId != caller` would be a second
// implementation of the tier, and the two would eventually disagree.
//
// There is no admin override. Read enforcement has no cluster-owner
// escape (the predicate is ANDed unconditionally), so an admin reading
// another operator's account gets zero rows too -- and an override
// here would mint a credential against a row the minting path cannot
// actually see. The isolation note calls this out as "owned and admin
// do not compose"; the honest surface refuses rather than pretending.

// accountTokenAuditor builds the audit sink for this file.
//
// Constructed per call rather than held on the service because
// component/grpc has no audit plumbing at all today (the worker-token
// and badge handlers emit nothing, which is its own gap). Threading an
// identity.AuditLogger through the service struct is the better home
// and is a wiring change in app/; until then this keeps the guarantee
// local and true: BOTH ends of an account token's life are audited.
//
// SlogAuditLogger fans out to the structured log (always) and the DB
// sink (best-effort). Failure to write the row is logged and swallowed
// -- the slog stream is the canonical destination, exactly as every
// other audit call site in the tree treats it.
func (s *streamSession) accountTokenAuditor() identity.AuditLogger {
	return &identity.SlogAuditLogger{
		Logger: s.service.logger,
		DB:     &identity.EngineAuditSink{Engine: s.service.engine, Logger: s.service.logger},
	}
}

// callerScopedContext returns the stream context with the caller's
// resolved AccessContext attached, which is what makes actor.userId
// bind inside the DSL. Same construction handleExecuteQuery performs;
// without it every actor-scoped read silently returns zero rows.
//
// Deliberately NOT contextWithSystemActor (the worker-token handler's
// choice): a system actor would defeat the very row-authz check this
// file relies on for authorization.
func (s *streamSession) callerScopedContext() context.Context {
	ctx := s.stream.Context()
	return auth.ContextWithAccess(ctx, s.ensureAccess(ctx))
}

// callerOwnsAccount asks the ENGINE whether this account is the
// caller's, by reading it as the caller. Returns false with no error
// when the read succeeds and returns nothing -- "no such account" and
// "not yours" are one answer on purpose, so the mint endpoint is not
// an oracle for which account ids exist on the cluster.
func (s *streamSession) callerOwnsAccount(ctx context.Context, accountId string) (bool, error) {
	res, err := s.service.engine.Execute(ctx,
		fmt.Sprintf(`query accountById(accountId:%q)`, accountId))
	if err != nil {
		return false, err
	}
	if res == nil {
		return false, nil
	}
	if res.Bundle != nil && len(res.Bundle.Nodes) > 0 {
		return true, nil
	}
	_, data, err := res.ToAPIResult()
	if err != nil {
		return false, err
	}
	return len(data) > 0, nil
}

// handleCreateAccountToken mints an account token. The plaintext is
// returned ONCE in the reply and never persisted -- only its SHA-256
// digest lands on the v1:identity:identity row.
func (s *streamSession) handleCreateAccountToken(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.CreateAccountTokenMsg) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	send := func(result *memqlv1.CreateAccountTokenResult) error {
		result.RequestId = requestId
		return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_CreateAccountTokenResult{
				CreateAccountTokenResult: result,
			},
		})
	}

	caller, err := auth.UserIdentityFromContext(s.stream.Context())
	if err != nil {
		return send(&memqlv1.CreateAccountTokenResult{
			ErrorCode:    "unauthenticated",
			ErrorMessage: "caller identity not resolved",
		})
	}
	callerUserId := strings.TrimSpace(caller.Subject)
	if callerUserId == "" {
		return send(&memqlv1.CreateAccountTokenResult{
			ErrorCode:    "unauthenticated",
			ErrorMessage: "caller subject empty",
		})
	}

	accountId := strings.TrimSpace(msg.GetAccountId())
	if accountId == "" {
		return send(&memqlv1.CreateAccountTokenResult{
			ErrorCode:    "bad_request",
			ErrorMessage: "account_id is required",
		})
	}
	label := strings.TrimSpace(msg.GetLabel())
	if label == "" {
		// Not pedantry: the revoke surface lists credentials by label,
		// and an operator cannot revoke the right one from a list of
		// blanks.
		return send(&memqlv1.CreateAccountTokenResult{
			ErrorCode:    "bad_request",
			ErrorMessage: "label is required -- an unlabelled credential cannot be revoked with confidence",
		})
	}

	// The engine check sits AFTER the client-side ones deliberately: a
	// malformed request is malformed whether or not this node happens to
	// have an engine wired, and answering "unavailable" to a request that
	// was never going to be accepted sends the caller to look at the
	// cluster instead of at their own call.
	if s.service.engine == nil {
		return send(&memqlv1.CreateAccountTokenResult{
			ErrorCode:    "unavailable",
			ErrorMessage: "engine not configured",
		})
	}

	ctx := s.callerScopedContext()
	auditor := s.accountTokenAuditor()

	owns, err := s.callerOwnsAccount(ctx, accountId)
	if err != nil {
		return send(&memqlv1.CreateAccountTokenResult{
			ErrorCode:    "lookup_failed",
			ErrorMessage: err.Error(),
		})
	}
	if !owns {
		eventId := auditAccountToken(ctx, auditor, caller, "account_token_create_blocked",
			accountId, "", label, identity.AuditOutcomeBlocked, "not_account_owner")
		return send(&memqlv1.CreateAccountTokenResult{
			ErrorCode:    "permission_denied",
			ErrorMessage: "no account with that id is yours",
			AuditEventId: eventId,
		})
	}

	plain, hash, err := accounttoken.Mint()
	if err != nil {
		return send(&memqlv1.CreateAccountTokenResult{
			ErrorCode:    "internal",
			ErrorMessage: "token mint failed: " + err.Error(),
		})
	}
	identityId, err := accounttoken.NewId()
	if err != nil {
		return send(&memqlv1.CreateAccountTokenResult{
			ErrorCode:    "internal",
			ErrorMessage: "id mint failed: " + err.Error(),
		})
	}

	store := &accounttoken.Store{Engine: s.service.engine, Logger: s.service.logger}
	if err := store.Create(ctx, identityId, accountId, label, hash, parseOptionalRFC3339(msg.GetExpiresAt())); err != nil {
		// The error is returned verbatim, and it cannot contain the
		// plaintext: Store.Create has no parameter for it.
		return send(&memqlv1.CreateAccountTokenResult{
			ErrorCode:    "persist_failed",
			ErrorMessage: err.Error(),
		})
	}

	canonical := accounttoken.CanonicalId(identityId)
	eventId := auditAccountToken(ctx, auditor, caller, "account_token_created",
		accountId, canonical, label, identity.AuditOutcomeSuccess, "")

	return send(&memqlv1.CreateAccountTokenResult{
		Success:    true,
		PlainToken: plain,
		IdentityId: canonical,
		AccountId:  accountId,
		// The credential's subject is the OPERATOR. Echoed so a client
		// cannot render "authenticated as <account>" without
		// contradicting a field the server handed it.
		SubjectUserId: callerUserId,
		AuditEventId:  eventId,
	})
}

// handleRevokeAccountToken flips active=false on the account_token
// identity row. Immediate and audited.
func (s *streamSession) handleRevokeAccountToken(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.RevokeAccountTokenMsg) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	send := func(result *memqlv1.RevokeAccountTokenResult) error {
		result.RequestId = requestId
		return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_RevokeAccountTokenResult{
				RevokeAccountTokenResult: result,
			},
		})
	}

	caller, err := auth.UserIdentityFromContext(s.stream.Context())
	if err != nil {
		return send(&memqlv1.RevokeAccountTokenResult{
			ErrorCode:    "unauthenticated",
			ErrorMessage: "caller identity not resolved",
		})
	}
	if strings.TrimSpace(caller.Subject) == "" {
		return send(&memqlv1.RevokeAccountTokenResult{
			ErrorCode:    "unauthenticated",
			ErrorMessage: "caller subject empty",
		})
	}

	identityId := strings.TrimSpace(msg.GetIdentityId())
	if identityId == "" {
		return send(&memqlv1.RevokeAccountTokenResult{
			ErrorCode:    "bad_request",
			ErrorMessage: "identity_id required",
		})
	}

	// After the client-side checks, for the reason the mint handler gives.
	if s.service.engine == nil {
		return send(&memqlv1.RevokeAccountTokenResult{
			ErrorCode:    "unavailable",
			ErrorMessage: "engine not configured",
		})
	}

	ctx := s.callerScopedContext()
	auditor := s.accountTokenAuditor()
	store := &accounttoken.Store{Engine: s.service.engine, Logger: s.service.logger}

	// Resolve the row AS THE CALLER. accountTokenById carries
	// userId==actor.userId, so a row that belongs to another operator
	// resolves to nothing and is refused with the same message a
	// nonexistent id gets.
	row, err := store.ByIdForCaller(ctx, identityId)
	if err != nil {
		return send(&memqlv1.RevokeAccountTokenResult{
			ErrorCode:    "lookup_failed",
			ErrorMessage: err.Error(),
		})
	}
	if row == nil {
		eventId := auditAccountToken(ctx, auditor, caller, "account_token_revoke_blocked",
			"", accounttoken.CanonicalId(identityId), "", identity.AuditOutcomeBlocked, "not_token_owner")
		return send(&memqlv1.RevokeAccountTokenResult{
			ErrorCode:    "permission_denied",
			ErrorMessage: "no account token with that id is yours",
			AuditEventId: eventId,
		})
	}

	if err := store.Revoke(ctx, identityId); err != nil {
		return send(&memqlv1.RevokeAccountTokenResult{
			ErrorCode:    "persist_failed",
			ErrorMessage: err.Error(),
		})
	}

	eventId := auditAccountToken(ctx, auditor, caller, "account_token_revoked",
		row.AccountId, row.ID, row.Label, identity.AuditOutcomeSuccess, "")
	return send(&memqlv1.RevokeAccountTokenResult{Success: true, AuditEventId: eventId})
}

// auditAccountToken writes one audit row and returns its correlation
// id, which the reply carries as audit_event_id.
//
// The correlation-id-as-reply-id shape is deploycontrol's
// (component/deploycontrol/service.go emitAudit): the sink mints the
// row's own primary key internally, so the value a caller can be
// handed is the correlationId, and the two are deliberately the same
// value here so a support conversation that starts with the id in the
// UI can find the row.
//
// category=auth and targetType=identity match every other credential
// path in the tree (pat_created / pat_revoked / node_token_revoked_admin).
// targetType is a CLOSED enum on createAuditEvent and does not carry an
// account-shaped member, so the account binding rides in `detail` --
// which is where it belongs anyway: the target of the event is the
// credential row, and the account is an attribute of it.
//
// The plaintext token is never a parameter here and never appears in
// `detail`. The label and the account id are the whole of what is
// recorded, and neither is secret.
func auditAccountToken(
	ctx context.Context,
	auditor identity.AuditLogger,
	caller auth.UserIdentity,
	action, accountId, identityId, label string,
	outcome identity.AuditOutcome,
	failureReason string,
) string {
	if auditor == nil {
		return ""
	}
	eventId := id.NewShortId()
	detail := map[string]any{}
	if accountId != "" {
		detail["accountId"] = accountId
	}
	if label != "" {
		detail["label"] = label
	}
	// Recorded on every event, including the blocked ones: the claim the
	// whole feature rests on is that the credential's subject is the
	// user, so the audit trail states it rather than leaving a reader to
	// infer it from actorUserId happening to match.
	detail["subjectKind"] = "user"
	detail["credentialFamily"] = "account_token"

	auditor.Log(ctx, identity.AuditEvent{
		OccurredAt:    time.Now().UTC(),
		Category:      identity.AuditCategoryAuth,
		Action:        action,
		ActorUserId:   caller.Subject,
		ActorEmail:    caller.Email,
		ActorRole:     caller.Role,
		TargetType:    "identity",
		TargetId:      identityId,
		Detail:        detail,
		Outcome:       outcome,
		FailureReason: failureReason,
		CorrelationId: eventId,
	})
	return eventId
}
