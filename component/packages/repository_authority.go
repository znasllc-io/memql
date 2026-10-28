package packages

import (
	"context"
	"strings"
)

// RepositoryAuthority is a fresh provider observation, never a transferable
// grant. A caller cannot serialize this value to authorize a later write.
type RepositoryAuthority struct {
	RepositoryID   int64  `json:"repositoryId"`
	Repository     string `json:"repository"`
	InstallationID int64  `json:"installationId"`
	CanWrite       bool   `json:"canWrite"`
}

// RepositoryAccess resolves the connection under its actual owner and checks
// both its installation membership and current provider permissions. It mints
// no token and therefore also supports read-only upstream request routing.
func (i *Integration) RepositoryAccess(ctx context.Context, credentialID, ownerUserID, owner, repo string) (RepositoryAuthority, error) {
	d, err := i.resolve()
	if err != nil {
		return RepositoryAuthority{}, err
	}
	return d.repositoryAccess(ctx, credentialID, ownerUserID, owner, repo)
}

func (d *Deps) repositoryAccess(ctx context.Context, credentialID, ownerUserID, owner, repo string) (RepositoryAuthority, error) {
	var out RepositoryAuthority
	credentialID, ownerUserID = strings.TrimSpace(credentialID), strings.TrimSpace(ownerUserID)
	owner, repo = strings.TrimSpace(owner), strings.TrimSpace(repo)
	if credentialID == "" || ownerUserID == "" || owner == "" || repo == "" || d.Credentials == nil {
		return out, refuse(CodeCredentialNotFound, "repository authority requires the source owner's GitHub connection")
	}
	credential, err := d.Credentials(ctx, credentialID, ownerUserID)
	if err != nil {
		return out, err
	}
	if !credential.IsGrant() {
		return out, refuse(CodeCredentialNotFound, "repository authority requires a connected GitHub identity")
	}
	gh := d.GitHubApp
	if gh == nil || !gh.Configured() {
		return out, refuse(CodeGithubAppNotConfigured, "repository authority requires a configured GitHub App")
	}
	installationID, err := gh.InstallationForRepo(ctx, owner, repo)
	if err != nil {
		return out, grantRefusal(err, credential, owner, repo, gh.InstallURL())
	}
	repository, err := readGrantRepository(ctx, gh, credential, owner, repo, installationID)
	if err != nil {
		return out, err
	}
	if repository.ID <= 0 || !strings.EqualFold(repository.FullName, owner+"/"+repo) {
		return out, refuse("repository_identity_unavailable", "GitHub did not confirm the requested repository's stable identity")
	}
	return RepositoryAuthority{RepositoryID: repository.ID, Repository: strings.ToLower(repository.FullName), InstallationID: installationID, CanWrite: repository.Permissions.CanWrite()}, nil
}

// RepositoryOwnerLogin is the GitHub login behind a source owner's grant,
// read LIVE with their user token after the same access check a write needs
// (release trains, design record 2026-10-27, section 7). A train lands only
// on this login's approval, so it must come from the person's verified
// connection, never from a stored or configured string: a connection that
// lost the repository names nobody.
func (i *Integration) RepositoryOwnerLogin(ctx context.Context, credentialID, ownerUserID, owner, repo string) (string, error) {
	d, err := i.resolve()
	if err != nil {
		return "", err
	}
	if _, err := d.repositoryAccess(ctx, credentialID, ownerUserID, owner, repo); err != nil {
		return "", err
	}
	credential, err := d.Credentials(ctx, strings.TrimSpace(credentialID), strings.TrimSpace(ownerUserID))
	if err != nil {
		return "", err
	}
	user, err := d.GitHubApp.User(ctx, credential.Bearer)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(user.Login) == "" {
		return "", refuse("repository_identity_unavailable", "GitHub did not name the account behind this connection")
	}
	return user.Login, nil
}

// RepositoryWriterToken is the only package-to-pipeline writer mint. A source
// owner who can merely see a repository must never borrow the App's wider
// installation permissions. Revalidate on every call, even on a replica that
// already holds a cached token; revoked authority wins over cached credentials.
// The effect adapter must additionally enforce its approved operation scope.
func (i *Integration) RepositoryWriterToken(ctx context.Context, credentialID, ownerUserID, owner, repo string) (string, int64, error) {
	d, err := i.resolve()
	if err != nil {
		return "", 0, err
	}
	authority, err := d.repositoryAccess(ctx, credentialID, ownerUserID, owner, repo)
	if err != nil {
		return "", 0, err
	}
	if !authority.CanWrite {
		return "", 0, refuse("repository_write_not_authorized", "this GitHub identity can read the repository but cannot authorize changes; request approval from its maintainer")
	}
	token, _, err := d.GitHubApp.ScopedInstallationToken(ctx, authority.InstallationID, []string{repo}, map[string]string{"contents": "write", "pull_requests": "write"})
	return token, authority.InstallationID, err
}
