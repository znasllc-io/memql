package worker

import (
	"context"
	"log/slog"

	"github.com/znasllc-io/memql/component/identity"
)

// IdentityAuditor bridges worker.AuditEvent into the identity audit
// pipeline (v1:identity:auditEvent rows + slog stream). The agent
// node wires its identity.AuditLogger through here so worker
// security events live alongside other audit traffic with the same
// retention + admin UI surface.
type IdentityAuditor struct {
	Logger      *slog.Logger
	AuditLogger identity.AuditLogger
}

// Emit translates a worker.AuditEvent to identity.AuditEvent and
// dispatches it. No-op when AuditLogger is nil (lets the agent node
// run audit-free in dev mode).
//
// ActorIdentityId goes to the identity field as-is and ActorLabel to
// detail.actor, on a COPY of the caller's detail: emitters build that map
// for this one call today, but a bridge that writes into a map it was
// handed is one shared map away from stamping a label on somebody else's
// event.
func (a *IdentityAuditor) Emit(ctx context.Context, ev AuditEvent) {
	if a == nil || a.AuditLogger == nil {
		return
	}
	detail := ev.Detail
	if ev.ActorLabel != "" {
		detail = make(map[string]any, len(ev.Detail)+1)
		for k, v := range ev.Detail {
			detail[k] = v
		}
		detail["actor"] = ev.ActorLabel
	}
	out := identity.AuditEvent{
		OccurredAt:    ev.Timestamp,
		Category:      identity.AuditCategoryAuthorization,
		Action:        ev.Action,
		ActorUserId:   "",
		ActorIdentity: ev.ActorIdentityId,
		TargetType:    ev.TargetType,
		TargetId:      ev.Target,
		Detail:        detail,
		CorrelationId: ev.CorrelationId,
		Outcome:       identity.AuditOutcomeSuccess,
	}
	if ev.OwnerUserId != "" {
		out.ActorUserId = ev.OwnerUserId
	}
	a.AuditLogger.Log(ctx, out)
}

// NoopAuditor discards every event. Used in tests + the dev-mode
// startup path that wants the worker subsystem alive without an
// audit pipeline.
type NoopAuditor struct{}

// Emit is the no-op implementation.
func (NoopAuditor) Emit(_ context.Context, _ AuditEvent) {}
