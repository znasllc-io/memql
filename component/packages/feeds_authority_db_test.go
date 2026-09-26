package packages

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// Exercise the actual owned-row gate: recordingEngine cannot distinguish an
// authorized sweep from the production failure that reported checked=0.
func TestRepositoryFeedMaintenanceActorsDiscoverOwnedPackages(t *testing.T) {
	eng, db := dbEngine(t)
	i, s := credentialHarness(t, eng, discardLogger())
	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	repoPath := "feed-auth/repository-" + suffix
	repoURL := "https://github.com/" + repoPath
	gh := &fakeGitHub{repoPath: repoPath, sha: "feed-poll-revision"}
	i.deps.HTTP = &http.Client{Transport: gh}
	owners := []context.Context{deployerCtx("feed-owner-a-" + suffix), deployerCtx("feed-owner-b-" + suffix)}
	ids := []string{"v1:platform:package:feed-a-" + suffix, "v1:platform:package:feed-b-" + suffix}
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("concept = ? AND id = ?", "v1:platform:package", id).Exec(context.Background())
		}
	})
	for n, id := range ids {
		mustExecute(t, eng, owners[n], fmt.Sprintf(
			`mutation createPackage(packageId: %s, name: "feed authorization test", sourceKind: "repo", repoUrl: %s)`,
			langparser.QuoteString(id), langparser.QuoteString(repoURL)))
	}

	// A plain system reader sees no owned sources. The production scheduler
	// starts without a caller, so this is the identity it previously received.
	reader := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "system:automation:pollPackageUpstreams", Role: auth.RoleReader, Unranked: true, Synthetic: true})
	rows, err := s.packagesTrackingRepos(reader)
	if err != nil || len(rows) != 0 {
		t.Fatalf("ordinary system reader saw owned sources: %v, %v", rows, err)
	}

	feedContext := func(name string) context.Context {
		actor := auth.MaintenanceActor(name)
		if actor == nil {
			t.Fatalf("repository feed %s has no maintenance identity", name)
		}
		ctx := auth.ContextWithAccess(context.Background(), actor)
		return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: actor.UserId})
	}
	if _, err := i.handlePollUpstream(feedContext("pollPackageUpstreams"), nil, 0); err != nil {
		t.Fatalf("scheduled repository poll: %v", err)
	}
	if got := len(gh.ours()); got != 2 {
		t.Fatalf("poll reached %d of two owners' sources", got)
	}
	for n, id := range ids {
		row := mustPackage(t, s, owners[n], id)
		if rowString(row, "latestKnownVersion") != gh.sha || !rowBool(row, "updateAvailable") {
			t.Fatalf("poll did not record the pending revision for %s", id)
		}
	}

	push := fmt.Sprintf(`{"ref":"refs/heads/main","after":"feed-webhook-revision","repository":{"html_url":%s,"default_branch":"main"}}`, langparser.QuoteString(repoURL))
	if _, err := i.handleNoteUpstreamFromWebhook(feedContext("notePackageUpstreamFromWebhook"), map[string]any{"source": "github", "body": push}, 0); err != nil {
		t.Fatalf("verified webhook feed: %v", err)
	}
	for n, id := range ids {
		row := mustPackage(t, s, owners[n], id)
		if rowString(row, "latestKnownVersion") != "feed-webhook-revision" || !rowBool(row, "updateAvailable") {
			t.Fatalf("webhook did not update both owners' sources: %s", id)
		}
		if rowBool(row, "autoDeploy") || rowString(row, "deployedVersion") != "" {
			t.Fatalf("feed changed manual deployment policy or published a source: %s", id)
		}
	}
}
