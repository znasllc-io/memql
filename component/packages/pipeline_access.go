package packages

import (
	"context"
	"strings"

	"github.com/znasllc-io/memql/component/identity/githubapp"
)

// pipeline_access.go -- what pipelines borrow from Deployables (epic
// memql#5477, decision 12 of its plan): the node's GitHub App client, and an
// installation token minted through the source owner's grant.
//
// A pipeline hangs off a source. Its owner is the source's owner, and every
// token a pipeline run holds is minted through that owner's GitHub
// connection, by the same path a fetch takes -- so a pipeline can never reach
// a repository its owner's connection no longer reaches, and there is no
// second place the rule could be written down differently.

// GitHub is the node's GitHub App client: the one Deps holds, so pipelines
// share its installation-token cache rather than doubling the mints against
// the same rate limit.
//
// It is NOT nil on a cluster with no app configured: that client answers
// githubapp.ErrNotConfigured on every call needing the app, and Configured()
// reports false -- the cluster's fact, not a nil to remember (a registration
// made from the product while the node runs is seen by the next call). Nil
// only when this integration cannot resolve at all.
//
// CALL IT WHEN AN OPERATION NEEDS IT, NEVER WHILE WIRING. The first call
// builds this integration's dependencies, once, from what app/ has installed
// by then; called before SetGitHubAppSource, it would fix this node's client
// to the environment alone for the life of the process -- for Deployables as
// well as for pipelines.
func (i *Integration) GitHub() *githubapp.Client {
	deps, err := i.resolve()
	if err != nil || deps == nil {
		return nil
	}
	return deps.GitHubApp
}

// InstallationToken mints the installation token a source owner's grant
// reaches for owner/repo, after verifyGrantRepository confirms the grant
// still reaches it. Memory only.
//
// The credential is resolved under the OWNER's actor (credentialID names
// their row; ownerUserID is whose name it is resolved under), never the
// caller's: a driver acting for a pipeline holds no person's authority of its
// own. The verification runs on EVERY call, including the one this replica
// could have answered from its token cache, because a person can lose a
// repository while the app stays installed for others.
//
// Refusals are Deployables' own codes, carried as typed errors:
// credential_not_found for an empty name, an empty owner or a pasted token --
// a pasted token is not a grant, has no installation behind it, and is never
// grounds to borrow the app's authority -- and for the rest exactly what a
// fetch refuses with (credential_revoked, reconnect_required,
// repository_not_installed, repository_not_accessible,
// github_app_not_configured).
func (i *Integration) InstallationToken(ctx context.Context, credentialID, ownerUserID, owner, repo string) (token string, installationID int64, err error) {
	deps, err := i.resolve()
	if err != nil {
		return "", 0, err
	}
	return deps.installationToken(ctx, credentialID, ownerUserID, owner, repo)
}

func (d *Deps) installationToken(ctx context.Context, credentialID, ownerUserID, owner, repo string) (string, int64, error) {
	credentialID, ownerUserID = strings.TrimSpace(credentialID), strings.TrimSpace(ownerUserID)
	owner, repo = strings.TrimSpace(owner), strings.TrimSpace(repo)
	switch {
	case credentialID == "":
		return "", 0, refuse(CodeCredentialNotFound,
			"this source names no GitHub connection, and a pipeline's token is minted only through its owner's")
	case ownerUserID == "":
		// An empty owner is a cluster-owned source, and the resolver would
		// read the credential under whoever is calling. A pipeline's caller
		// is a driver, not a person, so there is nobody whose grant to verify.
		return "", 0, refuse(CodeCredentialNotFound,
			"credential %q has no owner to resolve it under, and a pipeline's token is minted only through a person's GitHub connection", credentialID)
	case owner == "" || repo == "":
		return "", 0, refuse(CodeSourceUnreadable, "a pipeline's token is minted for one repository, and none was named")
	case d.Credentials == nil:
		return "", 0, refuse(CodeSourceUnreadable,
			"this source's pipeline runs under credential %q, and this node cannot resolve credentials", credentialID)
	}

	resolved, err := d.Credentials(ctx, credentialID, ownerUserID)
	if err != nil {
		return "", 0, err
	}
	if !resolved.IsGrant() {
		return "", 0, refuse(CodeCredentialNotFound,
			"credential %q is a pasted token, and a pipeline runs under the cluster's GitHub App: connect GitHub and switch this source to that connection.", credentialID)
	}
	return grantInstallationToken(ctx, d.GitHubApp, resolved, owner, repo)
}
