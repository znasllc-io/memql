package pipelinerun

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
)

const independentReviewPolicy = `@template
 automation pipelineIndependentReviewPolicy {
 args { repository string! pipelineId string! ownerUserId string! }
 return "reviewExactPullRequest"
 }`

type independentReviewFixture struct {
	model, reports, observes           int
	reject, uncertain                  bool
	captureHook, modelHook, reportHook func()
	validateErr                        error
}

func (f *independentReviewFixture) dependencies() *IndependentReviewDependencies {
	return &IndependentReviewDependencies{
		Capture: func(ctx context.Context, p Pipeline, r Run, prompt, policy string, fence func(context.Context) error) (IndependentReviewInput, error) {
			if e := fence(ctx); e != nil {
				return IndependentReviewInput{}, e
			}
			if f.captureHook != nil {
				f.captureHook()
			}
			return IndependentReviewInput{Kind: "ordinary", Source: ReviewSource{OwnerUserID: p.OwnerUserID, PipelineID: p.ID, SourceID: p.PackageID, CredentialID: p.CredentialID, Repository: p.Repository, Branch: p.DefaultBranch, InstallationID: p.InstallationID, RepositoryID: 21, AppID: 42}, PolicyVersion: policy, PromptName: prompt, Prompt: map[string]any{"name": prompt}, PullRequest: &githubapp.PullRequestObservation{Number: r.PullRequest, State: "open", HeadSHA: r.SHA, HeadRef: r.HeadBranch, HeadRepository: p.Repository, BaseSHA: r.BaseSHA, BaseRef: p.DefaultBranch, BaseRepository: p.Repository}, Diff: &githubapp.ReviewDiff{BeforeSHA: r.BaseSHA, AfterSHA: r.SHA}}, nil
		},
		Validate: func(ctx context.Context, _ Pipeline, _ Run, _ IndependentReviewInput, fence func(context.Context) error) error {
			if e := fence(ctx); e != nil {
				return e
			}
			return f.validateErr
		},
		Review: func(context.Context, IndependentReviewInput) (IndependentReviewVerdict, error) {
			f.model++
			if f.modelHook != nil {
				f.modelHook()
			}
			return IndependentReviewVerdict{Approved: !f.reject, Reason: "Reviewed complete exact source."}, nil
		},
		Report: func(ctx context.Context, in IndependentReviewInput, external string, observe bool, fence func(context.Context) error) (IndependentReviewReceipt, error) {
			if e := fence(ctx); e != nil {
				return IndependentReviewReceipt{}, e
			}
			if f.reportHook != nil {
				f.reportHook()
			}
			if observe {
				f.observes++
			} else {
				f.reports++
			}
			if f.uncertain {
				return IndependentReviewReceipt{}, errors.New("provider response unavailable")
			}
			if e := fence(ctx); e != nil {
				return IndependentReviewReceipt{}, e
			}
			return IndependentReviewReceipt{CheckName: pipelines.IndependentReviewCheckName, HeadSHA: in.PullRequest.HeadSHA, ExternalID: external, AppID: 42, CheckRunID: 81}, nil
		},
	}
}
func independentReviewDriver(t *testing.T) (*driveHarness, *runDriver, *independentReviewFixture) {
	t.Helper()
	dh := newDriveHarness(t, driveManifest)
	f := &independentReviewFixture{}
	a := workflowSource(t, independentReviewPolicy)
	dh.integ.Configure(func(d *Deps) { bindTestWorkflow(d, a); d.IndependentReview = f.dependencies() })
	r := dh.openRun(t, prOpening())
	dr := preparedSelectionDriver(t, dh, r)
	if err := dr.writeRun(context.Background(), RunPatch{Status: ptr(StatusInProgress)}); err != nil {
		t.Fatal(err)
	}
	return dh, dr, f
}
func TestIndependentReviewRecoveryUsesDurableVerdictAndObservesOnly(t *testing.T) {
	dh, first, f := independentReviewDriver(t)
	f.uncertain = true
	if _, err := first.independentReview(context.Background()); err == nil {
		t.Fatal("uncertain report succeeded")
	}
	stored := mustRun(t, dh.store, first.runID)
	saved, err := readIndependentReview(stored.ReviewEvidence)
	if err != nil || saved.Verdict == nil || !saved.Verdict.Approved || !saved.ReportAttempted || saved.Receipt != nil {
		t.Fatal("missing durable boundary", err)
	}
	d := dh.integ.snapshot()
	d.NodeID = "independent-second"
	d.Now = func() time.Time { return testNow.Add(leaseStaleAfter) }
	secondIntegration := New(d)
	second := claimedTestDriver(t, secondIntegration, stored.ID)
	second.p, second.installation, second.token = dh.p, dh.p.InstallationID, "read-token"
	if err = second.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	f.uncertain = false
	if _, err = second.independentReview(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.model != 1 || f.reports != 1 || f.observes != 1 {
		t.Fatalf("replayed effect: model%d reports%d observes%d", f.model, f.reports, f.observes)
	}
	if _, err = first.independentReview(context.Background()); err == nil {
		t.Fatal("old replica retained claim")
	}
}
func TestIndependentReviewRefusesLateAuthorityBeforePersistenceAndEffects(t *testing.T) {
	for _, phase := range []string{"capture", "model", "report"} {
		for _, change := range []string{"cancel", "lease", "source", "policy", "provider"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				dh, dr, f := independentReviewDriver(t)
				alter := func() {
					dh.store.mu.Lock()
					r := dh.store.runs[dr.runID]
					switch change {
					case "cancel":
						r.CancelRequested = true
					case "lease":
						r.DriverLeaseID = "replacement"
					case "source":
						p := dh.store.pipelines[dh.p.ID]
						p.CredentialID = "changed"
						dh.store.pipelines[p.ID] = p
					case "policy":
						r.PolicySnapshot = map[string]any{"invalid": true}
					case "provider":
						f.validateErr = errors.New("head or grant changed")
					}
					dh.store.runs[r.ID] = r
					dh.store.mu.Unlock()
				}
				switch phase {
				case "capture":
					f.captureHook = alter
				case "model":
					f.modelHook = alter
				case "report":
					f.reportHook = alter
				}
				if _, err := dr.independentReview(context.Background()); err == nil {
					t.Fatal("changed authority accepted")
				}
				r := mustRun(t, dh.store, dr.runID)
				if phase == "capture" && change != "provider" && r.ReviewEvidence != nil {
					t.Fatal("late capture persisted")
				}
				if r.ReviewEvidence != nil {
					saved, e := readIndependentReview(r.ReviewEvidence)
					if e != nil {
						t.Fatal(e)
					}
					if saved.Receipt != nil {
						t.Fatal("late receipt persisted")
					}
				}
			})
		}
	}
}
func TestIndependentReviewRejectedAndUnconfirmedModelCannotBeReplayed(t *testing.T) {
	t.Run("rejected", func(t *testing.T) {
		_, dr, f := independentReviewDriver(t)
		f.reject = true
		for range 2 {
			if _, err := dr.independentReview(context.Background()); err == nil {
				t.Fatal("rejected verdict succeeded")
			}
		}
		if f.model != 1 || f.reports != 0 {
			t.Fatal("rejected verdict repeated model or reported", f)
		}
	})
	t.Run("uncertain model", func(t *testing.T) {
		_, dr, f := independentReviewDriver(t)
		base := dr.d.IndependentReview.Review
		dr.d.IndependentReview.Review = func(ctx context.Context, in IndependentReviewInput) (IndependentReviewVerdict, error) {
			_, _ = base(ctx, in)
			return IndependentReviewVerdict{}, errors.New("model reply unavailable")
		}
		for range 2 {
			if _, err := dr.independentReview(context.Background()); err == nil {
				t.Fatal("uncertain model accepted")
			}
		}
		if f.model != 1 || f.reports != 0 {
			t.Fatal("uncertain model repeated", f)
		}
	})
}
func TestIndependentReviewNoOptInAndPublicArgumentsCannotStart(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	r := dh.openRun(t, prOpening())
	dr := preparedSelectionDriver(t, dh, r)
	f := &independentReviewFixture{}
	dr.d.IndependentReview = f.dependencies()
	if _, err := dr.independentReview(context.Background()); err == nil {
		t.Fatal("default policy enabled review")
	}
	_, active, actual := independentReviewDriver(t)
	h := &pipelineWorkflowHost{dr: active}
	if _, err := h.reviewPullRequest(context.Background(), map[string]any{"approved": true}); err == nil || actual.model != 0 {
		t.Fatal("caller chose verdict")
	}
	if _, err := workflowCapability("reviewPullRequest")(context.Background(), nil, 1); err == nil {
		t.Fatal("ambient review granted")
	}
}
func TestIndependentReviewPersistedInputAndReceiptTamperingRefused(t *testing.T) {
	for _, field := range []string{"input", "receipt", "run", "verdict", "model attempt"} {
		t.Run(field, func(t *testing.T) {
			dh, dr, _ := independentReviewDriver(t)
			if _, err := dr.independentReview(context.Background()); err != nil {
				t.Fatal(err)
			}
			r := mustRun(t, dh.store, dr.runID)
			copy := selectionJSON(t, r.ReviewEvidence)
			switch field {
			case "input":
				copy["Input"].(map[string]any)["PolicyVersion"] = "changed"
			case "receipt":
				copy["Receipt"].(map[string]any)["AppID"] = float64(91)
			case "run":
				copy["RunID"] = "foreign"
			case "verdict":
				copy["Verdict"].(map[string]any)["approved"] = false
			case "model attempt":
				copy["ModelAttempted"] = false
			}
			r.ReviewEvidence = copy
			dh.store.mu.Lock()
			dh.store.runs[r.ID] = r
			dh.store.mu.Unlock()
			if _, err := dr.independentReview(context.Background()); err == nil {
				t.Fatal("tampered record accepted")
			}
		})
	}
}
func TestOrdinaryMemberLatestAttemptCannotBorrowEarlierReview(t *testing.T) {
	dh, dr, _ := independentReviewDriver(t)
	if _, err := dr.independentReview(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, saved, err := dh.integ.ReviewForHead(context.Background(), dh.p, dr.run.PullRequest, dr.run.SHA)
	if err != nil || r.ID != dr.runID || saved.Receipt == nil {
		t.Fatal("actual receipt unavailable", err)
	}
	newer := r
	newer.ID = "new-review-attempt"
	newer.Attempt++
	newer.QueuedAt = r.QueuedAt.Add(time.Second)
	newer.CreatedAt = r.CreatedAt.Add(time.Second)
	newer.ReviewEvidence = nil
	dh.store.mu.Lock()
	dh.store.runs[newer.ID] = newer
	dh.store.mu.Unlock()
	if _, _, err = dh.integ.ReviewForHead(context.Background(), dh.p, r.PullRequest, r.SHA); err == nil {
		t.Fatal("missing newest review borrowed earlier accepted evidence")
	}
	dh.store.mu.Lock()
	newer.QueuedAt = r.QueuedAt
	dh.store.runs[newer.ID] = newer
	dh.store.mu.Unlock()
	if _, _, err = dh.integ.ReviewForHead(context.Background(), dh.p, r.PullRequest, r.SHA); err == nil {
		t.Fatal("ambiguous latest attempt borrowed an accepted receipt")
	}
}
