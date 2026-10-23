package packages

import (
	"context"
	"strings"
)

// RepositoryQueueObservationToken borrows only repository observation scopes.
// It cannot ready or enqueue a pull request, even when the App has write scopes.
func (i *Integration) RepositoryQueueObservationToken(ctx context.Context, credentialID, ownerUserID, owner, repo string) (string, int64, error) {
	d, err := i.resolve()
	if err != nil {
		return "", 0, err
	}
	authority, err := d.repositoryAccess(ctx, credentialID, ownerUserID, owner, repo)
	if err != nil {
		return "", 0, err
	}
	token, _, err := d.GitHubApp.ScopedInstallationToken(ctx, authority.InstallationID, []string{repo}, map[string]string{
		"metadata": "read", "contents": "read", "pull_requests": "read", "merge_queues": "read",
		"checks": "read", "statuses": "read", "administration": "read",
	})
	return token, authority.InstallationID, err
}

// RepositoryQueueUserToken returns the freshly verified person's bearer. Queue
// mutations must obey that person's repository protections, never the App's
// broader installation or bypass authority. It mints no installation token.
func (i *Integration) RepositoryQueueUserToken(ctx context.Context, credentialID, ownerUserID, owner, repo string) (string, int64, error) {
	d, err := i.resolve()
	if err != nil {
		return "", 0, err
	}
	if strings.TrimSpace(credentialID) == "" || strings.TrimSpace(ownerUserID) == "" || strings.TrimSpace(owner) == "" || strings.TrimSpace(repo) == "" || d.Credentials == nil {
		return "", 0, refuse(CodeCredentialNotFound, "queue admission requires the source owner's GitHub connection")
	}
	credential, err := d.Credentials(ctx, credentialID, ownerUserID)
	if err != nil {
		return "", 0, err
	}
	if !credential.IsGrant() {
		return "", 0, refuse(CodeCredentialNotFound, "queue admission requires a connected GitHub identity")
	}
	gh := d.GitHubApp
	if gh == nil || !gh.Configured() {
		return "", 0, refuse(CodeGithubAppNotConfigured, "queue admission requires a configured GitHub App")
	}
	installation, err := gh.InstallationForRepo(ctx, owner, repo)
	if err != nil {
		return "", 0, grantRefusal(err, credential, owner, repo, gh.InstallURL())
	}
	repository, err := readGrantRepository(ctx, gh, credential, owner, repo, installation)
	if err != nil {
		return "", 0, err
	}
	if repository.ID <= 0 || !strings.EqualFold(repository.FullName, owner+"/"+repo) || !repository.Permissions.CanWrite() {
		return "", 0, refuse("repository_write_not_authorized", "this GitHub identity cannot authorize queue admission for the repository")
	}
	return credential.Bearer, installation, nil
}
