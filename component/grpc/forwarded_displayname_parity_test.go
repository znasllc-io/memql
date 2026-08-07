package memql

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/metadata"
)

// forwarded_displayname_parity_test.go -- memql#3221.
//
// component/metadata's collectIdentity builds identity.displayName from
// UserIdentity.FirstName + " " + LastName, and the collector runs on EVERY
// mutation (executeInsert -> metadataCollector.Collect) on every node type,
// with no build tag. So the value it produces is a property of the CONTEXT the
// write happens under, and a hop that loses the names writes a different row.
//
// This drives the real chain -- producer session -> forwardedPrincipal ->
// forwardedAuthorityToProto -> the wire -> forwardedAuthorityFromProto ->
// VerifyForwardedAuthority -> bindForwardedContext -> the collector -- and
// compares what lands against the direct path.
//
// FALSIFIABILITY, mutation-checked rather than asserted:
//
//	dropping FirstName/LastName from forwardedAuthorityToProto:
//	  forwarded row carries identity.displayName="" -- direct-path rows for the
//	  same user carry "Alice Nakamura"
//	dropping the given_name/family_name claims from Principal():
//	  same failure, one layer earlier
//
// It compares against the direct path rather than against a hardcoded string
// on purpose: the deliverable is PARITY. A change that alters how
// displayName is composed should move both sides together or fail here.

// directPathIdentity is what a normal, non-forwarded session puts on the
// context: the verifier's claims, from which buildIdentityFromToken derives the
// names. This is the baseline the mesh must match.
func directPathContext(subject, email, first, last string) context.Context {
	return auth.ContextWithToken(context.Background(), auth.BuildTokenInfo(map[string]any{
		"sub":         subject,
		"email":       email,
		"role":        "writer",
		"given_name":  first,
		"family_name": last,
	}))
}

// forwardedPathContext runs the PRODUCTION producer and receiver halves over a
// simulated hop: a real streamSession builds the principal, the authority is
// marshalled to the node proto and back, the receiver verifies it and binds.
func forwardedPathContext(t *testing.T, subject, email, first, last string) context.Context {
	t.Helper()

	// Producer: a session with a resolved access context and an identity, which
	// is where the names live -- an AccessContext carries none.
	sess := &streamSession{
		stream: &captureStream{ctx: context.Background()},
		identity: auth.UserIdentity{
			Subject:   subject,
			Email:     email,
			Role:      "writer",
			FirstName: first,
			LastName:  last,
		},
		access: &auth.AccessContext{
			UserId:       subject,
			PrimaryEmail: email,
			Role:         auth.RoleWriter,
		},
		accessLoaded:    true,
		credentialClass: auth.ForwardedClassUser,
		closeChan:       make(chan struct{}),
	}

	principal, err := sess.forwardedPrincipal()
	if err != nil {
		t.Fatalf("forwardedPrincipal: %v", err)
	}

	// The wire: exactly the two adapters AiForwardRouter.Forward and
	// HandleForwardedRequest use.
	onWire := forwardedAuthorityToProto(principal.Authority, "bff-1", "bff")
	received := forwardedAuthorityFromProto(onWire)

	access, err := auth.VerifyForwardedAuthority(received, time.Now())
	if err != nil {
		t.Fatalf("the receiver refused a well-formed assertion: %v", err)
	}

	return bindForwardedContext(context.Background(), principal.Claims, access)
}

// The acceptance criterion, asserted where it actually lands: on the metadata
// map the engine stamps onto a written row.
func TestForwardedWriteCarriesTheSameDisplayNameAsADirectWrite(t *testing.T) {
	const (
		subject = "v1:identity:user:alice"
		email   = "alice@example.com"
		first   = "Alice"
		last    = "Nakamura"
	)
	collector := metadata.NewCollector(metadata.ServerMeta{NodeType: "agent"}, "", nil)

	direct := collector.Collect(directPathContext(subject, email, first, last))
	forwarded := collector.Collect(forwardedPathContext(t, subject, email, first, last))

	// The control. If the direct path stopped producing a display name, the
	// comparison below would pass on two empty strings -- and this test's whole
	// subject is that the two agree on a NON-empty value.
	if direct["identity.displayName"] == "" {
		t.Fatalf("control broken: the direct path produced no identity.displayName, so parity "+
			"with it proves nothing. direct=%v", direct)
	}

	if forwarded["identity.displayName"] != direct["identity.displayName"] {
		t.Errorf("identity.displayName: forwarded=%q direct=%q.\n\n"+
			"component/metadata runs on EVERY mutation on every node type, so this key lands in "+
			"the audit metadata of every row. Someone reading rows to answer 'who did this' "+
			"finds the answer present for BFF-written rows and absent for worker-written ones, "+
			"with nothing in the data explaining the difference. memql#3221.",
			forwarded["identity.displayName"], direct["identity.displayName"])
	}

	// The rest of the identity block must agree too -- the names were the
	// regression, but a divergence anywhere here is the same class of defect.
	for _, key := range []string{"identity.subject", "identity.role", "identity.email"} {
		if forwarded[key] != direct[key] {
			t.Errorf("%s: forwarded=%q direct=%q", key, forwarded[key], direct[key])
		}
	}
}

// The names must survive the PROTO hop specifically. Without this, a test that
// only exercised Principal() would pass while the fields were absent from
// node.proto and the value never left the producer's process.
func TestTheDisplayNameSurvivesTheProtoRoundTrip(t *testing.T) {
	a := auth.ForwardedAuthority{
		Version:         auth.ForwardedAuthorityVersion,
		Kind:            auth.ForwardedPrincipalUser,
		Subject:         "v1:identity:user:alice",
		PrimaryEmail:    "alice@example.com",
		Role:            auth.RoleWriter,
		CredentialClass: auth.ForwardedClassUser,
		FirstName:       "Alice",
		LastName:        "Nakamura",
	}

	got := forwardedAuthorityFromProto(forwardedAuthorityToProto(a, "bff-1", "bff"))
	if got.FirstName != "Alice" || got.LastName != "Nakamura" {
		t.Errorf("names after the proto round trip = %q / %q, want Alice / Nakamura.\n\n"+
			"If they are empty, the fields are missing from node.proto or from one of the two "+
			"adapters, and the display name never leaves the producing node.",
			got.FirstName, got.LastName)
	}
}

// A SYSTEM forward has no user behind it and must carry no name -- there is no
// person to attribute, and a synthesised one would be a fabricated provenance
// record rather than a missing one.
func TestASystemForwardCarriesNoDisplayName(t *testing.T) {
	a, err := auth.ForwardedAuthorityForSystem("system:planner", time.Now())
	if err != nil {
		t.Fatalf("ForwardedAuthorityForSystem: %v", err)
	}
	claims := a.Principal().Claims
	if _, ok := claims["given_name"]; ok {
		t.Error("a system forward carries a given_name claim")
	}
	if _, ok := claims["family_name"]; ok {
		t.Error("a system forward carries a family_name claim")
	}

	collector := metadata.NewCollector(metadata.ServerMeta{NodeType: "agent"}, "", nil)
	access, err := auth.VerifyForwardedAuthority(forwardedAuthorityFromProto(
		forwardedAuthorityToProto(a, "planner-1", "planner")), time.Now())
	if err != nil {
		t.Fatalf("VerifyForwardedAuthority: %v", err)
	}
	m := collector.Collect(bindForwardedContext(context.Background(), claims, access))
	if got := m["identity.displayName"]; got != "" {
		t.Errorf("a system-principal write stamped identity.displayName=%q; there is no person "+
			"behind this hop to name", got)
	}
}
