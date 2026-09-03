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

// AN ENROLMENT LINK IS A WAY TO SIGN IN AS SOMEBODY ELSE, so who it may be
// minted FOR is an authorization question and not only who may mint one.
//
// The link authorizes exactly one action -- register a passkey as the named
// user -- and neither the /enroll page nor the WebAuthn ceremony compares any
// ranks. So the holder of a link for the owner's account becomes the owner.
//
// Until the guard this file pins, IssueEnrolmentLink compared nothing: the
// target had to EXIST and nothing more. An admin could mint a link for the
// OWNER and take the cluster. That predates the admission capability; adding
// developer to the callers made it worse, which is why it is closed here
// rather than inherited.

// roledEngine answers the target read with one row carrying a chosen role.
type roledEngine struct {
	targetRole string
	targetID   string
	calls      int
}

func (e *roledEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.calls++
	if strings.Contains(q, "userByIdSystem(") {
		id := e.targetID
		if id == "" {
			id = "v1:identity:user:target"
		}
		return &memqlengine.ExecuteResult{
			Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{
				Id: id,
				Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
					"primaryEmail": structpb.NewStringValue("target@example.test"),
					"role":         structpb.NewStringValue(e.targetRole),
					"active":       structpb.NewBoolValue(true),
				}},
			}}},
		}, nil
	}
	return &memqlengine.ExecuteResult{}, nil
}

func serviceWithTarget(t *testing.T, targetRole, targetID string) (*Service, *capturingAudit) {
	t.Helper()
	audit := &capturingAudit{}
	svc, err := New(&Service{
		Engine:          &roledEngine{targetRole: targetRole, targetID: targetID},
		Audit:           audit,
		IdentityBaseURL: func(context.Context) string { return "https://identity.example.test" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, audit
}

func TestEnrolmentLinkRefusesATargetTheCallerDoesNotOutrank(t *testing.T) {
	for _, tc := range []struct {
		name       string
		caller     auth.Role
		targetRole string
		refused    bool
	}{
		// THE HOLE: an admin could mint a link for the OWNER and take the
		// account. Only an owner reaches an owner-ranked target.
		{"admin cannot mint for the owner", auth.RoleAdmin, "owner", true},

		// Peers do not outrank each other, which is what auth.CanManageUser
		// already refuses for a role change on the same account.
		{"admin cannot mint for another admin", auth.RoleAdmin, "admin", true},

		{"admin may mint for a reader", auth.RoleAdmin, "reader", false},
		{"admin may mint for a writer", auth.RoleAdmin, "writer", false},

		// An owner reaches everyone, including another owner.
		{"owner may mint for an owner", auth.RoleOwner, "owner", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, audit := serviceWithTarget(t, tc.targetRole, "")

			res := svc.IssueEnrolmentLink(ctxAs(tc.caller),
				EnrolmentLink{UserId: "v1:identity:user:target"})

			if !tc.refused {
				if res.Code == CodePermissionDenied {
					t.Fatalf("%s was refused: %s", tc.name, res.Message)
				}
				return
			}
			if res.Code != CodePermissionDenied {
				t.Fatalf("%s was NOT refused (code=%d, ok=%v) -- a link for that account "+
					"registers a passkey as them", tc.name, res.Code, res.OK)
			}
			if res.EnrolmentURL != "" {
				t.Error("a refused call still handed back a link")
			}
			if len(audit.events) == 0 ||
				audit.events[len(audit.events)-1].FailureReason != "target_outranks_caller" {
				t.Errorf("want a target_outranks_caller audit event, got %+v", audit.events)
			}
		})
	}
}

// Minting one for YOURSELF is always allowed -- registering another passkey of
// your own is the ordinary case, and the rank rule must not refuse it just
// because you do not outrank yourself.
func TestEnrolmentLinkAllowsMintingForYourself(t *testing.T) {
	// ctxAs stamps this user id; the target read answers with the same one.
	svc, _ := serviceWithTarget(t, "admin", "v1:identity:user:caller")

	res := svc.IssueEnrolmentLink(ctxAs(auth.RoleAdmin),
		EnrolmentLink{UserId: "v1:identity:user:caller"})

	if res.Code == CodePermissionDenied {
		t.Fatalf("an admin was refused an enrolment link for their own account: %s", res.Message)
	}
}
