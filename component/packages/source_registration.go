package packages

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// Registration restores the chooser entry, never the deployment lifecycle.
// The persisted tuple and its row ID survive removal and concurrent re-adds.
func (i *Integration) handleSourceRegister(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	d, err := i.resolve()
	if err != nil {
		return nil, err
	}
	owner := actorFromContext(ctx).UserId
	credential, binding, account := strings.TrimSpace(stringArg(args, "credentialId")), strings.TrimSpace(stringArg(args, "sourceConnectionId")), strings.TrimSpace(stringArg(args, "accountId"))
	if owner == "" || credential == "" || binding == "" || account == "" || d.Store == nil {
		return nil, sourceConnectionUnavailable()
	}
	repoOwner, repo, err := parseGitHubRepo(stringArg(args, "repoUrl"))
	if err != nil {
		return nil, err
	}
	repoURL := "https://github.com/" + strings.ToLower(repoOwner) + "/" + strings.ToLower(repo)
	ref := strings.TrimSpace(stringArg(args, "repoRef"))
	if err := d.ValidateSourceConnectionRepository(ctx, binding, credential, repoURL); err != nil {
		return nil, err
	}
	grant, _, err := resolveSourceConnection(ctx, d, binding, credential)
	if err != nil {
		return nil, err
	}
	repository, err := d.GitHubApp.UserRepository(ctx, grant.Bearer, repoOwner, repo)
	if err != nil {
		return nil, err
	}
	identityFor := func(row map[string]any) string {
		copy := make(map[string]any, len(row))
		for key, value := range row {
			copy[key] = value
		}
		if strings.TrimSpace(rowString(copy, "repoRef")) == "" {
			copy["repoRef"] = repository.DefaultBranch
		}
		return memql.PackageSourceIdentity(copy)
	}
	wanted := map[string]any{"ownerUserId": owner, "accountId": account, "credentialId": credential, "sourceConnectionId": binding, "repoUrl": repoURL, "repoRef": ref}
	key := identityFor(wanted)
	var db *sql.DB
	if d.Store.directDB != nil {
		db = d.Store.directDB()
	}
	var answer map[string]any
	err = githubconnect.WithGate(ctx, db, "repository-registration:"+key, func(ctx context.Context) error {
		rows, err := d.Store.queryAll(ctx, "query packagesForSourceRegistration()")
		if err != nil {
			return err
		}
		for _, row := range rows {
			if identityFor(row) != key {
				continue
			}
			packageID := rowString(row, "id")
			restored := rowBool(row, "sourceRemoved")
			// Even an already-listed entry is checked through the owner's governed
			// write: a read grant alone cannot authorize registration/restore.
			if err := d.Store.setPackageSourceRemoved(ctx, packageID, false); err != nil {
				return err
			}
			answer = map[string]any{"packageId": memql.BareShortId(packageID), "created": false, "restored": restored}
			return nil
		}
		packageID := id.NewShortId()
		query := fmt.Sprintf(`mutation createPackage(packageId: %s, name: %s, sourceKind: "repo", repoUrl: %s, repoRef: %s, credentialId: %s, sourceConnectionId: %s, accountId: %s, autoDeploy: %t)`, langparser.QuoteString(packageID), langparser.QuoteString(stringArg(args, "name")), langparser.QuoteString(repoURL), langparser.QuoteString(ref), langparser.QuoteString(credential), langparser.QuoteString(binding), langparser.QuoteString(account), boolArg(args, "autoDeploy"))
		if _, err := d.Store.engine.Execute(ctx, query); err != nil {
			return err
		}
		answer = map[string]any{"packageId": packageID, "created": true, "restored": false}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resultNode(answer), nil
}

func (s *store) setPackageSourceRemoved(ctx context.Context, packageID string, removed bool) error {
	_, err := s.engine.Execute(ctx, fmt.Sprintf(`mutation setPackageSourceRemoved(packageId: %s, removed: %t)`, langparser.QuoteString(packageID), removed))
	return err
}
