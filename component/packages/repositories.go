package packages

import (
	"context"
	"errors"
	"strconv"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/core/num"
)

// repositories.go -- the Source stop's repository picker (epic memql#4912, C7).
//
// A CAPABILITY RATHER THAN A QUERY, and the reason is where the answer lives:
// it is not in this cluster. What a grant can reach right now is what GitHub
// says, and GitHub's answer changes when somebody adds a repository to an
// installation with nothing to tell us. A row would be a cache nobody could
// invalidate, and a picker offering yesterday's list is the one failure a
// picker cannot survive.
//
// It runs under the CALLER's actor and resolves the CALLER's own grant. There
// is no argument by which one person lists another's repositories: an empty
// credentialId resolves through githubAppGrantForCaller, whose filter is the
// owner term and which takes no arguments at all, and a named one resolves
// through the same owner-scoped sealed read every other path uses.
//
// EVERY REFUSAL IS A `reason` IN THE REPLY rather than an error, because the
// picker renders in place: a person who has not connected GitHub sees Connect
// where the list would be, and a cluster with no app sees the token path. An
// error would collapse the stop into a failure banner and lose the repair.

// The reasons sourceRepositories answers with. Six, all but the first shared
// with a refusal code, so the OS keys one sentence per condition however it
// was reached.
const (
	RepositoriesReasonOK                = "ok"
	RepositoriesReasonNotConfigured     = CodeGithubAppNotConfigured
	RepositoriesReasonReconnectRequired = CodeReconnectRequired
	RepositoriesReasonCredentialMissing = CodeCredentialNotFound
	RepositoriesReasonCredentialRevoked = CodeCredentialRevoked
	RepositoriesReasonRateLimited       = ProbeReasonRateLimited
)

// SourceRepositoriesResult is the picker's whole answer.
type SourceRepositoriesResult struct {
	// Reason is exactly one of the RepositoriesReason* values. Everything
	// below is empty for anything but ok.
	Reason string `json:"reason"`
	// Repositories is what the grant reaches, across every installation, for
	// the requested page. Grouped by the client rather than here: `owner` is
	// carried on every row, and a server-side grouping would fix an order the
	// picker's search then has to undo.
	Repositories []PickerRepository `json:"repositories"`
	// Installations is every installation this grant can reach, so the picker
	// can say WHY a repository is missing -- an installation limited to
	// selected repositories, or suspended, is a different answer from one that
	// simply has none.
	Installations []PickerInstallation `json:"installations"`
	// Pending names the organizations whose installation is still waiting for
	// an owner's approval. By NAME, because the repair belongs to somebody
	// else and knowing whom to ask is the person's only useful next step.
	Pending []string `json:"pending"`
	// NextPage is the page to ask for next, or 0 when there is no more. Zero
	// rather than -1 or absent: the argument's own "empty or 0 means the first
	// page" makes 0 the value that cannot be mistaken for a page.
	NextPage int `json:"nextPage"`
}

// PickerRepository is one repository in the shape the Source stop needs: enough
// to group it, search it, show it and prefill a ref from it without a second
// call.
type PickerRepository struct {
	FullName       string `json:"fullName"`
	Owner          string `json:"owner"`
	Name           string `json:"name"`
	Url            string `json:"url"`
	Private        bool   `json:"private"`
	Visibility     string `json:"visibility"`
	DefaultBranch  string `json:"defaultBranch"`
	PushedAt       string `json:"pushedAt"`
	InstallationId string `json:"installationId"`
}

// PickerInstallation is one installation the grant reaches.
type PickerInstallation struct {
	Id                  string `json:"id"`
	Account             string `json:"account"`
	AccountType         string `json:"accountType"`
	RepositorySelection string `json:"repositorySelection"`
	// Suspended is an installation an owner has suspended. Carried rather
	// than filtered out: it explains why nothing under it works, and hiding
	// it would make those repositories simply absent.
	Suspended bool `json:"suspended"`
}

// SourceRepositories lists what the caller's GitHub App grant can reach.
func SourceRepositories(ctx context.Context, d *Deps, credentialId string, page int) (SourceRepositoriesResult, error) {
	res := SourceRepositoriesResult{
		Reason:        RepositoriesReasonOK,
		Repositories:  []PickerRepository{},
		Installations: []PickerInstallation{},
		Pending:       []string{},
	}
	if page < 1 {
		page = 1
	}
	if d.GitHubApp == nil || !d.GitHubApp.Configured() {
		// The operator's condition, answered before anything is resolved: a
		// cluster with no app has no grants either, and refusing here rather
		// than through a credential lookup is what lets the stop say "this
		// cluster offers the token path" instead of "no credential".
		res.Reason = RepositoriesReasonNotConfigured
		return res, nil
	}

	grant, reason, err := resolvePickerGrant(ctx, d, credentialId)
	if err != nil {
		return SourceRepositoriesResult{}, err
	}
	if reason != "" {
		res.Reason = reason
		return res, nil
	}

	installations, ierr := d.GitHubApp.UserInstallations(ctx, grant.Bearer)
	if ierr != nil {
		return pickerFailure(res, ierr)
	}

	ids := make([]string, 0, len(installations))
	for _, inst := range installations {
		res.Installations = append(res.Installations, PickerInstallation{
			Id:                  formatInstallationId(inst.Id),
			Account:             inst.Account.Login,
			AccountType:         inst.Account.Type,
			RepositorySelection: inst.RepositorySelection,
			Suspended:           strings.TrimSpace(inst.SuspendedAt) != "",
		})
		ids = append(ids, formatInstallationId(inst.Id))

		repos, total, rerr := d.GitHubApp.InstallationRepositories(ctx, grant.Bearer, inst.Id, page)
		if rerr != nil {
			return pickerFailure(res, rerr)
		}
		for _, repo := range repos {
			res.Repositories = append(res.Repositories, PickerRepository{
				FullName:       repo.FullName,
				Owner:          repo.Owner.Login,
				Name:           repo.Name,
				Url:            repo.HTMLURL,
				Private:        repo.Private,
				Visibility:     repo.Visibility,
				DefaultBranch:  repo.DefaultBranch,
				PushedAt:       repo.PushedAt,
				InstallationId: formatInstallationId(inst.Id),
			})
		}
		if total > page*githubapp.RepositoriesPerPage {
			res.NextPage = page + 1
		}
	}

	res.Pending = pendingInstallations(ctx, d, grant.Login)

	// THE GRANT'S STORED INSTALLATION IDS ARE REFRESHED FROM WHAT WE JUST
	// READ, and this is one of the three owner-actor paths that keep them
	// current (the connect callback and a probe under a grant are the others).
	// It is why this epic needs no privileged webhook automation: a delivery
	// names a GitHub identity rather than a MemQL user, so finding the grant
	// it belongs to would be a cross-owner read past the concept's own tier --
	// and nothing reads these ids on a hot path anyway, because the fetcher
	// asks GitHub which installation covers a repository, live. The list is a
	// display cache, and a display cache is exactly what an owner-actor
	// refresh is good enough for.
	//
	// BEST EFFORT: a failed refresh is a warning, never a failed listing. The
	// person asked for their repositories and they are in hand.
	if grant.Id != "" && d.Store != nil {
		if werr := d.Store.recordGrantInstallations(ctx, grant.Id, ids); werr != nil {
			d.log().Warn("packages: could not refresh a GitHub grant's installation ids",
				"component", "packages.repositories", "credential", grant.Id, "err", werr)
		}
	}
	return res, nil
}

// resolvePickerGrant finds the grant to list under, answering a typed reason
// rather than an error for everything a person can act on.
//
// A NAMED credential that is not a grant is refused as credential_not_found
// rather than listed under: a pasted token has no installations and no
// repository listing, so answering an empty list would say "you can reach
// nothing" about a credential that was never asked the question.
func resolvePickerGrant(ctx context.Context, d *Deps, credentialId string) (ResolvedCredential, string, error) {
	actor := actorFromContext(ctx)
	if d.PeekCredentials == nil {
		return ResolvedCredential{}, "", refuse(CodeSourceUnreadable,
			"this node cannot resolve credentials, so it cannot list a grant's repositories")
	}

	id := strings.TrimSpace(credentialId)
	if id == "" {
		if d.Store == nil {
			return ResolvedCredential{}, RepositoriesReasonCredentialMissing, nil
		}
		row, err := d.Store.githubAppGrantForCaller(ctx)
		if err != nil {
			return ResolvedCredential{}, "", err
		}
		if row == nil {
			// Not connected, or disconnected. The surface offers Connect.
			return ResolvedCredential{}, RepositoriesReasonCredentialMissing, nil
		}
		id = rowString(row, "id")
	}

	// The peek resolver, not the fetch one: listing repositories is a
	// question, and lastUsedAt is the record of a fetch (D11).
	grant, cerr := d.PeekCredentials(ctx, id, actor.UserId)
	if cerr != nil {
		switch RefusalCode(cerr) {
		case CodeCredentialNotFound:
			return ResolvedCredential{}, RepositoriesReasonCredentialMissing, nil
		case CodeCredentialRevoked:
			return ResolvedCredential{}, RepositoriesReasonCredentialRevoked, nil
		case CodeReconnectRequired:
			return ResolvedCredential{}, RepositoriesReasonReconnectRequired, nil
		case CodeGithubAppNotConfigured:
			return ResolvedCredential{}, RepositoriesReasonNotConfigured, nil
		}
		return ResolvedCredential{}, "", cerr
	}
	if !grant.IsGrant() {
		return ResolvedCredential{}, RepositoriesReasonCredentialMissing, nil
	}
	return grant, "", nil
}

// pickerFailure maps a GitHub failure onto the picker's typed reasons, keeping
// the shape of the reply intact so the stop renders in place.
func pickerFailure(res SourceRepositoriesResult, err error) (SourceRepositoriesResult, error) {
	empty := SourceRepositoriesResult{
		Repositories:  []PickerRepository{},
		Installations: []PickerInstallation{},
		Pending:       []string{},
	}
	switch {
	case errors.Is(err, githubapp.ErrReauthorize):
		empty.Reason = RepositoriesReasonReconnectRequired
		return empty, nil
	case errors.Is(err, githubapp.ErrNotConfigured):
		empty.Reason = RepositoriesReasonNotConfigured
		return empty, nil
	case githubapp.IsRateLimited(err):
		empty.Reason = RepositoriesReasonRateLimited
		return empty, nil
	}
	// Anything else is GitHub being unwell or unreachable, which is neither a
	// person's mistake nor a repair they can carry out. It stays an error, and
	// the stop shows the server's own sentence.
	return SourceRepositoriesResult{}, refuse(CodeSourceUnreadable,
		"this cluster could not read the repositories this GitHub connection reaches: %v", err)
}

// pendingInstallations names the organizations whose installation this person
// requested and an owner has not yet approved.
//
// GitHub surfaces these to the APP rather than to the person, so the call runs
// under the app JWT and the entries are matched on the REQUESTER's login --
// which is why the grant's own login is the argument. A FAILURE ANSWERS AN
// EMPTY LIST rather than failing the whole read: this endpoint is not
// available on every app plan, and a picker that refused to list anybody's
// repositories because it could not enumerate pending requests would trade the
// answer for the footnote.
func pendingInstallations(ctx context.Context, d *Deps, login string) []string {
	out := []string{}
	if d.GitHubApp == nil || strings.TrimSpace(login) == "" {
		return out
	}
	requests, err := d.GitHubApp.PendingInstallationRequests(ctx)
	if err != nil {
		return out
	}
	for _, r := range requests {
		if !strings.EqualFold(strings.TrimSpace(r.Requester.Login), strings.TrimSpace(login)) {
			continue
		}
		if account := strings.TrimSpace(r.Account.Login); account != "" {
			out = append(out, account)
		}
	}
	return out
}

// formatInstallationId renders an installation id as TEXT.
//
// Text on the wire and text on the row, because installationIds is a []string
// on the concept and a picker that answered a number would hand the client a
// value it has to re-render before it can compare the two.
func formatInstallationId(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

// ---------------------------------------------------------------------------
// The capability
// ---------------------------------------------------------------------------

func (i *Integration) handleSourceRepositories(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	deps, err := i.resolve()
	if err != nil {
		return nil, err
	}
	res, rerr := SourceRepositories(ctx, deps, stringArg(args, "credentialId"), intArg(args, "page"))
	if rerr != nil {
		return nil, rerr
	}
	return resultNode(map[string]any{
		"reason":        res.Reason,
		"repositories":  res.Repositories,
		"installations": res.Installations,
		"pending":       res.Pending,
		"nextPage":      res.NextPage,
	}), nil
}

// intArg reads a page number off the wire.
//
// Through core/num rather than a bare int(x): a decoded payload number arrives
// as float64 or int64, and Go leaves the out-of-range conversion
// implementation-defined -- on amd64 int(1e30) is hugely NEGATIVE, which would
// pass straight through the `< 1` guard as page one and hide the nonsense
// (memql#4779). OrZero is the named answer here because the caller already
// reads 0 as "the first page", so an unusable value lands on the behaviour an
// absent one has.
func intArg(args map[string]any, key string) int {
	if args == nil {
		return 0
	}
	switch v := args[key].(type) {
	case int:
		return v
	case int64:
		return num.Int64OrZero(v)
	case float64:
		return num.Float64OrZero(v)
	}
	return 0
}
