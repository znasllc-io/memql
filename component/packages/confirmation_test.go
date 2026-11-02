package packages

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

func TestBackgroundConfirmationRefusesBeforeClaim(t *testing.T) {
	for _, name := range []string{"unconfirmed", "automatic", "missing id", "missing report", "missing snapshot", "different snapshot", "cancel requested", "no database gate", "claim write unavailable"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, spaOnlyPackage(), ownerPackage())
			rep, err := Analyze(spaOnlyPackage(), Options{SourceVersion: "sha-abc123"})
			if err != nil {
				t.Fatal(err)
			}
			row := parkedRun("saved", "abc", map[string]any{"snapshotArtifactId": "snapshot", "report": reportMap(t, rep)})
			h.engine.rows["query packageDeploymentById"] = []map[string]any{row}
			req := DeployRequest{PackageId: "abc", DeploymentId: "saved", Confirmed: true, Actor: plainUser()}
			switch name {
			case "unconfirmed":
				req.Confirmed = false
			case "automatic":
				req.Automatic = true
			case "missing id":
				req.DeploymentId = ""
			case "missing report":
				delete(row, "report")
			case "missing snapshot":
				delete(row, "snapshotArtifactId")
			case "different snapshot":
				req.FromDeploymentId = "different"
			case "cancel requested":
				row["cancelRequested"] = true
			case "no database gate":
				h.deps.Store.deploymentGate = nil
			case "claim write unavailable":
				h.engine.fail = map[string]error{"mutation claimPackageDeploymentConfirmation": errors.New("fixture unavailable")}
			}
			if _, err := StartConfirmation(context.Background(), h.deps, req); err == nil {
				t.Fatal("invalid/unknown acceptance launched execution")
			}
			if name != "claim write unavailable" && h.engine.sawStatement("mutation claimPackageDeploymentConfirmation") {
				t.Fatal("invalid request claimed the attempt")
			}
			if len(h.builder.built) != 0 || len(h.publisher.published) != 0 || h.fetcher.repoFetches != 0 {
				t.Fatal("refused confirmation performed work")
			}
		})
	}
}

func TestConfirmationBudgetRetainsEachSelectedBuildAndAsset(t *testing.T) {
	t.Setenv(BuildTimeoutEnv, "7200")
	report := &Report{Deployables: []DeployableReport{
		{Name: "one"}, {Name: "two", Assets: []ManifestAsset{{Path: "asset"}}},
		{Name: "prebuilt", Prebuilt: true}, {Name: "skipped"},
	}}
	got, err := confirmationTimeout(report, map[string]Placement{"skipped": {Skip: true}})
	want := 30*time.Minute + 2*(2*time.Hour+time.Minute) + sourceDownloadTimeout
	if err != nil || got != want {
		t.Fatalf("budget=%s want=%s err=%v", got, want, err)
	}
	t.Setenv(BuildTimeoutEnv, "1")
	got, err = confirmationTimeout(report, map[string]Placement{"one": {Skip: true}, "two": {Skip: true}, "skipped": {Skip: true}})
	if err != nil || got != 30*time.Minute {
		t.Fatalf("prebuilt budget=%s %v", got, err)
	}
}

type cancelSource struct {
	*fakeFetcher
	cancel context.CancelFunc
}

func (f cancelSource) FetchRepo(ctx context.Context, _ RepoSource, _ Limits) (*SourceSnapshot, error) {
	f.cancel()
	return nil, ctx.Err()
}

type terminalContextEngine struct {
	*recordingEngine
	closed  bool
	actor   string
	bounded bool
}

func (e *terminalContextEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(q, "mutation closePackageDeployment(") {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		e.closed = true
		e.actor = actorFromContext(ctx).UserId
		deadline, ok := ctx.Deadline()
		e.bounded = ok && time.Until(deadline) > 0 && time.Until(deadline) <= 30*time.Second
	}
	return e.recordingEngine.Execute(ctx, q)
}

func TestCancelledSynchronousDeploymentStillRecordsBoundedTerminalOutcome(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	e := &terminalContextEngine{recordingEngine: h.engine}
	h.deps.Store.engine = e
	ctx, cancel := context.WithCancel(callerCtx("v1:identity:user:someone"))
	defer cancel()
	h.deps.Fetcher = cancelSource{fakeFetcher: h.fetcher, cancel: cancel}
	out, err := Deploy(ctx, h.deps, DeployRequest{PackageId: "abc", Confirmed: true, Actor: actorFromContext(ctx)})
	if !errors.Is(err, context.Canceled) || out == nil || out.Status != StatusFailed {
		t.Fatalf("outcome=%+v err=%v", out, err)
	}
	if !e.closed || !e.bounded || e.actor != actorFromContext(ctx).UserId {
		t.Fatalf("terminal context lost auth/lifetime: %+v", e)
	}
	if len(h.builder.built) != 0 || len(h.publisher.published) != 0 {
		t.Fatal("cancelled request reached effects")
	}
}
