package http

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// passkey_role_floor_test.go -- memql#4516.
//
// The passkey ceremony is the THIRD way the code flow reaches an auth code, and
// the one it would be easiest to forget: it does not go anywhere near the
// magic-link verifier. Forgetting it would mean a reader refused through the
// emailed link simply presents a passkey instead and is admitted -- so the
// floor sits on the MINT rather than on one factor.
//
// These drive the two pieces the handler delegates to. The ceremony itself is
// covered by webauthn_login_test.go and is not re-simulated here.

// userLookupEngine answers userByIdSystem with one row carrying `role`, and
// optionally fails instead.
type userLookupEngine struct {
	role string
	err  error
}

func (f *userLookupEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	if strings.Contains(q, "userByIdSystem(") {
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{
			Nodes: []*memqlv1.MemoryNode{{
				Id: "v1:identity:user:u1",
				Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
					"role": structpb.NewStringValue(f.role),
				}},
			}},
		}}, nil
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

func floorServer(role string, err error) *Server {
	return &Server{Store: &identity.Store{Engine: &userLookupEngine{role: role, err: err}}}
}

func TestPasskeyRoleFloor_AdmitsDeveloperAndAbove(t *testing.T) {
	for _, role := range []string{"owner", "admin", "developer"} {
		s := floorServer(role, nil)
		if r := s.passkeyRoleFloorRefusal(context.Background(),
			identity.BuiltinClientVSCode, "v1:identity:user:u1"); r != nil {
			t.Errorf("role %q was refused: %v", role, r)
		}
	}
}

func TestPasskeyRoleFloor_RefusesBelowDeveloper(t *testing.T) {
	for _, role := range []string{"writer", "reader", ""} {
		s := floorServer(role, nil)
		r := s.passkeyRoleFloorRefusal(context.Background(),
			identity.BuiltinClientVSCode, "v1:identity:user:u1")
		if r == nil {
			t.Fatalf("role %q was admitted through the passkey path", role)
		}
		if string(r.Actual) != role {
			t.Errorf("Actual = %q, want %q", r.Actual, role)
		}
	}
}

func TestPasskeyRoleFloor_SkipsTheLookupForANonFlooredClient(t *testing.T) {
	// The engine here would ERROR if consulted. A client that declares no
	// floor must not pay a database read -- which is every client but the
	// editor.
	s := floorServer("", errors.New("the store must not be consulted"))
	if r := s.passkeyRoleFloorRefusal(context.Background(), "mcp_abc123", "v1:identity:user:u1"); r != nil {
		t.Fatalf("a non-floored client was refused: %v", r)
	}
}

func TestPasskeyRoleFloor_FailsClosedWhenTheRoleCannotBeRead(t *testing.T) {
	// A floor that opens when the database blinks is not a floor. The person
	// retries; the operator sees the log line.
	s := floorServer("owner", errors.New("boom"))
	if r := s.passkeyRoleFloorRefusal(context.Background(),
		identity.BuiltinClientVSCode, "v1:identity:user:u1"); r == nil {
		t.Fatal("a failed role lookup admitted the sign-in; it must fail closed")
	}
}

func TestBuildClientErrorCallback_CarriesTheOAuthErrorEnvelope(t *testing.T) {
	got, err := buildClientErrorCallback("http://127.0.0.1:54321/callback",
		"access_denied", "your role is reader", "st-1")
	if err != nil {
		t.Fatalf("buildClientErrorCallback: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Query().Get("error") != "access_denied" {
		t.Errorf("error = %q", u.Query().Get("error"))
	}
	if u.Query().Get("error_description") != "your role is reader" {
		t.Errorf("error_description = %q", u.Query().Get("error_description"))
	}
	if u.Query().Get("state") != "st-1" {
		t.Errorf("state = %q", u.Query().Get("state"))
	}
	if u.Query().Get("code") != "" {
		t.Errorf("a refusal must carry no authorization code")
	}
	if _, err := buildClientErrorCallback("://nope", "access_denied", "x", ""); err == nil {
		t.Error("an unparseable redirect URI must error rather than produce a broken target")
	}
}
