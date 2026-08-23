package memql

// Non-graph subscription kinds are owner/admin-only (memql#4311).
//
// TELEMETRY, MESSAGE and AI_STREAM carry node-level events with no row
// owner to decide by, so the fan-out row gate has nothing to ask about
// them -- there is no row. ALL (`#`) is worse than either: it includes
// every graph topic AND every node-level one. The only honest place to
// decide these is SUBSCRIBE time, against the one thing they do have: the
// caller's role.

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func subscribeAs(t *testing.T, role auth.Role, kind memqlv1.SubscriptionKind) (*streamSession, *recordingClientStream) {
	t.Helper()
	rec := newRecordingClientStream(context.Background())
	svc := &service{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := newTestStreamSession(svc, rec)
	s.accessMu.Lock()
	s.access = &auth.AccessContext{UserId: "u1", Role: role}
	s.accessLoaded = true
	s.accessMu.Unlock()

	err := s.handleSubscribe(
		&memqlv1.MemqlClientMessage{MessageId: "m1"},
		&memqlv1.SubscribeMsg{SubscriptionId: "sub-1", Kind: kind},
	)
	require.NoError(t, err, "a refusal is a developer error, not a reason to tear the stream down")
	return s, rec
}

func subscriptionOutcome(t *testing.T, rec *recordingClientStream) (kind string, message string) {
	t.Helper()
	for _, m := range rec.snapshot() {
		ev := m.GetEvent()
		if ev == nil {
			continue
		}
		fields := ev.GetPayload().GetFields()
		if s := fields["status"]; s != nil {
			kind = s.GetStringValue()
		}
		if s := fields["message"]; s != nil {
			message = s.GetStringValue()
		}
	}
	return kind, message
}

func subscriptionCount(s *streamSession) int {
	n := 0
	s.subscriptions.Range(func(any, any) bool { n++; return true })
	return n
}

// A reader is refused every non-graph kind, and no subscription is left
// behind -- a refusal that still registers the bus subscription refuses
// nothing at all.
func TestNonGraphSubscriptionKindsAreRefusedBelowAdmin(t *testing.T) {
	kinds := map[string]memqlv1.SubscriptionKind{
		"ALL":       memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_ALL,
		"TELEMETRY": memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_TELEMETRY,
		"MESSAGE":   memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_MESSAGE,
		"AI_STREAM": memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_AI_STREAM,
	}
	for name, kind := range kinds {
		for _, role := range []auth.Role{auth.RoleReader, auth.RoleWriter, auth.RoleDeveloper} {
			t.Run(name+"/"+string(role), func(t *testing.T) {
				s, rec := subscribeAs(t, role, kind)
				status, message := subscriptionOutcome(t, rec)
				require.Equal(t, "error", status,
					"%s subscribed to %s and was not refused. These kinds carry node-level events "+
						"with no row owner to gate on, and ALL includes every graph topic.", role, name)
				require.Contains(t, message, "PermissionDenied")
				require.NotEmpty(t, message, "a refusal must carry a reason")
				require.Zero(t, subscriptionCount(s),
					"the subscription was registered anyway, so the refusal refused nothing")
			})
		}
	}
}

// Owner and admin keep them -- the cockpit is an operator console.
func TestNonGraphSubscriptionKindsAreAllowedForOwnerAndAdmin(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleAdmin} {
		t.Run(string(role), func(t *testing.T) {
			s, rec := subscribeAs(t, role, memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_ALL)
			status, _ := subscriptionOutcome(t, rec)
			require.Equal(t, "subscribed", status, "%s was refused an ALL subscription", role)
			require.Equal(t, 1, subscriptionCount(s))
		})
	}
}

// GRAPH_EVENTS is unaffected for every role: it is gated per-row at
// fan-out instead (memql#4309), which is a finer answer than a role check
// and the one the read path already gives.
func TestGraphSubscriptionsAreUnaffectedByTheKindGate(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleReader, auth.RoleWriter} {
		t.Run(string(role), func(t *testing.T) {
			s, rec := subscribeAs(t, role, memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS)
			status, _ := subscriptionOutcome(t, rec)
			require.Equal(t, "subscribed", status,
				"a %s was refused a GRAPH_EVENTS subscription; graph events are gated per row at "+
					"fan-out, not by role", role)
			require.Equal(t, 1, subscriptionCount(s))
		})
	}
}

// A stream that has resolved NO identity holds no role, so it cannot hold
// owner or admin. Fail closed (memql#2801: a missing AccessContext denies).
func TestNonGraphSubscriptionIsRefusedForAnUnresolvedStream(t *testing.T) {
	rec := newRecordingClientStream(context.Background())
	svc := &service{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := newTestStreamSession(svc, rec)

	require.NoError(t, s.handleSubscribe(
		&memqlv1.MemqlClientMessage{MessageId: "m1"},
		&memqlv1.SubscribeMsg{SubscriptionId: "sub-1", Kind: memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_ALL},
	))
	status, _ := subscriptionOutcome(t, rec)
	require.Equal(t, "error", status, "a stream with no resolved access context was granted ALL")
	require.Zero(t, subscriptionCount(s))
}
