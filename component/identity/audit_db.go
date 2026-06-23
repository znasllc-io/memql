package identity

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// EngineExecutor is the narrow interface this package depends on for
// running mutations / queries against the memQL engine.
// *memql.MemQLEngine satisfies this directly.
//
// Returning *memql.ExecuteResult (rather than `any`) lets the Store
// parse shaped query results without a runtime type assertion at every
// call site. The package still depends on memql, but only on this one
// concrete type — every other engine surface is hidden.
type EngineExecutor interface {
	Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error)
}

// EngineAuditSink writes AuditEvents into v1:identity:auditEvent via
// createAuditEvent. Replaces NoopAuditDBSink in any build that
// has an engine wired up.
type EngineAuditSink struct {
	Engine EngineExecutor
	Logger *slog.Logger
}

// WriteAuditEvent implements AuditDBSink. Emits a single
// createAuditEvent call. Errors are returned to the caller
// (SlogAuditLogger logs them at warn level so the slog stream stays
// the canonical audit destination even when the DB write fails).
func (s *EngineAuditSink) WriteAuditEvent(ctx context.Context, ev AuditEvent) error {
	if s == nil || s.Engine == nil {
		return nil
	}

	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}

	eventId, err := newAuditEventId()
	if err != nil {
		return fmt.Errorf("identity.audit: generate event id: %w", err)
	}

	detailJSON := encodeDetail(ev.Detail)

	// Build the mutation argument list. Required fields come first;
	// optional fields are appended only when non-empty so we minimize
	// the wire payload and avoid passing empty strings through the
	// mutation's args block.
	var b strings.Builder
	b.WriteString(`createAuditEvent({`)
	writeKVString(&b, "eventId", eventId, true)
	writeKVString(&b, "occurredAt", ev.OccurredAt.UTC().Format(time.RFC3339Nano), false)
	writeKVString(&b, "category", string(ev.Category), false)
	writeKVString(&b, "action", ev.Action, false)
	writeKVStringOpt(&b, "actorUserId", ev.ActorUserId)
	writeKVStringOpt(&b, "actorEmail", ev.ActorEmail)
	writeKVStringOpt(&b, "actorRole", ev.ActorRole)
	writeKVStringOpt(&b, "actorIdentityId", ev.ActorIdentity)
	writeKVStringOpt(&b, "targetType", ev.TargetType)
	writeKVStringOpt(&b, "targetId", ev.TargetId)
	writeKVStringOpt(&b, "targetEmail", ev.TargetEmail)
	writeKVStringOpt(&b, "sourceIP", ev.SourceIP)
	writeKVStringOpt(&b, "userAgent", ev.UserAgent)
	writeKVStringOpt(&b, "correlationId", ev.CorrelationId)
	writeKVStringOpt(&b, "outcome", string(ev.Outcome))
	writeKVStringOpt(&b, "failureReason", ev.FailureReason)
	writeKVStringOpt(&b, "prevEventHash", ev.PrevEventHash)
	if detailJSON != "" {
		// detail is an object, not a string. The DSL accepts the JSON
		// literal inline.
		b.WriteString(`,"detail":`)
		b.WriteString(detailJSON)
	}
	b.WriteString(`})`)

	if _, err := s.Engine.Execute(ctx, b.String()); err != nil {
		return fmt.Errorf("identity.audit: execute createAuditEvent: %w", err)
	}
	return nil
}

// escapeMemQLString escapes a string for embedding inside a `"..."`
// literal in a MemQL function-call argument. We keep this conservative
// and quote the same characters JSON does so the parser never
// re-interprets a stray backslash or quote in the payload.
func escapeMemQLString(s string) string {
	if s == "" {
		return ""
	}
	out := make([]byte, 0, len(s)+8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			out = append(out, '\\', '\\')
		case '"':
			out = append(out, '\\', '"')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			out = append(out, c)
		}
	}
	return string(out)
}

// writeKVString writes a JSON-style `"key":"value"` pair. first=true
// suppresses the leading comma.
func writeKVString(b *strings.Builder, key, value string, first bool) {
	if !first {
		b.WriteByte(',')
	}
	b.WriteByte('"')
	b.WriteString(key)
	b.WriteString(`":"`)
	b.WriteString(escapeMemQLString(value))
	b.WriteByte('"')
}

// writeKVStringOpt writes the pair only when value is non-empty.
func writeKVStringOpt(b *strings.Builder, key, value string) {
	if value == "" {
		return
	}
	writeKVString(b, key, value, false)
}

// writeKVInt writes a JSON-style `"key":<int>` pair (no quotes around
// the value). Mirrors writeKVString for numeric concept fields.
func writeKVInt(b *strings.Builder, key string, value int, first bool) {
	if !first {
		b.WriteByte(',')
	}
	b.WriteByte('"')
	b.WriteString(key)
	b.WriteString(`":`)
	b.WriteString(strconv.Itoa(value))
}

// newAuditEventId returns a fresh opaque shortId for an auditEvent
// row. Delegates to core/id.NewShortId so the auditEvent format
// matches every other instance row in the system (UUIDv4). The
// canonical id format already carries `:auditEvent:` in the middle
// position; the shortId stays a plain opaque token. Error return
// preserved for source compatibility -- always nil.
func newAuditEventId() (string, error) {
	return id.NewShortId(), nil
}
