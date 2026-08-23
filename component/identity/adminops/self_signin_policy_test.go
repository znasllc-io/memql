package adminops

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// The self-service sign-in policy, verified rather than asserted (memql#4319).
//
// Four properties, and each is a rule this path exists to hold:
//
//  1. It is NOT gated on owner/admin. Every sibling in this package is, and
//     applying `authorize` here would refuse the only caller the path is for.
//  2. The target is the RESOLVED CALLER, never an argument. That absence IS
//     the authorization, because setUserSignInPolicy is @serverOnly with no
//     actor-scoped filter to lean on.
//  3. passkey_only is refused with no active passkey.
//  4. ...and refused too when the passkey list cannot be READ. A transport
//     blip and "no passkeys" must not reach the same decision when the
//     difference is a lockout.
//
// The engine stub answers by query prefix, so a test can put the caller in a
// specific passkey state and then read what actually came out -- rather than
// asserting that some function was called.

type policyEngine struct {
	// passkeys is what `passkeysForSelf` answers with. Each entry is its
	// `active` flag.
	passkeys []bool
	// passkeyErr, when set, makes the passkey read FAIL -- the case the
	// fail-closed rule is for.
	passkeyErr error
	// userPolicy is the policy already stored on the caller's row.
	userPolicy string

	calls []struct {
		query  string
		origin auth.CallOrigin
	}
}

func (e *policyEngine) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.calls = append(e.calls, struct {
		query  string
		origin auth.CallOrigin
	}{q, auth.OriginFromContext(ctx)})

	switch {
	case strings.Contains(q, "passkeysForSelf"):
		if e.passkeyErr != nil {
			return nil, e.passkeyErr
		}
		nodes := make([]*memqlv1.MemoryNode, 0, len(e.passkeys))
		for i, active := range e.passkeys {
			nodes = append(nodes, &memqlv1.MemoryNode{
				Id: "v1:identity:identity:pk",
				Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
					"label":  structpb.NewStringValue(string(rune('A' + i))),
					"active": structpb.NewBoolValue(active),
				}},
			})
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}}, nil

	case strings.Contains(q, "userByIdSystem("):
		return &memqlengine.ExecuteResult{
			Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{
				Id: "v1:identity:user:caller",
				Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
					"primaryEmail": structpb.NewStringValue("caller@example.test"),
					"role":         structpb.NewStringValue("reader"),
					"active":       structpb.NewBoolValue(true),
					"signInPolicy": structpb.NewStringValue(e.userPolicy),
				}},
			}}},
		}, nil
	}
	return &memqlengine.ExecuteResult{}, nil
}

func newPolicyService(t *testing.T, eng *policyEngine) (*Service, *capturingAudit) {
	t.Helper()
	audit := &capturingAudit{}
	svc, err := New(&Service{Engine: eng, Audit: audit})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, audit
}

func TestSelfSignInPolicyIsNotGatedOnOwnerOrAdmin(t *testing.T) {
	// The point of this path. A reader changing their OWN account's sign-in
	// policy is the ordinary case, and the sibling operations' role gate would
	// refuse every one of them.
	for _, role := range []auth.Role{"owner", "admin", "developer", "writer", "reader"} {
		t.Run(string(role), func(t *testing.T) {
			eng := &policyEngine{passkeys: []bool{true}, userPolicy: identity.SignInPolicyAny}
			svc, audit := newPolicyService(t, eng)

			res := svc.SetOwnSignInPolicy(ctxAs(role), identity.SignInPolicyPasskeyOnly)

			if res.Code == CodePermissionDenied {
				t.Fatalf("role %q was refused by a role gate this path must not have: %s",
					role, res.ErrorMessage)
			}
			if !res.OK {
				t.Fatalf("role %q failed: code=%d %s", role, res.Code, res.ErrorMessage)
			}
			for _, ev := range audit.events {
				if ev.Action == "admin_auth_forbidden" {
					t.Fatalf("role %q produced an admin-forbidden audit event", role)
				}
			}
		})
	}
}

func TestSelfSignInPolicyWritesTheCallerAndNobodyElse(t *testing.T) {
	eng := &policyEngine{passkeys: []bool{true}, userPolicy: identity.SignInPolicyAny}
	svc, audit := newPolicyService(t, eng)

	res := svc.SetOwnSignInPolicy(ctxAs("reader"), identity.SignInPolicyPasskeyOnly)
	if !res.OK {
		t.Fatalf("refused: %s", res.ErrorMessage)
	}

	// Every id that reached the engine is the CALLER'S. There is no argument
	// on this path that could carry another, and this is what proves it rather
	// than restating it.
	var sawWrite bool
	for _, call := range eng.calls {
		if strings.Contains(call.query, "updateUser(") {
			sawWrite = true
			if !strings.Contains(call.query, "v1:identity:user:caller") {
				t.Fatalf("the write named somebody other than the caller: %s", call.query)
			}
			if !strings.Contains(call.query, identity.SignInPolicyPasskeyOnly) {
				t.Fatalf("the write did not carry the requested policy: %s", call.query)
			}
			// @serverOnly, so the stamp is mandatory -- and its whole safety
			// argument is that the precondition ran first.
			if call.origin != auth.OriginInternal {
				t.Fatalf("the write was not stamped internal-origin: %v", call.origin)
			}
		}
	}
	if !sawWrite {
		t.Fatalf("no write reached the engine at all; calls=%d", len(eng.calls))
	}

	// The audit trail says who, from what, to what.
	if len(audit.events) == 0 {
		t.Fatal("the change wrote no audit event")
	}
	last := audit.events[len(audit.events)-1]
	if last.ActorUserId != "v1:identity:user:caller" || last.TargetId != "v1:identity:user:caller" {
		t.Fatalf("audit actor/target are not the caller: actor=%q target=%q",
			last.ActorUserId, last.TargetId)
	}
	if last.Detail["by"] != "self" {
		t.Fatalf("audit detail does not mark this as self-service: %+v", last.Detail)
	}
}

func TestSelfSignInPolicyRefusesPasskeyOnlyWithNoPasskey(t *testing.T) {
	eng := &policyEngine{passkeys: nil, userPolicy: identity.SignInPolicyAny}
	svc, audit := newPolicyService(t, eng)

	res := svc.SetOwnSignInPolicy(ctxAs("owner"), identity.SignInPolicyPasskeyOnly)

	if res.OK {
		t.Fatal("passkey_only was accepted with no passkey enrolled -- that is a lockout")
	}
	if res.Code != CodeFailedPrecondition {
		t.Fatalf("code = %d, want FailedPrecondition (%d)", res.Code, CodeFailedPrecondition)
	}
	if res.ErrorMessage != identity.SignInPolicyNeedsPasskeyMessage {
		t.Fatalf("the refusal did not use the shared sentence:\n got %q\nwant %q",
			res.ErrorMessage, identity.SignInPolicyNeedsPasskeyMessage)
	}
	// Nothing was written.
	for _, call := range eng.calls {
		if strings.Contains(call.query, "updateUser(") {
			t.Fatalf("a refused change still wrote: %s", call.query)
		}
	}
	if len(audit.events) == 0 {
		t.Fatal("the refusal wrote no audit event")
	}
}

func TestSelfSignInPolicyRefusesWhenThePasskeyListCannotBeRead(t *testing.T) {
	// FAIL CLOSED. An unreadable list and an empty one must not reach the same
	// decision, and they must not produce the same SENTENCE either: one asks
	// the reader to enrol a passkey, the other to try again.
	eng := &policyEngine{
		passkeyErr: errors.New("stream refused the read"),
		userPolicy: identity.SignInPolicyAny,
	}
	svc, _ := newPolicyService(t, eng)

	res := svc.SetOwnSignInPolicy(ctxAs("owner"), identity.SignInPolicyPasskeyOnly)

	if res.OK {
		t.Fatal("the change was made blind, without a readable passkey list")
	}
	if res.ErrorMessage != identity.SignInPolicyPrecheckFailedMessage {
		t.Fatalf("an unreadable list produced the wrong sentence:\n got %q\nwant %q",
			res.ErrorMessage, identity.SignInPolicyPrecheckFailedMessage)
	}
	for _, call := range eng.calls {
		if strings.Contains(call.query, "updateUser(") {
			t.Fatalf("a refused change still wrote: %s", call.query)
		}
	}
}

func TestSelfSignInPolicyIgnoresInactivePasskeys(t *testing.T) {
	// A revoked passkey is exactly the thing that must not count as a way in.
	eng := &policyEngine{passkeys: []bool{false, false}, userPolicy: identity.SignInPolicyAny}
	svc, _ := newPolicyService(t, eng)

	res := svc.SetOwnSignInPolicy(ctxAs("owner"), identity.SignInPolicyPasskeyOnly)

	if res.OK {
		t.Fatal("two revoked passkeys were counted as a way back in")
	}
	if res.ErrorMessage != identity.SignInPolicyNeedsPasskeyMessage {
		t.Fatalf("wrong refusal: %q", res.ErrorMessage)
	}
}

func TestSelfSignInPolicyBackToAnyNeedsNoPasskey(t *testing.T) {
	// The permissive direction has no precondition -- it is the way OUT of a
	// lockout, so gating it on the thing somebody has just lost would be
	// exactly backwards.
	eng := &policyEngine{passkeys: nil, userPolicy: identity.SignInPolicyPasskeyOnly}
	svc, _ := newPolicyService(t, eng)

	res := svc.SetOwnSignInPolicy(ctxAs("reader"), identity.SignInPolicyAny)

	if !res.OK {
		t.Fatalf("turning links back on was refused: %s", res.ErrorMessage)
	}
	var sawWrite bool
	for _, call := range eng.calls {
		if strings.Contains(call.query, "updateUser(") {
			sawWrite = true
		}
	}
	if !sawWrite {
		t.Fatal("no write reached the engine")
	}
}

func TestSelfSignInPolicyRefusesAnUnknownPolicyAndAnAnonymousCaller(t *testing.T) {
	t.Run("unknown policy", func(t *testing.T) {
		eng := &policyEngine{passkeys: []bool{true}, userPolicy: identity.SignInPolicyAny}
		svc, _ := newPolicyService(t, eng)

		res := svc.SetOwnSignInPolicy(ctxAs("owner"), "password_only")
		if res.OK || res.Code != CodeInvalidArgument {
			t.Fatalf("an unknown policy was not refused as invalid: ok=%v code=%d", res.OK, res.Code)
		}
	})

	t.Run("no actor", func(t *testing.T) {
		eng := &policyEngine{passkeys: []bool{true}, userPolicy: identity.SignInPolicyAny}
		svc, _ := newPolicyService(t, eng)

		res := svc.SetOwnSignInPolicy(context.Background(), identity.SignInPolicyAny)
		if res.OK || res.Code != CodeUnauthenticated {
			t.Fatalf("an anonymous caller was not refused: ok=%v code=%d", res.OK, res.Code)
		}
		// And nothing reached the engine -- there was no row to name.
		if len(eng.calls) != 0 {
			t.Fatalf("an anonymous caller reached the engine: %v", eng.calls)
		}
	})
}

func TestSelfSignInPolicyReSetIsASuccessfulNoOp(t *testing.T) {
	// The caller asked for a state and the state holds. Reporting "failed"
	// would be wrong, and writing a new row version would be noise in an
	// append-only history.
	eng := &policyEngine{passkeys: []bool{true}, userPolicy: identity.SignInPolicyPasskeyOnly}
	svc, _ := newPolicyService(t, eng)

	res := svc.SetOwnSignInPolicy(ctxAs("owner"), identity.SignInPolicyPasskeyOnly)

	if !res.OK {
		t.Fatalf("re-setting the current policy failed: %s", res.ErrorMessage)
	}
	for _, call := range eng.calls {
		if strings.Contains(call.query, "updateUser(") {
			t.Fatalf("a no-op re-set still wrote a row version: %s", call.query)
		}
	}
}

func TestOwnSignInPolicyReadsTheCallersOwnRow(t *testing.T) {
	eng := &policyEngine{userPolicy: identity.SignInPolicyPasskeyOnly}
	svc, _ := newPolicyService(t, eng)

	got, err := svc.OwnSignInPolicy(ctxAs("reader"))
	if err != nil {
		t.Fatalf("OwnSignInPolicy: %v", err)
	}
	if got != identity.SignInPolicyPasskeyOnly {
		t.Fatalf("policy = %q, want %q", got, identity.SignInPolicyPasskeyOnly)
	}

	// An EMPTY stored value normalizes to the permissive one -- rows written
	// before the field existed carry nothing, and reading absence as
	// passkey_only would report every legacy account as locked down.
	eng2 := &policyEngine{userPolicy: ""}
	svc2, _ := newPolicyService(t, eng2)
	got2, err := svc2.OwnSignInPolicy(ctxAs("reader"))
	if err != nil {
		t.Fatalf("OwnSignInPolicy: %v", err)
	}
	if got2 != identity.SignInPolicyAny {
		t.Fatalf("an empty stored policy read as %q, want %q", got2, identity.SignInPolicyAny)
	}
}
