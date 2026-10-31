package memql

// The WIRING half of D14 for deployable access codes (review of Task 3,
// finding 3). component/memql proves the decision -- no client is admitted a row
// of v1:platform:siteAccessCode or v1:platform:siteAccessSession, the owner and a
// cluster owner included (TestNoSubscriptionDeliversAnAccessCodeOrSessionRow). This
// proves handleBusEvent actually asks for it with the real concepts, because a
// correct decision that the fan-out does not call is the memql#4309 failure again:
// a live feed of every code minted, to whoever the row tier happens to admit.
//
// The concepts are the REAL ones, loaded from the shipped tree by fanoutFixtures,
// not fixtures with a tier written to look like theirs.

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/metrics"
)

func TestFanOutDeliversNoAccessCodeOrSessionRowToAnyClient(t *testing.T) {
	fanoutFixtures(t)
	const (
		codeConcept    = "v1:platform:siteAccessCode"
		sessionConcept = "v1:platform:siteAccessSession"
		grantConcept   = "v1:platform:sitePreviewGrant"
		owner          = "site-owner-1"
	)

	// POSITIVE CONTROL, as fanoutFixtures asks for: the concepts carry a tier, and
	// a row of a sibling owner-tier concept IS delivered to its owner. Without the
	// second, every "nothing delivered" below would also pass for a fan-out that
	// delivers nothing at all.
	for _, name := range []string{codeConcept, sessionConcept} {
		c, err := memorynodes.Get(name)
		require.NoError(t, err)
		require.NotNil(t, c.RowAuthz, "%s carries no tier in the loaded tree", name)
	}
	ownerStream, ownerRec := subscribedSession(t, &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	ownerStream.handleBusEvent(graphEvent(grantConcept, grantConcept+":g1",
		map[string]any{"siteId": "v1:platform:site:s1", "ownerUserId": "v1:identity:user:" + owner}))
	require.Len(t, notificationsOf(t, ownerRec), 1,
		"an owner-tier row was not delivered to its owner, so this file cannot tell a gate from a broken fan-out")

	rows := map[string]map[string]any{
		codeConcept: {
			"siteId": "v1:platform:site:s1", "ownerUserId": "v1:identity:user:" + owner,
			"accountId": "v1:accounts:account:acme", "label": "Acme reviewers", "codeHint": "FBZA",
		},
		sessionConcept: {
			"siteId": "v1:platform:site:s1", "codeId": "v1:platform:siteAccessCode:c1",
			"ownerUserId": "v1:identity:user:" + owner, "generation": 1,
		},
	}
	callers := []struct {
		name   string
		access *auth.AccessContext
	}{
		{"the site's owner", &auth.AccessContext{UserId: owner, Role: auth.RoleWriter}},
		{"a cluster owner", &auth.AccessContext{UserId: "operator-1", Role: auth.RoleOwner}},
		{"a member of the code's organization", &auth.AccessContext{UserId: "member-1", Role: auth.RoleWriter}},
		{"a stranger", &auth.AccessContext{UserId: "stranger-1", Role: auth.RoleWriter}},
	}
	for concept, row := range rows {
		before := metrics.SubscriptionRowsDeniedValue(concept)
		for _, caller := range callers {
			s, rec := subscribedSession(t, caller.access)
			s.handleBusEvent(graphEvent(concept, concept+":x1", row))
			require.Empty(t, notificationsOf(t, rec),
				"%s received a live %s event: a graph subscription is a read, and no client reads these rows", caller.name, concept)
		}
		if after := metrics.SubscriptionRowsDeniedValue(concept); after < before+float64(len(callers)) {
			t.Errorf("memql_subscription_rows_denied_total for %s advanced %v -> %v, want at least %d: a drop an operator cannot see is "+
				"indistinguishable from a subscription that never matched", concept, before, after, len(callers))
		}
	}
}
