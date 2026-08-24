package memql

// THE OUTBOX APPEND (epic memql#4378, D5).
//
// A concept whose dataState is `origin` is one MemQL owns and one or
// more external systems mirror. Every write to such a row has to reach
// those systems, and "has to reach" is a durability statement rather
// than a best-effort one: the connector may be down, the receiver may be
// rate-limiting, the node may restart mid-delivery. So the write appends
// one v1:platform:outboxEntry per @mirroredTo target, and a per-connector
// drain worker delivers them.
//
// # Why the append is in the write's own transaction
//
// The two failure directions are not symmetric, and only a transaction
// closes both:
//
//   - Append first, write second: an entry describing a change that
//     never happened. The receiver is handed a version MemQL does not
//     have, and the next reconciliation "heals" the mirror backwards.
//   - Write first, append second: a committed change that nothing will
//     ever propagate. Silent, permanent, and invisible until somebody
//     compares the two systems by hand.
//
// The second is the one that actually bites -- a process that dies
// between two statements dies there far more often than anywhere else --
// and it is the one an ordering trick cannot fix. So both writes go
// through one bun transaction, and a rollback takes the pair.
//
// # What this costs the rest of the tree: nothing
//
// The transaction is opened ONLY for a concept that is an origin WITH
// targets. Native and mirror concepts -- every concept in the tree today
// but the ones an author declares -- take the same single-statement path
// they took before this file existed, byte for byte. That is deliberate:
// the write path is the engine's hottest seam, and a change that made
// every insert transactional to serve a handful of concepts would be
// paying for the feature everywhere it is not used.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// OutboxEntryConcept is the canonical id of the outbox queue, spelled
// once.
const OutboxEntryConcept = "v1:platform:outboxEntry"

// outboxAction mirrors the concept's `action` enum.
const (
	outboxActionUpsert = "upsert"
	outboxActionRetire = "retire"
)

// outboxTargetsFor returns the mirror targets a write to this concept
// must be recorded against, or nil when the concept is not an origin.
//
// Reads the concept out of the registry rather than taking it from the
// caller, so the answer comes from the same declaration the write guard
// and row admission read.
func outboxTargetsFor(conceptName string) []string {
	c, err := memorynodes.Get(strings.TrimSpace(conceptName))
	if err != nil || c == nil {
		return nil
	}
	if c.DataState() != langparser.DataStateOrigin {
		return nil
	}
	return c.MirroredTo
}

// outboxIdempotencyKey renders the key every attempt at one entry
// presents to the receiver: (concept, row, version, target).
//
// Rendered ONCE, at append time, and stored -- not recomputed per
// attempt. A key recomputed from live state would change if any of its
// inputs did, and a changed key is a second delivery of the same change
// wearing a new name, which is exactly what the key exists to prevent.
func outboxIdempotencyKey(conceptName, rowId, version, target string) string {
	return fmt.Sprintf("%s|%s|%s|%s", conceptName, rowId, version, target)
}

// outboxEntryId derives a deterministic id for one (concept, row,
// version, target).
//
// Deterministic so that a retried WRITE -- the same mutation replayed by
// a client, which MemQL's content-addressed ids make land on the same row
// -- appends the same entry rather than a second one. The row store's
// ON CONFLICT DO NOTHING then makes the duplicate append a no-op instead
// of a duplicate delivery.
func outboxEntryId(conceptName, rowId, version, target string) string {
	sum := sha256.Sum256([]byte(outboxIdempotencyKey(conceptName, rowId, version, target)))
	return hex.EncodeToString(sum[:16])
}

// outboxRowVersion is the version stamped on an entry: the row's own
// createdAt in RFC3339 nanoseconds.
//
// MemQL is append-only and the primary key is (id, createdAt), so a
// row's createdAt IS its version -- there is no separate counter to
// invent, and using one would create a second answer to "which write is
// newer" that could disagree with the store's own ordering.
func outboxRowVersion(createdAt time.Time) string {
	return createdAt.UTC().Format(time.RFC3339Nano)
}

// appendOutboxEntries writes one entry per target for a row that was
// just written, inside the transaction `store` belongs to.
//
// `retire` reflects what the write MEANT rather than what it did: MemQL
// has no hard delete, so a retirement is an ordinary append that marks
// the row gone, and the connector needs to be told which it was.
func (e *MemQLEngine) appendOutboxEntries(
	ctx context.Context,
	store memorynodes.Store,
	conceptName, rowId string,
	createdAt time.Time,
	targets []string,
	retire bool,
) error {
	entryConcept, err := memorynodes.Get(OutboxEntryConcept)
	if err != nil || entryConcept == nil {
		// The queue concept is part of the engine's own DSL tree, so its
		// absence is a broken build rather than a configuration. Refuse
		// rather than drop the entries: a silently un-propagated change
		// is the failure this whole file exists to prevent.
		return fmt.Errorf(
			"outbox: concept %q is not registered, so a write to origin concept %q cannot be recorded for delivery to %s. "+
				"Refusing the write rather than committing a change nothing will propagate (epic memql#4378)",
			OutboxEntryConcept, conceptName, strings.Join(targets, ", "))
	}

	action := outboxActionUpsert
	if retire {
		action = outboxActionRetire
	}
	version := outboxRowVersion(createdAt)

	// The entries are written under the ENGINE's own identity, not the
	// caller's. The queue is the deployment's operational state and its
	// concept declares the clusterOwner tier; a user writing their own
	// row must not need cluster-owner rights for the engine to record
	// that the change has to go out.
	writeCtx := auth.ContextWithInternalOrigin(ctx)

	for _, target := range targets {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		payload := map[string]any{
			"conceptId":      conceptName,
			"rowRef":         rowId,
			"action":         action,
			"version":        version,
			"target":         target,
			"status":         "pending",
			"attempts":       0,
			"idempotencyKey": outboxIdempotencyKey(conceptName, rowId, version, target),
		}
		if _, err := entryConcept.Create(writeCtx, store, memorynodes.CreateParams{
			Actor:   outboxWriterActor,
			ID:      outboxEntryId(conceptName, rowId, version, target),
			Payload: payload,
		}); err != nil {
			return fmt.Errorf("outbox: recording %s of %s %q for %q: %w", action, conceptName, rowId, target, err)
		}
	}
	return nil
}

// outboxWriterActor is what a queue entry records as its author. Not a
// user id: the entry is the engine's own bookkeeping, and attributing it
// to whoever happened to make the change would make the audit trail read
// as if a person queued the delivery.
const outboxWriterActor = "system:outbox"

// runInWriteTx runs fn inside one database transaction, handing it a
// store bound to that transaction.
//
// Used ONLY on the origin-concept path. Every other write keeps the
// single-statement path, which is why this takes a closure instead of
// executeWrite being restructured around a transaction it usually does
// not need.
func (e *MemQLEngine) runInWriteTx(ctx context.Context, fn func(store memorynodes.Store) error) error {
	db := e.database()
	if db == nil {
		return fmt.Errorf("memory engine database not configured")
	}
	return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Constructed directly rather than through newBunStore, which
		// takes a *bun.DB. bun.Tx is a STRUCT VALUE, so it can never be
		// the typed nil that constructor exists to keep out of the
		// interface, and RunInTx only ever calls this with a live one.
		return fn(&bunStore{db: tx})
	})
}

// outboxPayloadRetires reports whether a written payload marks the row
// gone.
//
// There is no engine-wide "deleted" field, so this reads the two
// conventions the tree actually uses -- `deleted: true` and
// `active: false` -- and treats anything else as an upsert. Getting this
// wrong costs a connector an upsert where it wanted a retirement, which
// the next reconciliation corrects; getting it BACKWARDS would delete
// live data at the origin, so the default leans to upsert deliberately.
func outboxPayloadRetires(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	if v, ok := payload["deleted"]; ok && boolFromAny(v) {
		return true
	}
	if v, ok := payload["active"]; ok {
		if b, isBool := v.(bool); isBool && !b {
			return true
		}
	}
	return false
}

// marshalOutboxPayload is here for the drain worker's benefit: it hands
// Propagate the row's payload, and a connector that cannot read it
// cannot deliver it.
func marshalOutboxPayload(payload map[string]any) (json.RawMessage, error) {
	if payload == nil {
		return json.RawMessage("{}"), nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return raw, nil
}
