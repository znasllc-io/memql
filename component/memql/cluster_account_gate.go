package memql

import (
	"context"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// clusterAccountOpenTo is the engine's ClusterAccountGate: whether the
// cluster's own vendor account may pay for a call made for this person
// (vendor_account_cluster.go holds the rule and why).
//
// The person is judged by what the cluster holds about them now: their role,
// their groups and their grants, resolved the way every capability gate
// resolves them (auth.CapableFor). A person the principal table does not
// resolve -- no row, deactivated, a role the cluster does not know -- holds
// nothing.
//
// Regression test: cluster_account_gate_db_test.go.
func (e *MemQLEngine) clusterAccountOpenTo(ctx context.Context, userId string) bool {
	subject, ok := e.personSubject(ctx, userId)
	if !ok {
		return false
	}
	return auth.CapableFor(ctx, subject, auth.VerbExecute, ClusterAccountPart)
}

// personSubject is the principal a call is FOR, as a capability gate judges
// one.
//
// When the context's actor IS that person, in a session of their own, the
// session says who they are and this is the subject every other gate would
// build (subjectFor). In every other case the role comes from the principal
// table: work continued on a person's behalf carries a stand-in role
// (auth.AccessContext.RoleStandIn), and a call that names a person while
// running as somebody or something else carries the wrong role altogether.
// tool_gate.go's toolFloorContext makes the same read for the same reason.
func (e *MemQLEngine) personSubject(ctx context.Context, userId string) (auth.Subject, bool) {
	userId = strings.TrimSpace(userId)
	bare := BareShortId(userId)
	if e == nil || bare == "" {
		return auth.Subject{}, false
	}
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil &&
		!ac.RoleStandIn && !ac.Synthetic && !ac.Unranked && BareShortId(strings.TrimSpace(ac.UserId)) == bare {
		return e.subjectFor(ctx)
	}
	role := e.personRole(ctx, userId)
	if role == "" {
		return auth.Subject{}, false
	}
	return auth.Subject{Role: role, UserId: userId, GroupIds: e.membershipsFor(ctx, userId).groupIds}, true
}
