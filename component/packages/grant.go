package packages

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/znasllc-io/memql/component/packages/githubapp"
)

// grant.go -- the bearer a request actually carries under a GitHub App grant,
// and how GitHub's answer is classified once it does (epic memql#4912, C6/D).
//
// TWO BEARERS, ONE CREDENTIAL, and knowing which is which is the whole of this
// file. A grant's `encryptedValue` is the PERSON's user token: it is what the
// picker lists repositories under, because the list is bounded by what that
// person can see. It is emphatically NOT what a fetch, a poll or an
// auto-deploy runs under -- those must not stop working because somebody's
// eight-hour token lapsed while they were asleep -- so background work asks
// GitHub which installation covers the repository and mints THAT
// installation's token from the app's own key.
//
// The classification below exists because the vocabulary a pasted token needs
// answers the wrong question under a grant. Under a token, 401/403/404 are one
// fact -- "this token does not reach that repository" -- and collapsing them is
// honest, because the cluster cannot tell them apart. Under a grant it can, and
// each answer names a different repair: reconnect (the authorization is over),
// an installation link (the app is not on that repository), or an operator's
// configuration.

// installationBearer answers what to put in Authorization for a request to
// owner/repo under rc.
//
// For a token credential it is the pasted value, unchanged -- everything about
// the pre-Connect path stays byte for byte what it was. For a grant it is a
// freshly minted (or cached) installation token, and a grant whose app is not
// installed on the repository is refused BY NAME before the request is made,
// which is the honest answer and the one carrying a repair.
func installationBearer(ctx context.Context, gh *githubapp.Client, rc ResolvedCredential, owner, repo string) (string, error) {
	if !rc.IsGrant() {
		return rc.Bearer, nil
	}
	if gh == nil || !gh.Configured() {
		return "", refuse(CodeGithubAppNotConfigured,
			"credential %q is a GitHub App grant and this cluster has no GitHub App configured, so it cannot mint the token this fetch needs. An operator sets %s.",
			rc.Id, strings.Join(gh.Missing(), ", "))
	}
	installationId, err := gh.InstallationForRepo(ctx, owner, repo)
	if err != nil {
		return "", grantRefusal(err, rc, owner, repo, gh.InstallURL())
	}
	token, terr := gh.InstallationToken(ctx, installationId)
	if terr != nil {
		return "", grantRefusal(terr, rc, owner, repo, gh.InstallURL())
	}
	return token, nil
}

// grantRefusal turns a githubapp failure into this package's own typed
// refusal, so every path that presents a grant refuses in the same words.
func grantRefusal(err error, rc ResolvedCredential, owner, repo, installURL string) error {
	switch {
	case errors.Is(err, githubapp.ErrNotConfigured):
		return refuse(CodeGithubAppNotConfigured,
			"credential %q is a GitHub App grant and this cluster has no GitHub App configured. An operator sets the MEMQL_GITHUB_APP_* values.", rc.Id)
	case errors.Is(err, githubapp.ErrNotInstalled):
		return refuse(CodeRepositoryNotInstalled, "%s", notInstalledSentence(owner, repo, installURL))
	case errors.Is(err, githubapp.ErrReauthorize), githubapp.StatusOf(err) == http.StatusUnauthorized:
		return refuse(CodeReconnectRequired, "%s", reconnectSentence(rc))
	}
	return refuse(CodeSourceUnreadable,
		"this cluster could not reach GitHub for %s/%s under its GitHub App: %v", owner, repo, err)
}

// notInstalledSentence names the repair, which is the point of the code: the
// person can see the repository and the app cannot, so the answer is a link
// and not another credential.
func notInstalledSentence(owner, repo, installURL string) string {
	sentence := "the GitHub App is not installed on " + owner + "/" + repo + ", so this cluster cannot read it. Install it on that repository"
	if strings.TrimSpace(installURL) != "" {
		sentence += " at " + installURL
	}
	return sentence + " -- your own access to the repository is not the question here, the app's is."
}

// reconnectSentence is the one refusal a person repairs with a single click.
func reconnectSentence(rc ResolvedCredential) string {
	who := ""
	if login := strings.TrimSpace(rc.Login); login != "" {
		who = " (connected as @" + login + ")"
	}
	return "GitHub no longer accepts this cluster's authorization" + who +
		". Reconnect GitHub in Settings -- it is one click and nothing to type; sources fetching under it refuse until then."
}
