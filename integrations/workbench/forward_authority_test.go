package workbench

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// forward_authority_test.go -- memql#3219.
//
// WorkbenchForwardRequest.auth was a map<string, string> that nothing
// populated and nothing read. Its doc comment claimed it "carries the
// originating principal's claims (subject, email, role) so the workbench node
// can reconstruct the actor context"; the only reference to it anywhere in the
// tree was its generated getter. Same class as the QueryForward field memql#2814
// removed: a dead auth carrier that reads as real, and that the next person to
// wire remote-workbench mode would reach for.
//
// These pin the three properties that make it not that any more:
//
//	1. the receiver REFUSES an envelope with no verifiable assertion,
//	2. it BINDS the actor the assertion names when there is one,
//	3. the producer FAILS CLOSED rather than sending an unauthenticated one.
//
// (2) is the one with no natural home in a refusal test, and it is the one the
// AI-forward path spent memql#3205 and memql#2876 learning to get right: a
// receiver that verifies and then forgets to bind leaves worker-side DSL with
// no actor, every owned read returning zero rows and every write stamping
// createdBy: "".

// collectResponse captures the single WorkbenchForwardResponse a handler sends.
func collectResponse(t *testing.T) (func(*nodev1.NodeServerMessage) error, func() *nodev1.WorkbenchForwardResponse) {
	t.Helper()
	var got *nodev1.WorkbenchForwardResponse
	send := func(m *nodev1.NodeServerMessage) error {
		if r := m.GetWorkbenchForwardResponse(); r != nil {
			got = r
		}
		return nil
	}
	return send, func() *nodev1.WorkbenchForwardResponse { return got }
}

func silentHandler(t *testing.T) *ForwardHandler {
	t.Helper()
	return NewForwardHandler(NewIntegration(slog.New(slog.DiscardHandler)), slog.New(slog.DiscardHandler))
}

func userAuthorityProto(t *testing.T) *nodev1.ForwardedAuthority {
	t.Helper()
	a, err := auth.ForwardedAuthorityForUser(
		&auth.AccessContext{
			UserId:       "v1:identity:user:alice",
			PrimaryEmail: "alice@example.com",
			Role:         auth.RoleWriter,
		},
		auth.ForwardedClassUser, "", time.Time{}, time.Now())
	if err != nil {
		t.Fatalf("ForwardedAuthorityForUser: %v", err)
	}
	return node.ForwardedAuthorityToProto(a.WithDisplayName("Alice", "Nakamura"), "agent-1", "agent")
}

// (1) An envelope with no assertion must be refused before anything touches
// the workspace. This is the case the dead `auth` map made look normal: with
// no carrier at all, "unauthenticated" was simply the shape every request had.
func TestWorkbenchForwardRefusesAnEnvelopeWithNoAuthority(t *testing.T) {
	send, got := collectResponse(t)

	silentHandler(t).HandleForwardedRequest(context.Background(), &nodev1.WorkbenchForwardRequest{
		RequestId: "req-1",
		PlanId:    "v1:planner:plan:p1",
		Action:    "fs_list",
		ArgsJson:  []byte(`{"path":"."}`),
		// No Authority.
	}, send)

	resp := got()
	if resp == nil {
		t.Fatal("the handler sent NO response. The agent's tool loop parks on one; a dropped " +
			"message is a hung turn rather than a refusal.")
	}
	if resp.GetErrorCode() != "forwarded_authority_refused" {
		t.Errorf("errorCode = %q, want forwarded_authority_refused.\n\n"+
			"An ABSENT assertion must take the same path as a malformed one -- the contract's "+
			"premise is that absence is never read as safe. memql#3219.", resp.GetErrorCode())
	}
}

// The refusal must survive the shapes an attacker would actually try, not just
// the empty one. Each of these is a rule VerifyForwardedAuthority enforces; the
// point here is that the workbench receiver runs it at all.
func TestWorkbenchForwardRefusesUnprovableAssertions(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*nodev1.ForwardedAuthority)
		because string
	}{
		{"wrong contract version", func(a *nodev1.ForwardedAuthority) { a.ContractVersion = "v2" },
			"an unsupported version must be refused, not best-effort parsed"},
		{"no credential class", func(a *nodev1.ForwardedAuthority) { a.CredentialClass = "" },
			"an empty class is what made an unstated badge ceiling undetectable (memql#2876)"},
		{"unknown credential class", func(a *nodev1.ForwardedAuthority) { a.CredentialClass = "superuser" },
			"an unrecognised class must refuse rather than land in the most permissive bucket"},
		{"badge with no ceiling", func(a *nodev1.ForwardedAuthority) { a.CredentialClass = auth.ForwardedClassBadge },
			"a badge assertion with no ceiling cannot be proved clamped"},
		{"system principal wearing a user id", func(a *nodev1.ForwardedAuthority) {
			a.PrincipalKind = nodev1.ForwardedPrincipalKind_FORWARDED_PRINCIPAL_KIND_SYSTEM
			a.CredentialClass = auth.ForwardedClassSystem
			a.Role = string(auth.RoleReader)
		}, "downstream owner resolution keys on the canonical user id prefix"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authority := userAuthorityProto(t)
			tc.mutate(authority)
			send, got := collectResponse(t)

			silentHandler(t).HandleForwardedRequest(context.Background(), &nodev1.WorkbenchForwardRequest{
				RequestId: "req-1",
				PlanId:    "v1:planner:plan:p1",
				Action:    "fs_list",
				ArgsJson:  []byte(`{"path":"."}`),
				Authority: authority,
			}, send)

			resp := got()
			if resp == nil || resp.GetErrorCode() != "forwarded_authority_refused" {
				t.Errorf("this envelope was not refused (%+v) -- %s", resp, tc.because)
			}
		})
	}
}

// (2) The accept-and-bind half, which no refusal test can reach. A receiver
// that verifies and then forgets to bind leaves worker-side DSL with NO actor:
// under the deny-on-nil default every owned read returns zero rows and every
// write stamps createdBy: "". That is memql#2876, and it looks like success.
func TestWorkbenchForwardBindsTheAssertedActor(t *testing.T) {
	// Through the PRODUCTION accept path, not a re-implementation of it:
	// deleting the bind from bindAuthority reddens this, which a test that
	// called auth.BindForwardedContext itself could not detect.
	ctx, err := silentHandler(t).bindAuthority(context.Background(), &nodev1.WorkbenchForwardRequest{
		RequestId: "req-1",
		PlanId:    "v1:planner:plan:p1",
		Action:    "fs_list",
		Authority: userAuthorityProto(t),
	})
	if err != nil {
		t.Fatalf("bindAuthority refused a well-formed assertion: %v", err)
	}

	bound, ok := auth.AccessFromContext(ctx)
	if !ok || bound == nil {
		t.Fatal("no AccessContext on the dispatch ctx -- worker-side DSL would resolve " +
			"actor.userId to \"\" and every owned query would return zero rows. memql#2876.")
	}
	if bound.UserId != "v1:identity:user:alice" {
		t.Errorf("bound actor = %q, want the asserted principal", bound.UserId)
	}
	if bound.Role != auth.RoleWriter {
		t.Errorf("bound role = %q, want the asserted (already-clamped) writer", bound.Role)
	}

	// Attribution rides the same bind, derived from the assertion rather than
	// from a second carrier -- including the provenance-only display name
	// (memql#3221), which lands on every row the workbench's dispatch writes.
	id, err := auth.UserIdentityFromContext(ctx)
	if err != nil {
		t.Fatalf("UserIdentityFromContext: %v", err)
	}
	if id.Subject != "v1:identity:user:alice" {
		t.Errorf("createdBy attribution subject = %q, want alice", id.Subject)
	}
	if id.FirstName != "Alice" || id.LastName != "Nakamura" {
		t.Errorf("display name lost on the workbench hop: %q / %q", id.FirstName, id.LastName)
	}

	// And the chain of custody, which is what lets a THIRD hop out of the
	// workbench node re-assert rather than rebuild.
	carried, ok := auth.ForwardedAuthorityFromContext(ctx)
	if !ok {
		t.Fatal("the verified assertion is not on the ctx; a further forward would have to " +
			"rebuild one from the AccessContext, which carries no class and no ceiling")
	}
	if carried.CredentialClass != auth.ForwardedClassUser {
		t.Errorf("carried class = %q, want the asserted one", carried.CredentialClass)
	}
}

// (3) The producer half. tryForward must not emit an envelope it cannot make
// an assertion for -- that is precisely how a dead auth carrier is born.
//
// The failure is a structured dispatchResult so the agent's tool loop surfaces
// it to the LLM, and it must NOT fall back to local dispatch: the per-Plan
// workspace lives on the workbench node, so running locally would silently
// operate on a different filesystem.
func TestWorkbenchProducerFailsClosedWithNoAuthorityOnTheContext(t *testing.T) {
	integ := NewIntegration(slog.New(slog.DiscardHandler))
	integ.SetForwardRouter(NewForwardRouter(nil, slog.New(slog.DiscardHandler)))

	nodes, handled := integ.tryForward(context.Background(), "v1:planner:plan:p1", "fs_list",
		map[string]any{"path": "."}, map[string]any{}, time.Now())

	if !handled {
		t.Fatal("tryForward reported the call unhandled, which sends it to LOCAL dispatch. " +
			"The per-Plan workspace lives on the workbench node; running locally operates on a " +
			"different filesystem and reports success for work the caller cannot see.")
	}
	if len(nodes) != 1 {
		t.Fatalf("want exactly one structured result node, got %d", len(nodes))
	}
	if got := string(nodes[0].Payload); !strings.Contains(got, "no_forwarded_authority") {
		t.Errorf("result payload = %s\nwant a no_forwarded_authority failure -- an envelope with "+
			"no assertion must never reach the wire. memql#3219.", got)
	}
}

// The producer re-asserts what this node accepted rather than rebuilding one.
// Rebuilding from the AccessContext would be a DOWNGRADE: an AccessContext
// carries no credential class and no role ceiling, so a badge session would
// arrive at the workbench as class="user" with no ceiling -- and "no badge"
// being indistinguishable from "not stated" is the exact property memql#3205
// removed from the AI-forward path.
func TestWorkbenchProducerReAssertsTheBadgeCeiling(t *testing.T) {
	expires := time.Now().Add(5 * time.Minute)
	received, err := auth.ForwardedAuthorityForUser(
		&auth.AccessContext{
			UserId:       "v1:identity:user:operator-9",
			PrimaryEmail: "op@example.com",
			Role:         auth.RoleReader,
		},
		auth.ForwardedClassBadge, auth.RoleReader, expires, time.Now())
	if err != nil {
		t.Fatalf("ForwardedAuthorityForUser: %v", err)
	}

	ctx := auth.ContextWithForwardedAuthority(context.Background(), received)
	carried, ok := auth.ForwardedAuthorityFromContext(ctx)
	if !ok {
		t.Fatal("the assertion did not survive the ctx round trip")
	}

	// What the producer would put on the wire, and what the workbench would
	// then be able to prove about it.
	onWire := node.ForwardedAuthorityFromProto(
		node.ForwardedAuthorityToProto(carried, "agent-1", "agent"))

	if onWire.CredentialClass != auth.ForwardedClassBadge {
		t.Errorf("class on the second hop = %q, want badge. A rebuilt assertion says \"user\" "+
			"here, and the workbench cannot then tell a kiosk badge from an ordinary session.",
			onWire.CredentialClass)
	}
	if onWire.RoleCeiling != auth.RoleReader {
		t.Errorf("ceiling on the second hop = %q, want reader -- an AccessContext has no ceiling "+
			"to rebuild this from", onWire.RoleCeiling)
	}
	if _, err := auth.VerifyForwardedAuthority(onWire, time.Now()); err != nil {
		t.Errorf("the workbench refused a faithfully re-asserted badge: %v", err)
	}
	// And the expiry still bites at the same instant as the direct path's gate.
	if _, err := auth.VerifyForwardedAuthority(onWire, expires.Add(time.Second)); err == nil {
		t.Error("an expired badge was accepted on the second hop; the expiry must travel with " +
			"the assertion, not be re-derived")
	}
}
