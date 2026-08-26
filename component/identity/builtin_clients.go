package identity

import (
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// Built-in first-party OAuth clients.
//
// -----------------------------------------------------------------------------
// WHY FIRST-PARTY TOOLING DOES NOT RIDE THE THIRD-PARTY DOOR
// -----------------------------------------------------------------------------
//
// RFC 7591 dynamic client registration exists so an application NOBODY
// configured can obtain a client_id on its own. That is the right shape for a
// third-party MCP connector, and the wrong shape for software that ships with
// this product: POST /register is an unauthenticated write that mints a
// v1:identity:oauthClient row whose client_name -- the string a human is then
// shown when asked to approve access -- is chosen by the caller. memql#3719
// therefore defaults MEMQL_IDENTITY_OAUTH_DCR_ENABLED to FALSE and pins that
// default with a test, so a cluster that exposes no MCP surface never opens it.
//
// The VS Code extension was reaching for that door anyway, so on every cluster
// in its default posture sign-in ended at
// `https://identity.<domain>/register returned 403: registration_disabled`,
// and the RFC 8628 device fallback dead-ended in the same place because
// /device/code also refuses an unregistered client.
//
// The editor ships WITH the product. Identity can therefore know it the way it
// knows any operator-configured relying party -- as a client that exists
// without registration. That is what this file is: a small compiled-in list,
// resolved between the operator's static config and the DCR store
// (client_resolver.go), adding no unauthenticated write, no runtime-chosen
// client_name, and no change whatsoever to POST /register or to memql#3719's
// pinned default.
//
// -----------------------------------------------------------------------------
// WHY THE REDIRECT URI IS PORTLESS
// -----------------------------------------------------------------------------
//
// The extension's loopback listener takes whatever ephemeral port the kernel
// hands it, so the redirect_uri the browser actually returns to carries a
// different number every sign-in. RFC 8252 §7.3 exists for exactly this and
// config.go implements it in matchesLoopbackAnyPort: a registered loopback
// redirect URI WITH NO EXPLICIT PORT matches an incoming URI on any port when
// the scheme, host and path agree -- and a registered URI that carries a port
// opts OUT of the exception and goes back to exact match. Adding a port to the
// value below would silently break every callback.
//
// The string must stay byte-identical to the extension's own
// WELL_KNOWN_REDIRECT_URI (editors/vscode/src/auth/wellKnownClient.ts), because
// the matcher compares scheme, host and PATH exactly.

// BuiltinClientVSCode is the client_id the MemQL VS Code extension
// authorizes with when clusters.yaml names no override.
//
// The value is part of the wire contract between a released extension and a
// released cluster: changing it strands every editor that already ships the
// old string. Treat it as frozen.
const BuiltinClientVSCode = "memql-vscode"

// builtinRedirectVSCode is the portless loopback redirect the editor
// registers. See the header for why the missing port is load-bearing.
const builtinRedirectVSCode = "http://127.0.0.1/callback"

// BuiltinClient is a compiled-in first-party relying party: the
// RegisteredClient the resolver hands back, plus the policy that is a
// property of what this particular application IS rather than of OAuth.
type BuiltinClient struct {
	Client RegisteredClient

	// MinRole is the lowest cluster-wide role allowed to complete sign-in
	// through this client, or "" for no floor.
	//
	// This is deliberately NOT an env var. The floor is a statement about the
	// application -- the editor is a management surface, so people who do not
	// manage the cluster have no business connecting one to it -- and a
	// per-cluster knob would turn that statement into a configuration
	// accident. An operator who genuinely needs different policy shadows the
	// client id in MEMQL_IDENTITY_REGISTERED_CLIENTS, which carries no floor;
	// that is an explicit, visible act rather than a default nobody reviewed.
	MinRole auth.Role
}

// builtinClients is the registry. A plain slice, iterated by the resolver with
// no per-client special cases, so adding the cockpit (or any other first-party
// surface) later is one entry and no code.
var builtinClients = []BuiltinClient{
	{
		Client: RegisteredClient{
			ClientId:     BuiltinClientVSCode,
			RedirectURIs: []string{builtinRedirectVSCode},
			Name:         "MemQL for VS Code",
		},
		// Developer and above: owner, admin and developer complete sign-in;
		// writer and reader are refused.
		//
		// INCLUDING ADMIN IS DELIBERATE. An admin operates the portal's admin
		// console (component/identity/adminops gates at owner/admin), so
		// admitting them there and refusing them the editor would be
		// incoherent. "Developer and above" is also the deploy gate
		// (auth.AtLeastDeveloper, memql#1876), which is the same authority the
		// editor exercises.
		MinRole: auth.RoleDeveloper,
	},
}

// BuiltinClients returns a copy of the registry. For tests and for anything
// that wants to enumerate first-party clients without being able to mutate
// them.
func BuiltinClients() []BuiltinClient {
	out := make([]BuiltinClient, len(builtinClients))
	copy(out, builtinClients)
	return out
}

// findBuiltinClient returns the registry entry for clientId, or nil.
func findBuiltinClient(clientId string) *BuiltinClient {
	clientId = strings.TrimSpace(clientId)
	if clientId == "" {
		return nil
	}
	for i := range builtinClients {
		if builtinClients[i].Client.ClientId == clientId {
			return &builtinClients[i]
		}
	}
	return nil
}

// FindBuiltinClient returns the RegisteredClient a built-in entry declares, or
// nil when clientId names no built-in. Callers that want the resolution ORDER
// (operator config first) must go through ResolveClient instead -- this is the
// raw registry lookup.
func FindBuiltinClient(clientId string) *RegisteredClient {
	b := findBuiltinClient(clientId)
	if b == nil {
		return nil
	}
	// Return a copy: the registry is package state and a caller holding a
	// pointer into it could rewrite the redirect set of every future
	// resolution.
	c := b.Client
	c.RedirectURIs = append([]string(nil), b.Client.RedirectURIs...)
	return &c
}

// -----------------------------------------------------------------------------
// The role floor
// -----------------------------------------------------------------------------

// RoleFloorRefusal describes a sign-in refused because the signed-in user's
// role is below the floor the requesting client declares. It is a VALUE rather
// than a sentinel error because every surface that can raise it needs the same
// three facts -- which client, what was required, what the person actually has
// -- and each renders them differently: the browser gets a sentence, the
// relying party gets an OAuth error envelope, the audit log gets a detail map.
type RoleFloorRefusal struct {
	ClientId string
	// ClientName is the display name, for copy that reads like a product
	// rather than like a config key. Falls back to ClientId.
	ClientName string
	// Required is the floor the client declares.
	Required auth.Role
	// Actual is the role the user holds. "" when the user carries no
	// cluster-wide role at all, which is below every floor.
	Actual auth.Role
}

// roleLabel renders a role for a human. An empty role is a real state -- an
// external user provisioned with no cluster-wide role -- and "none" says that
// without pretending they hold a tier they do not.
func roleLabel(r auth.Role) string {
	if strings.TrimSpace(string(r)) == "" {
		return "none"
	}
	return string(r)
}

// Description is the sentence shown to the person and handed to the relying
// party as the OAuth error_description. It names the role and the reason, so
// somebody reading it in an editor notification knows both what happened and
// who to ask.
func (r RoleFloorRefusal) Description() string {
	name := strings.TrimSpace(r.ClientName)
	if name == "" {
		name = r.ClientId
	}
	return name + " manages this cluster. Your role on it is " +
		roleLabel(r.Actual) + ", and signing in from an editor needs " +
		roleLabel(r.Required) + " or above. Ask a cluster owner or admin to raise your role."
}

// Error makes the refusal usable as an error value on the paths that carry one.
func (r RoleFloorRefusal) Error() string { return r.Description() }

// AuditDetail is the detail map every refusal audit event carries.
func (r RoleFloorRefusal) AuditDetail() map[string]any {
	return map[string]any{
		"clientId":     r.ClientId,
		"requiredRole": string(r.Required),
		"actualRole":   string(r.Actual),
	}
}

// AuditActionRoleFloorRefused is the audit action written on every refusal.
// One action string, so an operator greps for one thing.
const AuditActionRoleFloorRefused = "editor_signin_refused_role"

// ClientDeclaresRoleFloor reports whether clientId is a built-in that declares
// a role floor at all. Call sites use it to skip the user-role lookup entirely
// for the clients that have no floor -- which is every client but the editor,
// so the portal and every MCP connector pay nothing for this feature.
func ClientDeclaresRoleFloor(clientId string) bool {
	b := findBuiltinClient(clientId)
	return b != nil && strings.TrimSpace(string(b.MinRole)) != ""
}

// CheckClientRoleFloor is THE role-floor rule. Every surface that can complete
// a sign-in calls this one function; there is no second copy of the policy.
//
// It returns nil -- admit -- in the two cases that cover every client that
// exists today:
//
//   - clientId names no built-in, or names one that declares no floor. Static
//     MEMQL_IDENTITY_REGISTERED_CLIENTS clients and DCR-store clients are
//     therefore untouched, which is why nothing changes for the portal or for
//     an MCP connector.
//   - the role meets the floor.
//
// Otherwise it returns the refusal to render, redirect and audit.
//
// TOKEN REFRESH IS NOT GATED HERE, and that is a choice rather than an
// oversight. The floor runs at APPROVAL time -- the moment a human is present
// and can be told why -- so a role downgraded after sign-in takes effect at the
// next sign-in. The immediate lever for cutting an existing editor session is
// revoking the v1:identity:authSession row, which is what that surface is for;
// re-checking on every refresh would put a database read on the hot path to
// re-decide something no human is there to act on.
func CheckClientRoleFloor(clientId string, role auth.Role) *RoleFloorRefusal {
	b := findBuiltinClient(clientId)
	if b == nil || strings.TrimSpace(string(b.MinRole)) == "" {
		return nil
	}
	if roleMeetsFloor(role, b.MinRole) {
		return nil
	}
	return &RoleFloorRefusal{
		ClientId:   b.Client.ClientId,
		ClientName: b.Client.Name,
		Required:   b.MinRole,
		Actual:     role,
	}
}

// roleMeetsFloor answers the comparison through component/auth's own rank
// helpers rather than through string ordering, so this file cannot drift from
// the role model it is gating on.
//
// The switch is CLOSED and fails CLOSED: a floor value nobody wrote a case for
// admits nobody, because the alternative -- treating an unrecognized floor as
// "no floor" -- turns a typo in the registry into a silently open gate.
func roleMeetsFloor(role, floor auth.Role) bool {
	u := auth.UserContext{Role: role}
	switch floor {
	case auth.RoleOwner:
		return role == auth.RoleOwner
	case auth.RoleAdmin:
		return auth.AtLeastAdmin(u)
	case auth.RoleDeveloper:
		return auth.AtLeastDeveloper(u)
	case auth.RoleWriter:
		return auth.CanWrite(u)
	case auth.RoleReader:
		return auth.IsValidRole(role)
	default:
		return false
	}
}
