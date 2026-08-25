package identity

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// builtin_clients_test.go -- memql#4515 / memql#4516.
//
// The defect these pin: the VS Code extension could not sign in to a cluster in
// its DEFAULT posture, because it obtained its client_id through RFC 7591 DCR
// and DCR is off by default (memql#3719). Every case below therefore runs with
// OAuthDCREnabled false, no static clients and -- where it can -- a NIL store,
// which is the shape a hardened cluster presents.

func TestBuiltinEditorClient_ResolvesWithDCROffAndNilStore(t *testing.T) {
	cfg := Config{} // no static clients, OAuthDCREnabled false (the default)

	got := ResolveClient(context.Background(), cfg, nil, BuiltinClientVSCode)
	if got == nil {
		t.Fatalf("ResolveClient(%q) = nil; the built-in editor client must resolve with DCR off and no store", BuiltinClientVSCode)
	}
	if got.ClientId != BuiltinClientVSCode {
		t.Fatalf("ClientId = %q, want %q", got.ClientId, BuiltinClientVSCode)
	}
	if got.Name != "MemQL for VS Code" {
		t.Fatalf("Name = %q, want %q -- the consent page shows this", got.Name, "MemQL for VS Code")
	}
}

func TestBuiltinEditorClient_IsNotSelfRegistered(t *testing.T) {
	// memql#3794: the consent page renders a bundled logo (and withholds the
	// "this application described itself" warning) only for clients nobody
	// self-registered. A built-in is code, not a row, so it must report
	// first-party.
	_, selfRegistered := ResolveClientWithOrigin(context.Background(), Config{}, nil, BuiltinClientVSCode)
	if selfRegistered {
		t.Fatalf("ResolveClientWithOrigin(%q) reported selfRegistered=true; built-ins are compiled in, not registered", BuiltinClientVSCode)
	}
}

func TestBuiltinEditorRedirect_MatchesAnyEphemeralPort(t *testing.T) {
	// The registered URI is portless on purpose so RFC 8252 §7.3's any-port
	// exception applies -- the extension's loopback listener takes whatever
	// port the kernel hands it. A regression here is invisible until the
	// SECOND sign-in on a machine.
	cfg := Config{}
	ctx := context.Background()

	for _, uri := range []string{
		"http://127.0.0.1:54321/callback",
		"http://127.0.0.1:1/callback",
		"http://127.0.0.1:65535/callback",
		"http://127.0.0.1/callback", // exact match, no port at all
	} {
		if !ClientAllowsRedirectURI(ctx, cfg, nil, BuiltinClientVSCode, uri) {
			t.Errorf("ClientAllowsRedirectURI(%q) = false, want true", uri)
		}
	}

	for _, uri := range []string{
		"http://127.0.0.1:54321/other",           // wrong path
		"https://127.0.0.1:54321/callback",       // wrong scheme
		"http://evil.example.com:54321/callback", // not loopback
		"http://127.0.0.1:54321/callback/../x",   // not the registered path
	} {
		if ClientAllowsRedirectURI(ctx, cfg, nil, BuiltinClientVSCode, uri) {
			t.Errorf("ClientAllowsRedirectURI(%q) = true, want false", uri)
		}
	}
}

func TestBuiltinClient_StaticConfigShadowsIt(t *testing.T) {
	// OPERATOR CONFIG ALWAYS WINS. An operator who writes the built-in id into
	// MEMQL_IDENTITY_REGISTERED_CLIENTS is deliberately replacing it -- to
	// widen the redirect set, or to opt out of the role floor, which the
	// static entry does not carry. If the built-in could override that, the
	// env var would be advisory.
	cfg := Config{RegisteredClients: []RegisteredClient{{
		ClientId:     BuiltinClientVSCode,
		RedirectURIs: []string{"http://127.0.0.1:7777/custom"},
		Name:         "Our patched editor",
	}}}
	ctx := context.Background()

	got := ResolveClient(ctx, cfg, nil, BuiltinClientVSCode)
	if got == nil || got.Name != "Our patched editor" {
		t.Fatalf("ResolveClient = %+v, want the operator's static entry to shadow the built-in", got)
	}
	if !ClientAllowsRedirectURI(ctx, cfg, nil, BuiltinClientVSCode, "http://127.0.0.1:7777/custom") {
		t.Fatalf("the operator's redirect URI must be accepted")
	}
	// The built-in's redirect set is shadowed WHOLE, not merged: the static
	// entry pins an explicit port, which opts out of the any-port exception.
	if ClientAllowsRedirectURI(ctx, cfg, nil, BuiltinClientVSCode, "http://127.0.0.1:54321/callback") {
		t.Fatalf("the built-in redirect set must not survive alongside the operator's")
	}
	// And the floor goes with it: CheckClientRoleFloor keys on the registry,
	// which the operator has taken responsibility for shadowing.
	if r := CheckClientRoleFloor(BuiltinClientVSCode, auth.RoleReader); r == nil {
		t.Fatalf("sanity: the registry entry still declares a floor")
	}
}

func TestBuiltinClient_DoesNotShadowTheDCRStore(t *testing.T) {
	// A self-registered client with an unrelated id must still resolve out of
	// the store exactly as before -- the built-in tier is additive.
	cfg := Config{}
	store := &Store{Engine: &resolverFakeEngine{
		nodes: []*memqlv1.MemoryNode{
			oauthClientNode("mcp_abc123", `["https://claude.ai/api/mcp/auth_callback"]`),
		},
	}}
	got, selfRegistered := ResolveClientWithOrigin(context.Background(), cfg, store, "mcp_abc123")
	if got == nil || got.ClientId != "mcp_abc123" {
		t.Fatalf("ResolveClientWithOrigin(store client) = %+v, want the DCR row", got)
	}
	if !selfRegistered {
		t.Fatalf("a DCR-store client must still report selfRegistered=true")
	}
}

func TestUnknownClient_StillResolvesToNil(t *testing.T) {
	if got := ResolveClient(context.Background(), Config{}, nil, "not-a-client"); got != nil {
		t.Fatalf("ResolveClient(unknown) = %+v, want nil", got)
	}
	if got := ResolveClient(context.Background(), Config{}, nil, ""); got != nil {
		t.Fatalf("ResolveClient(\"\") = %+v, want nil", got)
	}
}

func TestFindBuiltinClient_ReturnsACopy(t *testing.T) {
	// The registry is package state. A caller that could write through the
	// returned pointer would rewrite the redirect set of every future
	// resolution in the process.
	got := FindBuiltinClient(BuiltinClientVSCode)
	if got == nil {
		t.Fatalf("FindBuiltinClient(%q) = nil", BuiltinClientVSCode)
	}
	got.RedirectURIs[0] = "http://evil.example/cb"
	got.ClientId = "hijacked"

	again := FindBuiltinClient(BuiltinClientVSCode)
	if again == nil || again.ClientId != BuiltinClientVSCode {
		t.Fatalf("registry was mutated through the returned value: %+v", again)
	}
	if again.RedirectURIs[0] != builtinRedirectVSCode {
		t.Fatalf("redirect set was mutated through the returned value: %q", again.RedirectURIs[0])
	}
}

func TestBuiltinRegistry_ShapeSupportsASecondEntry(t *testing.T) {
	// #4515: the registry must not preclude memql-cockpit later. This asserts
	// the resolver has no editor-specific branch -- it iterates the slice --
	// by appending a second entry and resolving it.
	original := builtinClients
	t.Cleanup(func() { builtinClients = original })

	builtinClients = append(append([]BuiltinClient(nil), original...), BuiltinClient{
		Client: RegisteredClient{
			ClientId:     "memql-test-second",
			RedirectURIs: []string{"http://127.0.0.1/second"},
			Name:         "Second first-party client",
		},
	})

	got := ResolveClient(context.Background(), Config{}, nil, "memql-test-second")
	if got == nil || got.Name != "Second first-party client" {
		t.Fatalf("a second registry entry did not resolve: %+v", got)
	}
	// No MinRole declared -> no floor, for anyone.
	if r := CheckClientRoleFloor("memql-test-second", auth.RoleReader); r != nil {
		t.Fatalf("an entry with no MinRole must impose no floor, got %+v", r)
	}
	// ...and the editor's own floor is unaffected by the neighbour.
	if r := CheckClientRoleFloor(BuiltinClientVSCode, auth.RoleReader); r == nil {
		t.Fatalf("the editor floor must survive a second registry entry")
	}
}

// -----------------------------------------------------------------------------
// The role floor (memql#4516)
// -----------------------------------------------------------------------------

func TestEditorRoleFloor_AdmitsDeveloperAndAbove(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleAdmin, auth.RoleDeveloper} {
		if r := CheckClientRoleFloor(BuiltinClientVSCode, role); r != nil {
			t.Errorf("role %q was refused (%v); owner, admin and developer must be admitted", role, r)
		}
	}
}

func TestEditorRoleFloor_RefusesBelowDeveloper(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleWriter, auth.RoleReader, auth.Role(""), auth.Role("nonsense")} {
		r := CheckClientRoleFloor(BuiltinClientVSCode, role)
		if r == nil {
			t.Fatalf("role %q was admitted; the editor floor is developer and above", role)
		}
		if r.Required != auth.RoleDeveloper {
			t.Errorf("Required = %q, want developer", r.Required)
		}
		if r.Actual != role {
			t.Errorf("Actual = %q, want %q", r.Actual, role)
		}
		if r.ClientId != BuiltinClientVSCode {
			t.Errorf("ClientId = %q, want %q", r.ClientId, BuiltinClientVSCode)
		}
	}
}

func TestRoleFloorRefusal_CopyNamesTheRoleAndTheRequirement(t *testing.T) {
	// The extension shows this string verbatim in a notification, so it has to
	// stand on its own: what happened, why, and what to do next.
	r := CheckClientRoleFloor(BuiltinClientVSCode, auth.RoleReader)
	if r == nil {
		t.Fatal("expected a refusal")
	}
	got := r.Description()
	for _, want := range []string{"MemQL for VS Code", "reader", "developer"} {
		if !strings.Contains(got, want) {
			t.Errorf("Description() = %q, missing %q", got, want)
		}
	}
	if r.Error() != got {
		t.Errorf("Error() and Description() must agree")
	}

	// An external user with no cluster-wide role at all is a real state, and
	// the copy must not claim they hold a tier they do not.
	none := CheckClientRoleFloor(BuiltinClientVSCode, auth.Role(""))
	if none == nil {
		t.Fatal("expected a refusal for a roleless user")
	}
	if !strings.Contains(none.Description(), "none") {
		t.Errorf("Description() for a roleless user = %q, want it to say \"none\"", none.Description())
	}

	detail := r.AuditDetail()
	if detail["clientId"] != BuiltinClientVSCode ||
		detail["requiredRole"] != string(auth.RoleDeveloper) ||
		detail["actualRole"] != string(auth.RoleReader) {
		t.Errorf("AuditDetail() = %+v", detail)
	}
}

func TestRoleFloor_DoesNotTouchOtherClients(t *testing.T) {
	// A static client and a DCR-store client must be completely unaffected --
	// the floor is a property of what the EDITOR is, not an OAuth feature.
	for _, clientId := range []string{"app", "mcp_abc123", "", "unknown"} {
		if r := CheckClientRoleFloor(clientId, auth.RoleReader); r != nil {
			t.Errorf("CheckClientRoleFloor(%q, reader) = %+v, want nil", clientId, r)
		}
	}
}

func TestRoleMeetsFloor_FailsClosedOnAnUnrecognisedFloor(t *testing.T) {
	// A typo in a future registry entry must admit nobody rather than
	// everybody. "No floor" is expressed by an EMPTY MinRole, which
	// CheckClientRoleFloor short-circuits on before reaching here.
	if roleMeetsFloor(auth.RoleOwner, auth.Role("superuser")) {
		t.Fatal("an unrecognised floor admitted an owner; the switch must fail closed")
	}
}
