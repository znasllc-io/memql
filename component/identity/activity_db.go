package identity

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/id"
)

// ActivitySink writes AuditEvents carrying Stream == StreamActivity into
// v1:identity:authActivity via createAuthActivity (memql#4328). The sibling of
// EngineAuditSink, and deliberately a separate type rather than a mode on it:
// the two concepts have different columns, different retention and different
// authorization, and one sink with a branch would have to keep all three
// straight on every field.
type ActivitySink struct {
	Engine EngineExecutor
	Logger *slog.Logger
}

// WriteAuditEvent implements AuditDBSink.
//
// THE ACTOR IS BORROWED, and that is the load-bearing part. authActivity
// declares @rowAuthz(owner="actorUserId", clusterOwner), so createAuthActivity
// stamps the owner field from actor.userId and never accepts it from args --
// TestDeclaredOwnerFieldsAreServerStamped hard-fails any declared owner field a
// mutation writes from caller args, and its exemption list is empty.
//
// But /auth/refresh is UNAUTHENTICATED by construction: it is the request that
// mints the credential, so the inbound context carries no AccessContext at all.
// What the identity service does have is the session row it just resolved,
// which names the user. So the write runs under that user's actor -- the engine
// borrowing the row owner's authority for a write on their behalf, the same
// shape component/campaigns/worker.go uses for a campaign's owner. Nothing here
// can name a user the caller could not: the id comes off a session row the
// verification path already resolved, never off a request field.
//
// A blank ActorUserId leaves the context untouched (ContextWithUserActor
// returns it unchanged), so the row lands with an empty owner and is a cluster
// owner's to read. That is the session_not_found case, where nothing resolved
// and there is no user to attribute the attempt to.
func (s *ActivitySink) WriteAuditEvent(ctx context.Context, ev AuditEvent) error {
	if s == nil || s.Engine == nil {
		return nil
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	activityId := id.NewShortId()

	writeCtx := ctx
	if strings.TrimSpace(ev.ActorUserId) != "" {
		writeCtx = auth.ContextWithUserActor(ctx, ev.ActorUserId)
	}
	// INTERNAL ORIGIN, and it is not optional. createAuthActivity is
	// @serverOnly, auth.OriginFromContext DEFAULTS to OriginClient, and
	// component/memql/engine.go refuses a @serverOnly construct on any origin
	// that is not internal -- so without this stamp every activity write is
	// refused at execute, leaving one WARN per rotation and an empty log. The
	// stamp goes AFTER the actor so the borrowed identity survives it; the two
	// carry different context keys and neither displaces the other, but the
	// ordering says which is which.
	writeCtx = auth.ContextWithInternalOrigin(writeCtx)

	if _, err := s.Engine.Execute(writeCtx, s.statementFor(ev, activityId)); err != nil {
		return fmt.Errorf("identity.activity: execute createAuthActivity: %w", err)
	}
	return nil
}

// statementFor renders the createAuthActivity call. Split out so the rendering
// can be parsed by a test through the real front end WITHOUT a database --
// component/identity/activity_sink_test.go. A sink covered only against a fake
// engine that records query strings and parses nothing is how five guest
// mutations shipped rendering a form the parser had rejected for months
// (memql#4256).
//
// TargetId carries the session id: every activity writer sets TargetType
// "session", so reusing the field keeps one AuditEvent shape across both
// streams instead of adding a second session-id member that only one sink
// reads.
func (s *ActivitySink) statementFor(ev AuditEvent, activityId string) string {
	var b strings.Builder
	b.WriteString(`mutation createAuthActivity(`)
	writeKVString(&b, "activityId", activityId, true)
	writeKVString(&b, "occurredAt", ev.OccurredAt.UTC().Format(time.RFC3339Nano), false)
	writeKVString(&b, "action", ev.Action, false)
	writeKVString(&b, "outcome", string(ev.Outcome), false)
	writeKVStringOpt(&b, "sessionId", ev.TargetId)
	writeKVStringOpt(&b, "actorEmail", ev.ActorEmail)
	writeKVStringOpt(&b, "actorRole", ev.ActorRole)
	writeKVStringOpt(&b, "actorIdentityId", ev.ActorIdentity)
	writeKVStringOpt(&b, "sourceIP", ev.SourceIP)
	writeKVStringOpt(&b, "userAgent", ev.UserAgent)
	writeKVStringOpt(&b, "clientLabel", ev.ClientLabel)
	writeKVStringOpt(&b, "failureReason", ev.FailureReason)
	writeKVStringOpt(&b, "retiredTokenHash", ev.RetiredHash)
	if detailJSON := encodeDetail(ev.Detail); detailJSON != "" {
		b.WriteString(`,detail: `)
		b.WriteString(detailJSON)
	}
	b.WriteString(`)`)
	return b.String()
}
