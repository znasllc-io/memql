package magiclink

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// role_floor_test.go -- memql#4516.
//
// The magic-link path is the PRIMARY way a person signs an editor in, so it is
// the one the role floor most has to be right on. What is pinned here:
//
//   - a role below the floor is refused BEFORE any auth code exists, and the
//     refusal carries the redirect URI + state the handler needs to send an
//     OAuth error envelope back to the editor;
//   - developer, admin and owner are admitted and DO get a code;
//   - a client that declares no floor is untouched;
//   - the refusal writes one audit row naming the client, the requirement and
//     the role.
//
// It is behavioural rather than structural (contrast bootstrap_stamp_test.go's
// note): identity.Store's Engine is an interface, which is seam enough to drive
// Finish end to end, and DirectDB being nil makes the advisory-lock gate a
// pass-through.

// mlFakeEngine answers the handful of calls Finish makes.
type mlFakeEngine struct {
	mu sync.Mutex

	email    string
	role     string
	oauthCtx string
	// consumed flips after the consume mutation so a second read reports it.
	consumed bool

	authCodeCalls int
	consumeCalls  int
	calls         []string
}

func (f *mlFakeEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, q)

	empty := &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}
	switch {
	case strings.Contains(q, "magicLinkRequestById("):
		fields := map[string]*structpb.Value{
			"id":           structpb.NewStringValue("v1:identity:magiclink:ml-1"),
			"email":        structpb.NewStringValue(f.email),
			"expiresAt":    structpb.NewStringValue(time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)),
			"approvedAt":   structpb.NewStringValue(time.Now().UTC().Format(time.RFC3339Nano)),
			"createdAt":    structpb.NewStringValue(time.Now().UTC().Format(time.RFC3339Nano)),
			"oauthCtxJSON": structpb.NewStringValue(f.oauthCtx),
		}
		if f.consumed {
			fields["consumedAt"] = structpb.NewStringValue(time.Now().UTC().Format(time.RFC3339Nano))
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{
			Nodes: []*memqlv1.MemoryNode{{Id: "v1:identity:magiclink:ml-1", Payload: &structpb.Struct{Fields: fields}}},
		}}, nil

	case strings.Contains(q, "consumeMagicLinkRequest("):
		f.consumeCalls++
		f.consumed = true
		return empty, nil

	case strings.Contains(q, "userByEmail("):
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{
			Nodes: []*memqlv1.MemoryNode{{
				Id: "v1:identity:user:u-1",
				Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
					"id":           structpb.NewStringValue("v1:identity:user:u-1"),
					"primaryEmail": structpb.NewStringValue(f.email),
					"role":         structpb.NewStringValue(f.role),
					"active":       structpb.NewBoolValue(true),
				}},
			}},
		}}, nil

	case strings.Contains(q, "createAuthCode("):
		f.authCodeCalls++
		return empty, nil
	}
	return empty, nil
}

// mlEditorOAuthCtx is what /authorize stamps onto the row for the built-in
// editor client: a portless-registered loopback redirect on an ephemeral port,
// PKCE required.
const mlEditorOAuthCtx = `{"clientId":"memql-vscode","redirectURI":"http://127.0.0.1:54321/callback","state":"st-42","codeChallenge":"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM","codeChallengeMethod":"S256"}`

type mlCapturingAudit struct {
	mu     sync.Mutex
	events []identity.AuditEvent
}

func (a *mlCapturingAudit) Log(_ context.Context, ev identity.AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
}

func (a *mlCapturingAudit) find(action string) *identity.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.events {
		if a.events[i].Action == action {
			return &a.events[i]
		}
	}
	return nil
}

func newFloorVerifier(role string) (*Verifier, *mlFakeEngine, *mlCapturingAudit) {
	eng := &mlFakeEngine{email: "someone@example.com", role: role, oauthCtx: mlEditorOAuthCtx}
	audit := &mlCapturingAudit{}
	return &Verifier{
		Cfg:   identity.Config{BaseURL: "https://identity.example.test"},
		Store: &identity.Store{Engine: eng},
		Audit: audit,
	}, eng, audit
}

func finishOnce(v *Verifier) (*VerifyResult, error) {
	return v.Finish(context.Background(), FinishInput{
		RequestId: "v1:identity:magiclink:ml-1",
		SourceIP:  "203.0.113.7",
		UserAgent: "Mozilla/5.0",
	})
}

func TestMagicLinkFinish_RefusesBelowTheEditorFloor(t *testing.T) {
	for _, role := range []string{"writer", "reader", ""} {
		t.Run("role="+role, func(t *testing.T) {
			v, eng, audit := newFloorVerifier(role)

			res, err := finishOnce(v)
			if err == nil {
				t.Fatalf("role %q completed sign-in; the editor floor is developer and above (res=%+v)", role, res)
			}
			var floor *RoleFloorError
			if !errors.As(err, &floor) {
				t.Fatalf("error = %T (%v), want *RoleFloorError", err, err)
			}
			// The handler needs both of these to send the OAuth error
			// envelope back to the editor rather than rendering a page the
			// extension will never see.
			if floor.RedirectURI != "http://127.0.0.1:54321/callback" {
				t.Errorf("RedirectURI = %q", floor.RedirectURI)
			}
			if floor.State != "st-42" {
				t.Errorf("State = %q, want st-42", floor.State)
			}
			for _, want := range []string{"developer"} {
				if !strings.Contains(floor.Error(), want) {
					t.Errorf("message %q is missing %q", floor.Error(), want)
				}
			}

			eng.mu.Lock()
			codes, consumes := eng.authCodeCalls, eng.consumeCalls
			eng.mu.Unlock()
			if codes != 0 {
				t.Errorf("a refused sign-in minted %d auth code(s); nothing redeemable may be created", codes)
			}
			// The link IS consumed: a refused attempt must not leave a live
			// link behind for somebody to retry.
			if consumes != 1 {
				t.Errorf("consume calls = %d, want 1", consumes)
			}

			ev := audit.find(identity.AuditActionRoleFloorRefused)
			if ev == nil {
				t.Fatalf("no %q audit row", identity.AuditActionRoleFloorRefused)
			}
			if ev.Outcome != identity.AuditOutcomeBlocked {
				t.Errorf("Outcome = %q", ev.Outcome)
			}
			if ev.Detail["clientId"] != identity.BuiltinClientVSCode ||
				ev.Detail["requiredRole"] != "developer" ||
				ev.Detail["actualRole"] != role {
				t.Errorf("Detail = %+v", ev.Detail)
			}
		})
	}
}

func TestMagicLinkFinish_AdmitsDeveloperAndAbove(t *testing.T) {
	for _, role := range []string{"owner", "admin", "developer"} {
		t.Run(role, func(t *testing.T) {
			v, eng, audit := newFloorVerifier(role)

			res, err := finishOnce(v)
			if err != nil {
				t.Fatalf("role %q was refused: %v", role, err)
			}
			if res.AuthCode == "" {
				t.Fatalf("no auth code was returned for %q", role)
			}
			if res.ClientId != identity.BuiltinClientVSCode {
				t.Errorf("ClientId = %q", res.ClientId)
			}
			eng.mu.Lock()
			codes := eng.authCodeCalls
			eng.mu.Unlock()
			if codes != 1 {
				t.Errorf("auth code calls = %d, want 1", codes)
			}
			if ev := audit.find(identity.AuditActionRoleFloorRefused); ev != nil {
				t.Errorf("an admitted role wrote a refusal row: %+v", ev)
			}
		})
	}
}

func TestMagicLinkFinish_NonFlooredClientIsUnaffected(t *testing.T) {
	// The floor is a property of the built-in editor client, not of OAuth. A
	// reader signing into the portal or an MCP connector must be unaffected.
	v, eng, audit := newFloorVerifier("reader")

	// A statically-configured relying party instead of the editor.
	v.Cfg.RegisteredClients = []identity.RegisteredClient{{
		ClientId:     "portal",
		RedirectURIs: []string{"https://portal.example.test/auth/callback"},
	}}
	eng.mu.Lock()
	eng.oauthCtx = `{"clientId":"portal","redirectURI":"https://portal.example.test/auth/callback","state":"st-9"}`
	eng.mu.Unlock()

	res, err := finishOnce(v)
	if err != nil {
		t.Fatalf("a reader was refused a client that declares no floor: %v", err)
	}
	if res.AuthCode == "" {
		t.Fatal("no auth code was returned")
	}
	if ev := audit.find(identity.AuditActionRoleFloorRefused); ev != nil {
		t.Fatalf("a non-floored client wrote a refusal row: %+v", ev)
	}
}
