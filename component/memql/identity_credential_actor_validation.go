package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// conceptIdentityIdentity is the credential-row concept id. Mirrors
// dsl/identity/concepts.memql:identity.
const conceptIdentityIdentity = "v1:identity:identity"

// machineCredentialIdentityTypes are the v1:identity:identity variants
// that authenticate a MACHINE or a physical token, never an interactive
// human login. Every legitimate write to one of these rows happens
// through a dedicated handler / CLI / bootstrap path running under a
// system actor (the credential-mint gRPC handlers wrap
// contextWithSystemActor; the identity service's HTTP surface stamps
// system:identity-svc; the CLI + /node/bootstrap use system actors).
//
// The rows are pure attribution/authentication keys: whoever can write
// one can forge a credential. A badge row is the sharpest case --
// credentials.keyHash is the hash of an attacker-controllable physical
// id, so a user-actor insert of {userId: victim, keyHash: hash(myCard)}
// yields direct impersonation of the victim at any shared terminal
// (memql#2513). worker_token / node_token / voice_agent_token /
// service_account share the shape.
//
// `passkey` is here too, and it is worth being precise about why, because
// its stored material is PUBLIC (a COSE key, memql#3406) and none of the
// "whoever can write one can forge a credential" reasoning about secrets
// applies. What applies is the badge argument with the roles reversed: an
// attacker who can insert {userId: victim, credentialId: mine, publicKey:
// mine} has bound their OWN authenticator to the victim's account, and the
// login ceremony will then verify a perfectly genuine signature and hand
// back the victim's session. The forgery is in the binding, not in the
// material. The legitimate writer is the register/finish handler, which
// stamps the system credential actor after verifying an attestation it
// challenged.
//
// `recovery_key` is the sharpest entry on this list and arrived with the
// variant itself (memql#3964). It is the badge argument with the stakes
// raised: an authenticated caller able to craft
// `mutation createRecoveryKeyIdentity(userId: <owner>, keyHash: hash(minted-by-me))`
// would hold a valid break-glass credential for the CLUSTER OWNER, and the
// redeem ceremony it feeds registers a passkey as that owner -- a complete
// account takeover with no further step. It would also look entirely
// legitimate afterwards: the row is indistinguishable from one the mint
// invariant produced, and redeeming it is exactly the flow the credential
// exists for.
//
// The break-glass gate on redemption (memql#3967 -- refuse while the owner
// still has a usable sign-in route) does not cover this case, and it is worth
// saying so rather than assuming defence in depth. An actor who can write
// credential rows can also deactivate the owner's magic-link and passkey
// identities, which is precisely the state that OPENS that gate.
var machineCredentialIdentityTypes = map[string]bool{
	"badge":             true,
	"worker_token":      true,
	"node_token":        true,
	"voice_agent_token": true,
	"service_account":   true,
	"passkey":           true,
	"recovery_key":      true,
}

// validateIdentityCredentialActorScope rejects a NON-system actor
// writing a v1:identity:identity row of a machine-credential
// identityType. This is the row-write backstop the DSL grammar cannot
// express: the per-row-authz classifier only sees literal
// payload.<userScopeField> filter references, and the credential-mint
// mutations bind the owner via `args.userId` shorthand, so they read as
// unscoped and slip past it. Without this guard any authenticated
// caller could craft a raw `mutation createBadgeIdentity(...)` over
// ExecuteQuery (handleExecuteQuery admits any authenticated role;
// per-row authz is the only gate) and register a credential mapping
// their own token to another user's account.
//
// Mirrors validateAgentKindActorScope / validateRbacBaseRoleImmutable:
// the mutation surface stays permissive so the legitimate system-actor
// paths keep writing; the rejection sits above the row write.
//
// Contract:
//   - payload with no identityType, or an interactive type
//     (magic_link / oauth / api_key): pass -- those have their own
//     write paths and are out of scope for this backstop.
//   - a machine-credential identityType written by a system actor:
//     pass.
//   - a machine-credential identityType written by any other actor:
//     reject with a typed error.
func (e *MemQLEngine) validateIdentityCredentialActorScope(ctx context.Context, payload map[string]any, actor string) error {
	if payload == nil {
		return nil
	}
	rawType, present := payload["identityType"]
	if !present {
		return nil
	}
	identityType := strings.ToLower(strings.TrimSpace(stringFromAny(rawType)))
	if !machineCredentialIdentityTypes[identityType] {
		return nil
	}

	identity, _ := auth.UserIdentityFromContext(ctx)
	if isSystemActor(identity, actor) {
		return nil
	}
	return fmt.Errorf(
		"v1:identity:identity: identityType=%q is a machine credential and may only be written by a "+
			"system actor (its dedicated mint handler / CLI / bootstrap path). A user-actor write to a "+
			"credential row is rejected to block credential forgery + impersonation. See "+
			"component/memql/identity_credential_actor_validation.go and memql#2513.",
		identityType,
	)
}
