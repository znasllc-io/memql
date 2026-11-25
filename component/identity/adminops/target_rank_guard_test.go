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

// A WRITE THAT NAMES ANOTHER PERSON ANSWERS TO THE RANK TREE, not only to the
// role gate (design record 2026-11-25, developer access).
//
// `authorize` asks whether the caller manages people AT ALL. Until the guard
// this file pins, that was the whole of the authorization for seven writes:
// an admin could suspend the owner, edit the owner's profile, turn the owner's
// sign-in links back on, revoke the owner's personal access tokens and rotate
// the owner recovery key -- which returns the new key in plain text. Only
// SetUserRole and IssueEnrolmentLink compared the two people.
//
// The rule is auth.GovernPrincipal's, the one those two already use: an owner
// is reachable only by an owner, an owner reaches everyone, anybody reaches
// themselves, and otherwise the caller must STRICTLY outrank the target.

// rankGuardEngine answers the target read with a person of a chosen role, and
// the token read with a token that person owns.
type rankGuardEngine struct {
	targetRole string
	targetID   string
	writes     []string
}

func (e *rankGuardEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	id := e.targetID
	if id == "" {
		id = "v1:identity:user:target"
	}
	node := func(nodeID string, fields map[string]*structpb.Value) (*memqlengine.ExecuteResult, error) {
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{
			Id: nodeID, Payload: &structpb.Struct{Fields: fields},
		}}}}, nil
	}
	switch {
	case strings.Contains(q, "userByIdSystem("):
		return node(id, map[string]*structpb.Value{
			"primaryEmail": structpb.NewStringValue("target@example.test"),
			"role":         structpb.NewStringValue(e.targetRole),
			"active":       structpb.NewBoolValue(true),
			"signInPolicy": structpb.NewStringValue("passkey_only"),
		})
	case strings.Contains(q, "patIdentityById("):
		return node("v1:identity:identity:pat", map[string]*structpb.Value{
			"userId": structpb.NewStringValue(id),
			"label":  structpb.NewStringValue("ci"),
			"active": structpb.NewBoolValue(true),
		})
	}
	if strings.HasPrefix(strings.TrimSpace(q), "mutation ") {
		e.writes = append(e.writes, q)
	}
	return &memqlengine.ExecuteResult{}, nil
}

// personTargetedOperations is every owner/admin write that names a person (or
// a credential belonging to one). RevokeNodeToken, UpdateClusterSettings and
// the OAuth client origins name no person, so the rank tree has nothing to say
// about them.
var personTargetedOperations = []gatedOp{
	{"set_user_display_name", func(s *Service, ctx context.Context) Result {
		return s.SetUserDisplayName(ctx, "v1:identity:user:target", "Target")
	}},
	{"update_user_profile", func(s *Service, ctx context.Context) Result {
		return s.UpdateUserProfile(ctx, UserProfile{UserId: "v1:identity:user:target", DisplayName: "Target"})
	}},
	{"suspend_user", func(s *Service, ctx context.Context) Result {
		return s.SetUserSuspended(ctx, "v1:identity:user:target", true, "policy")
	}},
	{"reinstate_user", func(s *Service, ctx context.Context) Result {
		return s.SetUserSuspended(ctx, "v1:identity:user:target", false, "")
	}},
	{"reset_sign_in_policy", func(s *Service, ctx context.Context) Result {
		return s.ResetSignInPolicy(ctx, "v1:identity:user:target")
	}},
	{"set_user_shared_mailbox", func(s *Service, ctx context.Context) Result {
		return s.SetUserSharedMailbox(ctx, "v1:identity:user:target", true)
	}},
	{"revoke_pat", func(s *Service, ctx context.Context) Result {
		return s.RevokePersonalAccessToken(ctx, "v1:identity:identity:pat")
	}},
	{"rotate_recovery_key", func(s *Service, ctx context.Context) Result {
		return s.RotateRecoveryKey(ctx, "v1:identity:user:target")
	}},
}

func rankGuardService(t *testing.T, targetRole, targetID string) (*Service, *rankGuardEngine, *capturingAudit) {
	t.Helper()
	eng := &rankGuardEngine{targetRole: targetRole, targetID: targetID}
	audit := &capturingAudit{}
	svc, err := New(&Service{Engine: eng, Audit: audit})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, eng, audit
}

func TestAWriteNamingAPersonRefusesATargetTheCallerDoesNotOutrank(t *testing.T) {
	for _, tc := range []struct {
		caller     auth.Role
		targetRole string
	}{
		// THE HOLE: nothing compared the two people, so an admin reached the owner.
		{auth.RoleAdmin, "owner"},
		// Peers do not outrank each other.
		{auth.RoleAdmin, "admin"},
		// Nor does a lower rung reach a higher one.
		{auth.RoleAdmin, "developer"},
	} {
		for _, op := range personTargetedOperations {
			t.Run(string(tc.caller)+" on "+tc.targetRole+"/"+op.name, func(t *testing.T) {
				svc, eng, audit := rankGuardService(t, tc.targetRole, "")

				res := op.call(svc, ctxAs(tc.caller))

				if res.Code != CodePermissionDenied {
					t.Fatalf("%s was NOT refused %s on a %s (code=%d ok=%v)",
						tc.caller, op.name, tc.targetRole, res.Code, res.OK)
				}
				if res.RecoveryKey != "" {
					t.Error("a refused call still handed back a recovery key")
				}
				if len(eng.writes) != 0 {
					t.Errorf("a refused call still wrote: %v", eng.writes)
				}
				if len(audit.events) == 0 ||
					audit.events[len(audit.events)-1].FailureReason != "target_outranks_caller" {
					t.Errorf("want a target_outranks_caller audit event, got %+v", audit.events)
				}
			})
		}
	}
}

// Positive control: the guard refuses nobody the tree admits.
func TestAWriteNamingAPersonAdmitsATargetBelowTheCaller(t *testing.T) {
	for _, tc := range []struct {
		caller     auth.Role
		targetRole string
	}{
		{auth.RoleOwner, "owner"}, // an owner reaches everyone, another owner included
		{auth.RoleOwner, "developer"},
		{auth.RoleAdmin, "writer"},
		{auth.RoleAdmin, "reader"},
	} {
		for _, op := range personTargetedOperations {
			t.Run(string(tc.caller)+" on "+tc.targetRole+"/"+op.name, func(t *testing.T) {
				svc, _, audit := rankGuardService(t, tc.targetRole, "")

				res := op.call(svc, ctxAs(tc.caller))

				if res.Code == CodePermissionDenied {
					t.Fatalf("%s was refused %s on a %s: %s", tc.caller, op.name, tc.targetRole, res.ErrorMessage)
				}
				for _, ev := range audit.events {
					if ev.FailureReason == "target_outranks_caller" {
						t.Fatalf("%s on a %s was audited as outranked", tc.caller, tc.targetRole)
					}
				}
			})
		}
	}
}

// Acting on YOUR OWN account is not a question of rank: nobody outranks
// themselves, and an admin renaming themselves is the ordinary case.
func TestAWriteNamingAPersonAdmitsTheCallerThemselves(t *testing.T) {
	svc, _, _ := rankGuardService(t, "admin", "v1:identity:user:caller")

	res := svc.SetUserDisplayName(ctxAs(auth.RoleAdmin), "v1:identity:user:caller", "Me")

	if res.Code == CodePermissionDenied {
		t.Fatalf("an admin was refused a write to their own account: %s", res.ErrorMessage)
	}
}
