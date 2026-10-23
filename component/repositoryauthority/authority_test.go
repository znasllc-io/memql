package repositoryauthority

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
func actor(name string) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: name, Role: auth.RoleDeveloper})
}

type fixture struct {
	i                     *Integration
	requester, maintainer string
	now                   time.Time
	mu                    sync.Mutex
	goals                 map[string]GoalReceipt
	decisions             map[string]Decision
	subjects              map[string]map[string]any
	writer                bool
	routeSource           string
	opens                 int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		dbtest.Unreachable(t, "repository authority", dbtest.DSN(), err)
	}
	t.Cleanup(func() { db.Close() })
	f := &fixture{requester: "requester-" + id.NewShortId(), maintainer: "maintainer-" + id.NewShortId(), now: time.Now().UTC(), goals: map[string]GoalReceipt{}, decisions: map[string]Decision{}, subjects: map[string]map[string]any{}, writer: true, routeSource: "maintainer-source"}
	d := Deps{DB: db, Now: func() time.Time { return f.now }, ExecutionActive: func(context.Context, string, string, string) error { return nil }, Source: func(_ context.Context, key string, trusted bool) (Source, error) {
		if key == "request-source" {
			return Source{ID: key, OwnerUserID: f.requester, CredentialID: "requester-connection", Repository: "core/engine"}, nil
		}
		if key == "maintainer-source" {
			return Source{ID: key, OwnerUserID: f.maintainer, CredentialID: "maintainer-connection", Repository: "core/engine"}, nil
		}
		return Source{}, errors.New("unknown source")
	}, RepositoryAccess: func(_ context.Context, s Source) (packages.RepositoryAuthority, error) {
		return packages.RepositoryAuthority{RepositoryID: 123, Repository: s.Repository, InstallationID: 456, CanWrite: s.OwnerUserID == f.maintainer && f.writer}, nil
	}, SystemVariable: func(_ context.Context, name string) (string, error) {
		if name == ResolverVariable {
			return "authorityPolicy", nil
		}
		return "{}", nil
	}, WorkflowSource: func(kind, name string) (string, error) {
		if kind == "automation" && name == "changeRecipe" {
			return "@template\nautomation changeRecipe { args { requestId string! repository string! repositoryId int! statement string! operation string! paths []string! parentRequestId string! } return true }", nil
		}
		if kind != "automation" || name != "authorityPolicy" {
			return "", errors.New("missing installed source")
		}
		return `@template
automation authorityPolicy {
 args {repository string! sourceId string! sourceOwnerId string! requesterCanWrite bool! bindings object!}
 return {sourceId: "` + f.routeSource + `", template: "changeRecipe", ttlSeconds: 3600}
}`, nil
	}}
	d.OpenGoal = func(ctx context.Context, g GoalRequest) (GoalReceipt, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		receipt, exists := f.goals[g.Key]
		if !exists {
			receipt = GoalReceipt{GoalID: "goal-" + g.Key, RunID: "run-" + g.Key, ApprovalID: "approval-" + g.Key}
			f.goals[g.Key] = receipt
			f.opens++
		}
		if err := g.BeforeRun(ctx, receipt.GoalID, receipt.RunID); err != nil {
			return receipt, err
		}
		f.subjects[g.Key] = g.Subject
		if _, ok := f.decisions[receipt.ApprovalID]; !ok {
			f.decisions[receipt.ApprovalID] = Decision{State: "pending", GoalID: receipt.GoalID, RunID: receipt.RunID, RequestID: g.Key, Fingerprint: str(g.Subject["fingerprint"])}
		}
		return receipt, nil
	}
	d.Decision = func(_ context.Context, owner, key string) (Decision, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if owner != f.maintainer {
			return Decision{}, errors.New("wrong decision owner")
		}
		return f.decisions[key], nil
	}
	f.i = New(d)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM repository_change_requests WHERE requester=$1 OR requester=$2`, f.requester, f.maintainer)
	})
	return f
}
func (f *fixture) request(source string) Request {
	return Request{SourceID: source, RequestID: id.NewShortId(), Statement: "Improve the scoped component", Operation: OperationPropose, Paths: []string{"component/example/"}}
}
func (f *fixture) approve(r Receipt) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.decisions[r.ApprovalID]
	d.State = "approved"
	f.decisions[r.ApprovalID] = d
}
func (f *fixture) execution(r Receipt) context.Context {
	return common.ContextWithRun(actor(f.maintainer), common.RunContext{RunId: r.RunID, GoalId: r.GoalID, OwnerUserId: f.maintainer, Mode: common.RunModeLive})
}

func TestRequestsRequireExactMaintainerDecisionAndSurviveAnotherReplica(t *testing.T) {
	f := newFixture(t)
	req := f.request("request-source")
	r, err := f.i.Request(actor(f.requester), req)
	if err != nil {
		t.Fatal(err)
	}
	if r.GoalID != "" || r.RunID != "" || r.ApprovalID != "" {
		t.Fatal("requester received maintainer-private journal identities")
	}
	stored, err := f.i.get(actor(f.maintainer), r.ID, f.maintainer)
	if err != nil {
		t.Fatal(err)
	}
	maintainerReceipt := stored.receipt(f.maintainer)
	if r.State != "pending" || f.opens != 1 {
		t.Fatalf("unapproved requester started work: %+v", r)
	}
	other := New(f.i.deps())
	again, err := other.Request(actor(f.requester), req)
	if err != nil || again.ID != r.ID || f.opens != 1 {
		t.Fatalf("duplicate request: %+v %v opens=%d", again, err, f.opens)
	}
	req.Statement = "Altered immutable request"
	if _, err = other.Request(actor(f.requester), req); err == nil {
		t.Fatal("request identity replay changed its scope")
	}
	if _, err = other.Status(actor("unrelated-owner"), r.ID); err == nil {
		t.Fatal("another local owner read a private request")
	}
	if _, err = other.AuthorizeEffect(f.execution(maintainerReceipt), r.ID, OperationPropose, []string{"component/example/change.go"}); err == nil {
		t.Fatal("pending review granted write")
	}
	if err = other.ValidateReview(actor(f.requester), f.subjects[r.ID]); err == nil {
		t.Fatal("requester self-approved upstream scope")
	}
	if err = other.ValidateReview(actor(f.maintainer), f.subjects[r.ID]); err != nil {
		t.Fatal(err)
	}
	f.approve(maintainerReceipt)
	if _, err = other.AuthorizeEffect(f.execution(maintainerReceipt), r.ID, OperationPropose, []string{"component/example/change.go"}); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][]string{{"component/other.go"}, {"component/example/../other.go"}, {"component/example/*"}, {"component/example/.git/config"}, {"component/example/a\nb.go"}} {
		if _, err = other.AuthorizeEffect(f.execution(maintainerReceipt), r.ID, OperationPropose, paths); err == nil {
			t.Fatalf("escaped approved paths: %v", paths)
		}
	}
	wrong := common.ContextWithRun(actor(f.maintainer), common.RunContext{RunId: "different-run", GoalId: maintainerReceipt.GoalID, OwnerUserId: f.maintainer, Mode: common.RunModeLive})
	if _, err = other.AuthorizeEffect(wrong, r.ID, OperationPropose, []string{"component/example/a.go"}); err == nil {
		t.Fatal("another executor borrowed approval")
	}
	if _, err = other.Revoke(actor(f.maintainer), r.ID, "Scope withdrawn"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.i.AuthorizeEffect(f.execution(maintainerReceipt), r.ID, OperationPropose, []string{"component/example/a.go"}); err == nil {
		t.Fatal("revocation did not reach original replica")
	}
}

func TestEffectRequiresCurrentExecutionAndUnchangedBoundedRecipe(t *testing.T) {
	for _, scenario := range []string{"missing execution adapter", "cancelled execution", "changed recipe"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			r, err := f.i.Request(actor(f.maintainer), f.request("maintainer-source"))
			if err != nil {
				t.Fatal(err)
			}
			canonical := common.ContextWithRun(actor(f.maintainer), common.RunContext{RunId: "v1:work:run:" + r.RunID, GoalId: "v1:work:goal:" + r.GoalID, OwnerUserId: f.maintainer, Mode: common.RunModeLive})
			if _, err := f.i.AuthorizeEffect(canonical, r.ID, OperationPropose, []string{"component/example/a.go"}); err != nil {
				t.Fatal(err)
			}
			oldSource := f.i.deps().WorkflowSource
			f.i.Configure(func(d *Deps) {
				switch scenario {
				case "missing execution adapter":
					d.ExecutionActive = nil
				case "cancelled execution":
					d.ExecutionActive = func(context.Context, string, string, string) error {
						return errors.New("run cancelled on another replica")
					}
				case "changed recipe":
					d.WorkflowSource = func(kind, name string) (string, error) {
						source, err := oldSource(kind, name)
						if name == "changeRecipe" {
							source = strings.Replace(source, "return true", "return false", 1)
						}
						return source, err
					}
				}
			})
			if _, err := f.i.AuthorizeEffect(canonical, r.ID, OperationPropose, []string{"component/example/a.go"}); err == nil {
				t.Fatal("stale or missing execution authority allowed an effect")
			}
		})
	}
	f := newFixture(t)
	old := f.i.deps().WorkflowSource
	f.i.Configure(func(d *Deps) {
		d.WorkflowSource = func(kind, name string) (string, error) {
			if name == "changeRecipe" {
				return "@template\nautomation changeRecipe {\n result := builtin unrestrictedWriter()\n return result\n}", nil
			}
			return old(kind, name)
		}
	})
	if _, err := f.i.Request(actor(f.requester), f.request("request-source")); err == nil {
		t.Fatal("unbounded operation admitted by installed recipe")
	}
}

func TestProposalSupportsBoundedDirectoryExpansion(t *testing.T) {
	p := Proposal{RequestID: "request", ExpectedHead: strings.Repeat("a", 40), BaseBranch: "main", Title: "Bounded files"}
	for n := 0; n < 256; n++ {
		p.Files = append(p.Files, Replacement{Path: fmt.Sprintf("component/example/f%d.go", n), Content: "package example"})
	}
	if _, err := validateProposal(p); err != nil {
		t.Fatal(err)
	}
	p.Files = append(p.Files, Replacement{Path: "component/example/extra.go"})
	if _, err := validateProposal(p); err == nil {
		t.Fatal("unbounded file count accepted")
	}
}
func TestOwnProviderWriterStartsImmediatelyButVisibilityAndLocalRoleDoNot(t *testing.T) {
	f := newFixture(t)
	access, err := f.i.Access(actor(f.requester), "maintainer-source")
	if err != nil || access.CanWrite {
		t.Fatalf("visible writable connection became requester authority: %+v %v", access, err)
	}
	req := f.request("maintainer-source")
	r, err := f.i.Request(actor(f.maintainer), req)
	if err != nil || r.State != "approved" {
		t.Fatalf("own provider writer: %+v %v", r, err)
	}
	f.writer = false
	if _, err = f.i.AuthorizeEffect(f.execution(r), r.ID, OperationPropose, []string{"component/example/a.go"}); err == nil {
		t.Fatal("revoked GitHub permission was cached")
	}
}
func TestDeclineExpiryAndRoutingChangesNeverGrantExecution(t *testing.T) {
	for _, change := range []string{"decline", "expiry", "route"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			r, err := f.i.Request(actor(f.requester), f.request("request-source"))
			if err != nil {
				t.Fatal(err)
			}
			if r.GoalID != "" || r.RunID != "" || r.ApprovalID != "" {
				t.Fatal("requester received maintainer-private journal identities")
			}
			stored, err := f.i.get(actor(f.maintainer), r.ID, f.maintainer)
			if err != nil {
				t.Fatal(err)
			}
			maintainerReceipt := stored.receipt(f.maintainer)
			f.approve(maintainerReceipt)
			switch change {
			case "decline":
				f.mu.Lock()
				d := f.decisions[maintainerReceipt.ApprovalID]
				d.State, d.Reason = "declined", "This change is outside the roadmap"
				f.decisions[maintainerReceipt.ApprovalID] = d
				f.mu.Unlock()
			case "expiry":
				f.now = f.now.Add(2 * time.Hour)
			case "route":
				f.routeSource = "different-source"
			}
			if _, err = f.i.AuthorizeEffect(f.execution(maintainerReceipt), r.ID, OperationPropose, []string{"component/example/a.go"}); err == nil {
				t.Fatal("stale approval remained executable")
			}
			if change == "decline" {
				status, e := f.i.Status(actor(f.requester), r.ID)
				if e != nil || status.Reason == "" || status.State != "declined" || status.GoalID != "" || status.RunID != "" || status.ApprovalID != "" {
					t.Fatalf("requester lost rejection explanation: %+v %v", status, e)
				}
			}
		})
	}
}
func TestProposalRecoveryUsesOneImmutableIntentAcrossReplicas(t *testing.T) {
	f := newFixture(t)
	r, err := f.i.Request(actor(f.maintainer), f.request("maintainer-source"))
	if err != nil {
		t.Fatal(err)
	}
	proposal := Proposal{RequestID: r.ID, ExpectedHead: strings.Repeat("a", 40), BaseBranch: "main", Title: "Scoped change", Files: []Replacement{{Path: "component/example/a.go", BeforeSHA256: strings.Repeat("b", 64), Content: "replacement"}}}
	calls := 0
	effectID := ""
	var effectTime time.Time
	f.i.Configure(func(d *Deps) {
		d.Propose = func(ctx context.Context, a Authorization, p Proposal) (ProposalReceipt, error) {
			if _, err := f.i.AuthorizeEffect(ctx, r.ID, OperationPropose, []string{p.Files[0].Path}); err != nil {
				return ProposalReceipt{}, err
			}
			calls++
			if effectID == "" {
				effectID, effectTime = a.EffectID, a.EffectCreatedAt
			} else if effectID != a.EffectID || !effectTime.Equal(a.EffectCreatedAt) {
				t.Fatal("recovery changed immutable provider intent")
			}
			if calls == 1 {
				return ProposalReceipt{}, errors.New("reply lost after provider accepted exact intent")
			}
			return ProposalReceipt{State: "complete", Branch: "requests/" + r.ID, Commit: strings.Repeat("c", 40), PullRequestURL: "https://github.com/core/engine/pull/1"}, nil
		}
	})
	if _, err = f.i.Propose(f.execution(r), proposal); err == nil {
		t.Fatal("lost response was reported as success")
	}
	other := New(f.i.deps())
	receipt, err := other.Propose(f.execution(r), proposal)
	if err != nil || receipt.State != "complete" || calls != 2 {
		t.Fatalf("same-intent recovery: %+v %v calls=%d", receipt, err, calls)
	}
	if _, err = other.Propose(f.execution(r), proposal); err != nil || calls != 2 {
		t.Fatal("confirmed receipt repeated the effect")
	}
	proposal.Files[0].Content = "different"
	if _, err = other.Propose(f.execution(r), proposal); err == nil {
		t.Fatal("second proposal changed immutable request effect")
	}
}
func TestRemoteInputCannotChooseAMaintainerOrImportAnApproval(t *testing.T) {
	f := newFixture(t)
	transfer := Transfer{RequestID: id.NewShortId(), Repository: "core/engine", RepositoryID: 123, Statement: "Requested from another host", Operation: OperationPropose, Paths: []string{"component/example/"}}
	r, err := f.i.Receive(actor(f.requester), transfer)
	if err != nil || r.State != "pending" {
		t.Fatalf("remote request must wait for local maintainer: %+v %v", r, err)
	}
	transfer.RequestID = id.NewShortId()
	transfer.RepositoryID = 999
	if _, err = f.i.Receive(actor(f.requester), transfer); err == nil {
		t.Fatal("sender substituted a stable repository identity")
	}
}

func TestReviewedProposalRunsWithoutStatusPollAndKeepsEffectGuardReadOnly(t *testing.T) {
	f := newFixture(t)
	public, err := f.i.Request(actor(f.requester), f.request("request-source"))
	if err != nil {
		t.Fatal(err)
	}
	if public.State != "pending" || public.GoalID != "" || public.RunID != "" || public.ApprovalID != "" {
		t.Fatalf("requester receipt: %+v", public)
	}
	saved, err := f.i.get(actor(f.maintainer), public.ID, f.maintainer)
	if err != nil {
		t.Fatal(err)
	}
	trusted := saved.receipt(f.maintainer)
	f.approve(trusted)
	writes := 0
	f.i.Configure(func(d *Deps) {
		d.Propose = func(ctx context.Context, grant Authorization, p Proposal) (ProposalReceipt, error) {
			// Production repeats this read-only guard immediately before each GitHub
			// write, while Propose holds its transaction. A refreshing writer deadlocks.
			if _, err := f.i.AuthorizeEffect(ctx, p.RequestID, OperationPropose, []string{"component/example/change.go"}); err != nil {
				return ProposalReceipt{}, err
			}
			writes++
			return ProposalReceipt{State: "complete", Branch: "memql/change-" + grant.EffectID, Commit: strings.Repeat("b", 40), PullRequestURL: "https://github.com/core/engine/pull/1"}, nil
		}
	})
	ctx, cancel := context.WithTimeout(f.execution(trusted), 3*time.Second)
	defer cancel()
	proposal := Proposal{RequestID: public.ID, ExpectedHead: strings.Repeat("a", 40), BaseBranch: "main", Title: "Repair", Files: []Replacement{{Path: "component/example/change.go", Content: "package example"}}}
	got, err := f.i.Propose(ctx, proposal)
	if err != nil || got.State != "complete" || writes != 1 {
		t.Fatalf("reviewed proposal without status poll: %+v %v writes=%d", got, err, writes)
	}
	saved, err = f.i.get(actor(f.maintainer), public.ID, f.maintainer)
	if err != nil || saved.State != "approved" || saved.ProposalReceipt == nil {
		t.Fatalf("decision/proposal not durably bound: %+v %v", saved, err)
	}
}

func TestReviewedEffectRechecksDecisionAtReservation(t *testing.T) {
	for _, mode := range []string{"proposal", "effect"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			public, err := f.i.Request(actor(f.requester), f.request("request-source"))
			if err != nil {
				t.Fatal(err)
			}
			saved, err := f.i.get(actor(f.maintainer), public.ID, f.maintainer)
			if err != nil {
				t.Fatal(err)
			}
			trusted := saved.receipt(f.maintainer)
			f.approve(trusted)
			writes := 0
			original := f.i.deps().RepositoryAccess
			// The first authorization observed approval. Another replica records a
			// denial before reservation; that stale observation must not create intent.
			f.i.Configure(func(d *Deps) {
				d.RepositoryAccess = func(ctx context.Context, s Source) (packages.RepositoryAuthority, error) {
					result, err := original(ctx, s)
					f.mu.Lock()
					decision := f.decisions[trusted.ApprovalID]
					decision.State, decision.Reason = "declined", "Scope withdrawn before reservation"
					f.decisions[trusted.ApprovalID] = decision
					f.mu.Unlock()
					return result, err
				}
				d.Propose = func(context.Context, Authorization, Proposal) (ProposalReceipt, error) {
					writes++
					return ProposalReceipt{}, nil
				}
			})
			if mode == "proposal" {
				_, err = f.i.Propose(f.execution(trusted), Proposal{RequestID: public.ID, ExpectedHead: strings.Repeat("a", 40), BaseBranch: "main", Title: "Repair", Files: []Replacement{{Path: "component/example/change.go", Content: "package example"}}})
			} else {
				err = f.i.WithAuthorizedEffect(f.execution(trusted), public.ID, OperationPropose, []string{"component/example/change.go"}, func(Authorization) error { writes++; return nil })
			}
			if err == nil || writes != 0 {
				t.Fatalf("changed review reached effect: err=%v writes=%d", err, writes)
			}
			stored, e := f.i.get(actor(f.maintainer), public.ID, f.maintainer)
			if e != nil || stored.ProposalHash != "" {
				t.Fatalf("rejected review reserved proposal: %+v %v", stored, e)
			}
		})
	}
}

func TestReviewedAuthorizedEffectDoesNotRequireStatusPoll(t *testing.T) {
	f := newFixture(t)
	public, err := f.i.Request(actor(f.requester), f.request("request-source"))
	if err != nil {
		t.Fatal(err)
	}
	saved, err := f.i.get(actor(f.maintainer), public.ID, f.maintainer)
	if err != nil {
		t.Fatal(err)
	}
	trusted := saved.receipt(f.maintainer)
	f.approve(trusted)
	calls := 0
	err = f.i.WithAuthorizedEffect(f.execution(trusted), public.ID, OperationPropose, []string{"component/example/change.go"}, func(Authorization) error { calls++; return nil })
	if err != nil || calls != 1 {
		t.Fatalf("reviewed effect: %v calls=%d", err, calls)
	}
	saved, err = f.i.get(actor(f.maintainer), public.ID, f.maintainer)
	if err != nil || saved.State != "approved" {
		t.Fatalf("review decision not durable: %+v %v", saved, err)
	}
}
