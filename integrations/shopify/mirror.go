package shopify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
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
// rows, and this is that write -- behind the same guard, by the contract's
// own rule (memqlsync.Refuses), not a copy of it.
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
// insert(<concept>, id=<row>, payload=<json>). A raw insert rather than a
// named mutation, so the payload is stated in full on every write. Note
// what the engine does with it on an EXISTING row: it read-merges, and an
// omitted top-level field keeps its stored value (memql#1709). The mapper
// therefore states every field it fetched, writing "" for a string the
// origin cleared; a cleared field of another type keeps its last value
// until the engine has a way to unset one.
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
//
// A WRITE THE ROW OUTRANKS IS REFUSED, by the guard's own rule
// (memqlsync.Refuses over memqlsync.StoredStamp, which the runtime's applier
// asks too). A sweep's page is read before it is written, and a delivery can
// record a newer state in between; written unguarded, the sweep put the older
// one back. Whether a record exists is judged by when the origin was read: a
// relist's tombstone (the listing's start) is refused by a row read after its
// listing began, and a sweep that reads a record after a tombstone's time
// puts it back. Which of two live states is newer is the origin's version. A
// row written before every write carried them holds none and is replaced.
//
// A ROW THE MIRROR ALREADY HOLDS IS NOT WRITTEN AGAIN. A sweep stamps every
// row it maps with syncedAt = the fetch time, and a type Shopify publishes no
// version for (about forty of the sixty-five, originVersion) with that time
// as updatedAt too, so every pass appended a version of every row whether or
// not the origin had changed anything: about 3,400 a day each for
// quantityRule and priceListPrice across 170 rows on production (2026-11-06),
// re-listed hourly. mirrorPayloadHolds skips the write when the stored row
// already carries every value the write states, syncedAt aside, and updatedAt
// aside when it is only the time of reading (fetchTimeVersion). With the
// origin's version on both sides, updatedAt IS compared: it moves only when
// Shopify changed the row. versionReadAt is the read time's pair and moves
// with it, so it is not compared either. A row written before versionSource
// existed lacks it, so the write's versionSource differs and the row is
// rewritten once. A skip leaves the row's read time where the last write put
// it, which makes a later tombstone easier to apply, never harder; the next
// read that finds the record puts it back.
//
// What a skip costs: syncedAt on a row a pass found unchanged keeps the time
// of the last write that changed it, so it says "last changed here", not
// "last confirmed against the origin". Nothing reads it to judge freshness:
// the store's health reports the domain's sync state (lastReconcileAt and
// the pass's drift count), never a row's syncedAt. And `healed` -- with the
// Drifted count the report carries -- counts the rows a pass actually
// changed, which is what drift was always documented to mean.
//
// The row is read once, for both questions, under the connector actor, the
// one identity the mirror admits. A read that fails writes nothing: without
// the stored row the guard cannot be asked, and an unguarded write is the
// fault this check exists for. So does a read that answers two rows for the
// one id: the raw read is by concept and id, and two answers mean the read is
// not the row this write would replace.
//
// EACH WRITE IS FENCED ON THE ROW IT WAS JUDGED AGAINST (healOne), so a
// delivery that commits between the read and the write is not replaced by the
// older state the page carried.
func (c *Connector) heal(ctx context.Context, writes []memqlsync.MirrorWrite) (healed int, failed int) {
	for _, w := range writes {
		wrote, err := c.healOne(ctx, w)
		if err != nil {
			failed++
			c.logger.Warn("shopify: reconcile could not heal a row; not written",
				"concept", w.Concept, "row", w.RowId, "error", err)
			continue
		}
		if wrote {
			healed++
		}
	}
	return healed, failed
}

// healOne judges one write against the row the mirror holds and writes it
// unless the row refuses it or already holds it, and reports whether it wrote.
//
// The read and the write are two statements. A delivery can commit a newer
// state in between, and the write this page was admitted for would put the
// older one back, on a domain whose next sweep reads only what changed since.
// So the write carries the judged row's version
// (memql.ContextWithRowVersionFence; a row that was not there must still not
// be), and the engine refuses it when the row has moved. The row is then read
// and judged again, ONCE; a second loss is this row's failure, and the sweep
// that counts it holds its watermark.
func (c *Connector) healOne(ctx context.Context, w memqlsync.MirrorWrite) (bool, error) {
	for try := 0; ; try++ {
		stored, version, err := c.storedMirrorRow(ctx, w)
		if err != nil {
			return false, fmt.Errorf("reading the row before healing it: %w", err)
		}
		if stored != nil {
			spec := mirrorDomain(w.Concept)
			if memqlsync.Refuses(memqlsync.StoredStamp(spec, stored), memqlsync.WriteStamp(spec, w)) {
				return false, nil
			}
			if !w.Retire && mirrorPayloadHolds(stored, w.Payload, fetchTimeVersion(w)) {
				return false, nil
			}
		}
		fenced := ctx
		if stored == nil || !version.IsZero() {
			// An existing row whose read carried no version cannot be fenced
			// on it: a zero version means "this row must not exist".
			fenced = memql.ContextWithRowVersionFence(ctx, strings.TrimSpace(w.Concept), strings.TrimSpace(w.RowId), version)
		}
		err = c.writeMirror(fenced, w)
		if err == nil {
			return true, nil
		}
		if try == 0 && errors.Is(err, memql.ErrRowVersionChanged) {
			continue
		}
		return false, err
	}
}

// storedMirrorRow reads the payload the mirror holds for w's row and that
// row's version (the stored version's time), or nil for a row it does not
// hold.
//
// The read is the runtime's own raw concept read (component/datasync
// StoredRow), so it needs no generated query and no cluster-owner filter
// the connector actor would fail, and it goes to the database rather than to
// a replica's result cache (memql.ContextWithFreshRead): a sibling's eviction
// is asynchronous, and a stale cached row could hide a change. A bundle with
// two nodes for the id is an error (heal).
func (c *Connector) storedMirrorRow(ctx context.Context, w memqlsync.MirrorWrite) (map[string]any, time.Time, error) {
	concept, rowID := strings.TrimSpace(w.Concept), strings.TrimSpace(w.RowId)
	if concept == "" || rowID == "" || c.engine == nil {
		return nil, time.Time{}, nil
	}
	q := fmt.Sprintf("concept==%s && id==%s", concept, langparser.QuoteString(rowID))
	res, err := c.engine.Execute(memql.ContextWithFreshRead(connectorContext(ctx)), q)
	if err != nil {
		return nil, time.Time{}, err
	}
	if res == nil || res.Bundle == nil {
		return nil, time.Time{}, nil
	}
	// The row is matched by its own id rather than taken as the only node:
	// an id is bare-ified on egress, and a bundle may carry other nodes.
	var stored map[string]any
	var version time.Time
	for _, node := range res.Bundle.GetNodes() {
		if memql.BareShortId(node.GetId()) != memql.BareShortId(rowID) {
			continue
		}
		if stored != nil {
			return nil, time.Time{}, fmt.Errorf("shopify: the mirror answered two rows for %s %q", concept, rowID)
		}
		stored = node.GetPayload().AsMap()
		if at := node.GetCreatedAt(); at != nil {
			version = at.AsTime()
		}
	}
	return stored, version, nil
}

// fetchTimeVersion reports whether the write's updatedAt is the TIME OF
// READING rather than the origin's version: the type publishes none, and no
// parent handed it one (originVersion). Such an updatedAt moves on every pass
// and says nothing about the row, so it is not compared. An origin version IS
// compared: it moves only when Shopify changed the row, and the guard reads
// it.
func fetchTimeVersion(w memqlsync.MirrorWrite) bool {
	source, _ := w.Payload["versionSource"].(string)
	return source != memqlsync.VersionFromOrigin
}

// mirrorPayloadHolds compares a write with the stored payload the way the raw
// insert would apply it: a top-level field the write omits keeps its stored
// value, so only the fields it states are compared; a nested object it states
// replaces the stored one whole, so a nested object must match key for key.
// syncedAt and versionReadAt are the pass's own stamps and are never compared.
func mirrorPayloadHolds(stored, write map[string]any, ignoreUpdatedAt bool) bool {
	if stored == nil {
		return false
	}
	for key, value := range write {
		if key == "syncedAt" || key == "versionReadAt" || (key == "updatedAt" && ignoreUpdatedAt) {
			continue
		}
		if !sameMirrorValue(stored[key], value) {
			return false
		}
	}
	return true
}

// sameMirrorValue compares one stored value with one written value as JSON.
//
// The two sides arrive in different Go types for the same JSON -- the stored
// one through a protobuf Struct (every number a float64), the written one from
// the Admin decode (json.Number, strings, nested maps) -- so both are
// re-encoded and decoded with UseNumber before comparing. Three equivalences
// are allowed and nothing more: an absent value, null, "" and an empty list
// are one unset value, as the engine reads them; two numbers are equal when
// they are the same rational number (a 64-bit id that lost digits in a float64
// is NOT equal, which makes it a write, the safe direction); and two RFC3339
// strings naming the same instant are equal, in case a time is stored in a
// different spelling than the origin sent.
func sameMirrorValue(stored, written any) bool {
	a, okA := mirrorJSON(stored)
	b, okB := mirrorJSON(written)
	return okA && okB && sameJSONValue(a, b)
}

func mirrorJSON(v any) (any, bool) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var out any
	if err := d.Decode(&out); err != nil {
		return nil, false
	}
	return out, true
}

func unsetMirrorValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	}
	return false
}

func sameJSONValue(a, b any) bool {
	if unsetMirrorValue(a) || unsetMirrorValue(b) {
		return unsetMirrorValue(a) && unsetMirrorValue(b)
	}
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		if x == y {
			return true
		}
		rx, okX := new(big.Rat).SetString(string(x))
		ry, okY := new(big.Rat).SetString(string(y))
		return okX && okY && rx.Cmp(ry) == 0
	case string:
		y, ok := b.(string)
		if !ok {
			return false
		}
		if x == y {
			return true
		}
		tx, errX := time.Parse(time.RFC3339Nano, x)
		ty, errY := time.Parse(time.RFC3339Nano, y)
		return errX == nil && errY == nil && tx.Equal(ty)
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !sameJSONValue(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			return false
		}
		for key, value := range x {
			if !sameJSONValue(value, y[key]) {
				return false
			}
		}
		for key, value := range y {
			if _, seen := x[key]; !seen && !unsetMirrorValue(value) {
				return false
			}
		}
		return true
	}
	return false
}
