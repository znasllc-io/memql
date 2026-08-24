package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// mirror.go -- the connector's own write of a MirrorWrite.
//
// # Why this exists at all when the runtime has one
//
// Apply hands its writes to the runtime, which applies them behind the
// version guard (component/datasync). RECONCILE cannot: the contract's
// ReconcileReport carries counts rather than writes, because a sweep
// heals as it goes and reporting a million writes back through a struct
// would be a different design. So a reconciliation sweep writes its own
// rows, and this is that write.
//
// # Why it renders the same statement rather than importing the runtime
//
// component/datasync's own doc says it: keeping the contract in the leaf
// package is what lets integrations implement a Connector WITHOUT
// pulling the runtime in. A connector importing the runtime that calls
// it is a back-edge, and the next connector would inherit it.
//
// The cost is that one statement shape is written in two places, and
// TestReconcileHealWritesTheRuntimesStatementShape is what keeps them
// from drifting: it pins the rendered form, so a change to either side
// fails rather than producing two mirrors written two ways.

// writeMirror applies one MirrorWrite under the connector actor.
func (c *Connector) writeMirror(ctx context.Context, w memqlsync.MirrorWrite) error {
	stmt, err := mirrorInsert(w)
	if err != nil {
		return err
	}
	if _, err := c.engine.Execute(connectorContext(ctx), stmt); err != nil {
		// The statement is NOT logged: it embeds the mirrored payload,
		// and a mirrored payload carries customer names, addresses and
		// email addresses. The concept and the row id are enough to act
		// on, and the engine logs its own detail server-side.
		return fmt.Errorf("shopify: writing %s %q: %w", w.Concept, w.RowId, err)
	}
	return nil
}

// mirrorInsert renders the raw concept insert.
//
// Deliberately the same form datasync's EngineMirrorWriter renders:
// insert(<concept>, id=<row>, payload=<json>). A raw insert rather than
// a named mutation because the payload is written WHOLESALE -- which is
// what makes a field the origin cleared actually clear, instead of being
// merged forward from the previous fetch.
func mirrorInsert(w memqlsync.MirrorWrite) (string, error) {
	concept := strings.TrimSpace(w.Concept)
	rowID := strings.TrimSpace(w.RowId)
	if concept == "" || rowID == "" {
		return "", fmt.Errorf("shopify: a mirror write names no concept or no row")
	}
	payload := map[string]any{}
	for k, v := range w.Payload {
		payload[k] = v
	}
	if w.Retire {
		payload["deleted"] = true
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("shopify: encoding the payload for %s %q: %w", concept, rowID, err)
	}
	return fmt.Sprintf("insert(%s, id=%s, payload=%s)",
		langparser.QuoteString(concept), langparser.QuoteString(rowID), string(raw)), nil
}

// heal applies a batch of writes and reports how many landed.
//
// A single row's failure does NOT abort the sweep: one malformed record
// at the origin would otherwise stop every domain behind it, and a sweep
// that healed nothing because of one row is worse than one that healed
// all but that row and said so.
func (c *Connector) heal(ctx context.Context, writes []memqlsync.MirrorWrite) (healed int, failed int) {
	for _, w := range writes {
		if err := c.writeMirror(ctx, w); err != nil {
			failed++
			c.logger.Warn("shopify: reconcile could not heal a row",
				"concept", w.Concept, "row", w.RowId, "error", err)
			continue
		}
		healed++
	}
	return healed, failed
}
