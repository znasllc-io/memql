package campaigns

import (
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// The runs an automation lists by status are an organization's to read: a
// member of the organization sees them, a member of another organization does
// not, even when they name the automation. The query carries no @serverOnly;
// the concept's @rowAuthz account tier is what scopes it.
func TestJourneyEnrollmentsByStatusAreTheOrganizationsToRead(t *testing.T) {
	w := newLayoutWorld(t)
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	w.engines[0].InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })

	org, rivalOrg := w.suffix+"-org", w.suffix+"-rival-org"
	w.createOrganization(t, org, "Runs client")
	w.createOrganization(t, rivalOrg, "Rival client")
	w.seedUser(t, w.suffix+"-colleague", "writer")
	w.seedUser(t, w.suffix+"-rival", "writer")
	w.seedMember(t, w.suffix+"-colleague", org)
	w.seedMember(t, w.suffix+"-rival", rivalOrg)
	colleague := layoutActor(w.suffix+"-colleague", auth.RoleWriter)
	rival := layoutActor(w.suffix+"-rival", auth.RoleWriter)

	journeyID := w.suffix + "-journey"
	for i, status := range []string{"failed", "failed", "completed"} {
		w.run(t, w.system, fmt.Sprintf(`insert("v1:campaigns:journeyEnrollment", id=%s, payload={"ownerUserId":%s,"accountId":%s,"journeyId":%s,"recipientId":%s,"emailDigest":"d","stepIndex":0,"status":%s,"enrolledAt":"2026-10-01T00:00:00Z","triggeredAt":"2026-10-01T00:00:00Z"})`,
			langparser.QuoteString(fmt.Sprintf("%s-run-%d", w.suffix, i)), langparser.QuoteString(w.ownerID), langparser.QuoteString(org),
			langparser.QuoteString(journeyID), langparser.QuoteString(fmt.Sprintf("%s-r%d", w.suffix, i)), langparser.QuoteString(status)))
	}
	list := call("query", "journeyEnrollmentsByStatus", arg{"journeyId", journeyID}, arg{"status", "failed"})
	if rows := memql.MaterializeRows(w.run(t, colleague, list)); len(rows) != 2 {
		t.Fatalf("a member of the organization sees %d failed runs, want 2: %v", len(rows), rows)
	}
	if rows := memql.MaterializeRows(w.run(t, rival, list)); len(rows) != 0 {
		t.Fatalf("a member of another organization sees %d runs: %v", len(rows), rows)
	}
}
