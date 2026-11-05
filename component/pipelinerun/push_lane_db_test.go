package pipelinerun

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
)

// push_lane_db_test.go -- the push lane over REAL rows and the production gate
// (push_lane.go): a claim held behind the run ahead writes nothing to the row,
// a supersede concludes a waiting run and leaves a claimed one alone, and the
// fenced conclusion a cancel of an undriven run now writes (concludeUndriven)
// lands through the engine's row-version fence. What the fakes make true by
// construction -- the new query's filter and sort, the snapshot saved and read
// back, the fence -- is the engine's and the database's here.
//
// Postgres-gated: it skips when no database is reachable, and CI's db-tests
// lane runs this package with MEMQL_REQUIRE_DB=1, where a skip is a failure.
func TestThePushLaneOverRealRows(t *testing.T) {
	eng := dbEngine(t)
	store := NewDSLStore(eng)
	ctx := context.Background()

	gateDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = gateDB.Close() })
	gate := watchedGate(t, func(ctx context.Context, key string, fn func(context.Context) error) error {
		return githubconnect.WithGate(ctx, gateDB, key, fn)
	})

	suffix := strings.ReplaceAll(id.NewShortId(), "-", "")[:12]
	owner, repo, pkgID, account := "lane-owner-"+suffix, "acme-"+suffix+"/shop", "lane-pkg-"+suffix, "lane-account-"+suffix
	if _, err := eng.Execute(seederCtx(), fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Push lane test")`,
		langparser.QuoteString(account))); err != nil {
		t.Fatalf("createClientAccount: %v", err)
	}
	if _, err := eng.Execute(signedIn(owner), fmt.Sprintf(
		`mutation createPackage(accountId: %s, packageId: %s, name: "shop", sourceKind: "repo", repoUrl: %s, credentialId: %s)`,
		langparser.QuoteString(account), langparser.QuoteString(pkgID), langparser.QuoteString("https://github.com/"+repo),
		langparser.QuoteString("cred-"+suffix))); err != nil {
		t.Fatalf("createPackage: %v", err)
	}
	pkg, err := store.PackageForCaller(signedIn(owner), pkgID)
	if err != nil || pkg == nil {
		t.Fatalf("the source: %+v %v", pkg, err)
	}
	if err := store.CreatePipeline(ctx, Pipeline{
		ID: PipelineIDFor(pkg.ID), OwnerUserID: pkg.OwnerUserID, AccountID: pkg.AccountID, PackageID: pkg.ID, Name: "shop",
		Repository: repo, DefaultBranch: "main", InstallationID: 51234567, CredentialID: pkg.CredentialID, Delivery: DeliveryWebhook,
	}); err != nil {
		t.Fatalf("createPipeline: %v", err)
	}
	p := mustPipeline(t, store, PipelineIDFor(pkg.ID))

	integ := New(Deps{
		Store: store, GitHub: newFakeGitHub(t), Gate: gate, NodeID: "lane-agent-a-" + suffix,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), OSOrigin: func() string { return "https://os.example.test" },
	})
	optInPushLane(t, integ)
	policy := laneSnapshot(t, integ, p)
	now := time.Now().UTC().Truncate(time.Second)
	push := func(sha string, checkRunID int64, queued time.Time) Run {
		r := pushRunAt(p, sha, checkRunID, policy)
		r.ID = r.ID + "-" + suffix
		r.QueuedAt = queued
		if err := store.CreateRun(ctx, r); err != nil {
			t.Fatalf("createPipelineRun (push %s): %v", sha[:7], err)
		}
		return r
	}
	set := func(r Run, patch RunPatch) {
		t.Helper()
		if err := store.UpdateRun(ctx, r.OwnerUserID, r.ID, patch); err != nil {
			t.Fatalf("updatePipelineRun %s: %v", r.ID, err)
		}
	}
	executing := push(shaBase, 600, now.Add(-time.Hour))
	set(executing, RunPatch{Status: ptr(StatusInProgress), DriverNodeID: ptr("lane-agent-b"), DriverHeartbeatAt: ptr(now), StartedAt: ptr(now)})
	waiting := push(shaA, 601, now.Add(-30*time.Minute))
	claimed := push(shaB, 602, now.Add(-20*time.Minute))
	set(claimed, RunPatch{DriverNodeID: ptr("lane-agent-c"), DriverLeaseID: ptr("lease-c"), DriverHeartbeatAt: ptr(now)})
	asked := push(shaC, 603, now.Add(-10*time.Minute))
	t.Cleanup(func() {
		for _, r := range []Run{executing, waiting, claimed, asked} {
			_ = store.UpdateRun(ctx, r.OwnerUserID, r.ID, RunPatch{Status: ptr(StatusCompleted), Conclusion: ptr(ConclusionCancelled), FinishedAt: ptr(now)})
		}
	})
	d := integ.snapshot()

	// The saved snapshot reads back and still opts the run in.
	read, err := store.RunByID(ctx, waiting.ID)
	if err != nil || read == nil || !inPushLane(ctx, *read) {
		t.Fatalf("the waiting push read back is a lane member: %+v %v", read, err)
	}

	// Held behind the run in progress: nothing written.
	if _, ok, err := integ.claim(ctx, d, waiting.ID); err != nil || ok {
		t.Fatalf("a waiting push is held while another executes: %v %v", ok, err)
	}
	if got := mustRun(t, store, waiting.ID); got.Status != StatusQueued || got.DriverNodeID != "" || !got.StartedAt.IsZero() {
		t.Errorf("a held run is written nothing: %+v", got)
	}

	// A claimed run is left alone by a supersede; a waiting one is concluded
	// through the fenced write.
	if _, concluded, err := integ.cancelIfWaiting(ctx, d, claimed.ID, supersededPrefix+"x"); err != nil || concluded {
		t.Errorf("a claimed run is left to finish: %v %v", concluded, err)
	}
	if got := mustRun(t, store, claimed.ID); got.CancelRequested || got.Finished() || got.DriverNodeID != "lane-agent-c" {
		t.Errorf("the claimed run is untouched: %+v", got)
	}
	if _, concluded, err := integ.cancelIfWaiting(ctx, d, waiting.ID, supersededPrefix+asked.ID); err != nil || !concluded {
		t.Fatalf("a waiting run is concluded: %v %v", concluded, err)
	}
	if got := mustRun(t, store, waiting.ID); got.Status != StatusCompleted || got.Conclusion != ConclusionCancelled ||
		!got.CancelRequested || got.CancelledBy != supersededPrefix+asked.ID || !stoppedBySupersede(got) {
		t.Errorf("superseded over real rows: %+v", got)
	}
	// A person's cancel of a run nobody drives writes the same fenced
	// conclusion.
	if err := integ.RequestCancel(ctx, asked.ID, owner); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := mustRun(t, store, asked.ID); got.Status != StatusCompleted || got.Conclusion != ConclusionCancelled || got.CancelledBy != owner {
		t.Errorf("a person's cancel over real rows: %+v", got)
	}

	// The control: with the runs ahead finished, a waiting push is claimed.
	set(executing, RunPatch{Status: ptr(StatusCompleted), Conclusion: ptr(ConclusionSuccess), FinishedAt: ptr(now)})
	set(claimed, RunPatch{Status: ptr(StatusCompleted), Conclusion: ptr(ConclusionSuccess), FinishedAt: ptr(now)})
	next := push(shaD, 604, now.Add(-time.Minute))
	t.Cleanup(func() {
		_ = store.UpdateRun(ctx, next.OwnerUserID, next.ID, RunPatch{Status: ptr(StatusCompleted), Conclusion: ptr(ConclusionCancelled), FinishedAt: ptr(now)})
	})
	if got, ok, err := integ.claim(ctx, d, next.ID); err != nil || !ok || got.DriverNodeID != d.NodeID {
		t.Errorf("with the lane free, the waiting push is claimed: %+v %v %v", got, ok, err)
	}
	if lane, err := store.RunsUnfinishedForPipelineEvent(ctx, p.ID, pipelines.EventPush); err != nil || len(lane) != 1 || lane[0].ID != next.ID {
		t.Errorf("the lane holds the one claimed push: %+v %v", lane, err)
	}
}
