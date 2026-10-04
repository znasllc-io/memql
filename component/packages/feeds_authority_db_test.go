package packages

import (
	"context"
	"errors"
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
func TestRepositoryPollMaintenanceActorDiscoversOwnedPackages(t *testing.T) {
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
			`mutation createPackage(accountId: "self", packageId: %s, name: "feed authorization test", sourceKind: "repo", repoUrl: %s)`,
			langparser.QuoteString(id), langparser.QuoteString(repoURL)))
	}

	// A plain system reader sees no owned sources. The production scheduler
	// starts without a caller, so this is the identity it previously received.
	reader := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "system:automation:pollPackageUpstreams", Role: auth.RoleReader, Unranked: true, Synthetic: true})
	rows, err := s.packagesTrackingRepos(reader)
	if err != nil || len(rows) != 0 {
		t.Fatalf("ordinary system reader saw owned sources: %v, %v", rows, err)
	}

	actor := auth.MaintenanceActor("pollPackageUpstreams")
	if actor == nil {
		t.Fatal("repository polling has no maintenance identity")
	}
	pollCtx := auth.ContextWithAccess(context.Background(), actor)
	pollCtx = auth.ContextWithToken(pollCtx, &auth.TokenInfo{Subject: actor.UserId})
	if _, err := i.handlePollUpstream(pollCtx, nil, 0); err != nil {
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
		if rowBool(row, "autoDeploy") || rowString(row, "deployedVersion") != "" {
			t.Fatalf("feed changed manual deployment policy or published a source: %s", id)
		}
	}
}

// THE WEBHOOK FEED OVER REAL ROWS (epic memql#5477). The unit suite drives a
// recording engine that answers whatever it is told; here every delivery is
// staged through the receiver's own @serverOnly mutation and the engine
// resolves inboundRequestById, so the row the feed reads -- its source, its
// body and its signatureVerified, through inboundRequestFull -- is the one
// Postgres holds.
//
// Every call runs under the package OWNER's actor. The shipped automation's
// own actor is the plain system reader, which the package tier does not admit
// (the webhook feed is deliberately off the maintenance list in
// component/auth/maintenance_actor.go), so the owner is the one actor that can
// both see this package and write its cue. That is what makes the two
// refusals measurable: without them, the same actor WOULD have moved the cue.
func TestTheWebhookFeedActsOnlyOnAStagedVerifiedDeliveryOverRealRows(t *testing.T) {
	eng, db := dbEngine(t)
	i, s := credentialHarness(t, eng, discardLogger())
	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	repoURL := "https://github.com/feed-webhook/repository-" + suffix
	owner := "feed-webhook-owner-" + suffix
	ownerCtx := deployerCtx(owner)
	packageID := "v1:platform:package:feed-webhook-" + suffix
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id LIKE ?", "%"+suffix+"%").Exec(context.Background())
	})
	mustExecute(t, eng, ownerCtx, fmt.Sprintf(
		`mutation createPackage(accountId: "self", packageId: %s, name: "webhook feed test", sourceKind: "repo", repoUrl: %s)`,
		langparser.QuoteString(packageID), langparser.QuoteString(repoURL)))

	// The receiver stages under internal origin and a system actor; nothing
	// else may (stageInboundRequest is @serverOnly).
	receiver := auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(context.Background(), "inbound-test"))
	pushTo := func(sha string) string {
		return fmt.Sprintf(`{"ref":"refs/heads/main","after":%q,"repository":{"html_url":%q,"default_branch":"main"}}`, sha, repoURL)
	}
	stage := func(tag, sha string, verified bool) string {
		requestID := "feed-webhook-" + tag + "-" + suffix
		mustExecute(t, eng, receiver, fmt.Sprintf(
			`mutation stageInboundRequest(requestId: %s, source: "github", medium: "webhook", body: %s, `+
				`signatureVerified: %t, verifiedBy: "env", receivedAt: "2026-10-04T12:00:00Z")`,
			langparser.QuoteString(requestID), langparser.QuoteString(pushTo(sha)), verified))
		return requestID
	}
	cue := func() string { return rowString(mustPackage(t, s, ownerCtx, packageID), "latestKnownVersion") }
	automation := auth.ContextWithInternalOrigin(ownerCtx)

	// A client: the automation's own argument for a verified push, on the
	// owner's context with no internal origin. Refused, and the cue stays put.
	signed := stage("signed", "signedsha00000000", true)
	if _, err := i.handleNoteUpstreamFromWebhook(ownerCtx, map[string]any{"inboundRequestId": signed}, 0); !errors.Is(err, errWebhookFeedClientOrigin) {
		t.Fatalf("a client call: err = %v, want errWebhookFeedClientOrigin", err)
	}
	if got := cue(); got != "" {
		t.Fatalf("a refused client call moved the cue to %q", got)
	}

	// An unsigned row, on the automation's origin: refused, the cue stays put.
	unsigned := stage("unsigned", "unsignedsha000000", false)
	if _, err := i.handleNoteUpstreamFromWebhook(automation, map[string]any{"inboundRequestId": unsigned}, 0); !errors.Is(err, errWebhookDeliveryUnverified) {
		t.Fatalf("an unsigned row: err = %v, want errWebhookDeliveryUnverified", err)
	}
	if got := cue(); got != "" {
		t.Fatalf("an unsigned delivery moved the cue to %q", got)
	}

	// THE CONTROL: the signed row on the automation's origin, with a forged
	// push in the arguments beside its id. The STAGED push is the one noted.
	if _, err := i.handleNoteUpstreamFromWebhook(automation, map[string]any{
		"inboundRequestId": signed, "source": "github", "body": pushTo("forgedsha00000000"),
	}, 0); err != nil {
		t.Fatalf("a signed delivery on the automation's origin: %v", err)
	}
	if got := cue(); got != "signedsha00000000" {
		t.Fatalf("the cue reads %q, want the staged push's signedsha00000000 -- not the argument's forged one", got)
	}
}
