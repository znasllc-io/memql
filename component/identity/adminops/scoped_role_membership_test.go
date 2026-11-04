package adminops

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// D10 (epic memql#5166): a role scoped to an organization is holdable only by a
// member of that organization.
//
// Until these tests, identity answered the membership question with a constant
// false, written when no role could carry a scope. The New role rail now
// authors scoped roles, so that constant turned every scoped role into one
// nobody could be given: an invitation naming one was refused, and so was every
// re-role to one. What membership MEANS here is what the groups integration's
// requireOrganizationTarget already decides: an ACTIVE membership in an ACTIVE
// group whose organization is the role's scope.

// scopedCatalog is the five seeded rungs plus `org-moderator`, scoped to
// organization `org-a`. The owner holds every principal verb and admission, so
// the outer gates and the capability half of MayAssignRole admit it and the
// membership clause is what decides.
type scopedCatalog struct{}

var scopedRanks = map[string]int{
	"owner": 400, "developer": 300, "admin": 200,
	"user": 100, "writer": 100, "viewer": 50, "reader": 50,
	"org-moderator": 20,
}

func (scopedCatalog) Rank(slug string) (int, bool) { r, ok := scopedRanks[slug]; return r, ok }
func (scopedCatalog) Name(slug string) string      { return slug }
func (scopedCatalog) Holds(slug, verb, resource string) bool {
	if slug != "owner" {
		return false
	}
	return resource == auth.ResourcePrincipal || (resource == auth.ResourceAdmission && verb == auth.VerbCreate)
}
func (c scopedCatalog) Grants(slug string) []auth.VerbResource {
	var out []auth.VerbResource
	for _, verb := range []string{auth.VerbRead, auth.VerbCreate, auth.VerbUpdate, auth.VerbDelete} {
		if c.Holds(slug, verb, auth.ResourcePrincipal) {
			out = append(out, auth.VerbResource{Verb: verb, Resource: auth.ResourcePrincipal})
		}
	}
	return out
}
func (scopedCatalog) Scope(slug string) string {
	if slug == "org-moderator" {
		return "org-a"
	}
	return ""
}
func (scopedCatalog) Active(slug string) bool { _, ok := scopedRanks[slug]; return ok }

func installScopedCatalog(t *testing.T) {
	t.Helper()
	auth.SetCapabilityCatalog(scopedCatalog{})
	t.Cleanup(func() { auth.SetCapabilityCatalog(nil) })
}

// membershipEngine answers the reads the membership question needs from fixed
// rows: one user (`userByIdSystem`), groups by id and by organization, and the
// members of a group. Every other query -- the writes -- is recorded and
// refused, so a case that reaches a write is distinguishable from one refused
// before it, exactly as roleReadingEngine does for SetUserRole.
type membershipEngine struct {
	groups  []map[string]any
	members []map[string]any
	readErr error
	queries []string
}

func (e *membershipEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.queries = append(e.queries, q)
	switch {
	case strings.HasPrefix(q, "query userByIdSystem"):
		return bundleOf(map[string]any{
			"id": "target", "role": "reader", "primaryEmail": "target@example.test",
			"displayName": "Target Person", "active": true,
		}), nil
	case strings.HasPrefix(q, "query groupById"), strings.HasPrefix(q, "query groupsForAccount"), strings.HasPrefix(q, "query membersOfGroup"):
		if e.readErr != nil {
			return nil, e.readErr
		}
	}
	switch {
	case strings.HasPrefix(q, "query groupById"):
		var rows []map[string]any
		for _, g := range e.groups {
			if strings.Contains(q, `"`+g["id"].(string)+`"`) {
				rows = append(rows, g)
			}
		}
		return shapedRows(rows...), nil
	case strings.HasPrefix(q, "query groupsForAccount"):
		var rows []map[string]any
		for _, g := range e.groups {
			if strings.Contains(q, `"`+g["accountId"].(string)+`"`) {
				rows = append(rows, g)
			}
		}
		return shapedRows(rows...), nil
	case strings.HasPrefix(q, "query membersOfGroup"):
		var rows []map[string]any
		for _, m := range e.members {
			if strings.Contains(q, `"`+m["groupId"].(string)+`"`) {
				rows = append(rows, m)
			}
		}
		return shapedRows(rows...), nil
	}
	if strings.HasPrefix(q, "mutation") {
		return nil, errTestEngine
	}
	return nil, nil
}

func (e *membershipEngine) wrote(prefix string) bool {
	for _, q := range e.queries {
		if strings.HasPrefix(q, prefix) {
			return true
		}
	}
	return false
}

// shapedRows is what a query with a `shape` returns: one flat map per row,
// projected under the shape's field names (groupFull, groupMembershipFull).
func shapedRows(rows ...map[string]any) *memqlengine.ExecuteResult {
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	return memqlengine.NewResultWithOutput(out)
}

// bundleOf is an unshaped read's answer, as userByIdSystem returns one.
func bundleOf(rows ...map[string]any) *memqlengine.ExecuteResult {
	nodes := make([]*memqlv1.MemoryNode, 0, len(rows))
	for _, r := range rows {
		payload, err := structpb.NewStruct(r)
		if err != nil {
			panic(err)
		}
		id, _ := r["id"].(string)
		nodes = append(nodes, &memqlv1.MemoryNode{Id: id, Payload: payload})
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}}
}

func group(id, accountID, kind, status string) map[string]any {
	return map[string]any{"id": id, "name": id, "accountId": accountID, "kind": kind, "status": status}
}

func membership(groupID, userID, status string) map[string]any {
	return map[string]any{"id": groupID + "-" + userID, "groupId": groupID, "userId": userID, "accountId": "", "status": status}
}

func newMembershipService(t *testing.T, eng *membershipEngine) (*Service, *capturingAudit) {
	t.Helper()
	audit := &capturingAudit{}
	svc, err := New(&Service{
		Engine: eng,
		Audit:  audit,
		IdentityBaseURL: func(context.Context) string {
			return "https://identity.example.test"
		},
		SendInvitationEmail: func(context.Context, InvitationEmail) error { return nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, audit
}

func refusedForMembership(audit *capturingAudit) bool {
	for _, ev := range audit.events {
		if ev.FailureReason == string(auth.AssignNotAMember) {
			return true
		}
	}
	return false
}

// An invitation's recipient is no principal yet, so "is this person a member"
// is answered by the invitation itself: they become a member of every group it
// carries when they accept. A scoped role is issuable exactly when one of those
// groups is an active group of the role's organization.
func TestAnInvitationNamesAScopedRoleOnlyWithAGroupOfItsOrganization(t *testing.T) {
	for _, tc := range []struct {
		name    string
		role    string
		groups  []map[string]any
		carries []string
		readErr error
		refused bool
	}{
		{
			name: "it carries the organization's own group", role: "org-moderator",
			groups:  []map[string]any{group("acct-org-a", "org-a", "account", "active")},
			carries: []string{"acct-org-a"},
		},
		{
			name: "it carries another active group of the organization", role: "org-moderator",
			groups:  []map[string]any{group("moderators", "org-a", "custom", "active")},
			carries: []string{"moderators"},
		},
		{
			name: "it carries no group", role: "org-moderator", refused: true,
		},
		{
			name: "it carries only another organization's group", role: "org-moderator",
			groups:  []map[string]any{group("acct-org-b", "org-b", "account", "active")},
			carries: []string{"acct-org-b"}, refused: true,
		},
		{
			name: "the organization's group is archived", role: "org-moderator",
			groups:  []map[string]any{group("acct-org-a", "org-a", "account", "archived")},
			carries: []string{"acct-org-a"}, refused: true,
		},
		{
			name: "the group cannot be read", role: "org-moderator",
			groups:  []map[string]any{group("acct-org-a", "org-a", "account", "active")},
			carries: []string{"acct-org-a"}, readErr: errors.New("database unavailable"), refused: true,
		},
		{
			// The clause runs for a scoped role only: a global role is
			// unaffected and still needs no group.
			name: "a global role needs no group", role: "reader",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installScopedCatalog(t)
			eng := &membershipEngine{groups: tc.groups, readErr: tc.readErr}
			svc, audit := newMembershipService(t, eng)

			res := svc.IssueUserInvitation(ctxAs(auth.RoleOwner), UserInvitation{
				Email:    "invitee@example.test",
				Role:     tc.role,
				GroupIds: tc.carries,
			})

			if tc.refused {
				if res.OK || res.Code != CodePermissionDenied {
					t.Fatalf("%s: got ok=%v code=%d %q, want PERMISSION_DENIED", tc.name, res.OK, res.Code, res.ErrorMessage)
				}
				if !refusedForMembership(audit) {
					t.Fatalf("%s: refused, but not for membership: %+v", tc.name, audit.events)
				}
				if eng.wrote("mutation") {
					t.Fatalf("%s: refused and still wrote: %v", tc.name, eng.queries)
				}
				return
			}
			if refusedForMembership(audit) || res.Code == CodePermissionDenied {
				t.Fatalf("%s: refused: code=%d %q (audit %+v)", tc.name, res.Code, res.ErrorMessage, audit.events)
			}
		})
	}
}

// A re-role names an existing person, so membership is read: an ACTIVE
// membership in an ACTIVE group of the role's organization, the same answer
// the groups integration gives before a delegated administrator may manage a
// person (requireOrganizationTarget).
func TestARoleChangeGivesAScopedRoleOnlyToAMemberOfItsOrganization(t *testing.T) {
	for _, tc := range []struct {
		name    string
		groups  []map[string]any
		members []map[string]any
		readErr error
		refused bool
	}{
		{
			name:    "they are an active member of the organization's group",
			groups:  []map[string]any{group("acct-org-a", "org-a", "account", "active")},
			members: []map[string]any{membership("acct-org-a", "target", "active")},
		},
		{
			name:    "they belong only to another organization",
			groups:  []map[string]any{group("acct-org-b", "org-b", "account", "active")},
			members: []map[string]any{membership("acct-org-b", "target", "active")},
			refused: true,
		},
		{
			name:    "their membership was removed",
			groups:  []map[string]any{group("acct-org-a", "org-a", "account", "active")},
			members: []map[string]any{membership("acct-org-a", "target", "removed")},
			refused: true,
		},
		{
			name:    "somebody else is the member",
			groups:  []map[string]any{group("acct-org-a", "org-a", "account", "active")},
			members: []map[string]any{membership("acct-org-a", "someone-else", "active")},
			refused: true,
		},
		{
			name:    "the organization's group is archived",
			groups:  []map[string]any{group("acct-org-a", "org-a", "account", "archived")},
			members: []map[string]any{membership("acct-org-a", "target", "active")},
			refused: true,
		},
		{
			name:    "membership cannot be read",
			groups:  []map[string]any{group("acct-org-a", "org-a", "account", "active")},
			members: []map[string]any{membership("acct-org-a", "target", "active")},
			readErr: errors.New("database unavailable"),
			refused: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installScopedCatalog(t)
			eng := &membershipEngine{groups: tc.groups, members: tc.members, readErr: tc.readErr}
			svc, audit := newMembershipService(t, eng)

			res := svc.SetUserRole(ctxAs(auth.RoleOwner), "v1:identity:user:target", "org-moderator")

			if tc.refused {
				if res.OK || res.Code != CodePermissionDenied {
					t.Fatalf("%s: got ok=%v code=%d %q, want PERMISSION_DENIED", tc.name, res.OK, res.Code, res.ErrorMessage)
				}
				if !refusedForMembership(audit) {
					t.Fatalf("%s: refused, but not for membership: %+v", tc.name, audit.events)
				}
				if eng.wrote("mutation") {
					t.Fatalf("%s: refused and still wrote: %v", tc.name, eng.queries)
				}
				return
			}
			if refusedForMembership(audit) || res.Code == CodePermissionDenied {
				t.Fatalf("%s: refused: code=%d %q", tc.name, res.Code, res.ErrorMessage)
			}
			// Past the rule, it reaches the user write (which this engine then
			// refuses): the difference between "admitted" and "refused with
			// another code".
			if !eng.wrote("mutation") {
				t.Fatalf("%s: never reached the write; queries=%v", tc.name, eng.queries)
			}
		})
	}
}
