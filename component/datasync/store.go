package datasync

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// store.go -- the engine seam for the sync runtime.
//
// Everything the runtime reads or writes goes through a NAMED DSL
// construct, never a hand-built query string, for the reason
// component/campaigns/store.go states: a named construct carries its
// filter (including the caller conjunct) and its declared row-authz
// binding, whereas a raw string has no binding to inject a tier from and
// is enforced only by the per-row admission gate. Reaching for a raw
// string here would quietly opt the runtime's own reads out of the
// narrower of the two mechanisms.
//
// The one deliberate exception is the connector's own sweep, which lives
// in the connector rather than here and documents its own reasoning.
//
// # The actor is the caller's business, not the store's
//
// No method here builds an actor context. Each issues its call under the
// ctx it is handed, and the callers are explicit about which of the two
// identities is in scope:
//
//   - the OPERATOR identity, for the engine's own clusterOwner-tier rows
//     -- the outbox queue, the health timeline, the staged inbound
//     requests;
//   - the CONNECTOR actor, for a write into a mirror, which is the only
//     identity a mirror will accept one from.
//
// Keeping that out of the store means a reader can see whose authority a
// given call runs under at the call site, which is the question this
// design turns on.

// Engine is the narrow engine surface the runtime needs. One method, so
// tests fake it with flat row envelopes -- the campaigns precedent.
type Engine interface {
	Execute(ctx context.Context, query string) (any, error)
}

// Store issues the sync domain's named constructs.
type Store struct{ engine Engine }

// NewStore wraps an engine.
func NewStore(engine Engine) *Store { return &Store{engine: engine} }

// OutboxEntry is one row of v1:platform:outboxEntry, in the shape the
// drain worker works with.
type OutboxEntry struct {
	ID             string
	ConceptID      string
	RowRef         string
	Action         string
	Version        string
	Target         string
	Status         string
	Attempts       int
	NextAttemptAt  time.Time
	LastError      string
	IdempotencyKey string
	CreatedAt      time.Time
}

// Due reports whether a worker may attempt this entry now.
//
// An entry with no nextAttemptAt is due: that is a fresh `pending` entry
// which has never been tried. Reading an absent time as "not due" would
// mean nothing is ever delivered on the first pass.
func (e OutboxEntry) Due(now time.Time) bool {
	if e.NextAttemptAt.IsZero() {
		return true
	}
	return !now.Before(e.NextAttemptAt)
}

// SyncState is one row of v1:platform:syncState.
type SyncState struct {
	ID              string
	ConceptID       string
	Connector       string
	Direction       string
	BackfillCursor  string
	BackfillStatus  string
	LastInboundAt   time.Time
	LagSeconds      int
	LastReconcileAt time.Time
	DriftCount      int
	OutboxDepth     int
	DeadLetterCount int
	Paused          bool
	LastError       string
}

// SyncStateID is the deterministic row id for one domain.
//
// Deterministic so the append-only history of that id IS the health
// timeline for that domain: every write is another version of the same
// row rather than a new row nobody can correlate with the last one.
func SyncStateID(conceptID, connector, direction string) string {
	return fmt.Sprintf("%s|%s|%s", strings.TrimSpace(conceptID), strings.TrimSpace(connector), strings.TrimSpace(direction))
}

// PendingOutbox returns the entries one connector still owes delivery
// on, oldest first.
func (s *Store) PendingOutbox(ctx context.Context, target string) ([]OutboxEntry, error) {
	rows, err := s.rows(ctx, call("query", "outboxPending", arg{"target", target}))
	if err != nil {
		return nil, err
	}
	return outboxEntriesFromRows(rows), nil
}

// DeadLetters returns one connector's dead-letter queue.
func (s *Store) DeadLetters(ctx context.Context, target string) ([]OutboxEntry, error) {
	rows, err := s.rows(ctx, call("query", "outboxDeadLetters", arg{"target", target}))
	if err != nil {
		return nil, err
	}
	return outboxEntriesFromRows(rows), nil
}

// MarkDelivering claims an entry, recording the attempt.
//
// The attempt is counted at CLAIM time rather than at outcome time, so a
// worker that dies mid-delivery still spends one of the entry's lives.
// Counting on the way out would give a crash-looping delivery infinite
// attempts, which is the one failure the ceiling exists to bound.
func (s *Store) MarkDelivering(ctx context.Context, entryID string, attempts int) error {
	_, err := s.exec(ctx, call("mutation", "markOutboxDelivering",
		arg{"entryId", entryID}, arg{"attempts", attempts}))
	return err
}

// MarkDelivered records a successful delivery.
func (s *Store) MarkDelivered(ctx context.Context, entryID string, at time.Time) error {
	_, err := s.exec(ctx, call("mutation", "markOutboxDelivered",
		arg{"entryId", entryID}, arg{"deliveredAt", at.UTC().Format(time.RFC3339)}))
	return err
}

// MarkFailed records a failed attempt and when to try again.
func (s *Store) MarkFailed(ctx context.Context, entryID string, nextAttemptAt time.Time, lastError string) error {
	_, err := s.exec(ctx, call("mutation", "markOutboxFailed",
		arg{"entryId", entryID},
		arg{"nextAttemptAt", nextAttemptAt.UTC().Format(time.RFC3339)},
		arg{"lastError", truncateError(lastError)}))
	return err
}

// MarkDead dead-letters an entry whose attempts are exhausted.
func (s *Store) MarkDead(ctx context.Context, entryID, lastError string) error {
	_, err := s.exec(ctx, call("mutation", "markOutboxDead",
		arg{"entryId", entryID}, arg{"lastError", truncateError(lastError)}))
	return err
}

// SyncStateFor reads one domain's health, or the zero value when the
// domain has never been written.
func (s *Store) SyncStateFor(ctx context.Context, conceptID, connector, direction string) (SyncState, error) {
	rows, err := s.rows(ctx, call("query", "syncStateFor",
		arg{"conceptId", conceptID}, arg{"connector", connector}, arg{"direction", direction}))
	if err != nil {
		return SyncState{}, err
	}
	if len(rows) == 0 {
		return SyncState{
			ID:        SyncStateID(conceptID, connector, direction),
			ConceptID: conceptID,
			Connector: connector,
			Direction: direction,
		}, nil
	}
	return syncStateFromRow(rows[0]), nil
}

// WriteSyncState persists one domain's health.
func (s *Store) WriteSyncState(ctx context.Context, st SyncState) error {
	id := strings.TrimSpace(st.ID)
	if id == "" {
		id = SyncStateID(st.ConceptID, st.Connector, st.Direction)
	}
	args := []arg{
		{"stateId", id},
		{"conceptId", st.ConceptID},
		{"connector", st.Connector},
		{"direction", st.Direction},
		{"backfillCursor", st.BackfillCursor},
		{"backfillStatus", st.BackfillStatus},
		{"lagSeconds", st.LagSeconds},
		{"driftCount", st.DriftCount},
		{"outboxDepth", st.OutboxDepth},
		{"deadLetterCount", st.DeadLetterCount},
		{"paused", st.Paused},
		{"lastError", truncateError(st.LastError)},
	}
	// Absent timestamps are sent as "" rather than as a zero time: the
	// engine's read-merge preserves a field the delta omits, but an
	// explicit zero time would overwrite a real one with 0001-01-01.
	if !st.LastInboundAt.IsZero() {
		args = append(args, arg{"lastInboundAt", st.LastInboundAt.UTC().Format(time.RFC3339)})
	}
	if !st.LastReconcileAt.IsZero() {
		args = append(args, arg{"lastReconcileAt", st.LastReconcileAt.UTC().Format(time.RFC3339)})
	}
	_, err := s.exec(ctx, call("mutation", "upsertSyncState", args...))
	return err
}

// SetPaused flips one domain's pause switch.
func (s *Store) SetPaused(ctx context.Context, stateID string, paused bool) error {
	_, err := s.exec(ctx, call("mutation", "setSyncPaused",
		arg{"stateId", stateID}, arg{"paused", paused}))
	return err
}

// OperatorContext stamps the engine's own operator identity: a cluster
// owner acting for no user.
//
// This is the campaigns precedent, and the reason it is a helper rather
// than a context the Store builds for itself is stated at the top of this
// file -- whose authority a call runs under has to be visible at the call
// site.
func OperatorContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: syncOperatorActor,
		Role:   auth.RoleOwner,
	})
	return auth.ContextWithInternalOrigin(ctx)
}

// syncOperatorActor is the identity the runtime's own bookkeeping is
// written under. Prefixed so a `createdBy` on a queue or health row says
// plainly that the deployment wrote it, not a person.
const syncOperatorActor = "system:datasync"

// ---- decoding ----

func outboxEntriesFromRows(rows []map[string]any) []OutboxEntry {
	out := make([]OutboxEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, OutboxEntry{
			ID:             stringField(r, "id"),
			ConceptID:      stringField(r, "conceptId"),
			RowRef:         stringField(r, "rowRef"),
			Action:         stringField(r, "action"),
			Version:        stringField(r, "version"),
			Target:         stringField(r, "target"),
			Status:         stringField(r, "status"),
			Attempts:       intField(r, "attempts"),
			NextAttemptAt:  timeField(r, "nextAttemptAt"),
			LastError:      stringField(r, "lastError"),
			IdempotencyKey: stringField(r, "idempotencyKey"),
			CreatedAt:      timeField(r, "createdAt"),
		})
	}
	return out
}

func syncStateFromRow(r map[string]any) SyncState {
	return SyncState{
		ID:              stringField(r, "id"),
		ConceptID:       stringField(r, "conceptId"),
		Connector:       stringField(r, "connector"),
		Direction:       stringField(r, "direction"),
		BackfillCursor:  stringField(r, "backfillCursor"),
		BackfillStatus:  stringField(r, "backfillStatus"),
		LastInboundAt:   timeField(r, "lastInboundAt"),
		LagSeconds:      intField(r, "lagSeconds"),
		LastReconcileAt: timeField(r, "lastReconcileAt"),
		DriftCount:      intField(r, "driftCount"),
		OutboxDepth:     intField(r, "outboxDepth"),
		DeadLetterCount: intField(r, "deadLetterCount"),
		Paused:          boolField(r, "paused"),
		LastError:       stringField(r, "lastError"),
	}
}

// ---- the engine calls ----

type arg struct {
	name  string
	value any
}

// call renders a named-construct invocation. Values are quoted through
// the parser's own quoter, so a value carrying a quote or a backslash
// cannot end the literal early and change what the engine executes.
func call(kind, name string, args ...arg) string {
	rendered := make([]string, 0, len(args))
	for _, a := range args {
		switch v := a.value.(type) {
		case string:
			rendered = append(rendered, a.name+": "+langparser.QuoteString(v))
		case int:
			rendered = append(rendered, fmt.Sprintf("%s: %d", a.name, v))
		case bool:
			rendered = append(rendered, fmt.Sprintf("%s: %t", a.name, v))
		default:
			rendered = append(rendered, a.name+": "+langparser.QuoteString(fmt.Sprintf("%v", v)))
		}
	}
	return fmt.Sprintf("%s %s(%s)", kind, name, strings.Join(rendered, ", "))
}

func (s *Store) rows(ctx context.Context, q string) ([]map[string]any, error) {
	res, err := s.exec(ctx, q)
	if err != nil {
		return nil, err
	}
	return memql.MaterializeRows(res), nil
}

func (s *Store) exec(ctx context.Context, q string) (any, error) {
	if s == nil || s.engine == nil {
		return nil, fmt.Errorf("datasync: no engine wired")
	}
	res, err := s.engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("datasync: %s: %w", firstWords(q), err)
	}
	return res, nil
}

// firstWords renders enough of a query to identify it in an error
// without echoing its arguments, which can carry an id or a cursor.
func firstWords(q string) string {
	if i := strings.IndexByte(q, '('); i > 0 {
		return q[:i]
	}
	if len(q) > 40 {
		return q[:40]
	}
	return q
}

// truncateError bounds what goes onto a row. An unbounded error string
// is a payload body one upstream error message away from being persisted,
// and the runtime does not persist payload bodies.
const maxStoredErrorLen = 500

func truncateError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxStoredErrorLen {
		return s
	}
	return s[:maxStoredErrorLen] + "..."
}
