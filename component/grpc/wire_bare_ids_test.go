package memql

// Wire contract scanner for the bare-ids cutover (#2441, WS-A A2).
//
// The structural guarantee: no canonical `{concept}:{shortId}` id may leave
// the engine at a client/SDK/LLM/voice-agent seam. This file proves it two
// ways:
//
//   - TestWireScanner_InverseOfBareifier / the mechanism tests: the egress
//     bare-ifier (component/memql/wire_bareids.go) and the scanner here share
//     ONE pattern (memqlengine.WireCanonicalIdPattern) and ONE concept-carrier
//     allowlist (memqlengine.WireConceptCarrierKeys), so they are exact
//     inverses -- a value the bare-ifier would strip is exactly a value the
//     scanner rejects, and vice versa. Drift is impossible.
//
//   - TestWireBareIds_EngineRoundTrip: representative query / mutation /
//     subscription / tool round-trips through a REAL DB-backed engine, with
//     every outbound MemqlServerMessage captured at the sendServerMessage
//     egress chokepoint (captureStream.Send is the stream.Send that
//     sendServerMessage calls) and scanned. Skips when no Postgres is
//     reachable so a PG-less CI job is unaffected.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/provenance"
)

// --- the scanner (exact inverse of the egress bare-ifier) --------------------

// scannerExemptKeys is the concept-carrier allowlist shared with the
// bare-ifier, PLUS the R8 non-structural error-text residuals (QueryError /
// typed error messages may echo an id in prose -- non-structural, not scanned).
func scannerExemptKeys() map[string]struct{} {
	m := memqlengine.WireConceptCarrierKeys()
	m["message"] = struct{}{}      // QueryError.message (R8)
	m["errorMessage"] = struct{}{} // typed error text (guest/voice acks)
	return m
}

// scanNoCanonicalIds fails if any string in an id position of the outbound
// message matches the canonical-id shape. Marshals via protojson so the typed
// fields AND the embedded structpb payloads flatten into one JSON tree walked
// with the shared key allowlist.
func scanNoCanonicalIds(t *testing.T, label string, msg *memqlv1.MemqlServerMessage) {
	t.Helper()
	raw, err := protojson.Marshal(msg)
	require.NoError(t, err, "%s: protojson marshal", label)
	var tree any
	require.NoError(t, json.Unmarshal(raw, &tree), "%s: json unmarshal", label)
	walkScanTree(t, label, "", tree, scannerExemptKeys())
}

func walkScanTree(t *testing.T, label, key string, v any, exempt map[string]struct{}) {
	t.Helper()
	switch typed := v.(type) {
	case map[string]any:
		for k, val := range typed {
			if _, skip := exempt[k]; skip {
				continue
			}
			walkScanTree(t, label, k, val, exempt)
		}
	case []any:
		for _, e := range typed {
			walkScanTree(t, label, key, e, exempt)
		}
	case string:
		if memqlengine.WireCanonicalIdPattern().MatchString(typed) {
			t.Errorf("%s: canonical id leaked at key %q -> %q (must be a bare shortId at this wire seam)", label, key, typed)
		}
	}
}

// --- mechanism tests (no DB; always run) -------------------------------------

// TestWireBareShortId_TotalAndFixedPointOnRealIds -- renamed from
// ...TotalIdempotent by memql#2981. BareShortId is TOTAL, and it is a
// fixed point on every id shape this tree mints, which is what these cases
// cover and what the wire seam relies on. It is NOT idempotent in general:
// it strips ONE canonical prefix, so "v1:a:b:v2:c:d:e" -> "v2:c:d:e" -> "e".
// The old name asserted the general property in the one place a reader is
// most likely to take it as settled.
func TestWireBareShortId_TotalAndFixedPointOnRealIds(t *testing.T) {
	cases := []struct{ in, want string }{
		{"v1:agents:agent:a9f3b7c2", "a9f3b7c2"},          // canonical -> bare
		{"a9f3b7c2", "a9f3b7c2"},                          // already bare -> unchanged
		{"", ""},                                          // empty -> empty
		{"v1:cognition:space", "v1:cognition:space"},      // 3-seg concept TYPE -> unchanged
		{"bff-local", "bff-local"},                        // human slug -> unchanged
		{"v1:identity:user:user-30bf", "user-30bf"},       // canonical user id -> bare
		{"v1:cognition:utterance:474e-57df", "474e-57df"}, // hyphenated short -> bare
	}
	for _, c := range cases {
		if got := memqlengine.BareShortId(c.in); got != c.want {
			t.Errorf("BareShortId(%q) = %q, want %q", c.in, got, c.want)
		}
		// A second application is a no-op ON THESE INPUTS -- every id this
		// tree mints is colon- and whitespace-free, so its short id is a
		// fixed point. Not a claim about arbitrary input (memql#2981).
		if again := memqlengine.BareShortId(memqlengine.BareShortId(c.in)); again != c.want {
			t.Errorf("BareShortId(BareShortId(%q)) = %q, want %q -- the short id of a real "+
				"tree id must be a fixed point even though the function is not idempotent "+
				"in general (memql#2981)", c.in, again, c.want)
		}
	}
}

// The scanner pattern and the bare-ifier are exact inverses: a string is
// id-shaped (scanner would reject it) iff bare-ifying it changes it, and the
// bare-ifier's output is never id-shaped.
func TestWireScanner_InverseOfBareifier(t *testing.T) {
	pat := memqlengine.WireCanonicalIdPattern()
	idShaped := []string{
		"v1:agents:agent:abc",
		"v1:identity:user:user-1",
		"v1:cognition:utterance:474e57df-aaaa",
		"v2:knowledge:documentChunk:hash123",
	}
	notIdShaped := []string{
		"abc",                  // bare
		"v1:cognition:space",   // 3-seg concept type
		"graph.node.created",   // topic-ish
		"assistant",            // node type
		"warm and concise",     // free text
		"2026-07-05T00:00:00Z", // timestamp
	}
	for _, s := range idShaped {
		if !pat.MatchString(s) {
			t.Errorf("pattern should MATCH id-shaped %q", s)
		}
		bare := memqlengine.BareShortId(s)
		if bare == s {
			t.Errorf("bare-ifier should CHANGE id-shaped %q", s)
		}
		if pat.MatchString(bare) {
			t.Errorf("bare-ifier output %q for %q is still id-shaped", bare, s)
		}
	}
	for _, s := range notIdShaped {
		if pat.MatchString(s) {
			t.Errorf("pattern should NOT match %q", s)
		}
		if got := memqlengine.BareShortId(s); got != s {
			t.Errorf("bare-ifier should LEAVE %q unchanged, got %q", s, got)
		}
	}
}

// BareifyEventPayload strips every id-position value (id/nodeId/createdBy/actor/
// FK) and, critically, the chunk `replyId` forward-reference IN LOCKSTEP with
// the node id (RISK A: the SPA keys its streaming bubble by replyId and de-dups
// against the committed utterance id -- both must bare-ify together or the
// bubble never reconciles). The concept-carrier keys survive; the input is not
// mutated except in place on the copy the caller owns.
func TestBareifyEventPayload_ReplyIdLockstep(t *testing.T) {
	const utt = "v1:cognition:utterance:reply-xyz"
	payload := map[string]any{
		"id":            utt,
		"nodeId":        utt,
		"replyId":       utt, // chunk carrier -- must match the committed id after bare-ify
		"concept":       "v1:cognition:utterance",
		"nodeType":      "utterance",
		"createdBy":     "v1:identity:user:owner-1",
		"actor":         "v1:identity:user:owner-1",
		"participantId": "v1:cognition:participant:p-1",
		"payload": map[string]any{
			"replyId":       utt,
			"participantId": "v1:cognition:participant:p-1",
		},
	}
	memqlengine.BareifyEventPayload(payload)

	assert.Equal(t, "reply-xyz", payload["id"])
	assert.Equal(t, "reply-xyz", payload["nodeId"])
	assert.Equal(t, "reply-xyz", payload["replyId"])
	// Lockstep: the committed id and the chunk carrier resolve to the SAME bare
	// string, so committedIds.has(replyId) matches post-cutover.
	assert.Equal(t, payload["id"], payload["replyId"])
	assert.Equal(t, "owner-1", payload["createdBy"])
	assert.Equal(t, "owner-1", payload["actor"])
	assert.Equal(t, "p-1", payload["participantId"])
	// concept type preserved verbatim.
	assert.Equal(t, "v1:cognition:utterance", payload["concept"])
	// nested payload object bare-ified too.
	nested := payload["payload"].(map[string]any)
	assert.Equal(t, "reply-xyz", nested["replyId"])
	assert.Equal(t, "p-1", nested["participantId"])
}

// --- DB-backed round-trip (skips without Postgres) ---------------------------

func openWireTestDB(t *testing.T) *bun.DB {
	t.Helper()
	dsn := os.Getenv("MEMQL_DATABASE_DSN")
	if dsn == "" {
		dsn = "postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable"
	}
	// Cheap reachability probe so the no-DB path skips fast instead of blocking
	// on the migrate lifecycle.
	probe := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := probe.PingContext(ctx); err != nil {
		_ = probe.Close()
		dbtest.Unreachable(t, "DB-gated test for the #2441 wire-scanner round-trip", dsn, err)
	}
	_ = probe.Close()

	// Boot the production MemoryNodesDatabase (migrate-on-start) so a blank
	// database is fully migrated -- mirrors the conformance harness.
	_ = os.Setenv("MEMQL_DATABASE_DSN", dsn)
	mnd, err := concept.NewMemoryNodesDatabase()
	require.NoError(t, err, "NewMemoryNodesDatabase")
	mnd.Start(context.Background())
	select {
	case <-mnd.Ready():
	case <-time.After(90 * time.Second):
		t.Fatal("database did not become ready within 90s (migrations stuck?)")
	}
	require.NoError(t, mnd.AssertCriticalSchema(context.Background()), "migrations must apply cleanly")
	db := mnd.BunDB()
	require.NotNil(t, db, "BunDB() after ready")
	return db
}

func TestWireBareIds_EngineRoundTrip(t *testing.T) {
	db := openWireTestDB(t) // migrate-on-start; owned by the lifecycle, not closed per-test
	ctx := provenance.ContextWithProvenance(context.Background(), provenance.Direct("test:#2441"))

	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concept.DefaultRegistry()
	eng, err := memqlengine.New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(registry))

	const ownerId = "v1:identity:user:wire-owner-2441"
	const agentShort = "wire-agent-2441"
	const agentId = "v1:agents:agent:" + agentShort

	// Seed a user row so the searchUsers tool returns a real node with a
	// canonical id + createdBy.
	seedUserRow(ctx, t, db, ownerId)
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id = ?", ownerId).Exec(context.Background())
		_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id = ?", agentId).Exec(context.Background())
	})

	svc := &service{
		engine: eng,
		logger: slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
	}
	cs := newCaptureStream(t)
	// The gRPC interceptor normally stamps the verified token onto the stream
	// context; the engine derives the mutation actor from it (ActorFromContext).
	cs.ctx = auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: ownerId})
	s := &streamSession{
		service:      svc,
		stream:       cs,
		logger:       svc.logger,
		access:       &auth.AccessContext{UserId: ownerId, Role: auth.RoleOwner},
		accessLoaded: true,
	}

	// 1) MUTATION round-trip: create an agent (owner actor). The reply bundle
	//    carries the new node whose id / ownerUserId / createdBy are stored
	//    canonical and must land bare on the wire.
	createQ := fmt.Sprintf(`createAgent(agentId: %q, ownerUserId: %q, name: "WireTest")`, agentShort, ownerId)
	driveQuery(t, s, "req-create", createQ)
	createRes := waitForQueryResult(t, cs, "req-create", 8*time.Second)
	assertAgentNodeBare(t, createRes, agentShort)

	// 2) QUERY round-trip: read it back with a BARE id (A1 resolves it against
	//    the bound concept). Shaped result (data axis).
	driveQuery(t, s, "req-read", fmt.Sprintf(`agentById(agentId: %q)`, agentShort))
	_ = waitForQueryResult(t, cs, "req-read", 8*time.Second)

	// 3) SUBSCRIPTION / event egress: register a graph subscription and hand
	//    handleBusEvent a canonical-shaped event (id/nodeId/FK + the chunk
	//    replyId forward-ref). The wire copy must be fully bare; the internal
	//    event map must stay canonical (clone safety).
	s.subscriptions.Store("wire-sub", &subscriptionInfo{
		kind:     memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
		patterns: []string{"graph.#"},
	})
	internalPayload := map[string]any{
		"id":          agentId,
		"nodeId":      agentId,
		"concept":     "v1:agents:agent",
		"nodeType":    "agent",
		"ownerUserId": ownerId,
		"createdBy":   ownerId,
		"replyId":     "v1:cognition:utterance:reply-2441",
	}
	beforeSent := len(cs.sent)
	s.handleBusEvent(events.Event{
		Topic:     "graph.node.created.v1:agents:agent",
		Kind:      events.KindNodeCreated,
		Timestamp: time.Now().UTC(),
		Payload:   internalPayload,
		Metadata:  map[string]string{"actor": ownerId},
	})
	// Internal event payload untouched (clone safety -- automations / dispatch
	// gates / cross-node substrate keep canonical).
	assert.Equal(t, agentId, internalPayload["id"], "internal event map must stay canonical")
	assert.Equal(t, "v1:cognition:utterance:reply-2441", internalPayload["replyId"])
	// The emitted wire event is bare.
	evtMsg := findEventAfter(t, cs, beforeSent)
	require.NotNil(t, evtMsg, "handleBusEvent must emit an Event notification")
	evtPayload := evtMsg.GetEvent().GetPayload().AsMap()
	assert.Equal(t, agentShort, evtPayload["id"])
	assert.Equal(t, agentShort, evtPayload["nodeId"])
	assert.Equal(t, ownerId[len("v1:identity:user:"):], evtPayload["ownerUserId"])
	assert.Equal(t, "reply-2441", evtPayload["replyId"])
	assert.Equal(t, "v1:agents:agent", evtPayload["concept"], "concept type preserved on the wire")

	// 4) TOOL round-trip: the LLM tool-result JSON must carry bare ids. Tools
	//    are agent-only, so stamp an acting agent on the context.
	toolCtx := memqlengine.WithActingAgentRole(memqlengine.WithActingAgentId(ctx, agentId), "assistant")
	toolJSON, err := eng.ExecuteToolByName(toolCtx, "searchUsers", map[string]any{"active": true, "limit": 10})
	require.NoError(t, err)
	assert.NotRegexp(t, memqlengine.WireCanonicalIdPattern(), toolJSON, "tool JSON leaked a canonical id: %s", toolJSON)

	// Structural sweep: EVERY captured outbound message is canonical-id-free.
	cs.mu.Lock()
	all := append([]*memqlv1.MemqlServerMessage(nil), cs.sent...)
	cs.mu.Unlock()
	require.NotEmpty(t, all, "the round-trip must have produced outbound messages to scan")
	for i, m := range all {
		scanNoCanonicalIds(t, fmt.Sprintf("captured[%d]", i), m)
	}
}

// --- helpers -----------------------------------------------------------------

func driveQuery(t *testing.T, s *streamSession, requestId, query string) {
	t.Helper()
	env := &memqlv1.MemqlClientMessage{
		MessageId: requestId,
		Payload: &memqlv1.MemqlClientMessage_ExecuteQuery{
			ExecuteQuery: &memqlv1.ExecuteQueryMsg{RequestId: requestId, Query: query},
		},
	}
	require.NoError(t, s.handleExecuteQuery(env, env.GetExecuteQuery()))
}

func waitForQueryResult(t *testing.T, cs *captureStream, requestId string, timeout time.Duration) *memqlv1.QueryResultChunk {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cs.mu.Lock()
		for _, m := range cs.sent {
			if qr := m.GetQueryResult(); qr != nil && qr.GetRequestId() == requestId && qr.GetDone() {
				cs.mu.Unlock()
				return qr
			}
			if qe := m.GetQueryError(); qe != nil && qe.GetRequestId() == requestId {
				cs.mu.Unlock()
				t.Fatalf("query %q errored: %s", requestId, qe.GetError().GetMessage())
			}
		}
		cs.mu.Unlock()
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for query result %q", requestId)
	return nil
}

func findEventAfter(t *testing.T, cs *captureStream, after int) *memqlv1.MemqlServerMessage {
	t.Helper()
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for i := after; i < len(cs.sent); i++ {
		if cs.sent[i].GetEvent() != nil {
			return cs.sent[i]
		}
	}
	return nil
}

// assertAgentNodeBare positively confirms the created agent's node carries a
// bare id and bare FK/provenance fields (not merely "no canonical leak").
func assertAgentNodeBare(t *testing.T, res *memqlv1.QueryResultChunk, wantShort string) {
	t.Helper()
	bundle := res.GetResult().GetBundle()
	require.NotNil(t, bundle, "create reply must carry a bundle")
	require.NotEmpty(t, bundle.GetNodes(), "create reply bundle must carry the new node")
	n := bundle.GetNodes()[0]
	assert.Equal(t, wantShort, n.GetId(), "node id must be bare on the wire")
	assert.NotContains(t, n.GetCreatedBy(), ":", "createdBy must be bare on the wire")
	if owner, ok := n.GetPayload().AsMap()["ownerUserId"].(string); ok {
		assert.NotContains(t, owner, ":", "payload.ownerUserId must be bare on the wire")
	}
	// concept stays a full type reference.
	assert.Equal(t, "v1:agents:agent", n.GetConcept())
	for _, root := range bundle.GetRootIds() {
		assert.NotContains(t, root, ":v1:", "bundle rootId must be bare")
		assert.False(t, memqlengine.WireCanonicalIdPattern().MatchString(root), "rootId must not be canonical: %s", root)
	}
}

func seedUserRow(ctx context.Context, t *testing.T, db *bun.DB, userId string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"email":       "wire-owner@example.com",
		"role":        "owner",
		"displayName": "Wire Owner",
		"active":      true,
		"deleted":     false,
	})
	node := &concept.MemoryNode{
		ID:         userId,
		CreatedAt:  time.Now(),
		CreatedBy:  userId,
		Concept:    "v1:identity:user",
		Type:       "user",
		Schema:     json.RawMessage(`{}`),
		Payload:    payload,
		Metadata:   json.RawMessage(`{}`),
		Provenance: json.RawMessage(`{"source":"test"}`),
	}
	if _, err := db.NewInsert().Model(node).Exec(ctx); err != nil {
		t.Logf("seed user row (best-effort): %v", err)
	}
}
