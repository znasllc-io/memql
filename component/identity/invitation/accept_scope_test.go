package invitation_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/enrolment"
	"github.com/znasllc-io/memql/component/identity/invitation"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// D10 AGAIN, AT ACCEPTANCE. Issuing an invitation for a role scoped to an
// organization checks that a group it carries belongs to that organization.
// Between issue and acceptance that group can be archived, or re-tied, and
// PlaceInGroups -- which re-reads every group -- then places nobody, so the
// person would hold the organization's role without being in it. Accept asks
// the question again BEFORE it writes anything, and refuses: nothing is
// created, the invitation stays pending, and whoever invited them sees why.

type scopeCatalog struct{}

var scopeRanks = map[string]int{"owner": 400, "admin": 200, "reader": 50, "org-moderator": 20}

func (scopeCatalog) Rank(slug string) (int, bool)      { r, ok := scopeRanks[slug]; return r, ok }
func (scopeCatalog) Name(slug string) string           { return slug }
func (scopeCatalog) Holds(string, string, string) bool { return false }
func (scopeCatalog) Grants(string) []auth.VerbResource { return nil }
func (scopeCatalog) Active(slug string) bool           { _, ok := scopeRanks[slug]; return ok }
func (scopeCatalog) Scope(slug string) string {
	if slug == "org-moderator" {
		return "org-a"
	}
	return ""
}

// pendingInvitationEngine answers the token lookup with one live, pending user
// invitation, and records and refuses every write -- so a case that reached
// the user write is distinguishable from one refused before it.
type pendingInvitationEngine struct {
	role    string
	groups  []string
	queries []string
}

func (e *pendingInvitationEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.queries = append(e.queries, q)
	if !strings.HasPrefix(q, "query userInvitationByTokenHash") {
		return nil, errors.New("test engine: no database")
	}
	groups := make([]any, 0, len(e.groups))
	for _, g := range e.groups {
		groups = append(groups, g)
	}
	payload, err := structpb.NewStruct(map[string]any{
		"id": "v1:identity:invitation:inv-1", "kind": "user", "status": "pending", "active": true,
		"inviteeEmail": "invitee@example.test", "inviteeRole": e.role, "inviterId": "inviter",
		"expiresAt": time.Now().UTC().Add(time.Hour).Format(time.RFC3339), "groupIds": groups,
	})
	if err != nil {
		return nil, err
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{
		Id: "v1:identity:invitation:inv-1", Payload: payload,
	}}}}, nil
}

func (e *pendingInvitationEngine) wroteAUser() bool {
	for _, q := range e.queries {
		if strings.HasPrefix(q, "mutation createUserOnFirstLogin") {
			return true
		}
	}
	return false
}

func (e *pendingInvitationEngine) wroteAnything() bool {
	for _, q := range e.queries {
		if strings.HasPrefix(q, "mutation") {
			return true
		}
	}
	return false
}

func TestAcceptRefusesAScopedRoleWhoseOrganizationNoGroupStillJoins(t *testing.T) {
	auth.SetCapabilityCatalog(scopeCatalog{})
	t.Cleanup(func() { auth.SetCapabilityCatalog(nil) })

	type asked struct {
		groups  []string
		account string
	}
	for _, tc := range []struct {
		name    string
		role    string
		joins   func(*[]asked) func(context.Context, []string, string) bool
		refused bool
	}{
		{
			name: "the carried group no longer joins the organization", role: "org-moderator",
			joins: func(log *[]asked) func(context.Context, []string, string) bool {
				return func(_ context.Context, g []string, a string) bool { *log = append(*log, asked{g, a}); return false }
			},
			refused: true,
		},
		{
			name: "nothing can answer the question", role: "org-moderator",
			joins:   func(*[]asked) func(context.Context, []string, string) bool { return nil },
			refused: true,
		},
		{
			name: "a carried group still joins the organization", role: "org-moderator",
			joins: func(log *[]asked) func(context.Context, []string, string) bool {
				return func(_ context.Context, g []string, a string) bool { *log = append(*log, asked{g, a}); return true }
			},
		},
		{
			// The question is for a scoped role only.
			name: "a global role is not asked", role: "reader",
			joins: func(log *[]asked) func(context.Context, []string, string) bool {
				return func(_ context.Context, g []string, a string) bool { *log = append(*log, asked{g, a}); return false }
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &pendingInvitationEngine{role: tc.role, groups: []string{"acct-org-a"}}
			var log []asked
			deps := invitation.AcceptDeps{
				Store:             &identity.Store{Engine: eng},
				Enrolments:        &enrolment.Store{Engine: eng},
				InternalEmail:     func(string) bool { return false },
				JoinsOrganization: tc.joins(&log),
			}

			_, err := invitation.Accept(context.Background(), deps, "inv_token", "203.0.113.7")

			if tc.refused {
				if !errors.Is(err, invitation.ErrOrganizationNotJoined) {
					t.Fatalf("err = %v, want ErrOrganizationNotJoined", err)
				}
				if eng.wroteAnything() {
					t.Fatalf("refused and still wrote: %v", eng.queries)
				}
			} else {
				if errors.Is(err, invitation.ErrOrganizationNotJoined) {
					t.Fatalf("refused: %v", err)
				}
				if !eng.wroteAUser() {
					t.Fatalf("never reached the user write; queries=%v", eng.queries)
				}
			}
			if tc.role == "reader" && len(log) != 0 {
				t.Fatalf("a global role asked the organization question: %+v", log)
			}
			if tc.role == "org-moderator" && tc.joins(&[]asked{}) != nil {
				if len(log) != 1 || log[0].account != "org-a" || !slices.Equal(log[0].groups, []string{"acct-org-a"}) {
					t.Fatalf("asked %+v, want the invitation's groups and the role's organization org-a", log)
				}
			}
		})
	}
}
