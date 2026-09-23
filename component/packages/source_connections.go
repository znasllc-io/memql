package packages

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/packages/githubapp"
)

const sourceConnectionConcept = "v1:platform:sourceConnection"
const CodeSourceConnectionUnavailable = "source_connection_unavailable"

// A connection is a personal selection of one installation, not a credential
// and not a MemQL owning account. Removing it cannot revoke a shared grant.
func sourceConnectionID(owner, credential, installation string) string {
	sum := sha256.Sum256([]byte(memql.BareShortId(owner) + "\x00" + memql.BareShortId(credential) + "\x00" + installation))
	return fmt.Sprintf("%s:%x", sourceConnectionConcept, sum[:16])
}

func (s *store) sourceConnectionByID(ctx context.Context, id string) (map[string]any, error) {
	return s.queryOne(ctx, "query sourceConnectionById(connectionId: "+langparser.QuoteString(id)+")")
}

func (s *store) recordSourceConnection(ctx context.Context, id, credential string, inst githubapp.Installation) error {
	return s.writeInternal(ctx, fmt.Sprintf("mutation recordSourceConnection(connectionId: %s, credentialId: %s, installationId: %s, providerAccountId: %s, accountLogin: %s, accountType: %s)",
		langparser.QuoteString(id), langparser.QuoteString(credential), langparser.QuoteString(formatInstallationId(inst.Id)), langparser.QuoteString(strconv.FormatInt(inst.Account.Id, 10)), langparser.QuoteString(inst.Account.Login), langparser.QuoteString(inst.Account.Type)))
}

func sourceConnectionUnavailable() error {
	return refuse(CodeSourceConnectionUnavailable, "this source connection is unavailable; choose an active source of your own")
}

func liveInstallation(ctx context.Context, d *Deps, grant ResolvedCredential, installation string) (githubapp.Installation, error) {
	if d.GitHubApp == nil || !d.GitHubApp.Configured() {
		return githubapp.Installation{}, refuse(CodeGithubAppNotConfigured, "GitHub is not configured on this cluster")
	}
	installations, err := d.GitHubApp.UserInstallations(ctx, grant.Bearer)
	if err != nil {
		return githubapp.Installation{}, grantRefusal(err, grant, "", "", d.GitHubApp.InstallURL())
	}
	for _, inst := range installations {
		if formatInstallationId(inst.Id) == installation && strings.TrimSpace(inst.SuspendedAt) == "" {
			return inst, nil
		}
	}
	return githubapp.Installation{}, sourceConnectionUnavailable()
}

func resolveSourceConnection(ctx context.Context, d *Deps, id, credential string) (ResolvedCredential, githubapp.Installation, error) {
	return resolveSourceConnectionForUse(ctx, d, id, credential, false)
}

func resolveSourceConnectionForUse(ctx context.Context, d *Deps, id, credential string, existingPackage bool) (ResolvedCredential, githubapp.Installation, error) {
	if strings.TrimSpace(id) == "" || d.Store == nil {
		return ResolvedCredential{}, githubapp.Installation{}, sourceConnectionUnavailable()
	}
	row, err := d.Store.sourceConnectionByID(ctx, id)
	if err != nil {
		return ResolvedCredential{}, githubapp.Installation{}, err
	}
	if row == nil || (rowString(row, "status") != "active" && !(existingPackage && rowString(row, "status") == "removed")) || !sameShortId(rowString(row, "ownerUserId"), actorFromContext(ctx).UserId) {
		return ResolvedCredential{}, githubapp.Installation{}, sourceConnectionUnavailable()
	}
	boundCredential := rowString(row, "credentialId")
	if credential != "" && !sameShortId(credential, boundCredential) {
		return ResolvedCredential{}, githubapp.Installation{}, sourceConnectionUnavailable()
	}
	grant, reason, err := resolvePickerGrant(ctx, d, boundCredential)
	if err != nil {
		return ResolvedCredential{}, githubapp.Installation{}, err
	}
	if reason != "" {
		return ResolvedCredential{}, githubapp.Installation{}, refuse(reason, "the source's GitHub connection is unavailable")
	}
	inst, err := liveInstallation(ctx, d, grant, rowString(row, "installationId"))
	return grant, inst, err
}

// ValidateSourceConnectionRepository is the shared authority for probes and
// package writes. The source's live binding, not browser-supplied metadata,
// determines which GitHub grant and installation may supply a repository.
func (d *Deps) ValidateSourceConnectionRepository(ctx context.Context, connectionID, credentialID, repoURL string) error {
	grant, inst, err := resolveSourceConnection(ctx, d, connectionID, credentialID)
	if err != nil {
		return err
	}
	return d.validateConnectionRepository(ctx, grant, inst, repoURL)
}

func (d *Deps) validateConnectionRepository(ctx context.Context, grant ResolvedCredential, inst githubapp.Installation, repoURL string) error {
	owner, repo, err := parseGitHubRepo(repoURL)
	if err != nil {
		return err
	}
	repoInstallation, err := d.GitHubApp.InstallationForRepo(ctx, owner, repo)
	if err != nil {
		return grantRefusal(err, grant, owner, repo, d.GitHubApp.InstallURL())
	}
	if repoInstallation != inst.Id {
		return refuse("source_repository_mismatch", "the repository is outside the selected source")
	}
	return verifyGrantRepository(ctx, d.GitHubApp, grant, owner, repo, inst.Id)
}

// An existing package keeps its binding after the chooser entry is removed,
// but never switches installations or owners implicitly. The package owner
// was established by the caller's authorized package read before this call.
func (d *Deps) validatePackageSourceConnection(ctx context.Context, pkg map[string]any) error {
	id := rowString(pkg, "sourceConnectionId")
	if id == "" {
		return nil
	}
	owner := rowString(pkg, "ownerUserId")
	credential := rowString(pkg, "credentialId")
	if owner == "" || credential == "" {
		return sourceConnectionUnavailable()
	}
	owned := auth.ContextWithUserActor(ctx, owner)
	grant, inst, err := resolveSourceConnectionForUse(owned, d, id, credential, true)
	if err != nil {
		return err
	}
	return d.validateConnectionRepository(owned, grant, inst, rowString(pkg, "repoUrl"))
}

func (i *Integration) handleSourceConnectionCreate(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	d, err := i.resolve()
	if err != nil {
		return nil, err
	}
	owner := actorFromContext(ctx).UserId
	credential := strings.TrimSpace(stringArg(args, "credentialId"))
	installation := strings.TrimSpace(stringArg(args, "installationId"))
	if owner == "" || credential == "" || installation == "" {
		return nil, sourceConnectionUnavailable()
	}
	grant, reason, err := resolvePickerGrant(ctx, d, credential)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return nil, refuse(reason, "the GitHub connection is unavailable")
	}
	inst, err := liveInstallation(ctx, d, grant, installation)
	if err != nil {
		return nil, err
	}
	id := sourceConnectionID(owner, grant.Id, formatInstallationId(inst.Id))
	if d.Store == nil {
		return nil, sourceConnectionUnavailable()
	}
	if err = d.Store.recordSourceConnection(ctx, id, grant.Id, inst); err != nil {
		return nil, err
	}
	return resultNode(map[string]any{"connectionId": memql.BareShortId(id), "status": "active"}), nil
}

func (i *Integration) handleSourceConnectionRemove(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	d, err := i.resolve()
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(stringArg(args, "connectionId"))
	if d.Store == nil || id == "" {
		return nil, sourceConnectionUnavailable()
	}
	row, err := d.Store.sourceConnectionByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if row == nil || !sameShortId(rowString(row, "ownerUserId"), actorFromContext(ctx).UserId) {
		return nil, sourceConnectionUnavailable()
	}
	if rowString(row, "status") != "removed" {
		if err = d.Store.writeInternal(ctx, "mutation removeSourceConnection(connectionId: "+langparser.QuoteString(id)+")"); err != nil {
			return nil, err
		}
	}
	return resultNode(map[string]any{"connectionId": memql.BareShortId(id), "status": "removed"}), nil
}

func (i *Integration) handleValidateSourceConnectionRepository(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if strings.TrimSpace(stringArg(args, "credentialId")) == "" {
		return nil, sourceConnectionUnavailable()
	}
	d, err := i.resolve()
	if err != nil {
		return nil, err
	}
	if err = d.ValidateSourceConnectionRepository(ctx, stringArg(args, "connectionId"), stringArg(args, "credentialId"), stringArg(args, "repoUrl")); err != nil {
		return nil, err
	}
	return resultNode(map[string]any{"valid": true}), nil
}

func (i *Integration) handleSourceInstallations(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	d, err := i.resolve()
	if err != nil {
		return nil, err
	}
	res := SourceRepositoriesResult{Reason: RepositoriesReasonOK, Repositories: []PickerRepository{}, Installations: []PickerInstallation{}, Pending: []string{}}
	grant, reason, err := resolvePickerGrant(ctx, d, stringArg(args, "credentialId"))
	if err != nil {
		return nil, err
	}
	if reason != "" {
		res.Reason = reason
	} else if d.GitHubApp == nil || !d.GitHubApp.Configured() {
		res.Reason = RepositoriesReasonNotConfigured
	} else {
		installations, ierr := d.GitHubApp.UserInstallations(ctx, grant.Bearer)
		if ierr != nil {
			res, err = pickerFailure(res, ierr)
			if err != nil {
				return nil, err
			}
		} else {
			for _, inst := range installations {
				res.Installations = append(res.Installations, pickerInstallation(inst))
			}
			res.Pending = pendingInstallations(ctx, d, grant.Login)
		}
	}
	return resultNode(map[string]any{"reason": res.Reason, "installations": res.Installations, "pending": res.Pending}), nil
}
