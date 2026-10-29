package pipelinerun

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
)

func (g *fakeGitHub) QualificationCheckAuthority(ctx context.Context, credential, owner, repository string) (string, int64, error) {
	token, _, err := g.InstallationToken(ctx, credential, owner, repository)
	g.mu.Lock()
	app := g.qualificationAppID
	g.mu.Unlock()
	if app == 0 {
		app = 42
	}
	return token, app, err
}

func (g *fakeGitHub) FindQualificationCheck(ctx context.Context, token, repository, head, name, external string, appID int64) (int64, error) {
	watchGitHubCall(g.t, ctx, "FindQualificationCheck")
	g.mu.Lock()
	hook := g.afterQualificationObservation
	g.mu.Unlock()
	if hook != nil {
		hook()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.qualificationObserveErr != nil {
		return 0, g.qualificationObserveErr
	}
	if g.qualificationMissingCheck {
		return 0, nil
	}
	app := g.qualificationAppID
	if app == 0 {
		app = 42
	}
	if app != appID {
		return 0, errors.New("provider App differs")
	}
	var found int64
	for _, check := range g.created {
		if check.Repository == repository && check.Run.HeadSHA == head && check.Run.Name == name && check.Run.ExternalID == external {
			if found != 0 {
				return 0, errors.New("ambiguous provider checks")
			}
			found = check.ID
		}
	}
	return found, nil
}

func qualificationPR() githubapp.PullRequestHead {
	return githubapp.PullRequestHead{Number: 42, State: "open", Title: "Add native checks", HeadSHA: shaA, HeadRef: "native-checks", HeadRepository: repoName, BaseSHA: shaBase, BaseRef: "main", BaseRepository: repoName}
}

func prepareQualification(h *harness) {
	h.store.addPackage(PackageSource{ID: packageID, OwnerUserID: "v1:identity:user:" + ownerID, AccountID: "acct-1", Name: "shop", SourceKind: "repo", RepoURL: "https://github.com/" + repoName, CredentialID: credID, Status: "active"})
	h.github.repos[repoName] = githubapp.RepositoryInfo{ID: 100, FullName: repoName, DefaultBranch: "main"}
	h.github.pulls[repoName] = []githubapp.PullRequestHead{qualificationPR()}
}

func qualificationRequest(t *testing.T, h *harness) QualifyPullRequestRequest {
	t.Helper()
	preview, err := h.integ.PreviewPullRequest(personCtx(ownerID), packageID, 42)
	if err != nil || preview.Refusal != nil {
		t.Fatalf("preview: %+v refusal=%+v %v", preview, preview.Refusal, err)
	}
	return QualifyPullRequestRequest{PackageID: packageID, RequestID: "reviewed-request-1", PullRequest: 42, ExpectedHeadSHA: preview.SHA, ExpectedBaseSHA: preview.BaseSHA, ExpectedManifestDigest: preview.ManifestDigest, SecretNames: []string{"SHOP_TOKEN"}, ExpiresAt: testNow.Add(time.Hour)}
}

func TestQualificationPreviewReadsPRWithoutDefaultManifestOrWrites(t *testing.T) {
	h := connectHarness(t, "")
	prepareQualification(h)
	h.github.trees[repoName+"@"+shaB] = fstest.MapFS{pipelines.ManifestPath: &fstest.MapFile{Data: []byte(connectManifest)}}
	pr := qualificationPR()
	pr.HeadSHA = shaB
	h.github.pulls[repoName] = []githubapp.PullRequestHead{pr}
	preview, err := h.integ.PreviewPullRequest(personCtx(ownerID), packageID, 42)
	if err != nil || preview.Refusal != nil || preview.SHA != shaB || preview.BaseSHA != shaBase || len(preview.ManifestDigest) != 64 || preview.CheckName != "MemQL / shop" {
		t.Fatalf("preview: %+v refusal=%+v %v", preview, preview.Refusal, err)
	}
	if len(h.store.pipelineCreates)+len(h.store.runCreates)+len(h.github.created) != 0 {
		t.Fatal("preview wrote state")
	}
}

func TestQualificationOpensRealFullPRRunAndDeduplicatesExactRequest(t *testing.T) {
	h := connectHarness(t, connectManifest)
	prepareQualification(h)
	req := qualificationRequest(t, h)
	r, err := h.integ.QualifyPullRequest(personCtx(ownerID), req)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := h.store.pipeline(r.PipelineID)
	if p.Active() || p.Status != PipelineQualification || r.Event != pipelines.EventPullRequest || r.Mode != pipelines.ModeFull || r.CheckRunID <= 0 {
		t.Fatalf("pipeline/run = %+v %+v", p, r)
	}
	if err := ValidateQualification(p, r, testNow); err != nil {
		t.Fatal(err)
	}
	check := h.github.created[0].Run
	if check.Name != "MemQL / shop" || check.ExternalID != r.ID || check.HeadSHA != shaA || check.Status != "queued" {
		t.Fatalf("real check identity: %+v", check)
	}
	if len(h.store.runCreates) != 1 || h.store.runCreates[0].Qualification == nil || h.store.runCreates[0].CheckRunID != 0 {
		t.Fatal("intent was not durable before check creation")
	}
	again, err := h.integ.QualifyPullRequest(personCtx(ownerID), req)
	if err != nil || again.ID != r.ID || len(h.github.created) != 1 {
		t.Fatalf("retry duplicated request: %+v %v", again, err)
	}
	req.ExpiresAt = req.ExpiresAt.Add(time.Minute)
	if _, err := h.integ.QualifyPullRequest(personCtx(ownerID), req); err == nil {
		t.Fatal("same requestId accepted altered scope")
	}
}

func TestQualificationRefusesBeforeStateOrCheckWrites(t *testing.T) {
	for name, mutate := range map[string]func(*harness, *QualifyPullRequestRequest){
		"stale head":          func(h *harness, r *QualifyPullRequestRequest) { r.ExpectedHeadSHA = shaB },
		"stale base":          func(h *harness, r *QualifyPullRequestRequest) { r.ExpectedBaseSHA = shaB },
		"stale manifest":      func(h *harness, r *QualifyPullRequestRequest) { r.ExpectedManifestDigest = strings.Repeat("0", 64) },
		"expired":             func(h *harness, r *QualifyPullRequestRequest) { r.ExpiresAt = testNow },
		"no request identity": func(h *harness, r *QualifyPullRequestRequest) { r.RequestID = "" },
		"secret not allowed":  func(h *harness, r *QualifyPullRequestRequest) { r.SecretNames = nil },
		"fork": func(h *harness, r *QualifyPullRequestRequest) {
			h.github.pulls[repoName][0].HeadRepository = "other/shop"
		},
		"closed PR":     func(h *harness, r *QualifyPullRequestRequest) { h.github.pulls[repoName][0].State = "closed" },
		"retargeted PR": func(h *harness, r *QualifyPullRequestRequest) { h.github.pulls[repoName][0].BaseRef = "other" },
		"foreign base": func(h *harness, r *QualifyPullRequestRequest) {
			h.github.pulls[repoName][0].BaseRepository = "other/shop"
		},
		"revoked grant": func(h *harness, r *QualifyPullRequestRequest) { h.github.tokenErr = errors.New("grant revoked") },
		"archived source": func(h *harness, r *QualifyPullRequestRequest) {
			p := h.store.packages[packageID]
			p.Status = "archived"
			h.store.addPackage(p)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := connectHarness(t, connectManifest)
			prepareQualification(h)
			req := qualificationRequest(t, h)
			mutate(h, &req)
			if _, err := h.integ.QualifyPullRequest(personCtx(ownerID), req); err == nil {
				t.Fatal("refusal missing")
			}
			if len(h.store.pipelineCreates)+len(h.store.runCreates)+len(h.github.created) != 0 {
				t.Fatal("refused request wrote state")
			}
		})
	}
}

func TestQualificationCannotReplaceOrdinaryPipelineOrBorrowOwner(t *testing.T) {
	for _, status := range []string{PipelineActive, PipelineDisconnected, PipelineQualification} {
		t.Run(status, func(t *testing.T) {
			h := connectHarness(t, connectManifest)
			prepareQualification(h)
			req := qualificationRequest(t, h)
			p := testPipeline(DeliveryWebhook)
			p.ID = PipelineIDFor(packageID)
			p.PackageID = packageID
			p.Status = status
			if status == PipelineQualification {
				p.PackageID = "another-source"
				p.ID = PipelineIDFor(p.PackageID)
			}
			h.store.addPipeline(p)
			if _, err := h.integ.QualifyPullRequest(personCtx(ownerID), req); err == nil {
				t.Fatal("existing authority replaced")
			}
			if len(h.store.pipelineCreates)+len(h.github.created) != 0 {
				t.Fatal("refusal wrote")
			}
		})
	}
	h := connectHarness(t, connectManifest)
	prepareQualification(h)
	req := qualificationRequest(t, h)
	if _, err := h.integ.QualifyPullRequest(clusterOwnerCtx(otherID), req); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("foreign cluster owner: %v", err)
	}
}

func TestQualificationCannotOpenOrdinaryRunsOrBorrowTrainAdmission(t *testing.T) {
	h := connectHarness(t, connectManifest)
	prepareQualification(h)
	r, err := h.integ.QualifyPullRequest(personCtx(ownerID), qualificationRequest(t, h))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.integ.RunLatest(personCtx(ownerID), r.PipelineID, ""); err == nil {
		t.Fatal("manual widened scope")
	}
	if _, err := h.integ.RunRelease(personCtx(ownerID), r.PipelineID, "v1.0.0"); err == nil {
		t.Fatal("release widened scope")
	}
	if _, err := h.integ.Rerun(personCtx(ownerID), r.ID, false); err == nil {
		t.Fatal("rerun inherited authority")
	}
	r.HeadBranch = "train/now"
	if IsTrainRun(r) {
		t.Fatal("qualification borrowed train-only admission")
	}
}

func TestQualificationNewRequestWaitsForTerminalAndCapturesNewAttempt(t *testing.T) {
	h := connectHarness(t, connectManifest)
	prepareQualification(h)
	req := qualificationRequest(t, h)
	r, err := h.integ.QualifyPullRequest(personCtx(ownerID), req)
	if err != nil {
		t.Fatal(err)
	}
	req.RequestID = "reviewed-request-2"
	if _, err := h.integ.QualifyPullRequest(personCtx(ownerID), req); !errors.Is(err, ErrRunInProgress) {
		t.Fatalf("parallel attempt: %v", err)
	}
	r.Status, r.Conclusion = StatusCompleted, ConclusionCancelled
	h.store.addRun(r)
	next, err := h.integ.QualifyPullRequest(personCtx(ownerID), req)
	if err != nil || next.ID == r.ID || next.Attempt != 2 || next.RerunOf != "" || next.RerunFailedOnly {
		t.Fatalf("fresh authority: %+v %v", next, err)
	}
}

func TestQualificationConcurrentSameRequestCreatesOneIntentAndCheck(t *testing.T) {
	h := connectHarness(t, connectManifest)
	prepareQualification(h)
	req := qualificationRequest(t, h)
	var wg sync.WaitGroup
	answers := make(chan Run, 6)
	errs := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := h.integ.QualifyPullRequest(personCtx(ownerID), req)
			answers <- r
			errs <- e
		}()
	}
	wg.Wait()
	close(answers)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var expected string
	for r := range answers {
		if expected == "" {
			expected = r.ID
		}
		if r.ID != expected {
			t.Fatal("different retry IDs")
		}
	}
	if len(h.store.runCreates) != 1 || len(h.github.created) != 1 {
		t.Fatal("duplicate effect")
	}
}

func newQualificationDrive(t *testing.T) (*driveHarness, Run) {
	t.Helper()
	return qualificationDriveManifest(t, driveManifest)
}

func qualificationDriveManifest(t *testing.T, manifest string) (*driveHarness, Run) {
	t.Helper()
	dh := newDriveHarness(t, manifest)
	dh.store.mu.Lock()
	delete(dh.store.pipelines, dh.p.ID)
	dh.store.mu.Unlock()
	prepareQualification(dh.harness)
	r, err := dh.integ.QualifyPullRequest(personCtx(ownerID), qualificationRequest(t, dh.harness))
	if err != nil {
		t.Fatal(err)
	}
	dh.p, _ = dh.store.pipeline(r.PipelineID)
	return dh, r
}

func TestQualificationExecutesFullRealPlanOnReplacementReplica(t *testing.T) {
	dh, r := newQualificationDrive(t)
	peer := dh.peer("qualification-replacement")
	deliver(t, peer, r)
	got, _ := dh.store.run(r.ID)
	if got.Conclusion != ConclusionSuccess || len(dh.exec.sent()) != 3 {
		t.Fatalf("real execution: %+v sent=%v logs=%s", got, dh.exec.sentKeys(), dh.logs.String())
	}
	if !reflect.DeepEqual(got.Qualification, r.Qualification) {
		t.Fatal("recovery rewrote approved authority")
	}
	if p, _ := dh.store.pipeline(r.PipelineID); p.Active() {
		t.Fatal("execution enabled automatic delivery")
	}
}

func TestQualificationDriverRefusesChangedAuthorityBeforeDispatch(t *testing.T) {
	for name, mutate := range map[string]func(*driveHarness, Run){
		"head":         func(dh *driveHarness, r Run) { dh.github.pulls[repoName][0].HeadSHA = shaB },
		"base":         func(dh *driveHarness, r Run) { dh.github.pulls[repoName][0].BaseSHA = shaB },
		"closed":       func(dh *driveHarness, r Run) { dh.github.pulls[repoName][0].State = "closed" },
		"installation": func(dh *driveHarness, r Run) { dh.github.installationID = 8 },
		"App":          func(dh *driveHarness, r Run) { dh.github.qualificationAppID = 43 },
		"recreated repository": func(dh *driveHarness, r Run) {
			info := dh.github.repos[repoName]
			info.ID++
			dh.github.repos[repoName] = info
		},
		"run account": func(dh *driveHarness, r Run) { r.AccountID = "other-account"; dh.store.addRun(r) },
		"revoked": func(dh *driveHarness, r Run) {
			p, _ := dh.store.pipeline(r.PipelineID)
			p.Status = PipelineDisconnected
			dh.store.addPipeline(p)
		},
		"source owner": func(dh *driveHarness, r Run) {
			p := dh.store.packages[packageID]
			p.OwnerUserID = otherID
			dh.store.addPackage(p)
		},
		"manifest": func(dh *driveHarness, r Run) { dh.setTree(shaA, strings.Replace(driveManifest, "go vet", "go fmt", 1)) },
	} {
		t.Run(name, func(t *testing.T) {
			dh, r := newQualificationDrive(t)
			mutate(dh, r)
			deliver(t, dh.integ, r)
			got, _ := dh.store.run(r.ID)
			if got.Conclusion != ConclusionFailure || len(dh.exec.sent()) != 0 {
				t.Fatalf("scope did not fail closed: %+v sent=%d", got, len(dh.exec.sent()))
			}
		})
	}
}

func TestQualificationUncertainCreateObservesWithoutSecondPost(t *testing.T) {
	h := connectHarness(t, connectManifest)
	prepareQualification(h)
	h.github.qualificationCreateLosesResponse = true
	req := qualificationRequest(t, h)
	r, err := h.integ.QualifyPullRequest(personCtx(ownerID), req)
	if err != nil {
		t.Fatal(err)
	}
	if r.CheckRunID != 0 || len(h.github.created) != 1 {
		t.Fatal("fixture did not lose successful response")
	}
	p, _ := h.store.pipeline(r.PipelineID)
	w := publishCheckRun(context.Background(), h.integ.snapshot(), p, r, ReportFor(p, r, nil))
	if w.Err != nil || w.ID <= 0 || len(h.github.created) != 1 {
		t.Fatalf("did not reconcile only: %+v", w)
	}
	h.github.qualificationMissingCheck = true
	w = publishCheckRun(context.Background(), h.integ.snapshot(), p, r, ReportFor(p, r, nil))
	if w.Err == nil || len(h.github.created) != 1 {
		t.Fatal("missing receipt repeated ambiguous POST")
	}
}

func TestQualificationAbsentReceiptCannotDispatchOrReportSuccess(t *testing.T) {
	dh, r := newQualificationDrive(t)
	dh.github.qualificationMissingCheck = true
	deliver(t, dh.integ, r)
	got, _ := dh.store.run(r.ID)
	if got.Conclusion == ConclusionSuccess || len(dh.exec.sent()) != 0 {
		t.Fatal("absent receipt was qualified")
	}
	if len(dh.github.created) != 1 {
		t.Fatal("absent receipt caused another POST")
	}
}

func TestQualificationNoProviderWriteWhenIntentCannotPersist(t *testing.T) {
	h := connectHarness(t, connectManifest)
	prepareQualification(h)
	h.store.failCreateRun = errors.New("storage unavailable")
	_, err := h.integ.QualifyPullRequest(personCtx(ownerID), qualificationRequest(t, h))
	if err == nil || len(h.github.created) != 0 {
		t.Fatal("external effect preceded durable authority")
	}
}

// Qualification must observe changes after inspection but before its one POST.
func TestQualificationRefreshesProviderBeforeInitialIntent(t *testing.T) {
	h := connectHarness(t, connectManifest)
	prepareQualification(h)
	req := qualificationRequest(t, h)
	h.store.afterCreatePipeline = func() { h.github.pulls[repoName][0].HeadSHA = shaB }
	if _, err := h.integ.QualifyPullRequest(personCtx(ownerID), req); err == nil {
		t.Fatal("stale inspected head opened")
	}
	if len(h.store.runCreates) != 0 || len(h.github.created) != 0 {
		t.Fatal("stale provider observation created intent/check")
	}
}

func TestQualificationCancelDuringInitialPostReconcilesReceipt(t *testing.T) {
	h := connectHarness(t, connectManifest)
	prepareQualification(h)
	h.github.beforeCreateCheck = func(check githubapp.CheckRun) {
		if _, err := h.integ.requestCancel(context.Background(), h.integ.snapshot(), check.ExternalID, ownerID); err != nil {
			t.Fatal(err)
		}
	}
	r, err := h.integ.QualifyPullRequest(personCtx(ownerID), qualificationRequest(t, h))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := h.store.run(r.ID)
	if got.Conclusion != ConclusionCancelled || got.CheckRunID <= 0 || len(h.github.created) != 1 {
		t.Fatalf("cancel/receipt lost: %+v", got)
	}
	if len(h.github.updated) == 0 {
		t.Fatal("late queued POST was not reconciled")
	}
	last := h.github.updated[len(h.github.updated)-1].Run
	if last.Status != StatusCompleted || last.Conclusion != ConclusionCancelled {
		t.Fatalf("late initial report overwrote cancellation: %+v", last)
	}
}

func TestQualificationRechecksAfterSecretResolution(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "head moved", true: "expired"}[expired], func(t *testing.T) {
			dh, r := newQualificationDrive(t)
			clock := &driveClock{at: testNow}
			dh.integ.Configure(func(d *Deps) {
				d.Now = clock.now
				inner := d.Secrets
				d.Secrets = func(ctx context.Context, name string) (string, error) {
					if expired {
						clock.set(testNow.Add(2 * time.Hour))
					} else {
						dh.github.mu.Lock()
						dh.github.pulls[repoName][0].HeadSHA = shaB
						dh.github.mu.Unlock()
					}
					return inner(ctx, name)
				}
			})
			deliver(t, dh.integ, r)
			got, _ := dh.store.run(r.ID)
			if got.Conclusion == ConclusionSuccess {
				t.Fatal("stale qualification succeeded")
			}
			for _, req := range dh.exec.sent() {
				if req.StepKey == "tests.unit" {
					t.Fatal("step dispatched after secret resolution changed authority")
				}
			}
		})
	}
}

func TestQualificationCannotSucceedWithEmptyOrSkippedOnlyPlan(t *testing.T) {
	manifests := map[string]string{
		"empty": strings.ReplaceAll(driveManifest, "      steps:", "      on: [release]\n      steps:"),
		"skipped": `formatVersion: 1
name: shop
pipeline:
  select: { go: import-graph }
  image: ghcr.io/acme/toolchain@sha256:abc
  stages:
    - name: checks
      steps:
        - name: unit
          run: go test $MEMQL_PACKAGES
          packages: all
          packageRoots: [not-present]
`,
	}
	for name, manifest := range manifests {
		t.Run(name, func(t *testing.T) {
			dh, r := qualificationDriveManifest(t, manifest)
			deliver(t, dh.integ, r)
			got, _ := dh.store.run(r.ID)
			if got.Conclusion != ConclusionFailure || len(dh.exec.sent()) != 0 {
				t.Fatalf("unexecuted plan qualified: %+v calls=%v logs=%s", got, dh.exec.sentKeys(), dh.logs.String())
			}
		})
	}
}

func TestQualificationDoesNotAnswerOrdinaryDeliveryAfterConnect(t *testing.T) {
	h := connectHarness(t, connectManifest)
	prepareQualification(h)
	r, err := h.integ.QualifyPullRequest(personCtx(ownerID), qualificationRequest(t, h))
	if err != nil {
		t.Fatal(err)
	}
	r.Status, r.Conclusion = StatusCompleted, ConclusionSuccess
	h.store.addRun(r)
	if _, err := connect(h, personCtx(ownerID), ConnectRequest{}); err != nil {
		t.Fatal(err)
	}
	p, _ := h.store.pipeline(r.PipelineID)
	// A different policy version also proves the ordinary opening captured
	// installed policy instead of adopting the qualification's saved closure.
	aWorkflow := workflowSource(t, `@template automation pipelineVersionForEvent { args { eventName string! sha string! tag string! } return "new-installed-version" }`)
	h.integ.Configure(func(d *Deps) { bindTestWorkflow(d, aWorkflow) })
	next, err := h.integ.open(context.Background(), h.integ.snapshot(), p, prOpening())
	if err != nil || !next.Opened || next.Run.ID == r.ID || next.Run.Qualification != nil || next.Run.Mode != pipelines.ModeAffected {
		t.Fatalf("qualification inherited: %+v %v", next, err)
	}
	a, _ := readPipelinePolicy(r.PolicySnapshot)
	b, _ := readPipelinePolicy(next.Run.PolicySnapshot)
	if a.Version == b.Version {
		t.Fatal("ordinary opening reused old policy")
	}
	// Same-key full ordinary delivery still must not be answered by qualifier.
	o := prOpening()
	o.Mode = pipelines.ModeFull
	o.Trigger = TriggerRerun
	o.DeliveryID = "ordinary-retry"
	retry, err := h.integ.open(context.Background(), h.integ.snapshot(), p, o)
	if err != nil || !retry.Opened || retry.Run.Qualification != nil || retry.Run.Attempt != 2 {
		t.Fatalf("full ordinary attempt: %+v %v", retry, err)
	}
}

func TestQualificationFinalRecoveryCannotPromoteStaleSuccess(t *testing.T) {
	for name, mutate := range map[string]func(*driveHarness){
		"head": func(dh *driveHarness) { dh.github.pulls[repoName][0].HeadSHA = shaB },
		"expired": func(dh *driveHarness) {
			dh.integ.Configure(func(d *Deps) { d.Now = func() time.Time { return testNow.Add(2 * time.Hour) } })
		},
		"revoked source": func(dh *driveHarness) {
			p := dh.store.packages[packageID]
			p.Status = "archived"
			dh.store.addPackage(p)
		},
		"App": func(dh *driveHarness) { dh.github.qualificationAppID = 999 },
	} {
		t.Run(name, func(t *testing.T) {
			dh, r := newQualificationDrive(t)
			r.Status, r.Conclusion, r.CheckRunState, r.FinishedAt = StatusCompleted, ConclusionSuccess, CheckRunUnavailable, testNow
			dh.store.addRun(r)
			mutate(dh)
			if err := dh.integ.republishFinalCheckRuns(context.Background(), dh.integ.snapshot()); err != nil {
				t.Fatal(err)
			}
			if len(dh.github.updated) != 0 {
				t.Fatal("recovery published stale success")
			}
			got, _ := dh.store.run(r.ID)
			if got.CheckRunState != CheckRunUnavailable {
				t.Fatal("unpublished final marked written")
			}
		})
	}
}

func TestQualificationArtifactAuthorityDoesNotOutliveApproval(t *testing.T) {
	dh, r := newQualificationDrive(t)
	r.Status, r.Conclusion, r.WorkRunID = StatusCompleted, ConclusionSuccess, "work-qualification"
	dh.store.addRun(r)
	p, _ := dh.store.pipeline(r.PipelineID)
	if _, err := sarifGrant(personCtx(ownerID), dh.integ.snapshot(), ownerID, r, p); err != nil {
		t.Fatal(err)
	}
	dh.github.pulls[repoName][0].HeadSHA = shaB
	if _, err := sarifGrant(personCtx(ownerID), dh.integ.snapshot(), ownerID, r, p); err == nil {
		t.Fatal("artifact publication accepted moved head")
	}
	p.Status = PipelineDisconnected
	dh.store.addPipeline(p)
	if _, _, err := sarifSource(personCtx(ownerID), dh.integ.snapshot(), ownerID, r.ID); err == nil {
		t.Fatal("artifact publication accepted disconnected authority")
	}
}

func TestQualificationRechecksLocalAuthorityAfterProviderObservation(t *testing.T) {
	for _, source := range []bool{false, true} {
		t.Run(map[bool]string{false: "pipeline revoked", true: "source revoked"}[source], func(t *testing.T) {
			dh, r := newQualificationDrive(t)
			dh.github.afterQualificationObservation = func() {
				if source {
					dh.store.mu.Lock()
					p := dh.store.packages[packageID]
					p.Status = "archived"
					dh.store.packages[packageID] = p
					dh.store.mu.Unlock()
				} else {
					p, _ := dh.store.pipeline(r.PipelineID)
					p.Status = PipelineDisconnected
					dh.store.addPipeline(p)
				}
			}
			deliver(t, dh.integ, r)
			got, _ := dh.store.run(r.ID)
			if got.Conclusion != ConclusionFailure || len(dh.exec.sent()) != 0 {
				t.Fatal("provider wait concealed local revocation")
			}
		})
	}
}

func TestQualificationNewHeadWaitsForPriorAttemptToFinish(t *testing.T) {
	h := connectHarness(t, connectManifest)
	prepareQualification(h)
	req := qualificationRequest(t, h)
	if _, err := h.integ.QualifyPullRequest(personCtx(ownerID), req); err != nil {
		t.Fatal(err)
	}
	h.github.pulls[repoName][0].HeadSHA = shaB
	h.github.trees[repoName+"@"+shaB] = h.github.trees[repoName+"@"+shaA]
	next := qualificationRequest(t, h)
	next.RequestID = "new-head"
	if _, err := h.integ.QualifyPullRequest(personCtx(ownerID), next); !errors.Is(err, ErrRunInProgress) {
		t.Fatalf("parallel authority: %v", err)
	}
	if len(h.github.created) != 1 {
		t.Fatal("parallel request wrote another check")
	}
}

func TestQualificationDisconnectPreservesRevocationUntilNormalConnect(t *testing.T) {
	h := connectHarness(t, connectManifest)
	prepareQualification(h)
	req := qualificationRequest(t, h)
	r, err := h.integ.QualifyPullRequest(personCtx(ownerID), req)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		p, err := h.integ.Disconnect(personCtx(ownerID), r.PipelineID)
		if err != nil || p.Status != PipelineQualificationRevoked {
			t.Fatalf("revocation lost: %+v %v", p, err)
		}
		if _, err := h.integ.RunLatest(personCtx(ownerID), p.ID, ""); err == nil {
			t.Fatal("revoked qualification became a manual grant")
		}
		if _, err := h.integ.RunRelease(personCtx(ownerID), p.ID, "v1.0.0"); err == nil {
			t.Fatal("revoked qualification became a release grant")
		}
	}
	if _, err := h.integ.QualifyPullRequest(personCtx(ownerID), req); err == nil {
		t.Fatal("old request recovered revoked authority")
	}
	req.RequestID = "new-after-revocation"
	if _, err := h.integ.QualifyPullRequest(personCtx(ownerID), req); err == nil {
		t.Fatal("revoked source reopened without default manifest connection")
	}
	if _, err := connect(h, personCtx(ownerID), ConnectRequest{}); err != nil {
		t.Fatal(err)
	}
	p, _ := h.store.pipeline(r.PipelineID)
	if !p.Active() {
		t.Fatal("ordinary default-branch Connect did not activate")
	}
	if _, err := h.integ.RunLatest(personCtx(ownerID), p.ID, ""); err != nil {
		t.Fatal(err)
	}
}

func TestQualificationCannotBorrowTrainAdmissionAtClaim(t *testing.T) {
	dh, r := newQualificationDrive(t)
	// The real claim sees the qualification trigger even if its branch looks
	// like a train. No source permission or admission setting is changed.
	r.HeadBranch = "train/fixture"
	q, _ := readQualification(r.Qualification)
	q.HeadBranch = r.HeadBranch
	q.RequestDigest = q.digest()
	r.Qualification = q.object()
	dh.store.addRun(r)
	dh.integ.Configure(func(d *Deps) { d.AdmitRun = func(_ time.Time, train bool) (bool, error) { return train, nil } })
	deliver(t, dh.integ, r)
	got, _ := dh.store.run(r.ID)
	if got.Status != StatusQueued || len(dh.exec.sent()) != 0 {
		t.Fatal("qualification escaped closed general admission")
	}
}

func TestQualificationDisconnectRechecksOwnerInsideGate(t *testing.T) {
	h := connectHarness(t, connectManifest)
	prepareQualification(h)
	r, err := h.integ.QualifyPullRequest(personCtx(ownerID), qualificationRequest(t, h))
	if err != nil {
		t.Fatal(err)
	}
	h.integ.Configure(func(d *Deps) {
		gate := d.Gate
		d.Gate = func(ctx context.Context, key string, fn func(context.Context) error) error {
			if key == RepositoryGateKey(repoName) {
				p, _ := h.store.pipeline(r.PipelineID)
				p.OwnerUserID = otherID
				h.store.addPipeline(p)
			}
			return gate(ctx, key, fn)
		}
	})
	if _, err := h.integ.Disconnect(personCtx(ownerID), r.PipelineID); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("former owner changed source: %v", err)
	}
	p, _ := h.store.pipeline(r.PipelineID)
	if p.Status != PipelineQualification {
		t.Fatal("former owner revoked new owner's pipeline")
	}
}
