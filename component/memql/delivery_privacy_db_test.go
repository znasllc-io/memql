package memql

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// The queries, generic row admission and subscription admission all need to
// agree. Internal origin admits a server-only construct, not its private rows.
func TestDeliveryBodiesAreOperatorOnly(t *testing.T) {
	eng, db, base := sharedReadMergeEngine(t)
	operator := auth.ContextWithSystemActor(base, "delivery-privacy-test")
	internal := auth.ContextWithInternalOrigin(operator)
	id := uniqueSuffix("delivery-privacy")
	inbound := runMutation(t, internal, eng, "stageInboundRequest", map[string]any{
		"requestId": id + "-in", "source": id, "medium": "webhook",
		"body": `{"private":"commit message"}`, "signatureVerified": true,
		"dedupeKey": id, "receivedAt": "2026-10-05T12:00:00Z",
	})
	outbound := runMutation(t, internal, eng, "stageOutboundRequest", map[string]any{
		"requestId": id + "-out", "medium": "email", "target": "private@example.com",
		"body": "private delivery text",
	})
	cases := []struct {
		concept, id string
		queries     []string
	}{
		{"v1:platform:inboundRequest", inbound, []string{
			fmt.Sprintf("query inboundRequestById(requestId: %s)", langparser.QuoteString(inbound)),
			fmt.Sprintf("query inboundRequestByDedupeKey(source: %s, dedupeKey: %s)", langparser.QuoteString(id), langparser.QuoteString(id)),
			`query inboundRequestsByStatus(status: "received")`,
		}},
		{"v1:platform:outboundRequest", outbound, []string{
			fmt.Sprintf("query outboundRequestById(requestId: %s)", langparser.QuoteString(outbound)),
			`query outboundRequestsByStatus(status: "pending")`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.concept, func(t *testing.T) {
			payload := latestPayload(t, internal, db, tc.concept, tc.id)
			for _, role := range []auth.Role{auth.RoleReader, auth.RoleWriter, auth.RoleDeveloper, auth.RoleOwner} {
				t.Run(string(role), func(t *testing.T) {
					ac := &auth.AccessContext{UserId: id + "-person", Role: role}
					caller := auth.ContextWithToken(auth.ContextWithAccess(base, ac), &auth.TokenInfo{Subject: ac.UserId})
					for _, query := range tc.queries {
						n := queryRowCount(t, auth.ContextWithInternalOrigin(caller), eng, query)
						if role == auth.RoleOwner {
							require.Positive(t, n, query)
						} else {
							require.Zero(t, n, query)
						}
					}
					want := SubscriptionDeny
					wantRow := rowAuthzDeny
					if role == auth.RoleOwner {
						want = SubscriptionAdmit
						wantRow = rowAuthzAdmit
					}
					require.Equal(t, wantRow, rowAuthzAdmits(caller, tc.concept, tc.id, subPayload(t, payload)))
					require.Equal(t, want, AdmitSubscriptionRow(context.Background(), ac, tc.concept, tc.id, subPayload(t, payload)))
				})
			}
		})
	}
}
