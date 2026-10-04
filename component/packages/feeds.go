package packages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// feeds.go is D11: two feeds, one effect.
//
// Both detect upstream revisions regardless of deployment policy. Automatic
// sources reconcile pending revisions through the same guarded deploy pipeline.

// WebhookSourceEnv names the inbound source segment GitHub deliveries arrive
// on. A cluster whose allowlist calls it something else sets this; unset means
// "github", which is what the runbook tells an operator to use.
const WebhookSourceEnv = "MEMQL_PACKAGES_WEBHOOK_SOURCE"

const defaultWebhookSource = "github"

// DefaultWebhookSource is defaultWebhookSource, exported for ONE reader: the
// test in app/ that pins this name to the URL the cluster's GitHub App is told
// to post to (component/identity/githubconnect.WebhookPath) and to the inbound
// policy registered for it. Three modules spell it, none can import the others
// downward, and a drift between them is a webhook nothing reads.
const DefaultWebhookSource = defaultWebhookSource

func webhookSource() string {
	if v := strings.TrimSpace(envValue(WebhookSourceEnv)); v != "" {
		return v
	}
	return defaultWebhookSource
}

// handleNoteUpstreamFromWebhook is the webhook feed.
//
// It reads a row the inbound receiver ALREADY verified -- source allowlist and
// per-source HMAC both -- so there is no signature check here and there must
// not be one: a second, weaker copy of a check that already passed is how a
// bypass gets written. A delivery matching no package is a no-op, because most
// of a cluster's webhooks are about something else entirely.
func (i *Integration) handleNoteUpstreamFromWebhook(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	deps, err := i.resolve()
	if err != nil {
		return nil, err
	}
	source := strings.TrimSpace(stringArg(args, "source"))
	if source != webhookSource() {
		return resultNode(map[string]any{"skipped": "not a package source", "source": source}), nil
	}

	ev, perr := parseGitHubPush(stringArg(args, "body"))
	if perr != nil {
		// A body this cluster cannot read is not a failure of the delivery --
		// GitHub sends event types nobody here models. Skipped, and named, so
		// an operator reading the trail can tell "ignored" from "broken".
		//
		// errNotAPush lands here too, and it is the commonest: the pull
		// request, merge queue and check deliveries pipelines subscribe to
		// arrive on this same source (epic memql#5477), and every one of them
		// is somebody else's, not a fault. Nothing is looked up, nothing is
		// noted, and no auto-deploy starts.
		return resultNode(map[string]any{"skipped": perr.Error()}), nil
	}

	matched, uerr := noteUpstreamEvent(ctx, deps, ev)
	if uerr != nil {
		return nil, uerr
	}
	return resultNode(map[string]any{
		"repoUrl": ev.RepoUrl,
		"version": ev.Version,
		"matched": matched,
	}), nil
}

// errNotAPush is a delivery that is neither a push nor a release, and is
// nothing to note.
//
// Once the GitHub App subscribes to pipelines' events (epic memql#5477), much
// of what arrives on this source is one: a pull request, a merge queue, a
// check run or suite. Each names the repository a push names, and each carries
// a SHA somewhere -- a pull request's synchronize puts `before` and `after` at
// the top of its body, exactly where a push does. None of them moves what a
// SOURCE deploys, so none may move the update cue or start an auto-deploy:
// noting a pull request's head would offer a branch nobody merged as the
// source's next version.
var errNotAPush = errors.New("the delivery is not a push")

// notAPushKeys are the top-level keys that mark another event's body. Read
// from the SIGNED BODY's shape, deliberately: X-GitHub-Event names the event
// too, but it is a header, and the signature the inbound seam verified does
// not cover it.
//
// `release` is NOT among them. A release is an upstream event this feed has
// always read -- it is how a source tracking a tag learns of a new one -- and
// the pipelines reading the same delivery do not change what it means here.
var notAPushKeys = []string{"pull_request", "merge_group", "check_run", "check_suite"}

// upstreamEvent is what either feed learned about a repository.
type upstreamEvent struct {
	RepoUrl       string
	Version       string
	Ref           string
	DefaultBranch string
}

// parseGitHubPush reads the facts a push or release carries, and answers
// errNotAPush for any other delivery.
//
// Version is the commit SHA for a push and the tag for a release, MIRRORING
// what sourceVersion and deployedVersion record -- otherwise the comparison
// that lights the cue would be between two different kinds of string, and
// updateAvailable would be permanently true.
func parseGitHubPush(body string) (upstreamEvent, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		return upstreamEvent{}, fmt.Errorf("the delivery body is not JSON this cluster reads")
	}
	for _, key := range notAPushKeys {
		if _, ok := top[key]; ok {
			return upstreamEvent{}, fmt.Errorf("%w: it carries %s", errNotAPush, key)
		}
	}
	var payload struct {
		Ref        string `json:"ref"`
		Deleted    bool   `json:"deleted"`
		After      string `json:"after"`
		Repository struct {
			HTMLURL       string `json:"html_url"`
			DefaultBranch string `json:"default_branch"`
			CloneURL      string `json:"clone_url"`
		} `json:"repository"`
		Release struct {
			TagName string `json:"tag_name"`
		} `json:"release"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return upstreamEvent{}, fmt.Errorf("the delivery body is not JSON this cluster reads")
	}
	tag := strings.TrimSpace(payload.Release.TagName)
	ref := strings.TrimSpace(payload.Ref)
	if ref == "" && tag != "" {
		ref = "refs/tags/" + tag
	}
	// A push names its ref and a release names its tag. A body with neither
	// is another event, and an EMPTY ref used to skip the branch filter in
	// noteUpstreamEvent: such a body was noted on every package tracking the
	// repository, whatever branch each tracks.
	if ref == "" {
		return upstreamEvent{}, fmt.Errorf("%w: it names no ref and no release tag", errNotAPush)
	}
	repo := strings.TrimSpace(payload.Repository.HTMLURL)
	if repo == "" {
		repo = strings.TrimSuffix(strings.TrimSpace(payload.Repository.CloneURL), ".git")
	}
	if repo == "" {
		return upstreamEvent{}, fmt.Errorf("the delivery names no repository")
	}
	version := tag
	if version == "" {
		version = strings.TrimSpace(payload.After)
	}
	if version == "" || payload.Deleted || strings.Trim(version, "0") == "" {
		return upstreamEvent{}, fmt.Errorf("the delivery names no version")
	}
	return upstreamEvent{RepoUrl: repo, Version: version, Ref: ref, DefaultBranch: payload.Repository.DefaultBranch}, nil
}

// noteUpstream writes the two feed-owned fields on every package tracking a
// repository, and reports how many it touched.
//
// It compares against deployedVersion rather than against latestKnownVersion,
// because updateAvailable means "there is something newer than what is LIVE".
// Comparing against the last thing a feed saw would leave the flag true
// forever after one deploy.
func noteUpstream(ctx context.Context, d *Deps, repoUrl, version string) (int, error) {
	return noteUpstreamEvent(ctx, d, upstreamEvent{RepoUrl: repoUrl, Version: version})
}

func noteUpstreamEvent(ctx context.Context, d *Deps, ev upstreamEvent) (int, error) {
	packages, err := d.Store.packagesByRepoUrl(ctx, normalizeRepoUrl(ev.RepoUrl))
	if err != nil {
		return 0, err
	}
	matched := 0
	for _, pkg := range packages {
		if ev.Ref != "" {
			ref := rowString(pkg, "repoRef")
			if ref == "" {
				ref = ev.DefaultBranch
			}
			// An absent default branch is ambiguous; the poll resolves it safely.
			if ref == "" || (ev.Ref != ref && ev.Ref != "refs/heads/"+ref && ev.Ref != "refs/tags/"+ref) {
				continue
			}
		}
		n, err := notePackageUpstream(ctx, d, pkg, ev.Version)
		matched += n
		if err != nil {
			return matched, err
		}
	}
	return matched, nil
}

// Detection is independent of deployment policy. Reconcile even an unchanged
// pending revision: a previous run may have been busy, or Manual was just armed.
func notePackageUpstream(ctx context.Context, d *Deps, pkg map[string]any, version string) (int, error) {
	id := rowString(pkg, "id")
	if id == "" || rowString(pkg, "status") != "active" {
		return 0, nil
	}
	available := version != "" && version != rowString(pkg, "deployedVersion")
	changed := 0
	if rowString(pkg, "latestKnownVersion") != version || rowBool(pkg, "updateAvailable") != available {
		if err := d.Store.recordUpstreamVersion(ctx, id, version, available); err != nil {
			return 0, err
		}
		changed = 1
	}
	if available {
		if _, err := d.startAutoRun(ctx, pkg, version); err != nil {
			d.log().Warn("packages: an auto-deploy could not be started", "component", "packages.autodeploy", "package", id, "err", err)
		}
	}
	return changed, nil
}

// normalizeRepoUrl makes the webhook's spelling and the stored spelling the
// same string. GitHub sends html_url without a trailing slash and without
// .git; a person pastes either.
func normalizeRepoUrl(raw string) string {
	u := strings.TrimSpace(raw)
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimSuffix(u, ".git")
	return u
}

// handlePollUpstream is the polling fallback for clusters no webhook reaches.
func (i *Integration) handlePollUpstream(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	deps, err := i.resolve()
	if err != nil {
		return nil, err
	}
	packages, err := deps.Store.packagesTrackingRepos(ctx)
	if err != nil {
		return nil, err
	}

	checked, updated := 0, 0
	for _, pkg := range packages {
		repoUrl := rowString(pkg, "repoUrl")
		if repoUrl == "" {
			continue
		}
		checked++
		head, herr := i.upstreamHead(ctx, deps, pkg)
		if herr != nil {
			// One unreachable repository must not stop the sweep: a private
			// repo whose token was rotated is somebody's problem to fix, and
			// every other package's cue still deserves to be current.
			i.logger.Warn("packages: could not read the upstream head",
				"component", "packages.feeds", "package", rowString(pkg, "id"), "err", herr)
			continue
		}
		n, uerr := notePackageUpstream(ctx, deps, pkg, head)
		if uerr != nil {
			return nil, uerr
		}
		updated += n
	}
	return resultNode(map[string]any{"checked": checked, "updated": updated}), nil
}

// upstreamHead asks GitHub for the ref's current commit.
func (i *Integration) upstreamHead(ctx context.Context, d *Deps, pkg map[string]any) (string, error) {
	if err := d.validatePackageSourceConnection(ctx, pkg); err != nil {
		return "", err
	}
	owner, repo, err := parseGitHubRepo(rowString(pkg, "repoUrl"))
	if err != nil {
		return "", err
	}
	ref := strings.TrimSpace(rowString(pkg, "repoRef"))
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/commits/%s", owner, repo, refOrHead(ref))

	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if rerr != nil {
		return "", rerr
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "memql-packages")

	// D10 again: resolved here, under the package owner's actor, used once,
	// stored nowhere. A credential that does not resolve is a REFUSAL of this
	// package's poll, not a fall-through to an anonymous request: polling a
	// private repository without its bearer answers 404, which would read as
	// "unreachable" today and as "unchanged" the day the comparison is
	// tightened -- and either way the person whose credential was revoked
	// would learn it from a stale cue rather than from the warning this
	// returns. The caller logs it and skips the package, exactly as an
	// unreachable repository is skipped.
	if id := strings.TrimSpace(rowString(pkg, "credentialId")); id != "" {
		if d.Credentials == nil {
			return "", refuse(CodeSourceUnreadable,
				"this package fetches under credential %q, and this node cannot resolve credentials", id)
		}
		resolved, cerr := d.Credentials(ctx, id, rowString(pkg, "ownerUserId"))
		if cerr != nil {
			return "", cerr
		}
		// The same two bearers the fetcher chooses between, for the same
		// reason (epic memql#4912, C6): a poll runs every ten minutes with
		// nobody watching, so under a grant it must carry an INSTALLATION
		// token rather than the person's own. A grant that needs reconnecting
		// refuses here, and handlePollUpstream skips this package with a
		// warning -- exactly as it skips one whose credential was revoked, and
		// never by polling anonymously.
		bearer, berr := installationBearer(ctx, d.GitHubApp, resolved, owner, repo)
		if berr != nil {
			return "", berr
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, derr := d.httpClient().Do(req)
	if derr != nil {
		return "", derr
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("GitHub answered HTTP %d", resp.StatusCode)
	}

	var payload struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if strings.TrimSpace(payload.SHA) == "" {
		return "", fmt.Errorf("GitHub returned no commit sha")
	}
	return payload.SHA, nil
}

// refOrHead resolves an empty ref to the default branch, which the commits
// endpoint spells as HEAD.
func refOrHead(ref string) string {
	if strings.TrimSpace(ref) == "" {
		return "HEAD"
	}
	return ref
}
