package app

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// pipelines_token_minter.go -- the clone token a pipeline step is handed (epic
// memql#5478). The agent mints it for a step it sends to a machine; the
// workbench's runner, for the Secret of a step's Job. Either way it is the
// seam's narrowed mint: contents read, on the step's one repository, never the
// installation-wide token the client caches, which reaches every repository
// the installation covers.

// pipelinesTokenMinter implements pipelinesteps.TokenMinter over the node's
// GitHub App client.
type pipelinesTokenMinter struct {
	// mint is the client's narrowed mint, asked for the client AT EACH CALL
	// and never held: the packages integration builds it from what app/
	// installed by the time of the first call.
	mint func(ctx context.Context, installationID int64, repositories []string, permissions map[string]string) (string, time.Time, error)
}

var _ pipelinesteps.TokenMinter = pipelinesTokenMinter{}

// pipelinesTokenMinterFor mints through the packages integration's GitHub App
// client.
func (a *App) pipelinesTokenMinterFor() pipelinesTokenMinter {
	return pipelinesTokenMinter{mint: func(ctx context.Context, installationID int64, repositories []string, permissions map[string]string) (string, time.Time, error) {
		pkgs := a.lookupPackagesIntegration()
		if pkgs == nil {
			return "", time.Time{}, githubapp.ErrNotConfigured
		}
		client := pkgs.GitHub()
		if client == nil {
			return "", time.Time{}, githubapp.ErrNotConfigured
		}
		return client.ScopedInstallationToken(ctx, installationID, repositories, permissions)
	}}
}

// CloneToken mints a token that reads owner/name's contents and nothing
// else. Installation 0 is an anonymous clone of a public repository: no token,
// and nothing asked of GitHub.
func (m pipelinesTokenMinter) CloneToken(ctx context.Context, installationID int64, owner, name string) (string, error) {
	if installationID == 0 {
		return "", nil
	}
	if m.mint == nil {
		return "", githubapp.ErrNotConfigured
	}
	// GitHub's access_tokens body names repositories within the
	// installation's account: "widget", not "acme/widget".
	token, _, err := m.mint(ctx, installationID, []string{name}, map[string]string{"contents": "read"})
	return token, err
}
