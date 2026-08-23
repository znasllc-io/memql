package memql

// The WIRING half of memql#4309. component/memql proves the decision;
// this proves handleBusEvent actually asks for it, because a correct gate
// that nothing calls is the failure mode this epic exists to fix -- the
// engine had exactly one function for "may this caller see this row" and
// subscriptions were the one egress that never called it.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/metrics"
)

const (
	fanoutOwnedConcept   = "v1:fanoutfixture:ownedrow"
	fanoutGrantedConcept = "v1:fanoutfixture:grantedrow"
)

// fanoutFixtures registers the two tiers this file needs and restores the
// registry afterwards.
func fanoutFixtures(t *testing.T) {
	t.Helper()
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	before := memorynodes.All()
	memorynodes.MergeAll(map[string]*memorynodes.Concept{
		fanoutOwnedConcept: {
			Name: fanoutOwnedConcept, NodeType: "probe",
			RowAuthz: &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"},
		},
		fanoutGrantedConcept: {
			Name: fanoutGrantedConcept, NodeType: "probe",
			RowAuthz: &langparser.RowAuthzDecl{Tier: langparser.RowAuthzGranted, Spec: "spaceMember"},
		},
	})
	t.Cleanup(func() { memorynodes.ReplaceAll(before) })

	// POSITIVE CONTROL: an unregistered fixture is an UNDECLARED concept,
	// which admits everything -- so every "denied" assertion below would
	// fail loudly, but every "delivered" one would pass for the wrong
	// reason. Assert the tier is really there.
	for _, name := range []string{fanoutOwnedConcept, fanoutGrantedConcept} {
		c, err := memorynodes.Get(name)
		if err != nil || c == nil || c.RowAuthz == nil {
			t.Fatalf("fixture %s carries no tier; this file would measure an undeclared concept", name)
		}
	}
}

// subscribedSession builds a session already subscribed to every graph
// topic, with `access` as its resolved identity.
func subscribedSession(t *testing.T, access *auth.AccessContext) (*streamSession, *recordingClientStream) {
	t.Helper()
	rec := newRecordingClientStream(context.Background())
	svc := &service{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := newTestStreamSession(svc, rec)
	s.subscriptions.Store("sub-1", &subscriptionInfo{
		kind:     memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
		patterns: []string{"graph.node.*.#"},
	})
	s.accessMu.Lock()
	s.access = access
	s.accessLoaded = true
	s.accessMu.Unlock()
	return s, rec
}

// graphEvent builds the envelope executeWrite publishes: the row's fields
// flattened into the top level AND retained under "payload".
func graphEvent(concept, id string, row map[string]any) events.Event {
	payload := map[string]any{
		"id": id, "nodeId": id, "concept": concept,
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}
	for k, v := range row {
		payload[k] = v
	}
	payload["payload"] = row
	return events.Event{
		Topic:     "graph.node.created." + concept,
		Kind:      events.KindNodeCreated,
		Timestamp: time.Now(),
		Payload:   payload,
	}
}

func notificationsOf(t *testing.T, rec *recordingClientStream) []*memqlv1.EventNotification {
	t.Helper()
	var out []*memqlv1.EventNotification
	for _, m := range rec.snapshot() {
		if ev := m.GetEvent(); ev != nil {
			out = append(out, ev)
		}
	}
	return out
}

// THE LEAK, closed. A stream owned by user-b subscribed to a declared
// owned concept receives nothing when user-a's row is written; user-a's
// stream receives it.
func TestFanOutDeniesARowTheReadPathWouldDeny(t *testing.T) {
	fanoutFixtures(t)
	event := graphEvent(fanoutOwnedConcept, fanoutOwnedConcept+":r1",
		map[string]any{"ownerUserId": "user-a", "title": "user-a's row"})

	before := metrics.SubscriptionRowsDeniedValue(fanoutOwnedConcept)

	stranger, strangerRec := subscribedSession(t, &auth.AccessContext{UserId: "user-b", Role: auth.RoleWriter})
	stranger.handleBusEvent(event)
	require.Empty(t, notificationsOf(t, strangerRec),
		"a stream owned by user-b received user-a's row on a concept declaring the owned tier. "+
			"This is the memql#4309 leak: the read path denies this row and the subscription "+
			"delivered it.")

	if after := metrics.SubscriptionRowsDeniedValue(fanoutOwnedConcept); after <= before {
		t.Errorf("memql_subscription_rows_denied_total did not advance for %s (%v -> %v); "+
			"a drop an operator cannot see is indistinguishable from a subscription that never "+
			"matched", fanoutOwnedConcept, before, after)
	}

	owner, ownerRec := subscribedSession(t, &auth.AccessContext{UserId: "user-a", Role: auth.RoleWriter})
	owner.handleBusEvent(event)
	got := notificationsOf(t, ownerRec)
	require.Len(t, got, 1, "the row's own owner was denied their row")
	require.False(t, got[0].GetPayloadOmitted())
	require.Equal(t, "user-a", got[0].GetPayload().GetFields()["ownerUserId"].GetStringValue(),
		"the admitted event lost its payload")
}

// A stream that has resolved no identity is an empty actor, and the owned
// tier already answers that: no identity, no rows.
func TestFanOutDeniesAStreamWithNoResolvedAccess(t *testing.T) {
	fanoutFixtures(t)
	s, rec := subscribedSession(t, nil)
	s.handleBusEvent(graphEvent(fanoutOwnedConcept, fanoutOwnedConcept+":r2",
		map[string]any{"ownerUserId": "user-a"}))
	require.Empty(t, notificationsOf(t, rec),
		"a stream with no access context received a row on a declared owned concept")
}

// The `granted` tier arrives ID-ONLY with the flag set, and the payload
// carries the four keys the client needs to re-read -- and NOT the row.
func TestFanOutSendsAnIdOnlyNotificationForAnUndecidedRow(t *testing.T) {
	fanoutFixtures(t)
	s, rec := subscribedSession(t, &auth.AccessContext{UserId: "user-a", Role: auth.RoleWriter})
	s.handleBusEvent(graphEvent(fanoutGrantedConcept, fanoutGrantedConcept+":g1",
		map[string]any{"spaceId": "s1", "secretBody": "must not travel"}))

	got := notificationsOf(t, rec)
	require.Len(t, got, 1, "a granted-tier row was dropped silently instead of notified id-only; "+
		"that is what makes a future granted concept's live feed die without a trace (design D3)")
	require.True(t, got[0].GetPayloadOmitted(), "payload_omitted was not set on the id-only event")

	fields := got[0].GetPayload().GetFields()
	require.Contains(t, fields, "id")
	require.Contains(t, fields, "concept")
	require.NotContains(t, fields, "secretBody",
		"the id-only notification carried the row's payload, which is the whole thing it exists "+
			"to withhold")
	require.NotContains(t, fields, "payload",
		"the id-only notification retained the nested payload object")
}

// An UNDECLARED concept is delivered as before. The seam mirrors the read
// path and does not invent a second, stricter rulebook (design D1) -- a
// subscription stricter than a read is a second authz implementation, and
// it will drift.
func TestFanOutLeavesUndeclaredConceptsAlone(t *testing.T) {
	fanoutFixtures(t)
	s, rec := subscribedSession(t, &auth.AccessContext{UserId: "user-b", Role: auth.RoleWriter})
	s.handleBusEvent(graphEvent("v1:fanoutfixture:undeclared", "v1:fanoutfixture:undeclared:u1",
		map[string]any{"anything": "at all"}))

	got := notificationsOf(t, rec)
	require.Len(t, got, 1, "an undeclared concept's event was dropped; the read path admits it, so "+
		"the subscription must too")
	require.False(t, got[0].GetPayloadOmitted())
}

// A NON-GRAPH topic carries no row to decide about and must pass through
// this seam untouched -- it is gated at subscribe time instead
// (memql#4311). Running a row gate here would ask a question the event
// cannot answer and then have to invent a default.
func TestFanOutDoesNotGateNonGraphTopics(t *testing.T) {
	fanoutFixtures(t)
	rec := newRecordingClientStream(context.Background())
	svc := &service{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := newTestStreamSession(svc, rec)
	s.subscriptions.Store("sub-t", &subscriptionInfo{
		kind:     memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_TELEMETRY,
		patterns: []string{"telemetry.#"},
	})
	s.accessMu.Lock()
	s.access = &auth.AccessContext{UserId: "user-b", Role: auth.RoleWriter}
	s.accessLoaded = true
	s.accessMu.Unlock()

	s.handleBusEvent(events.Event{
		Topic:     "telemetry.channel.fill",
		Kind:      events.KindTelemetry,
		Timestamp: time.Now(),
		Payload:   map[string]any{"channel": "engine", "depth": 3},
	})
	require.Len(t, notificationsOf(t, rec), 1, "a telemetry event was gated by the ROW gate")
}

// graphRowFromEvent resolves the row's OWN payload, and falls back to the
// flattened envelope when no nested object is present. Asserted directly
// because a wrong answer here is silent: the gate denies a row whose owner
// field it cannot read, so the failure looks like a working gate.
func TestGraphRowFromEventResolvesTheRowPayload(t *testing.T) {
	concept, id, payload, ok := graphRowFromEvent(graphEvent(fanoutOwnedConcept, "x", map[string]any{"ownerUserId": "user-a"}))
	require.True(t, ok)
	require.Equal(t, fanoutOwnedConcept, concept)
	require.Equal(t, "x", id)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.Equal(t, "user-a", decoded["ownerUserId"])

	// No nested object: the flattened envelope still carries the field.
	_, _, flat, ok := graphRowFromEvent(events.Event{
		Topic:   "graph.node.updated." + fanoutOwnedConcept,
		Payload: map[string]any{"id": "y", "concept": fanoutOwnedConcept, "ownerUserId": "user-a"},
	})
	require.True(t, ok)
	require.NoError(t, json.Unmarshal(flat, &decoded))
	require.Equal(t, "user-a", decoded["ownerUserId"],
		"the fallback lost the owner field, so every such event would be DENIED to its own owner")

	// A non-graph topic is not a row.
	_, _, _, ok = graphRowFromEvent(events.Event{Topic: "telemetry.x", Payload: map[string]any{}})
	require.False(t, ok)
}
