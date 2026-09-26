package packages

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

type analysisAudit chan DeployAuditEvent

func (a analysisAudit) Deploy(_ context.Context, ev DeployAuditEvent) { a <- ev }

type waitingSource struct {
	*fakeFetcher
	entered chan context.Context
	release chan struct{}
}

func (f *waitingSource) FetchRepo(ctx context.Context, src RepoSource, limits Limits) (*SourceSnapshot, error) {
	f.entered <- ctx
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.release:
		return f.fakeFetcher.FetchRepo(ctx, src, limits)
	}
}

// Each node has its own Store/Integration; the durable flag is the only link.
type sharedAnalysisRows struct {
	*recordingEngine
	cancelRequested atomic.Bool
	parked          atomic.Bool
	opened          atomic.Bool
	cancelAtPark    bool
	parkAtCancel    bool
}

func (e *sharedAnalysisRows) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(q, "mutation openPackageDeployment(") {
		e.opened.Store(true)
	}
	if strings.HasPrefix(q, "mutation requestPackageDeploymentCancel(") {
		e.cancelRequested.Store(true)
		if e.parkAtCancel {
			e.parked.Store(true)
		}
	}
	if strings.HasPrefix(q, "mutation advancePackageDeployment(") && strings.Contains(q, `"awaiting_confirm"`) {
		e.parked.Store(true)
		if e.cancelAtPark {
			e.cancelRequested.Store(true)
		}
	}
	if strings.HasPrefix(q, "query packageDeploymentById(") && e.opened.Load() {
		status := StatusAnalyzing
		if e.parked.Load() {
			status = StatusAwaitingConfirm
		}
		return &memql.ExecuteResult{Bundle: asBundle([]map[string]any{{
			"id": "v1:platform:packageDeployment:fixed1", "packageId": "v1:platform:package:abc",
			"status": status, "cancelRequested": e.cancelRequested.Load(),
		}})}, nil
	}
	return e.recordingEngine.Execute(ctx, q)
}

func awaitAnalysis(t *testing.T, audit analysisAudit) DeployAuditEvent {
	t.Helper()
	select {
	case ev := <-audit:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("analysis did not finish")
		return DeployAuditEvent{}
	}
}

func TestBackgroundAnalysisSurvivesLeavingAndCanBeCancelledOnAnotherReplica(t *testing.T) {
	for _, cancelOnReplica := range []bool{false, true} {
		t.Run(map[bool]string{false: "leave finishes at review", true: "cancel stops the actual fetch"}[cancelOnReplica], func(t *testing.T) {
			t.Setenv(HeartbeatIntervalEnv, "1")
			h := newHarness(t, spaOnlyPackage(), ownerPackage())
			rows := &sharedAnalysisRows{recordingEngine: h.engine}
			h.deps.Store = &store{engine: rows, deploymentGate: offlineDeploymentGate}
			fetch := &waitingSource{fakeFetcher: h.fetcher, entered: make(chan context.Context, 1), release: make(chan struct{})}
			h.deps.Fetcher = fetch
			audit := make(analysisAudit, 1)
			h.deps.Auditor = audit
			requestCtx, leave := context.WithCancel(callerCtx("v1:identity:user:someone"))
			starter := NewIntegration(rows, discardLogger())
			starter.depsOnce.Do(func() { starter.deps = h.deps })
			reply, err := starter.handleDeploy(requestCtx, map[string]any{"packageId": "v1:platform:package:abc", "background": true, "confirm": false}, 0)
			if err != nil {
				t.Fatal(err)
			}
			accepted := replyPayload(t, reply)
			out := &DeployOutcome{DeploymentId: accepted["deploymentId"].(string), Status: accepted["status"].(string)}
			if err != nil || out.Status != StatusAnalyzing || out.DeploymentId == "" {
				t.Fatalf("start: %+v %v", out, err)
			}
			var running context.Context
			select {
			case running = <-fetch.entered:
			case <-time.After(time.Second):
				t.Fatal("fetch never started")
			}
			if !hasCall(h.engine.statements(), "mutation openPackageDeployment(") {
				t.Fatal("returned ID before persistence")
			}
			leave()
			if running.Err() != nil {
				t.Fatal("leaving cancelled background work")
			}
			if cancelOnReplica {
				other := NewIntegration(rows, discardLogger())
				other.depsOnce.Do(func() { other.deps = &Deps{Store: &store{engine: rows}, Logger: discardLogger()} })
				_, err = other.handleCancelDeployment(callerCtx("v1:identity:user:someone"), map[string]any{"packageId": "abc", "deploymentId": out.DeploymentId}, 0)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				close(fetch.release)
			}
			event := awaitAnalysis(t, audit)
			want := StatusAwaitingConfirm
			if cancelOnReplica {
				want = StatusCancelled
			}
			if event.Status != want {
				t.Fatalf("want %s, got %+v", want, event)
			}
			if len(h.builder.built) > 0 {
				t.Fatal("analysis built something")
			}
		})
	}
}

func TestCancellationRacingAnalysisParkingIsNeverLeftUnread(t *testing.T) {
	t.Run("flag written during park", func(t *testing.T) {
		h := newHarness(t, spaOnlyPackage(), ownerPackage())
		rows := &sharedAnalysisRows{recordingEngine: h.engine, cancelAtPark: true}
		h.deps.Store = &store{engine: rows, deploymentGate: offlineDeploymentGate}
		out, err := Deploy(context.Background(), h.deps, DeployRequest{PackageId: "abc", Actor: plainUser()})
		if RefusalCode(err) != CodeDeploymentCancelled || out.Status != StatusCancelled || out.AwaitingConfirm {
			t.Fatalf("lost cancellation: %+v %v", out, err)
		}
	})
	t.Run("parked between cancel read and flag", func(t *testing.T) {
		rows := &sharedAnalysisRows{recordingEngine: &recordingEngine{}, parkAtCancel: true}
		rows.opened.Store(true)
		other := NewIntegration(rows, discardLogger())
		other.depsOnce.Do(func() { other.deps = &Deps{Store: &store{engine: rows}, Logger: discardLogger()} })
		reply, err := other.handleCancelDeployment(context.Background(), map[string]any{"packageId": "abc", "deploymentId": "fixed1"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if replyPayload(t, reply)["status"] != StatusCancelled {
			t.Fatal("parked run left flagged with nobody to read it")
		}
	})
}

func TestBackgroundAnalysisCannotConfirmOrResumeAnExistingRun(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	for _, req := range []DeployRequest{{PackageId: "abc", Confirmed: true}, {PackageId: "abc", Automatic: true}, {PackageId: "abc", DeploymentId: "other"}} {
		if _, err := StartAnalysis(context.Background(), h.deps, req); err == nil {
			t.Fatal("background analysis accepted a deployment")
		}
	}
	if len(h.engine.statements()) != 0 {
		t.Fatal("invalid start touched storage")
	}
}
