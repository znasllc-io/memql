package pipelinerun

import (
	"context"
	"errors"
	"sort"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
)

// installations.go -- the permissions-changed prompt (epic memql#5479, D15:
// "a permissions-changed prompt from GitHub is surfaced by the item rather
// than left to block runs silently").
//
// WHY IT ASKS GITHUB. An app's permissions grow -- pipelines added checks
// write, merge queues and pull requests read (epic memql#5477) -- and every
// EXISTING installation keeps the set it accepted until the account that
// installed it approves the change GitHub sends it. Until then every
// check-run write there answers 403: the run goes ahead and its check never
// appears, which holds a merge on a required check with nothing in the
// cluster saying why. A run's own note (pipeline_check_permission_missing)
// says so only after a run tried; this read says it before, of every
// installation, with the page on GitHub where the change waits.
//
// WHAT "LAGS" MEANS: a permission the app asks for that the installation has
// not accepted at that level or higher (none < read < write). A WIDER grant is
// not a lag. The app's ask is githubconnect.RequestedPermissions, the one
// definition the manifest flow registers the app with.

// LaggingInstallation is one installation that has not accepted what the app
// asks for.
type LaggingInstallation struct {
	InstallationID int64
	Account        string
	AccountType    string
	HTMLURL        string
	// Missing is "permission:level" for each the installation lacks, sorted.
	Missing   []string
	Suspended bool
}

func (i *Integration) handleInstallations(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	lagging, err := i.Installations(handlerContext(ctx))
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(lagging))
	for _, in := range lagging {
		rows = append(rows, map[string]any{
			"installationId": in.InstallationID, "account": in.Account, "accountType": in.AccountType,
			"htmlUrl": in.HTMLURL, "missingPermissions": nonNil(in.Missing), "suspended": in.Suspended,
		})
	}
	return resultNode(map[string]any{"installations": rows}), nil
}

// Installations answers the installations of the cluster's app whose accepted
// permissions lag what it asks for, in id order. A cluster with no app has
// none; a GitHub that could not be asked is an error, never an empty list --
// "nothing lags" is the one answer it must not fake.
func (i *Integration) Installations(ctx context.Context) ([]LaggingInstallation, error) {
	if _, err := personFrom(ctx); err != nil {
		return nil, err
	}
	d := i.snapshot()
	if d.GitHub == nil || !d.GitHub.Configured() {
		return nil, nil
	}
	all, err := d.GitHub.Installations(ctx)
	if err != nil {
		if errors.Is(err, githubapp.ErrNotConfigured) {
			return nil, nil
		}
		return nil, err
	}
	asks := githubconnect.RequestedPermissions()
	var out []LaggingInstallation
	for _, in := range all {
		var missing []string
		for permission, level := range asks {
			if permissionRank(in.Permissions[permission]) < permissionRank(level) {
				missing = append(missing, permission+":"+level)
			}
		}
		if len(missing) == 0 {
			continue
		}
		sort.Strings(missing)
		out = append(out, LaggingInstallation{
			InstallationID: in.ID, Account: in.Account, AccountType: in.AccountType, HTMLURL: in.HTMLURL,
			Missing: missing, Suspended: strings.TrimSpace(in.SuspendedAt) != "",
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].InstallationID < out[b].InstallationID })
	return out, nil
}

// permissionRank orders GitHub's permission levels: none, read, write.
func permissionRank(level string) int {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "write", "admin":
		return 2
	case "read":
		return 1
	}
	return 0
}
