package memql

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// Keyset (cursor) pagination — the engine's runtime continuation primitive.
//
// Authors keep declaring `sort` + `paginate <pageSize>`; the engine emits a
// `nextCursor` on a full page and accepts an inbound cursor to continue. The
// cursor is opaque to clients: a base64url-encoded JSON envelope carrying the
// keyset position `(createdAt, id)` of the last row on the previous page plus
// a short hash of the query's sort signature. A cursor minted under one `sort`
// replayed against a different ordering is rejected with a typed error
// (ErrCursorSortMismatch) rather than silently returning a wrong page.
//
// This is a RUNTIME mechanism, not authored DSL: there is no new grammar. The
// inbound cursor rides the gRPC ExecuteQueryMsg.cursor field (and the WS
// bridge that tunnels to it), is threaded onto the context by the handler, and
// the engine lifts it onto plan.After before execution.

// ErrCursorMalformed is returned when an inbound cursor cannot be decoded.
var ErrCursorMalformed = errors.New("pagination cursor is malformed")

// ErrCursorSortMismatch is returned when an inbound cursor was minted under a
// different sort ordering than the query it is being replayed against. The
// keyset position is only meaningful under the ordering it was produced for, so
// continuing would silently skip or duplicate rows; we reject instead.
var ErrCursorSortMismatch = errors.New("pagination cursor does not match the query sort order")

// cursorVersion tags the encoded envelope so the codec can evolve.
const cursorVersion = 1

// cursorPayload is the decoded shape of an opaque pagination cursor. It carries
// the keyset position of the last row returned on the prior page plus a sort
// signature guard. Field names are short because the JSON is base64url-encoded
// into an opaque token; they are never seen by clients.
type cursorPayload struct {
	V         int    `json:"v"` // codec version
	CreatedAt int64  `json:"t"` // last row createdAt, unix nanoseconds (UTC)
	ID        string `json:"i"` // last row id (tie-breaker)
	Sig       string `json:"s"` // sort-signature guard (hex, truncated sha256)
}

// keysetPosition is the resolved, validated continuation point handed to the
// executor. It is request-scoped and rides the context alongside the cursor.
type keysetPosition struct {
	createdAt time.Time
	id        string
}

// sortSignatureHash returns the short guard hash stored in a cursor for the
// given sort signature. Truncated sha256 is plenty to detect an ordering change
// (this guards correctness, not authenticity).
func sortSignatureHash(signature string) string {
	sum := sha256.Sum256([]byte(signature))
	return hex.EncodeToString(sum[:8])
}

// encodeCursor produces the opaque continuation token for the last row of a
// full page, bound to the query's sort signature.
func encodeCursor(last memorynodes.MemoryNode, sortSignature string) (string, error) {
	id := strings.TrimSpace(last.ID)
	if id == "" {
		return "", fmt.Errorf("cannot encode cursor: last row has no id")
	}
	payload := cursorPayload{
		V:         cursorVersion,
		CreatedAt: last.CreatedAt.UTC().UnixNano(),
		ID:        id,
		Sig:       sortSignatureHash(sortSignature),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// decodeCursor parses an inbound opaque cursor and validates it against the
// query's sort signature. A mismatch yields ErrCursorSortMismatch; an
// undecodable token yields ErrCursorMalformed.
func decodeCursor(token, sortSignature string) (keysetPosition, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return keysetPosition{}, ErrCursorMalformed
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return keysetPosition{}, fmt.Errorf("%w: %v", ErrCursorMalformed, err)
	}
	var payload cursorPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return keysetPosition{}, fmt.Errorf("%w: %v", ErrCursorMalformed, err)
	}
	if payload.V != cursorVersion {
		return keysetPosition{}, fmt.Errorf("%w: unsupported cursor version %d", ErrCursorMalformed, payload.V)
	}
	if strings.TrimSpace(payload.ID) == "" {
		return keysetPosition{}, fmt.Errorf("%w: cursor missing id", ErrCursorMalformed)
	}
	if payload.Sig != sortSignatureHash(sortSignature) {
		return keysetPosition{}, ErrCursorSortMismatch
	}
	return keysetPosition{
		createdAt: time.Unix(0, payload.CreatedAt).UTC(),
		id:        strings.TrimSpace(payload.ID),
	}, nil
}

// --- request-scoped plumbing ---------------------------------------------

type inboundCursorKey struct{}
type keysetPositionKey struct{}

// ContextWithCursor threads an opaque inbound pagination cursor from the
// request onto the context. The engine lifts it onto plan.After at execution
// time. Empty cursors are a no-op (offset pagination and unpaginated queries
// continue to work unchanged).
func ContextWithCursor(ctx context.Context, cursor string) context.Context {
	if strings.TrimSpace(cursor) == "" {
		return ctx
	}
	return context.WithValue(ctx, inboundCursorKey{}, strings.TrimSpace(cursor))
}

// cursorFromContext returns the inbound cursor threaded by the request handler.
func cursorFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(inboundCursorKey{}).(string); ok {
		return v
	}
	return ""
}

// contextWithKeyset stashes the resolved keyset position so the deep executor
// path (executeCombinedFilterQuery) can push the keyset predicate into SQL
// without threading a new parameter through every intermediate call.
func contextWithKeyset(ctx context.Context, pos keysetPosition) context.Context {
	return context.WithValue(ctx, keysetPositionKey{}, pos)
}

// keysetFromContext returns the resolved keyset position, if any.
func keysetFromContext(ctx context.Context) (keysetPosition, bool) {
	if ctx == nil {
		return keysetPosition{}, false
	}
	pos, ok := ctx.Value(keysetPositionKey{}).(keysetPosition)
	return pos, ok
}

// keysetWhere returns the SQL predicate + args that continue a keyset scan from
// pos under the engine's universal (createdAt, id) tie-break ordering.
//
// compileSortFields always appends `createdAt DESC, id ASC` as deterministic
// tie-breakers, so the continuation predicate against the last row is:
//
//	(createdAt < ?) OR (createdAt = ? AND id > ?)
//
// for the default/desc ordering. When the leading declared sort is `createdAt
// asc`, the comparison flips to `>` / `<`. Anything with a leading payload sort
// (requiresFullScan) is NOT eligible for the SQL keyset push-down; the caller
// gates on keysetEligibleSort before reaching here.
func keysetWhere(pos keysetPosition, createdAtDesc bool) (string, []any) {
	if createdAtDesc {
		return `("createdAt" < ? OR ("createdAt" = ? AND id > ?))`,
			[]any{pos.createdAt, pos.createdAt, pos.id}
	}
	return `("createdAt" > ? OR ("createdAt" = ? AND id > ?))`,
		[]any{pos.createdAt, pos.createdAt, pos.id}
}

// keysetEligibleSort reports whether the compiled sort can use the SQL keyset
// push-down. Eligible iff the ordering reduces to the intrinsic (createdAt, id)
// keyset — i.e. no leading payload-path sort (which forces a full in-memory
// scan and cannot be expressed as a stable (createdAt, id) predicate). The
// hot time-series surface this primitive targets (chat / utterance / token
// streams, ordered by createdAt) is always eligible.
//
// It also returns whether the leading createdAt ordering is descending, which
// keysetWhere needs to pick the comparison direction.
func keysetEligibleSort(sorter *compiledSort) (eligible bool, createdAtDesc bool) {
	if sorter == nil {
		// nil sorter => default createdAt DESC, id ASC.
		return true, true
	}
	if sorter.needsFullScan() {
		return false, false
	}
	// Find the effective leading createdAt direction. The compiled fields end
	// with the appended createdAt-desc / id-asc fallbacks; an author-declared
	// `sort createdAt asc` lands ahead of them.
	for _, f := range sorter.fields {
		if f.kind == sortFieldCreatedAt {
			return true, f.direction != SortDirectionAsc
		}
		// A leading non-createdAt intrinsic sort (e.g. createdBy) is not a pure
		// (createdAt, id) keyset; reject to avoid an incorrect predicate.
		if f.kind != sortFieldId {
			return false, false
		}
	}
	return true, true
}
