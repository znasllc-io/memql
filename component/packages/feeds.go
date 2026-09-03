package packages

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// feeds.go is D11: two feeds, one effect.
//
// Both write EXACTLY latestKnownVersion + updateAvailable, through the one
// mutation that can write them, and neither ever starts a deployment. That
// narrowness is the design rather than an implementation detail: a feed able
// to write anything else would be deciding to deploy somebody's code, and
// deploying an update is a person's click starting a new run through analyze
// and confirm again.

// WebhookSourceEnv names the inbound source segment GitHub deliveries arrive
// on. A cluster whose allowlist calls it something else sets this; unset means
// "github", which is what the runbook tells an operator to use.
const WebhookSourceEnv = "MEMQL_PACKAGES_WEBHOOK_SOURCE"

const defaultWebhookSource = "github"

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
		return resultNode(map[string]any{"skipped": perr.Error()}), nil
	}

	matched, uerr := noteUpstream(ctx, deps, ev.RepoUrl, ev.Version)
	if uerr != nil {
		return nil, uerr
	}
	return resultNode(map[string]any{
		"repoUrl": ev.RepoUrl,
		"version": ev.Version,
		"matched": matched,
	}), nil
}

// upstreamEvent is what either feed learned about a repository.
type upstreamEvent struct {
	RepoUrl string
	Version string
}

// parseGitHubPush reads the two facts a push or release carries.
//
// Version is the commit SHA for a push and the tag for a release, MIRRORING
// what sourceVersion and deployedVersion record -- otherwise the comparison
// that lights the cue would be between two different kinds of string, and
// updateAvailable would be permanently true.
func parseGitHubPush(body string) (upstreamEvent, error) {
	var payload struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Repository struct {
			HTMLURL  string `json:"html_url"`
			CloneURL string `json:"clone_url"`
		} `json:"repository"`
		Release struct {
			TagName string `json:"tag_name"`
		} `json:"release"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return upstreamEvent{}, fmt.Errorf("the delivery body is not JSON this cluster reads")
	}
	repo := strings.TrimSpace(payload.Repository.HTMLURL)
	if repo == "" {
		repo = strings.TrimSuffix(strings.TrimSpace(payload.Repository.CloneURL), ".git")
	}
	if repo == "" {
		return upstreamEvent{}, fmt.Errorf("the delivery names no repository")
	}
	version := strings.TrimSpace(payload.Release.TagName)
	if version == "" {
		version = strings.TrimSpace(payload.After)
	}
	if version == "" {
		return upstreamEvent{}, fmt.Errorf("the delivery names no version")
	}
	return upstreamEvent{RepoUrl: repo, Version: version}, nil
}

// noteUpstream writes the two feed-owned fields on every package tracking a
// repository, and reports how many it touched.
//
// It compares against deployedVersion rather than against latestKnownVersion,
// because updateAvailable means "there is something newer than what is LIVE".
// Comparing against the last thing a feed saw would leave the flag true
// forever after one deploy.
func noteUpstream(ctx context.Context, d *Deps, repoUrl, version string) (int, error) {
	packages, err := d.Store.packagesByRepoUrl(ctx, normalizeRepoUrl(repoUrl))
	if err != nil {
		return 0, err
	}
	matched := 0
	for _, pkg := range packages {
		id := rowString(pkg, "id")
		if id == "" {
			continue
		}
		deployed := rowString(pkg, "deployedVersion")
		known := rowString(pkg, "latestKnownVersion")
		available := version != "" && version != deployed
		if known == version && rowBool(pkg, "updateAvailable") == available {
			// NOTHING CHANGED, so nothing is written. A write here would
			// broadcast a row change to every subscriber and re-fire the OS
			// arrival cue on a heartbeat -- the exact "a heartbeat is not
			// news" failure clients/os/README.md names.
			continue
		}
		if err := d.Store.recordUpstreamVersion(ctx, id, version, available); err != nil {
			return matched, err
		}
		matched++

		// AND THEN, ONLY IF THE SOURCE ASKED FOR IT (epic memql#4900, task
		// memql#4903), the deploy this feed has never been allowed to start.
		//
		// The rule the header states -- "neither ever starts a deployment" --
		// held for one reason: deploying somebody's code is a decision, and a
		// feed had no way to know it had been made. The switch is that
		// decision, taken once, in advance, by the person who owns the
		// source. So the feed still decides nothing; it acts on a decision
		// that is already on the row.
		//
		// Only when there is genuinely something newer than what is live: a
		// re-announcement of the version already deployed must not start a
		// run, or a repository with a chatty webhook would redeploy itself
		// forever.
		if available {
			if _, aerr := d.startAutoRun(ctx, pkg, version); aerr != nil {
				// One package's auto-run must not stop the sweep: the feed's
				// own job -- recording what moved -- is already done for this
				// row, and every other package still deserves its cue.
				d.log().Warn("packages: an auto-deploy could not be started",
					"component", "packages.autodeploy", "package", id, "err", aerr)
			}
		}
	}
	return matched, nil
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
		n, uerr := noteUpstream(ctx, deps, repoUrl, head)
		if uerr != nil {
			return nil, uerr
		}
		updated += n
	}
	return resultNode(map[string]any{"checked": checked, "updated": updated}), nil
}

// upstreamHead asks GitHub for the ref's current commit.
func (i *Integration) upstreamHead(ctx context.Context, d *Deps, pkg map[string]any) (string, error) {
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

	// D14 again: resolved here, used once, stored nowhere.
	if name := strings.TrimSpace(rowString(pkg, "repoTokenRef")); name != "" {
		token, serr := d.Store.resolveSecret(ctx, name)
		if serr == nil && strings.TrimSpace(token) != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, derr := client.Do(req)
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
