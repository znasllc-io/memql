package identity

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// AuditCategory mirrors the enum on v1:identity:auditEvent.category.
type AuditCategory string

const (
	AuditCategoryAuth          AuditCategory = "auth"
	AuditCategoryIdentity      AuditCategory = "identity"
	AuditCategoryAuthorization AuditCategory = "authorization"
	AuditCategoryConfiguration AuditCategory = "configuration"
	AuditCategoryAdmin         AuditCategory = "admin"
	AuditCategoryData          AuditCategory = "data"
)

// AuditOutcome mirrors the enum on v1:identity:auditEvent.outcome.
type AuditOutcome string

const (
	AuditOutcomeSuccess AuditOutcome = "success"
	AuditOutcomeFailure AuditOutcome = "failure"
	AuditOutcomeBlocked AuditOutcome = "blocked"
	AuditOutcomeNone    AuditOutcome = ""
)

// AuditEvent is the in-memory representation of an audit-log entry.
// Mirrors the fields on the v1:identity:auditEvent concept. The
// AuditLogger emits these to BOTH a slog stream (for unbounded
// retention via the operator's external log infrastructure) and to
// the database via a memql mutation (for the in-app admin view +
// 365-day default retention).
type AuditEvent struct {
	OccurredAt    time.Time
	Category      AuditCategory
	Action        string
	ActorUserId   string
	ActorEmail    string
	ActorRole     string
	ActorIdentity string
	TargetType    string
	TargetId      string
	TargetEmail   string
	Detail        map[string]any
	SourceIP      string
	UserAgent     string
	CorrelationId string
	Outcome       AuditOutcome
	FailureReason string
	PrevEventHash string
}

// AuditLogger is the interface call sites depend on. Keeping it as
// an interface lets us swap the DB sink for a mock in unit tests.
type AuditLogger interface {
	Log(ctx context.Context, ev AuditEvent)
}

// AuditDBSink writes events to the v1:identity:auditEvent table via
// a mutation. Phase 2 wires the actual mutation; for Phase 1 the
// sink is plumbed through but a no-op DB writer is acceptable since
// no audit-relevant flows have shipped yet.
type AuditDBSink interface {
	WriteAuditEvent(ctx context.Context, ev AuditEvent) error
}

// SlogAuditLogger emits audit events to a structured slog logger
// AND optionally to a DB sink. The slog stream is the canonical
// always-on destination (operator log retention applies); the DB
// sink is best-effort and any error is logged at warn but does not
// break the audit chain.
type SlogAuditLogger struct {
	Logger *slog.Logger
	DB     AuditDBSink // nil = slog-only
}

// Log implements AuditLogger.
func (s *SlogAuditLogger) Log(ctx context.Context, ev AuditEvent) {
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	if s.Logger != nil {
		s.Logger.LogAttrs(ctx, slog.LevelInfo, "audit",
			slog.Time("occurred_at", ev.OccurredAt),
			slog.String("category", string(ev.Category)),
			slog.String("action", ev.Action),
			slog.String("actor_user_id", ev.ActorUserId),
			slog.String("actor_email", ev.ActorEmail),
			slog.String("actor_role", ev.ActorRole),
			slog.String("actor_identity_id", ev.ActorIdentity),
			slog.String("target_type", ev.TargetType),
			slog.String("target_id", ev.TargetId),
			slog.String("target_email", ev.TargetEmail),
			slog.String("source_ip", ev.SourceIP),
			slog.String("user_agent", ev.UserAgent),
			slog.String("correlation_id", ev.CorrelationId),
			slog.String("outcome", string(ev.Outcome)),
			slog.String("failure_reason", ev.FailureReason),
			slog.String("detail_json", encodeDetail(ev.Detail)),
		)
	}
	if s.DB != nil {
		if err := s.DB.WriteAuditEvent(ctx, ev); err != nil && s.Logger != nil {
			s.Logger.WarnContext(ctx, "audit_db_write_failed",
				slog.String("action", ev.Action),
				slog.String("error", err.Error()),
			)
		}
	}
}

// NoopAuditDBSink swallows audit events. Used in early-phase wiring
// before the v1:identity:auditEvent mutations land. Lets the rest
// of the codebase code against AuditLogger without conditionals.
type NoopAuditDBSink struct{}

// WriteAuditEvent is a no-op.
func (NoopAuditDBSink) WriteAuditEvent(context.Context, AuditEvent) error { return nil }

// encodeDetail serializes the Detail map for the slog field. Returns
// "" when the map is empty so log lines stay tidy.
func encodeDetail(detail map[string]any) string {
	if len(detail) == 0 {
		return ""
	}
	out, err := json.Marshal(detail)
	if err != nil {
		return ""
	}
	return string(out)
}
