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
func (a *IdentityAuditor) Emit(ctx context.Context, ev AuditEvent) {
	if a == nil || a.AuditLogger == nil {
		return
	}
	out := identity.AuditEvent{
		OccurredAt:    ev.Timestamp,
		Category:      identity.AuditCategoryAuthorization,
		Action:        ev.Action,
		ActorUserId:   "",
		ActorIdentity: ev.Actor,
		TargetType:    ev.TargetType,
		TargetId:      ev.Target,
		Detail:        ev.Detail,
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
