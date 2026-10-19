package pipelinerun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

type repositoryRemote struct {
	*fakeGitHub
	writes, mints, prs int
	fail               bool
	beforeWrite        func()
	effects            map[string]string
}

func (g *repositoryRemote) RepositoryWriterToken(context.Context, string, string, string) (string, int64, error) {
	g.mints++
	return "writer-never-in-work", testInstallation, nil
}
func (g *repositoryRemote) ApplyBranchChange(ctx context.Context, token, repo string, e githubapp.BranchChange) (string, error) {
	if g.beforeWrite != nil {
		g.beforeWrite()
	}
	commit := strings.Repeat("f", 40)
	if _, err := e.Authorize(ctx, commit); err != nil {
		return "", err
	}
	if g.effects == nil {
		g.effects = map[string]string{}
	}
	if prior := g.effects[e.OperationID]; prior != "" {
		return prior, nil
	}
	g.writes++
	g.effects[e.OperationID] = commit
	if !e.Create {
		g.pulls[repo][0].HeadSHA = commit
		g.heads[repo+"@"+e.Branch] = headAnswer{SHA: commit, Message: e.Title + "\n\nMemQL-Operation: " + e.OperationID}
	}
	if g.fail {
		return "", errors.New("response lost")
	}
	return commit, nil
}
func (g *repositoryRemote) EnsureChangePullRequest(context.Context, string, string, string, string, string, string) (string, error) {
	g.prs++
	return "https://github.com/acme/shop/pull/7", nil
}

type repositoryHarness struct {
	i          *Integration
	h          *harness
	g          *repositoryRemote
	l          *sarifLibraryFixture
	run        Run
	engine     *recordingEngine
	rule       pipelines.RepositoryChangeRule
	config     string
	work, step map[string]any
}

func newRepositoryHarness(t *testing.T) *repositoryHarness {
	t.Helper()
	h, r, p, rule := repositoryOriginFixture(t)
	r.WorkRunID = id.NewShortId()
	r.Status = StatusInProgress
	r.QueuedAt = testNow
	h.store.addRun(r)
	proposal := pipelines.RepositoryProposal{Schema: 1, Commit: r.SHA, Key: "tidy", Title: "Tidy modules", Files: []pipelines.RepositoryReplacement{{Path: "go.sum", Content: "new sum\n"}}}
	raw, _ := json.Marshal(proposal)
	digest := sha256.Sum256(raw)
	intent := strings.Repeat("c", 64)
	l := &sarifLibraryFixture{t: t, body: raw, receipt: pipelinesteps.StoredFileReceipt{OwnerUserID: ownerID, WorkRunID: r.WorkRunID, StepKey: "proposal.generate", Attempt: 1, IntentID: intent, FileID: id.NewShortId(), Path: "proposal.json", ETag: "immutable-version", SHA256: hex.EncodeToString(digest[:]), Size: int64(len(raw))}}
	fp := "work-definition-v2:" + strings.Repeat("a", 64)
	work := map[string]any{"id": r.WorkRunID, "ownerUserId": ownerID, "status": "running", "mode": "live", "templateFingerprint": fp, "triggeredBy": "pipeline:affected", "input": map[string]any{"repository": r.Repository, "sha": r.SHA, "mode": "affected", "event": "pull_request", "pipelineId": p.ID, "pipelineRunId": r.ID, "attempt": 1}}
	step := map[string]any{"id": id.NewShortId(), "createdAt": "2026-10-16T10:00:00Z", "finishedAt": "2026-10-16T10:01:00Z", "ownerUserId": ownerID, "runId": r.WorkRunID, "key": "proposal.generate", "status": "done", "stepType": "exec", "kind": "deterministic", "attempt": 1, "call": map[string]any{"construct": "pipeline", "pipelineKind": "command", "definitionFingerprint": fp}, "result": map[string]any{"status": "succeeded", "metadata": map[string]any{"exitCode": 0, "artifactIntentIds": []string{intent}}}}
	engine := newRecordingEngine()
	engine.answers["workRunForOwner"] = []any{work}
	engine.answers["workStepForOwnerRun"] = []any{step}
	g := &repositoryRemote{fakeGitHub: h.github}
	g.heads[repoName+"@"+r.HeadBranch] = headAnswer{SHA: r.SHA}
	cfg, _ := json.Marshal(map[string]any{"schema": 1, "rules": []pipelines.RepositoryChangeRule{rule}})
	out := &repositoryHarness{h: h, g: g, l: l, run: r, engine: engine, rule: rule, config: string(cfg), work: work, step: step}
	d := h.integ.snapshot()
	d.GitHub = g
	d.SARIF = &SARIFDependencies{Engine: engine, Library: l}
	d.SystemVariable = func(context.Context, string) (string, error) { return out.config, nil }
	out.i = New(d)
	return out
}
func (h *repositoryHarness) apply(i *Integration) (map[string]any, error) {
	return i.repositoryChange(personCtx(ownerID), h.run.ID, pipelines.RepositoryChange{Rule: "tidy", Step: "proposal.generate", Path: "proposal.json"}, func() error { return nil })
}

func TestRepositoryChangeLostResponseRecoversOnAnotherReplica(t *testing.T) {
	h := newRepositoryHarness(t)
	h.g.fail = true
	if _, err := h.apply(h.i); err == nil {
		t.Fatal("unconfirmed write passed")
	}
	if h.l.releases.Load() != 0 {
		t.Fatal("uncertain write lost pinned evidence")
	}
	h.g.fail = false
	peer := New(h.i.snapshot())
	result, err := h.apply(peer)
	if err != nil || result["repositoryChangeState"] != "applied" || h.g.writes != 1 {
		t.Fatalf("recovery=%v err=%v writes=%d", result, err, h.g.writes)
	}
	if h.l.releases.Load() != 0 {
		t.Fatal("write released evidence before its durable work receipt")
	}
}

func TestRepositoryChangeRefusesForgedEvidenceAndRevokedAuthority(t *testing.T) {
	for _, failure := range []string{"wrong actor", "wrong repository", "wrong bytes", "wrong object version", "failed producer", "foreign owner", "cancelled", "changed rule", "moved branch"} {
		t.Run(failure, func(t *testing.T) {
			h := newRepositoryHarness(t)
			switch failure {
			case "wrong actor":
				h.rule.Actor = "stranger"
				cfg, _ := json.Marshal(map[string]any{"schema": 1, "rules": []pipelines.RepositoryChangeRule{h.rule}})
				h.config = string(cfg)
			case "wrong repository":
				h.rule.Repository = "acme/elsewhere"
				cfg, _ := json.Marshal(map[string]any{"schema": 1, "rules": []pipelines.RepositoryChangeRule{h.rule}})
				h.config = string(cfg)
			case "wrong bytes":
				h.l.body = append(h.l.body, ' ')
			case "wrong object version":
				h.l.onPin = func() { h.l.receipt.ETag = "changed" }
			case "failed producer":
				h.step["status"] = "failed"
			case "foreign owner":
				h.work["ownerUserId"] = "other"
			case "cancelled":
				h.run.CancelRequested = true
				h.h.store.addRun(h.run)
			case "changed rule":
				h.g.beforeWrite = func() { h.config = `{"schema":1,"rules":[]}` }
			case "moved branch":
				h.g.beforeWrite = func() { h.g.pulls[repoName][0].HeadSHA = shaB }
			}
			if _, err := h.apply(h.i); err == nil {
				t.Fatal("unsafe repository change accepted")
			}
			if h.g.writes != 0 {
				t.Fatal("unsafe write occurred")
			}
			if failure == "wrong actor" && h.g.mints != 0 {
				t.Fatal("writer minted before actor authorization")
			}
		})
	}
}

func TestRepositoryChangeEmptyProposalDoesNotMintWriter(t *testing.T) {
	h := newRepositoryHarness(t)
	proposal := pipelines.RepositoryProposal{Schema: 1, Commit: h.run.SHA, Key: "tidy", Title: "Nothing to change", Files: []pipelines.RepositoryReplacement{}}
	raw, _ := json.Marshal(proposal)
	digest := sha256.Sum256(raw)
	h.l.body = raw
	h.l.receipt.Size = int64(len(raw))
	h.l.receipt.SHA256 = hex.EncodeToString(digest[:])
	result, err := h.apply(h.i)
	if err != nil || result["repositoryChangeState"] != "unchanged" || h.g.mints != 0 || h.g.writes != 0 {
		t.Fatalf("result=%v err=%v mints=%d", result, err, h.g.mints)
	}
}

func TestRepositoryChangeDriverJournalsTheEffectWithoutGivingWorkAWriter(t *testing.T) {
	const manifest = `formatVersion: 1
name: shop
pipeline:
  image: toolchain
  stages:
    - name: analysis
      on: [manual]
      steps:
        - name: go
          run: prepare-proposal
          artifacts: [proposal.json]
    - name: apply
      on: [manual]
      steps:
        - name: change
          repositoryChange: {rule: tidy, step: analysis.go, path: proposal.json}
`
	for _, scenario := range []string{"success", "revoked", "lost journal"} {
		t.Run(scenario, func(t *testing.T) {
			revoked := scenario == "revoked"
			h := newRepositoryHarness(t)
			dh := newDriveHarness(t, manifest)
			h.g.fakeGitHub = dh.github
			h.rule.Mode = "propose"
			h.rule.Author = ""
			h.rule.Actor = ""
			h.rule.BranchPrefix = "maintenance/"
			cfg, _ := json.Marshal(map[string]any{"schema": 1, "rules": []pipelines.RepositoryChangeRule{h.rule}})
			h.config = string(cfg)
			h.l.receipt.StepKey = "analysis.go"
			h.g.repos[repoName] = githubapp.RepositoryInfo{FullName: repoName, DefaultBranch: "main"}
			h.g.heads[repoName+"@main"] = headAnswer{SHA: shaA}
			if revoked {
				h.g.beforeWrite = func() { h.config = `{"schema":1,"rules":[]}` }
			}
			if scenario == "lost journal" {
				h.g.beforeWrite = func() {
					dh.work.mu.Lock()
					dh.work.refuseCall, dh.work.refuseErr = "updateWorkStep", errors.New("journal unavailable after Git write")
					dh.work.mu.Unlock()
				}
			}
			dh.integ.Configure(func(d *Deps) {
				d.GitHub = h.g
				d.SystemVariable = h.i.snapshot().SystemVariable
				d.SARIF = &SARIFDependencies{Engine: reportJournalReader{h: &sarifHarness{owner: dh.p.OwnerUserID, library: h.l}, work: dh.work}, Library: h.l}
			})
			dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
				if req.StepKey != "analysis.go" || len(req.Secrets) != 0 {
					t.Error("writer leaked into work", req.StepKey)
				}
				h.l.receipt.WorkRunID = req.WorkRunID
				r := passed(req)
				r.ArtifactIntentIDs = []string{h.l.receipt.IntentID}
				return r, nil
			}
			opening := pushOpening()
			opening.Event = pipelines.EventManual
			opening.Branch = "main"
			run := dh.openRun(t, opening)
			deliver(t, dh.integ, run)
			got, _ := dh.store.run(run.ID)
			if scenario == "lost journal" {
				if got.Status == StatusCompleted || h.l.releases.Load() != 0 || h.g.writes != 1 {
					t.Fatal("unrecorded external write lost recoverability", got.Status, h.l.releases.Load(), h.g.writes)
				}
				h.g.beforeWrite = nil
				dh.work.mu.Lock()
				dh.work.refuseCall, dh.work.refuseErr = "", nil
				dh.work.mu.Unlock()
				peer := New(dh.integ.snapshot())
				dh.configureDriver(peer, "agent-b")
				peer.Configure(func(d *Deps) { d.Now = func() time.Time { return testNow.Add(3 * time.Minute) } })
				if err := peer.RecoverRuns(context.Background()); err != nil {
					t.Fatal(err)
				}
				waitDrives(t, peer)
				got, _ = dh.store.run(run.ID)
			}
			want := ConclusionSuccess
			if revoked {
				want = ConclusionFailure
			}
			if got.Conclusion != want {
				t.Fatalf("conclusion=%s want=%s receipts=%+v\n%s", got.Conclusion, want, dh.work.receiptsOf("apply.change"), dh.logs.String())
			}
			if len(dh.exec.sentKeys()) != 1 {
				t.Fatal("repository effect reached executor")
			}
			receipts := dh.work.receiptsOf("apply.change")
			if len(receipts) != 1 {
				t.Fatal("effect has no durable receipt", len(receipts))
			}
			if !revoked && h.l.releases.Load() != 1 {
				t.Fatal("durably completed change retained its evidence pin")
			}
			if !revoked && (h.g.writes != 1 || h.g.prs < 1) {
				t.Fatal("proposal was not written and opened")
			}
			if revoked && h.g.writes != 0 {
				t.Fatal("revoked rule wrote a branch")
			}
		})
	}
}

func TestRepositoryChangeOwnerRetryKeepsTheUncertainOriginalEffect(t *testing.T) {
	h := newRepositoryHarness(t)
	h.g.fail = true
	if _, err := h.apply(h.i); err == nil {
		t.Fatal("lost response passed")
	}
	original := h.run
	h.run.ID = id.NewShortId()
	h.run.RerunOf = original.ID
	h.run.QueuedAt = original.QueuedAt.Add(time.Hour)
	h.h.store.addRun(h.run)
	h.work["input"].(map[string]any)["pipelineRunId"] = h.run.ID
	h.g.fail = false
	result, err := h.apply(New(h.i.snapshot()))
	if err != nil || result["repositoryChangeState"] != "applied" || h.g.writes != 1 {
		t.Fatalf("retry duplicated or lost original effect: %v %v writes=%d", result, err, h.g.writes)
	}
	h.run.RerunOf = h.run.ID
	h.h.store.addRun(h.run)
	if _, err := h.apply(h.i); err == nil {
		t.Fatal("cyclic retry lineage accepted")
	}
}
