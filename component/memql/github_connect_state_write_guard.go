package memql

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
)

// conceptIdentityGithubConnectState is the single-use state row GitHub Connect
// and GitHub App setup share. Mirrors dsl/identity/concepts.memql:githubConnectState.
const conceptIdentityGithubConnectState = "v1:identity:githubConnectState"

// validateGithubConnectStateServerOnly refuses every write to a connect state
// that does not carry internal origin (memql#5623).
//
// The row IS the authority its callback acts on: the callback resolves the
// person from the row's userId and the flow from its purpose, and trusts both
// because only the begin handler could have written them. Nothing enforced
// that. The concept declares no row tier (it cannot: the callback is pre-actor,
// see undeclared4913GithubConnectReason), so the row-authz write guard passes
// it, and the raw insert(...) literal never consults the create mutation's
// @serverOnly. Any signed-in caller could plant a state with a digest they
// chose, naming any user and purpose -- an app_setup state naming the cluster
// owner passes the setup callback's "still an active cluster owner" re-check,
// because that check reads the planted user id.
//
// Keyed on ORIGIN, not on the actor, unlike validateIdentityCredentialActorScope:
// the one legitimate writer is component/identity/store_githubconnect.go, which
// stamps internal origin inline on its create and its consume. No caller --
// the cluster owner and a system actor included -- has a reason to write one
// any other way.
func validateGithubConnectStateServerOnly(ctx context.Context) error {
	if auth.OriginFromContext(ctx).IsInternal() {
		return nil
	}
	return fmt.Errorf(
		"%s: write refused -- a connect state is written only by the identity service's own Go, "+
			"which stamps internal origin. A client write would let the caller choose whose "+
			"account, and which flow, a later GitHub callback lands on. See "+
			"component/memql/github_connect_state_write_guard.go and memql#5623.",
		conceptIdentityGithubConnectState,
	)
}
