package adminops

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// SetUserRole HAD NO ROLE RULE AT ALL (epic memql#5166, D4).
//
// Its only gate was `authorize` -- "does the caller manage users" -- so an
// admin could make anybody an owner, including themselves. Three comments
// elsewhere in this tree name "the uncapped SetUserRole" as the second move in
// a path to owner: mint a credential for an existing admin through an
// invitation or an enrolment link, sign in as them, then come here.
//
// Every case below is asserted through the OPERATION rather than through
// auth.MayAssignRole directly. The matrix of that function is pinned in
// component/auth; what these prove is that this seam CALLS it, on the target's
// real current role, and writes the refusal to the trail.

// roleReadingEngine answers `userByIdSystem` with one user row carrying a
// chosen role, and records every query it is handed. Every other query returns
// an error, which is what the write attempt hits -- so a case that reaches the
// write is distinguishable from one refused before it.
type roleReadingEngine struct {
	targetRole string
	queries    []string
}

func (e *roleReadingEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.queries = append(e.queries, q)
	if !strings.HasPrefix(q, "query userByIdSystem") {
		return nil, errTestEngine
	}
	return &memqlengine.ExecuteResult{
		Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{
			Id: "v1:identity:user:target",
			Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
				"role":         structpb.NewStringValue(e.targetRole),
				"primaryEmail": structpb.NewStringValue("target@example.test"),
				"displayName":  structpb.NewStringValue("Target Person"),
				"active":       structpb.NewBoolValue(true),
			}},
		}}},
	}, nil
}

func (e *roleReadingEngine) wroteAUser() bool {
	for _, q := range e.queries {
		if strings.Contains(q, "mutation") {
			return true
		}
	}
	return false
}

func newRoleService(t *testing.T, targetRole string) (*Service, *roleReadingEngine, *capturingAudit) {
	t.Helper()
	eng := &roleReadingEngine{targetRole: targetRole}
	audit := &capturingAudit{}
	svc, err := New(&Service{
		Engine: eng,
		Audit:  audit,
		IdentityBaseURL: func(context.Context) string {
			return "https://identity.example.test"
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, eng, audit
}

func TestSetUserRoleAppliesTheAssignmentRule(t *testing.T) {
	for _, tc := range []struct {
		name       string
		caller     auth.Role
		targetNow  string
		newRole    string
		refused    bool
		auditieson string
	}{
		{
			name: "admin cannot promote anybody to owner", caller: auth.RoleAdmin,
			targetNow: "writer", newRole: "owner", refused: true,
			auditieson: "ownership_transfer_required",
		},
		{
			// THE ESCALATION THIS CLOSES, spelled out: developer outranks admin
			// by rank and holds strictly fewer principal verbs, so a rank-only
			// rule would let an admin here mint one.
			name: "admin cannot promote anybody to developer", caller: auth.RoleAdmin,
			targetNow: "writer", newRole: "developer", refused: true,
			auditieson: "role_above_caller",
		},
		{
			name: "admin cannot re-role a developer they do not outrank", caller: auth.RoleAdmin,
			targetNow: "developer", newRole: "writer", refused: true,
			auditieson: "target_outranks_caller",
		},
		{
			name: "admin may demote a member", caller: auth.RoleAdmin,
			targetNow: "writer", newRole: "reader", refused: false,
		},
		{
			name: "owner may re-role an admin", caller: auth.RoleOwner,
			targetNow: "admin", newRole: "writer", refused: false,
		},
		{
			name: "an unknown slug is refused", caller: auth.RoleOwner,
			targetNow: "writer", newRole: "superuser", refused: true,
			auditieson: "invalid_role_or_user",
		},
		{
			// THE ALIAS. Every ordinary principal's row spells the member tier
			// `writer`, and IsValidRole resolving only the catalog's `user`
			// would refuse the commonest assignment in the product.
			name: "an alias is assignable", caller: auth.RoleOwner,
			targetNow: "reader", newRole: "writer", refused: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, eng, audit := newRoleService(t, tc.targetNow)

			res := svc.SetUserRole(ctxAs(tc.caller), "v1:identity:user:target", tc.newRole)

			if !tc.refused {
				if res.Code == CodePermissionDenied {
					t.Fatalf("%s was refused: %s", tc.name, res.ErrorMessage)
				}
				// It got past the rule and reached the WRITE, which the test
				// engine then refuses. Asserting the write was attempted is
				// what distinguishes "the rule admitted this" from "the rule
				// refused it with a different code".
				if !eng.wroteAUser() {
					t.Fatalf("%s never reached the write; queries=%v", tc.name, eng.queries)
				}
				return
			}
			if res.OK {
				t.Fatalf("%s was permitted", tc.name)
			}
			if eng.wroteAUser() {
				t.Fatalf("%s was refused and STILL wrote: %v", tc.name, eng.queries)
			}
			if len(audit.events) != 1 {
				t.Fatalf("want exactly 1 audit event, got %d: %+v", len(audit.events), audit.events)
			}
			ev := audit.events[0]
			if ev.Action != "user_role_changed" {
				t.Errorf("audit action = %q, want user_role_changed", ev.Action)
			}
			if ev.FailureReason != tc.auditieson {
				t.Errorf("audit failure reason = %q, want %q", ev.FailureReason, tc.auditieson)
			}
			// THE SLUGS ARE IN THE TRAIL (section H). An audit row that records
			// a role change without naming the roles is a row an auditor
			// cannot use.
			if got, _ := ev.Detail["newRole"].(string); got != strings.ToLower(tc.newRole) {
				t.Errorf("audit detail newRole = %v, want %q", ev.Detail["newRole"], tc.newRole)
			}
		})
	}
}

// TestSetUserRoleRecordsTheOldSlugOnARefusal. The target's CURRENT role is what
// the rank rule compares against, so a refusal that does not record it cannot
// be checked afterwards.
func TestSetUserRoleRecordsTheOldSlugOnARefusal(t *testing.T) {
	svc, _, audit := newRoleService(t, "developer")

	svc.SetUserRole(ctxAs(auth.RoleAdmin), "v1:identity:user:target", "reader")

	if len(audit.events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(audit.events))
	}
	if got, _ := audit.events[0].Detail["oldRole"].(string); got != "developer" {
		t.Errorf("audit detail oldRole = %v, want developer -- the rule compared against it "+
			"and the trail has to say so", audit.events[0].Detail["oldRole"])
	}
}
