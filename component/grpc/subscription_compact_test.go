package memql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestCompactGraphSubscriptionAfterMeshForward(t *testing.T) {
	meshFixture(t)
	event := forwardAcrossMesh(t, graphEvent(meshOwnedConcept, meshOwnedConcept+":r1", map[string]any{
		"ownerUserId": "user-b", "body": strings.Repeat("large webhook body", 2000),
	}), "node-1")
	config, err := structpb.NewStruct(map[string]any{"idOnly": true})
	require.NoError(t, err)
	for _, user := range []string{"user-a", "user-b"} {
		s, rec := meshSession(t, user)
		require.NoError(t, s.handleSubscribe(&memqlv1.MemqlClientMessage{MessageId: "subscribe"}, &memqlv1.SubscribeMsg{
			SubscriptionId: "compact", Kind: memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
			Concept: meshOwnedConcept, Config: config,
		}))
		s.handleBusEvent(event)
		var compact, full *memqlv1.EventNotification
		for _, n := range notificationsOf(t, rec) {
			if n.GetPayload().GetFields()["id"].GetStringValue() != "r1" {
				continue
			}
			if n.GetSubscriptionId() == "compact" {
				compact = n
			} else {
				full = n
			}
		}
		if user == "user-a" {
			require.Nil(t, compact, "compact subscription must not bypass row authorization")
			require.Nil(t, full)
			continue
		}
		require.NotNil(t, compact)
		require.True(t, compact.GetPayloadOmitted())
		require.NotContains(t, compact.GetPayload().AsMap(), "body")
		require.NotContains(t, compact.GetPayload().AsMap(), "payload")
		require.Equal(t, meshOwnedConcept, compact.GetPayload().GetFields()["concept"].GetStringValue())
		require.NotNil(t, full)
		require.False(t, full.GetPayloadOmitted())
		require.NotEmpty(t, full.GetPayload().GetFields()["body"].GetStringValue())
	}
}
