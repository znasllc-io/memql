package datasync

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
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
type Store struct {
	engine Engine
	// retryWait is the pause before a write that lost its row-version fence
	// is decided again (PatchSyncState); a test replaces it so it does not
	// sleep.
	retryWait func(attempt int)
}

// NewStore wraps an engine.
func NewStore(engine Engine) *Store {
	return &Store{engine: engine, retryWait: func(attempt int) { time.Sleep(retryWaitFor(attempt)) }}
}

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
	ID        string
	ConceptID string
	Connector string
	Direction string
	// Partition is the tenant of a memqlsync.Partitioned connector this
	// row reports on, "" for a connector without partitions. Stored as
	// partitionKey: the engine reserves `partition` as a payload field.
	Partition       string
	BackfillCursor  string
	BackfillStatus  string
	LastInboundAt   time.Time
	LagSeconds      int
	LastReconcileAt time.Time
	LastAttemptAt   time.Time
	DriftCount      int
	OutboxDepth     int
	DeadLetterCount int
	Paused          bool
	LastError       string
	// Stored says a row came back from the database; false is the empty
	// state SyncStateFor hands out for a domain nothing has written yet,
	// which already carries the id it WOULD have.
	Stored bool
	// Version is the stored version's time (row.createdAt), the witness a
	// partial write is fenced on (PatchSyncState). Zero for no row.
	Version time.Time
}

// SyncStateID is the deterministic row id for one domain.
//
// Deterministic so the append-only history of that id IS the health
// timeline for that domain: every write is another version of the same
// row rather than a new row nobody can correlate with the last one.
//
// It is derived through core/id from the tuple, not the tuple spelled out.
// The first form, "<concept>|<connector>|<direction>", carried the concept
// id's colons, and the engine's identifier rule
// (docs/public/concepts/identifiers.md) refuses a short id with a colon in
// it. Every health write was refused, every caller discarded the refusal,
// and a cluster ran for weeks with no health row at all: every domain due
// on every tick, no cadence honoured, no sweep failure ever shown. The tuple
// stays readable on the row's own fields.
//
// A partition (memqlsync.Partitioned) is appended as a FOURTH part, and an
// empty one appends nothing: every row written before partitions existed
// keeps the id it always had, and its history stays one timeline.
//
// It is memqlsync.StateID, which a connector reading its own health by id
// derives the same way.
func SyncStateID(conceptID, connector, direction, partition string) string {
	return memqlsync.StateID(conceptID, connector, direction, partition)
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

// StagedInboundRequest reads one staged delivery by its row id: the ONLY
// source of what the dispatcher hands a connector (epic memql#5477). The
// automation that fires on the row passes its id and nothing else, so the
// source, the body, the delivery metadata, the verifying tier and the
// receiver's signature verdict are the row's -- the values the receiver wrote
// under the @serverOnly stage -- and never an argument a caller could choose.
// Each field reads as the automation used to hand it over: as stored, untrimmed
// (the body is what the signature covered), an absent one as "".
func (s *Store) StagedInboundRequest(ctx context.Context, requestID string) (StagedRequest, bool, error) {
	rows, err := s.rows(ctx, call("query", "inboundRequestById", arg{"requestId", requestID}))
	if err != nil || len(rows) == 0 {
		return StagedRequest{}, false, err
	}
	return stagedFromRow(rows[0], requestID), true, nil
}

// stagedFromRow reads one inbound request row as the dispatcher works it.
func stagedFromRow(row map[string]any, requestID string) StagedRequest {
	return StagedRequest{
		RequestId:         requestID,
		Source:            argString(row, "source"),
		Body:              []byte(argString(row, "body")),
		HeadersJSON:       argString(row, "headersJson"),
		ReceivedAt:        argString(row, "receivedAt"),
		VerifiedBy:        argString(row, "verifiedBy"),
		SignatureVerified: boolField(row, "signatureVerified"),
		Attempts:          intField(row, "attempts"),
		Status:            argString(row, "status"),
		Version:           timeField(row, "createdAt"),
	}
}

// stagedFromRows reads a page of inbound request rows, each under its own id.
func stagedFromRows(rows []map[string]any) []StagedRequest {
	out := make([]StagedRequest, 0, len(rows))
	for _, row := range rows {
		out = append(out, stagedFromRow(row, stringField(row, "id")))
	}
	return out
}

// inboundRequestConcept is the concept the staged deliveries are rows of.
const inboundRequestConcept = "v1:platform:inboundRequest"

// InboundRequestsDueForRetry reads ONE page of the failed deliveries whose
// next try has come by asOf, oldest first, bodies included. Read fresh: a
// cached page could hand back rows another pass has already claimed.
func (s *Store) InboundRequestsDueForRetry(ctx context.Context, asOf time.Time) ([]StagedRequest, error) {
	rows, err := s.rows(memql.ContextWithFreshRead(ctx), call("query", "inboundRequestsDueForRetry",
		arg{"asOf", asOf.UTC().Format(time.RFC3339)}))
	if err != nil {
		return nil, err
	}
	return stagedFromRows(rows), nil
}

// ClaimInboundRetry moves a due delivery's next try to until, fenced on the
// version the due read returned, and reports whether this caller holds it. A
// claim another writer overtook -- another pass's claim, or any write of the
// row since it was read -- answers false and writes nothing. The claim names
// only the status (still failed) and the next try, so the try count is the
// row's: a pass that dies mid-dispatch leaves the row due again once until
// passes, with nothing spent.
func (s *Store) ClaimInboundRetry(ctx context.Context, staged StagedRequest, until time.Time) (bool, error) {
	fenced := memql.ContextWithRowVersionFence(ctx, inboundRequestConcept, staged.RequestId, staged.Version)
	_, err := s.exec(fenced, call("mutation", "updateInboundRequestStatus",
		arg{"requestId", staged.RequestId},
		arg{"status", "failed"},
		arg{"nextAttemptAt", until.UTC().Format(time.RFC3339)}))
	if errors.Is(err, memql.ErrRowVersionChanged) {
		return false, nil
	}
	return err == nil, err
}

// InboundRequestsGivenUp reads one page of one source's given-up deliveries,
// newest first and without their bodies. Read fresh, because Requeue reads it
// again after putting the last page back in line.
func (s *Store) InboundRequestsGivenUp(ctx context.Context, source string) ([]StagedRequest, error) {
	rows, err := s.rows(memql.ContextWithFreshRead(ctx), call("query", "inboundRequestsGivenUp", arg{"source", source}))
	if err != nil {
		return nil, err
	}
	return stagedFromRows(rows), nil
}

// RequeueInboundRequest puts one delivery back in line: no tries counted, due
// at at.
func (s *Store) RequeueInboundRequest(ctx context.Context, requestID string, at time.Time) error {
	_, err := s.exec(ctx, call("mutation", "requeueInboundRequest",
		arg{"requestId", requestID}, arg{"nextAttemptAt", at.UTC().Format(time.RFC3339)}))
	return err
}

// SyncStateFor reads one domain's health -- one partition's, for a
// partitioned connector -- or the zero value when it has never been written.
//
// It reads the row BY ITS ID (syncStateById), not by its fields: a row
// written before partitions has no partitionKey at all, and whether an
// absent field matches "" is not a rule to rely on. The id is what the
// partition derives, so the partition read back is the one asked for.
func (s *Store) SyncStateFor(ctx context.Context, conceptID, connector, direction, partition string) (SyncState, error) {
	partition = strings.TrimSpace(partition)
	stateID := SyncStateID(conceptID, connector, direction, partition)
	rows, err := s.rows(ctx, call("query", "syncStateById", arg{"stateId", stateID}))
	if err != nil {
		return SyncState{}, err
	}
	if len(rows) == 0 {
		return SyncState{
			ID:        stateID,
			ConceptID: conceptID,
			Connector: connector,
			Direction: direction,
			Partition: partition,
		}, nil
	}
	st := syncStateFromRow(rows[0])
	st.Partition = partition
	st.Stored = true
	return st, nil
}

// SyncStates reads every health row of one connector, every partition's,
// across every page of syncStatesAll.
//
// The page rides the context, as component/campaigns walks an audience: a
// read that stopped at the first page would silently drop the stores past
// the hundredth domain row.
func (s *Store) SyncStates(ctx context.Context, connector string) ([]SyncState, error) {
	var args []arg
	if c := strings.TrimSpace(connector); c != "" {
		args = append(args, arg{"connector", c})
	}
	q := call("query", "syncStatesAll", args...)
	var out []SyncState
	cursor := ""
	seen := map[string]bool{}
	for {
		res, err := s.exec(memql.ContextWithCursor(ctx, cursor), q)
		if err != nil {
			return nil, err
		}
		for _, r := range memql.MaterializeRows(res) {
			st := syncStateFromRow(r)
			st.Stored = true
			out = append(out, st)
		}
		next, err := memql.ResultPageCursor(res)
		if err != nil {
			return nil, fmt.Errorf("datasync: syncStatesAll: %w", err)
		}
		if next == "" {
			return out, nil
		}
		if seen[next] {
			return nil, fmt.Errorf("datasync: syncStatesAll: the engine handed back a cursor it had already given")
		}
		seen[next] = true
		cursor = next
	}
}

// WriteSyncState persists one domain's health.
func (s *Store) WriteSyncState(ctx context.Context, st SyncState) error {
	id := strings.TrimSpace(st.ID)
	if id == "" {
		id = SyncStateID(st.ConceptID, st.Connector, st.Direction, st.Partition)
	}
	args := []arg{
		{"stateId", id},
		{"conceptId", st.ConceptID},
		{"connector", st.Connector},
		{"direction", st.Direction},
		{"partitionKey", strings.TrimSpace(st.Partition)},
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
	if !st.LastAttemptAt.IsZero() {
		args = append(args, arg{"lastAttemptAt", st.LastAttemptAt.UTC().Format(time.RFC3339)})
	}
	_, err := s.exec(ctx, call("mutation", "upsertSyncState", args...))
	return err
}

// syncStateConcept is the concept the health rows are versions of.
const syncStateConcept = "v1:platform:syncState"

// SyncPatch is the fields of one domain's health row that ONE writer owns,
// by their DSL names: backfillCursor, backfillStatus, lastInboundAt,
// lagSeconds, lastReconcileAt, lastAttemptAt, driftCount, paused, lastError.
// A time is a time.Time.
type SyncPatch map[string]any

// maxPatchAttempts bounds how often a write overtaken by another writer of the
// same row is decided again.
const maxPatchAttempts = 5

// PatchSyncState writes the fields one writer owns onto a domain's row, and
// nothing else, so writers that run at once cannot erase each other's fields.
//
// WHY. The continuing pass, the sweep, the queue, the inbound dispatcher and
// the pause switch all write one row, and they run at once (two schedules
// fire in the same second, the scheduler runs automations in parallel). Each
// used to write the whole row from a copy it had read earlier, so the sweep
// put back the cursor it read before a backfill page, and every page put back
// the watermark and error the sweep had just recorded.
//
// HOW. decide is handed the row as it is now (read fresh) and answers the
// patch, or false to write nothing. On an existing row the patch is sent
// through patchSyncState, whose omitted arguments the engine's read-merge
// keeps; a domain with no row yet is created whole through upsertSyncState.
// The read-merge is not atomic on its own -- the engine reads the prior
// version outside the write's transaction (executeWrite) -- so the write
// carries a row-version fence on the version decide was handed. A write that
// another writer overtook is refused with memql.ErrRowVersionChanged, and the
// patch is decided again on a fresh read, at most maxPatchAttempts times.
// Deciding again matters as much as merging: the queue decides "not running"
// and the clean-sweep rule decides "not running" on what it read.
func (s *Store) PatchSyncState(ctx context.Context, conceptID, connector, direction, partition string, decide func(SyncState) (SyncPatch, bool)) error {
	partition = strings.TrimSpace(partition)
	for attempt := 0; attempt < maxPatchAttempts; attempt++ {
		if attempt > 0 && s.retryWait != nil {
			// The last write lost the fence: another writer of this row
			// committed first. Deciding again at once could lose to the next
			// write of a burst (inbound deliveries arrive in bursts) every
			// time, so each fresh decision waits a little longer, jittered.
			s.retryWait(attempt)
		}
		cur, err := s.SyncStateFor(memql.ContextWithFreshRead(ctx), conceptID, connector, direction, partition)
		if err != nil {
			return err
		}
		patch, ok := decide(cur)
		if !ok || len(patch) == 0 {
			return nil
		}
		id := SyncStateID(conceptID, connector, direction, partition)
		fenced := memql.ContextWithRowVersionFence(ctx, syncStateConcept, id, cur.Version)
		if cur.Stored {
			_, err = s.exec(fenced, call("mutation", "patchSyncState", patchArgs(id, patch)...))
		} else {
			cur.ConceptID, cur.Connector, cur.Direction, cur.Partition = conceptID, connector, direction, partition
			if cur.BackfillStatus == "" {
				cur.BackfillStatus = "none"
			}
			err = s.WriteSyncState(fenced, applyPatch(cur, patch))
		}
		if errors.Is(err, memql.ErrRowVersionChanged) {
			continue
		}
		return err
	}
	return fmt.Errorf("datasync: the health row of %s on %q%s kept changing under %d writes; nothing was written",
		conceptID, connector, partitionSuffix(partition), maxPatchAttempts)
}

// retryWaitBounds is the range the wait before fresh decision `attempt`
// (1-based) is drawn from: 2-4 ms, then 4-8, 8-16 and 16-32. A few
// milliseconds is longer than one health write takes, and the whole budget of
// four waits stays under a tenth of a second.
func retryWaitBounds(attempt int) (time.Duration, time.Duration) {
	if attempt < 1 {
		attempt = 1
	}
	lo := 2 * time.Millisecond << (attempt - 1)
	return lo, 2 * lo
}

// retryWaitFor draws one wait from retryWaitBounds, so writers that lost to
// each other do not decide again in step.
func retryWaitFor(attempt int) time.Duration {
	lo, hi := retryWaitBounds(attempt)
	return lo + time.Duration(rand.Int64N(int64(hi-lo)))
}

// patchArgs renders a patch in a stable order, times as RFC3339.
func patchArgs(id string, patch SyncPatch) []arg {
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := []arg{{"stateId", id}}
	for _, k := range keys {
		v := patch[k]
		switch t := v.(type) {
		case time.Time:
			v = t.UTC().Format(time.RFC3339)
		case string:
			if k == "lastError" {
				v = truncateError(t)
			}
		}
		args = append(args, arg{k, v})
	}
	return args
}

// applyPatch sets a patch's fields on a whole state, for a row's first write.
func applyPatch(st SyncState, patch SyncPatch) SyncState {
	for k, v := range patch {
		switch k {
		case "backfillCursor":
			st.BackfillCursor, _ = v.(string)
		case "backfillStatus":
			st.BackfillStatus, _ = v.(string)
		case "lastInboundAt":
			st.LastInboundAt, _ = v.(time.Time)
		case "lagSeconds":
			st.LagSeconds, _ = v.(int)
		case "lastReconcileAt":
			st.LastReconcileAt, _ = v.(time.Time)
		case "lastAttemptAt":
			st.LastAttemptAt, _ = v.(time.Time)
		case "driftCount":
			st.DriftCount, _ = v.(int)
		case "paused":
			st.Paused, _ = v.(bool)
		case "lastError":
			st.LastError, _ = v.(string)
		}
	}
	return st
}

// OperatorContext stamps the engine's own operator identity: a cluster
// owner acting for no user.
//
// This is the campaigns precedent, and the reason it is a helper rather
// than a context the Store builds for itself is stated at the top of this
// file -- whose authority a call runs under has to be visible at the call
// site.
//
// IT STAMPED ONE SURFACE AND COULD NOT WRITE (memql#5574). The body here set
// only the AccessContext, and `createdBy` resolves through ActorFromContext ->
// TokenInfoFromContext -- a different surface (memql#2989). So every read this
// package makes worked and every WRITE answered `no actor found in context`:
// WriteSyncState, SetPaused, and the outbox drain's own status writes. Measured
// against a real engine, both shapes side by side, before this was changed.
//
// Its consequence reached further than this package. The sync-health row is
// what MemQL OS's Data origins page reads to say whether a connector is
// reporting, so a runtime that cannot write one makes every connector on every
// cluster read as silent -- a false alarm on a healthy deployment, and
// indistinguishable from the real silence memql#5574 was about.
//
// Now the one shared definition, which stamps all three surfaces and carries
// Unranked and Synthetic. Both flags are corrections rather than changes here:
// the concepts this package writes (v1:platform:syncState, outboxEntry) are
// clusterOwner-tier with no owner field, so neither alters what a write lands
// -- they close the holes a rank-strict or owner-bearing concept would fall
// into later.
func OperatorContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return auth.ContextWithInternalOrigin(
		auth.ContextWithSystemActor(ctx, syncOperatorActorName))
}

// syncOperatorActor is the identity the runtime's own bookkeeping is
// written under. Prefixed so a `createdBy` on a queue or health row says
// plainly that the deployment wrote it, not a person.
const syncOperatorActor = "system:datasync"

// syncOperatorActorName is what this package calls itself to
// auth.SystemActor -- the part after `system:` in the id above. The literal
// above is kept so a test asserting a stored `createdBy` reads the exact
// string it asserts, and TestSyncOperatorActorMatchesTheSharedHelper holds
// the two together.
const syncOperatorActorName = "datasync"

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
		Partition:       stringField(r, "partitionKey"),
		BackfillCursor:  stringField(r, "backfillCursor"),
		BackfillStatus:  stringField(r, "backfillStatus"),
		LastInboundAt:   timeField(r, "lastInboundAt"),
		LagSeconds:      intField(r, "lagSeconds"),
		LastReconcileAt: timeField(r, "lastReconcileAt"),
		LastAttemptAt:   timeField(r, "lastAttemptAt"),
		DriftCount:      intField(r, "driftCount"),
		OutboxDepth:     intField(r, "outboxDepth"),
		DeadLetterCount: intField(r, "deadLetterCount"),
		Paused:          boolField(r, "paused"),
		LastError:       stringField(r, "lastError"),
		Version:         timeField(r, "createdAt"),
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
