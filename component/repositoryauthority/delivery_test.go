package repositoryauthority

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

type deliveryFixture struct {
	*fixture
	receipt                    Receipt
	proposal                   Proposal
	ctx                        context.Context
	sourceHead                 string
	approved                   bool
	reviews, proposals, queues int
	queueModes                 []bool
	lifecycleError             error
	runFinished                bool
	queueAnswer                DeliveryReceipt
	queueError                 error
}

func newDeliveryFixture(t *testing.T, reviewed bool, recipeOverride ...string) *deliveryFixture {
	t.Helper()
	f := &deliveryFixture{fixture: newFixture(t), sourceHead: strings.Repeat("a", 40), approved: true, queueAnswer: DeliveryReceipt{State: "waiting", Reason: "Required check is pending.", HeadSHA: strings.Repeat("c", 40)}}
	loader := f.i.deps().WorkflowSource
	f.i.Configure(func(d *Deps) {
		d.WorkflowSource = func(kind, name string) (string, error) {
			if name == "deliveryRecipe" {
				if len(recipeOverride) > 0 {
					return recipeOverride[0], nil
				}
				return `@template
automation deliveryRecipe {
 args {requestId string!}
 result := builtin repositoryChangeQueue(requestId: args.requestId)
 return result
}`, nil
			}
			src, err := loader(kind, name)
			if name == "authorityPolicy" {
				src = strings.Replace(src, `template: "changeRecipe",`, `template: "changeRecipe", reviewTemplate: "reviewPrompt", deliveryTemplate: "deliveryRecipe",`, 1)
			}
			return src, err
		}
		d.CaptureReview = func(context.Context, string) (map[string]any, error) {
			return map[string]any{"source": "Review exact proposed changes independently", "partials": map[string]any{"common": "No tool calls"}}, nil
		}
		d.Read = func(context.Context, Authorization, []string) (RepositorySnapshot, error) {
			return RepositorySnapshot{ExpectedHead: f.sourceHead, BaseBranch: "main", Files: []Replacement{{Path: "component/example/a.go", BeforeSHA256: strings.Repeat("b", 64), Content: "original"}}}, nil
		}
		d.Review = func(ctx context.Context, a Authorization, snapshot map[string]any, in ReviewInput) (ReviewResult, error) {
			f.reviews++
			if snapshot["source"] != "Review exact proposed changes independently" || in.Statement != "Improve the scoped component" || in.Source.Files[0].Content != "original" || in.Proposal.Files[0].Content != "replacement" || a.RequestID != f.receipt.ID {
				t.Fatal("review was not built from the captured prompt, source and request")
			}
			if _, err := f.i.AuthorizeEffect(ctx, a.RequestID, OperationDeliver, []string{in.Proposal.Files[0].Path}); err != nil {
				return ReviewResult{}, err
			}
			return ReviewResult{Approved: f.approved, Reason: "Independent review evaluated the exact replacement against the request."}, nil
		}
		d.Propose = func(ctx context.Context, a Authorization, p Proposal) (ProposalReceipt, error) {
			if _, err := f.i.AuthorizeEffect(ctx, p.RequestID, OperationPropose, []string{p.Files[0].Path}); err != nil {
				return ProposalReceipt{}, err
			}
			f.proposals++
			return ProposalReceipt{State: "complete", Branch: "change/" + a.EffectID, Commit: strings.Repeat("c", 40), PullRequestURL: "https://github.com/core/engine/pull/42"}, nil
		}
		d.Queue = func(ctx context.Context, a Authorization, in DeliveryIntent) (DeliveryReceipt, error) {
			if _, err := f.i.AuthorizeDelivery(ctx, a.RequestID); err != nil {
				return DeliveryReceipt{}, err
			}
			if in.ProposalHash != digest(f.proposal) || in.Proposal.Commit != strings.Repeat("c", 40) || in.Proposal.PullRequestURL != "https://github.com/core/engine/pull/42" || in.BaseBranch != "main" {
				t.Fatal("queue identity drifted from immutable reviewed proposal")
			}
			f.queues++
			f.queueModes = append(f.queueModes, in.ObserveOnly)
			return f.queueAnswer, f.queueError
		}
		d.ExecutionActive = func(context.Context, string, string, string) error {
			if f.runFinished {
				return errors.New("run already succeeded")
			}
			return f.lifecycleError
		}
		d.DeliveryContext = func(ctx context.Context, owner, goal, run string) (context.Context, error) {
			if owner != f.maintainer || goal != f.receipt.GoalID || run != f.receipt.RunID {
				return nil, errors.New("wrong original execution")
			}
			if f.lifecycleError != nil {
				return nil, f.lifecycleError
			}
			return f.execution(f.receipt), nil
		}
	})
	req := f.request("maintainer-source")
	if reviewed {
		req.SourceID = "request-source"
	}
	req.Operation = OperationDeliver
	who := f.maintainer
	if reviewed {
		who = f.requester
	}
	public, err := f.i.Request(actor(who), req)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := f.i.get(context.Background(), public.ID, f.maintainer)
	if err != nil {
		t.Fatal(err)
	}
	f.receipt = stored.receipt(f.maintainer)
	f.ctx = f.execution(f.receipt)
	f.proposal = Proposal{RequestID: public.ID, ExpectedHead: f.sourceHead, BaseBranch: "main", Title: "Scoped replacement", Files: []Replacement{{Path: "component/example/a.go", BeforeSHA256: strings.Repeat("b", 64), Content: "replacement"}}}
	if reviewed {
		f.approve(f.receipt)
	}
	return f
}
func (f *deliveryFixture) publish(t *testing.T) {
	t.Helper()
	if _, err := f.i.Review(f.ctx, f.proposal); err != nil {
		t.Fatal(err)
	}
	if _, err := f.i.Propose(f.ctx, f.proposal); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryRequiresExplicitScopeAndExactIndependentReview(t *testing.T) {
	t.Run("draft cannot upgrade", func(t *testing.T) {
		f := newFixture(t)
		access, err := f.i.Access(actor(f.maintainer), "maintainer-source")
		if err != nil || len(access.Operations) != 1 || access.Operations[0] != OperationPropose {
			t.Fatalf("draft route advertised delivery: %+v %v", access, err)
		}
		req := f.request("maintainer-source")
		req.Operation = OperationDeliver
		if _, err = f.i.Request(actor(f.maintainer), req); err == nil {
			t.Fatal("missing delivery policy admitted request")
		}
		req.Operation = OperationPropose
		r, err := f.i.Request(actor(f.maintainer), req)
		if err != nil {
			t.Fatal(err)
		}
		p := Proposal{RequestID: r.ID, ExpectedHead: strings.Repeat("a", 40), BaseBranch: "main", Title: "Draft", Files: []Replacement{{Path: "component/example/a.go", Content: "replacement"}}}
		if _, err = f.i.Review(f.execution(r), p); err == nil {
			t.Fatal("draft borrowed independent delivery review")
		}
		if _, err = f.i.Queue(f.execution(r), r.ID); err == nil {
			t.Fatal("draft borrowed queue scope")
		}
	})
	t.Run("review exact bytes", func(t *testing.T) {
		f := newDeliveryFixture(t, true)
		access, err := f.i.Access(actor(f.requester), "request-source")
		if err != nil || access.CanWrite || !access.CanRequest || access.Route != "maintainer" || len(access.Operations) != 2 || access.Operations[1] != OperationDeliver {
			t.Fatalf("developer request authority was confused with writer permission: %+v %v", access, err)
		}
		if _, err := f.i.Propose(f.ctx, f.proposal); err == nil || f.proposals != 0 {
			t.Fatal("unreviewed delivery wrote")
		}
		review, err := f.i.Review(f.ctx, f.proposal)
		if err != nil || !review.Approved {
			t.Fatalf("review: %+v %v", review, err)
		}
		other := New(f.i.deps())
		if _, err = other.Review(f.ctx, f.proposal); err != nil || f.reviews != 1 {
			t.Fatal("recorded review repeated model call")
		}
		changed := f.proposal
		changed.Title = "Different title"
		if _, err = other.Propose(f.ctx, changed); err == nil || f.proposals != 0 {
			t.Fatal("review permitted changed proposal")
		}
		if _, err = other.Review(f.ctx, changed); err == nil || f.reviews != 1 {
			t.Fatal("review request changed captured bytes")
		}
		if _, err = other.Propose(f.ctx, f.proposal); err != nil || f.proposals != 1 {
			t.Fatalf("exact reviewed proposal refused: %v", err)
		}
		receipt, err := other.Status(actor(f.requester), f.receipt.ID)
		if err != nil || receipt.Review == nil || !receipt.Review.Approved || receipt.GoalID != "" || receipt.Operation != OperationDeliver {
			t.Fatalf("public reviewed receipt/privacy: %+v %v", receipt, err)
		}
	})
	t.Run("rejected review", func(t *testing.T) {
		f := newDeliveryFixture(t, false)
		f.approved = false
		if _, err := f.i.Review(f.ctx, f.proposal); err != nil {
			t.Fatal(err)
		}
		if _, err := f.i.Propose(f.ctx, f.proposal); err == nil || f.proposals != 0 {
			t.Fatal("rejected bytes published")
		}
	})
	t.Run("source moved before review", func(t *testing.T) {
		f := newDeliveryFixture(t, false)
		f.sourceHead = strings.Repeat("d", 40)
		if _, err := f.i.Review(f.ctx, f.proposal); err == nil || f.reviews != 0 {
			t.Fatal("stale source reached independent reviewer")
		}
	})
}

func TestDeliveryRecoveryUsesSameReviewedHeadAfterSucceededRun(t *testing.T) {
	f := newDeliveryFixture(t, true)
	f.publish(t)
	f.queueError = errors.New("response lost after GitHub accepted request")
	receipt, err := f.i.Queue(f.ctx, f.receipt.ID)
	if err != nil || receipt.State != "waiting" || f.queues != 1 {
		t.Fatalf("uncertain response lost identity: %+v %v", receipt, err)
	}
	f.runFinished = true
	if _, err = f.i.Queue(f.ctx, f.receipt.ID); err == nil {
		t.Fatal("completed run retained ambient effect authority")
	}
	f.queueError = nil
	f.queueAnswer = DeliveryReceipt{State: "queued", Reason: "GitHub confirmed queue admission.", HeadSHA: strings.Repeat("c", 40), QueueEntryID: "queue-entry-42"}
	other := New(f.i.deps())
	if _, err = other.RecoverDeliveries(actor(f.maintainer)); err == nil {
		t.Fatal("ordinary actor invoked internal recovery")
	}
	n, err := other.RecoverDeliveries(auth.ContextWithInternalOrigin(context.Background()))
	if err != nil || n != 1 || f.queues != 2 || f.proposals != 1 || f.reviews != 1 {
		t.Fatalf("replica recovery n=%d err=%v q=%d proposals=%d reviews=%d", n, err, f.queues, f.proposals, f.reviews)
	}
	receiptRow, err := f.i.Status(actor(f.requester), f.receipt.ID)
	if err != nil || receiptRow.Delivery == nil || receiptRow.Delivery.State != "queued" || receiptRow.Delivery.QueueEntryID != "queue-entry-42" {
		t.Fatalf("durable receipt: %+v %v", receiptRow, err)
	}
	if n, err = other.RecoverDeliveries(auth.ContextWithInternalOrigin(context.Background())); err != nil || n != 0 || f.queues != 2 {
		t.Fatal("completed admission repeated provider call")
	}
}

func TestDeliveryRecoveryRefusesRevokedMovedAndChangedAuthority(t *testing.T) {
	for _, scenario := range []string{"revoked", "closed goal", "cancelled run", "writer removed", "changed prompt", "changed recipe", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDeliveryFixture(t, true)
			f.publish(t)
			if _, err := f.i.Queue(f.ctx, f.receipt.ID); err != nil {
				t.Fatal(err)
			}
			f.runFinished = true
			switch scenario {
			case "revoked":
				if _, err := f.i.Revoke(actor(f.maintainer), f.receipt.ID, "Withdraw delivery scope"); err != nil {
					t.Fatal(err)
				}
			case "closed goal", "cancelled run":
				f.lifecycleError = errors.New(scenario)
			case "writer removed":
				f.writer = false
			case "changed prompt":
				f.i.Configure(func(d *Deps) {
					d.CaptureReview = func(context.Context, string) (map[string]any, error) {
						return map[string]any{"source": "replacement prompt"}, nil
					}
				})
			case "changed recipe":
				loader := f.i.deps().WorkflowSource
				f.i.Configure(func(d *Deps) {
					d.WorkflowSource = func(k, n string) (string, error) {
						s, e := loader(k, n)
						if n == "deliveryRecipe" {
							s = strings.Replace(s, "return result", "return false", 1)
						}
						return s, e
					}
				})
			case "expired":
				f.now = f.now.Add(2 * time.Hour)
			}
			other := New(f.i.deps())
			if _, err := other.RecoverDeliveries(auth.ContextWithInternalOrigin(context.Background())); err != nil {
				t.Fatal(err)
			}
			if f.queues != 1 || f.proposals != 1 {
				t.Fatal("revoked or altered authority reached provider")
			}
		})
	}
	t.Run("head moved stops later retries", func(t *testing.T) {
		f := newDeliveryFixture(t, false)
		f.publish(t)
		f.queueAnswer = DeliveryReceipt{State: "attention", Reason: "The PR head changed after review.", HeadSHA: strings.Repeat("d", 40)}
		got, err := f.i.Queue(f.ctx, f.receipt.ID)
		if err != nil || got.State != "attention" {
			t.Fatalf("head refusal: %+v %v", got, err)
		}
		if _, err = f.i.Queue(f.ctx, f.receipt.ID); err != nil || f.queues != 1 {
			t.Fatal("moved head retried admission")
		}
		if n, err := f.i.RecoverDeliveries(auth.ContextWithInternalOrigin(context.Background())); err != nil || n != 0 {
			t.Fatalf("moved head recovered: %d %v", n, err)
		}
	})
}

func TestDeliveryLeaseIsExclusiveAcrossReplicasAndExpires(t *testing.T) {
	f := newDeliveryFixture(t, false)
	f.publish(t)
	if _, err := f.i.Queue(f.ctx, f.receipt.ID); err != nil {
		t.Fatal(err)
	}
	other := New(f.i.deps())
	var wg sync.WaitGroup
	batches := make(chan []record, 2)
	errs := make(chan error, 2)
	for _, instance := range []*Integration{f.i, other} {
		wg.Add(1)
		go func(i *Integration) {
			defer wg.Done()
			r, e := i.claimDeliveries(context.Background(), 25)
			batches <- r
			errs <- e
		}(instance)
	}
	wg.Wait()
	close(batches)
	close(errs)
	total := 0
	var leased record
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for batch := range batches {
		total += len(batch)
		if len(batch) > 0 {
			leased = batch[0]
		}
	}
	if total != 1 {
		t.Fatalf("two replicas claimed %d deliveries", total)
	}
	f.runFinished = true
	forged := context.WithValue(f.ctx, deliveryRecoveryKey{}, deliveryRecovery{id: f.receipt.ID, lease: "other-lease"})
	if _, err := other.AuthorizeDelivery(forged, f.receipt.ID); err == nil {
		t.Fatal("wrong lease borrowed completed run")
	}
	f.now = f.now.Add(4 * time.Minute)
	expired := context.WithValue(f.ctx, deliveryRecoveryKey{}, deliveryRecovery{id: f.receipt.ID, lease: leased.DeliveryLease})
	if _, err := other.AuthorizeDelivery(expired, f.receipt.ID); err == nil {
		t.Fatal("expired lease borrowed completed run")
	}
	if n, err := other.RecoverDeliveries(auth.ContextWithInternalOrigin(context.Background())); err != nil || n != 1 || f.queues != 2 {
		t.Fatalf("lease recovery %d %v calls=%d", n, err, f.queues)
	}
}

func TestAdmittedDeliveryContinuesReadOnlyUntilMergeOrQueueRemoval(t *testing.T) {
	for _, scenario := range []string{"merged", "removed", "outage"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDeliveryFixture(t, false)
			f.publish(t)
			f.queueAnswer = DeliveryReceipt{State: "queued", HeadSHA: strings.Repeat("c", 40), QueueEntryID: "queue-42"}
			if _, err := f.i.Queue(f.ctx, f.receipt.ID); err != nil {
				t.Fatal(err)
			}
			f.runFinished = true
			switch scenario {
			case "merged":
				f.queueAnswer = DeliveryReceipt{State: "merged", HeadSHA: strings.Repeat("c", 40), Reason: "GitHub confirmed merge."}
			case "removed":
				f.queueAnswer = DeliveryReceipt{State: "attention", HeadSHA: strings.Repeat("c", 40), Reason: "The change left the queue; review its outcome before any new admission."}
			case "outage":
				f.queueError = errors.New("provider unavailable")
			}
			other := New(f.i.deps())
			if n, err := other.RecoverDeliveries(auth.ContextWithInternalOrigin(context.Background())); err != nil || n != 1 {
				t.Fatalf("observation n=%d %v", n, err)
			}
			if len(f.queueModes) != 2 || f.queueModes[0] || !f.queueModes[1] {
				t.Fatalf("admitted recovery regained write mode: %v", f.queueModes)
			}
			record, err := f.i.get(context.Background(), f.receipt.ID, f.maintainer)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "outage" {
				if !record.DeliveryAdmitted || record.DeliveryReceipt.State != "waiting" || record.DeliveryState != "queued" {
					t.Fatalf("outage forgot previous admission: %+v", record.DeliveryReceipt)
				}
				f.queueError = nil
				f.queueAnswer = DeliveryReceipt{State: "merged", HeadSHA: strings.Repeat("c", 40), Reason: "GitHub confirmed merge."}
				f.now = f.now.Add(2 * time.Minute)
				if n, err := other.RecoverDeliveries(auth.ContextWithInternalOrigin(context.Background())); err != nil || n != 1 || len(f.queueModes) != 3 || !f.queueModes[2] {
					t.Fatalf("outage recovery repeated admission: n=%d %v modes=%v", n, err, f.queueModes)
				}
			} else if scenario == "removed" {
				if record.DeliveryReceipt.State != "attention" || record.DeliveryState != "blocked" {
					t.Fatalf("removed queue not surfaced: %+v", record.DeliveryReceipt)
				}
			} else if record.DeliveryReceipt.State != "merged" {
				t.Fatalf("merge not recorded: %+v", record.DeliveryReceipt)
			}
			f.now = f.now.Add(2 * time.Minute)
			before := f.queues
			if n, err := other.RecoverDeliveries(auth.ContextWithInternalOrigin(context.Background())); err != nil || n != 0 || f.queues != before || f.proposals != 1 || f.reviews != 1 {
				t.Fatalf("settled delivery was resubmitted: n=%d %v", n, err)
			}
		})
	}
}

func TestDeliveryRecipeCannotMultiplyObservationsWithinOneLease(t *testing.T) {
	f := newDeliveryFixture(t, false, `@template
automation deliveryRecipe {
 args { requestId string! }
 first := builtin repositoryChangeQueue(requestId: args.requestId)
 second := builtin repositoryChangeQueue(requestId: args.requestId)
 return second
}`)
	f.publish(t)
	if _, err := f.i.Queue(f.ctx, f.receipt.ID); err != nil {
		t.Fatal(err)
	}
	f.runFinished = true
	if n, err := f.i.RecoverDeliveries(auth.ContextWithInternalOrigin(context.Background())); err != nil || n != 0 || f.queues != 2 {
		t.Fatalf("recipe multiplied provider calls: n=%d err=%v calls=%d", n, err, f.queues)
	}
	r, err := f.i.Status(actor(f.maintainer), f.receipt.ID)
	if err != nil || r.Delivery == nil || r.Delivery.State != "attention" || !strings.Contains(r.Delivery.Reason, "one observation per lease") {
		t.Fatalf("bounded refusal not readable: %+v %v", r, err)
	}
}
