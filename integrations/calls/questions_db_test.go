package calls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

func questionFixture(t *testing.T) (*callWorld, accountTestKey, *CommerceConfig, callrules.StartResult, *callrules.WorkflowRun) {
	t.Helper()
	cw, key, c, start := workflowFixture(t, "moderated-questions", true)
	if cw == nil {
		return nil, key, c, start, nil
	}
	t.Cleanup(func() {
		for _, site := range cw.sites {
			_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", questionConcept).Where("payload->>'siteId' = ?", siteConcept+":"+site).Exec(context.Background())
		}
	})
	run := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "start", SkillID: "working-session", Version: 1, IdempotencyKey: "questions-start"}).Run
	return cw, key, c, start, run
}
func questionRequest(t *testing.T, cw *callWorld, session callrules.Session, run *callrules.WorkflowRun, action, op, text string) callrules.WorkflowRequest {
	t.Helper()
	held, _, e := cw.svc.session(context.Background(), cw.ref, session.Token, cw.clock.Now())
	require.NoError(t, e)
	seat, room, e := cw.svc.seatAndRoom(context.Background(), cw.ref.OwnerUserID, held)
	require.NoError(t, e)
	q := &callrules.QuestionRequest{RoomID: room.ID, Identity: seat.Identity, RunID: run.ID, IdempotencyKey: op, Text: text}
	if action == "question_submit" {
		q.Publication = callrules.QuestionPublication
	}
	return callrules.WorkflowRequest{Action: action, Question: q}
}
func questionRead(t *testing.T, cw *callWorld, session callrules.Session, run *callrules.WorkflowRun) *callrules.Questions {
	t.Helper()
	r, e := cw.svc.Workflow(context.Background(), cw.ref, session.Token, questionRequest(t, cw, session, run, "questions", "", ""))
	require.NoError(t, e)
	require.NotNil(t, r.Questions)
	return r.Questions
}
func TestQuestionsPrivateSelectionTerminalAndExactReplay(t *testing.T) {
	cw, key, _, start, run := questionFixture(t)
	if cw == nil {
		return
	}
	ctx := context.Background()
	_, guest, e := cw.join(start.Call.GuestToken, "Question author")
	require.NoError(t, e)
	_, other, e := cw.join(start.Call.GuestToken, "Other guest")
	require.NoError(t, e)
	before := questionRead(t, cw, other, run)
	req := questionRequest(t, cw, guest, run, "question_submit", "first-question", "Private benign fixture question?")
	out, e := cw.svc.Workflow(ctx, cw.ref, guest.Token, req)
	require.NoError(t, e)
	require.Len(t, out.Questions.Items, 1)
	require.Equal(t, "pending", out.QuestionReceipt.Status)
	own := out.Questions.Items[0]
	require.True(t, own.Own)
	require.True(t, own.CanWithdraw)
	require.False(t, own.CanSelect)
	require.False(t, own.CanDismiss)
	require.False(t, own.CanResolve)
	hidden := questionRead(t, cw, other, run)
	require.Empty(t, hidden.Items)
	require.Nil(t, hidden.Selected)
	require.Equal(t, before.QueueRevision, hidden.QueueRevision)
	host := questionRead(t, cw, *key.result.Session, run)
	require.Len(t, host.Items, 1)
	require.True(t, host.Items[0].CanSelect)
	require.True(t, host.Items[0].CanDismiss)
	require.False(t, host.Items[0].CanResolve)
	replay, e := cw.svc.Workflow(ctx, cw.ref, guest.Token, req)
	require.NoError(t, e)
	require.Equal(t, out.QuestionReceipt, replay.QuestionReceipt)
	req.Question.Text = "Changed intent"
	_, e = cw.svc.Workflow(ctx, cw.ref, guest.Token, req)
	require.ErrorContains(t, e, "questions_idempotency_conflict")
	choose := questionRequest(t, cw, *key.result.Session, run, "question_select", "choose-question", "")
	choose.Question.ID = own.ID
	choose.Question.Revision = 1
	choose.Question.ExpectedEpoch = run.ControlEpoch
	selected, e := cw.svc.Workflow(ctx, cw.ref, key.result.Session.Token, choose)
	require.NoError(t, e)
	require.NotNil(t, selected.Questions.Selected)
	require.True(t, selected.Questions.Selected.CanResolve)
	require.False(t, selected.Questions.Selected.CanSelect)
	visible := questionRead(t, cw, other, run)
	require.Equal(t, own.Text, visible.Selected.Text)
	cw.clock.Advance(time.Second)
	withdraw := questionRequest(t, cw, guest, run, "question_withdraw", "withdraw-question", "")
	withdraw.Question.ID = own.ID
	withdraw.Question.Revision = 2
	withdrawn, e := cw.svc.Workflow(ctx, cw.ref, guest.Token, withdraw)
	require.NoError(t, e)
	require.Equal(t, "withdrawn", withdrawn.QuestionReceipt.Status)
	require.Nil(t, withdrawn.Questions.Selected)
	saved, e := cw.svc.store.question(ctx, cw.ref.OwnerUserID, own.ID)
	require.NoError(t, e)
	require.Empty(t, saved.Body)
	require.EqualValues(t, 3, saved.Revision)
	require.Empty(t, cw.media.mutes())
	state, e := cw.svc.store.workflowState(ctx, cw.ref.OwnerUserID, workflowStateID(cw.ref.SiteID, "run", start.Call.ID))
	require.NoError(t, e)
	require.Equal(t, run.Revision, state.Run.Revision)
}
func TestQuestionsTwoReplicasSelectAtMostOne(t *testing.T) {
	cw, key, c, _, run := questionFixture(t)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host := *key.result.Session
	var ids []string
	for i := range 2 {
		if i > 0 {
			cw.clock.Advance(11 * time.Second)
		}
		out, e := cw.svc.Workflow(ctx, cw.ref, host.Token, questionRequest(t, cw, host, run, "question_submit", fmt.Sprintf("submit-%d", i), "Benign question"))
		require.NoError(t, e)
		ids = append(ids, out.QuestionReceipt.ID)
	}
	cw.clock.Advance(time.Second)
	replica := allowanceReplica(t, cw)
	replica.commerce = func() CommerceConfig { return *c }
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i, svc := range []*Service{cw.svc, replica} {
		ask := questionRequest(t, cw, host, run, "question_select", fmt.Sprintf("select-%d", i), "")
		ask.Question.ID = ids[i]
		ask.Question.Revision = 1
		ask.Question.ExpectedEpoch = run.ControlEpoch
		wg.Add(1)
		go func(svc *Service, r callrules.WorkflowRequest) {
			defer wg.Done()
			_, e := svc.Workflow(ctx, cw.ref, host.Token, r)
			errs <- e
		}(svc, ask)
	}
	wg.Wait()
	close(errs)
	ok := 0
	for e := range errs {
		if e == nil {
			ok++
		} else {
			require.ErrorContains(t, e, "questions_selection_active")
		}
	}
	require.Equal(t, 1, ok)
}

func TestQuestionsRunChangeKeepsOwnCleanupAndStableCallkeyCap(t *testing.T) {
	cw, key, _, start, run := questionFixture(t)
	if cw == nil {
		return
	}
	ctx := context.Background()
	_, guest, e := cw.join(start.Call.GuestToken, "Author")
	require.NoError(t, e)
	submit := questionRequest(t, cw, guest, run, "question_submit", "old-run-question", "Old run question")
	out, e := cw.svc.Workflow(ctx, cw.ref, guest.Token, submit)
	require.NoError(t, e)
	firstID := out.QuestionReceipt.ID
	change := workflowCommand(run, "change", "new-run")
	change.SkillID = "working-session"
	change.Version = 1
	changed := workflowDo(t, cw, key, change)
	require.NotNil(t, changed.Run)
	run = changed.Run
	visible := questionRead(t, cw, guest, run)
	require.Len(t, visible.Items, 1)
	require.Equal(t, 1, visible.OwnOpenCount)
	cw.clock.Advance(time.Second)
	withdraw := questionRequest(t, cw, guest, run, "question_withdraw", "cleanup-old", "")
	withdraw.Question.ID = firstID
	withdraw.Question.Revision = 1
	_, e = cw.svc.Workflow(ctx, cw.ref, guest.Token, withdraw)
	require.NoError(t, e)
	for i := 1; i < 10; i++ {
		cw.clock.Advance(11 * time.Second)
		out, e = cw.svc.Workflow(ctx, cw.ref, guest.Token, questionRequest(t, cw, guest, run, "question_submit", fmt.Sprintf("cap-submit-%d", i), "Question"))
		require.NoError(t, e)
		cw.clock.Advance(time.Second)
		withdraw = questionRequest(t, cw, guest, run, "question_withdraw", fmt.Sprintf("cap-withdraw-%d", i), "")
		withdraw.Question.ID = out.QuestionReceipt.ID
		withdraw.Question.Revision = 1
		_, e = cw.svc.Workflow(ctx, cw.ref, guest.Token, withdraw)
		require.NoError(t, e)
	}
	cw.clock.Advance(11 * time.Second)
	_, e = cw.svc.Workflow(ctx, cw.ref, guest.Token, questionRequest(t, cw, guest, run, "question_submit", "eleventh", "Blocked by stable Callkey limit"))
	require.ErrorContains(t, e, "questions_callkey_limit")
	oldIdentity := submit.Question.Identity
	require.NoError(t, cw.svc.Leave(ctx, guest))
	_, _, e = cw.svc.Join(ctx, cw.ref, &guest, callrules.JoinBody{Token: start.Call.GuestToken, Name: "Returning", Confirm: confirmed})
	require.NoError(t, e)
	newRequest := questionRequest(t, cw, guest, run, "question_submit", "reentry-eleventh", "Still blocked")
	require.NotEqual(t, oldIdentity, newRequest.Question.Identity)
	_, e = cw.svc.Workflow(ctx, cw.ref, guest.Token, newRequest)
	require.ErrorContains(t, e, "questions_callkey_limit")
	_, e = cw.svc.Workflow(ctx, cw.ref, guest.Token, submit)
	require.ErrorContains(t, e, "questions_admission_changed")
}

func TestQuestionsEntitlementLossAllowsOnlyCleanup(t *testing.T) {
	cw, key, c, _, run := questionFixture(t)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host := *key.result.Session
	out, e := cw.svc.Workflow(ctx, cw.ref, host.Token, questionRequest(t, cw, host, run, "question_submit", "before-loss", "Cleanup after unavailable"))
	require.NoError(t, e)
	c.Invalid = true
	view := questionRead(t, cw, host, run)
	require.False(t, view.Available)
	require.False(t, view.CanSubmit)
	require.Len(t, view.Items, 1)
	choose := questionRequest(t, cw, host, run, "question_select", "cannot-select", "")
	choose.Question.ID = out.QuestionReceipt.ID
	choose.Question.Revision = 1
	choose.Question.ExpectedEpoch = run.ControlEpoch
	_, e = cw.svc.Workflow(ctx, cw.ref, host.Token, choose)
	require.ErrorContains(t, e, "questions_insights_required")
	cw.clock.Advance(time.Second)
	withdraw := questionRequest(t, cw, host, run, "question_withdraw", "withdraw-unavailable", "")
	withdraw.Question.ID = out.QuestionReceipt.ID
	withdraw.Question.Revision = 1
	final, e := cw.svc.Workflow(ctx, cw.ref, host.Token, withdraw)
	require.NoError(t, e)
	require.Equal(t, "withdrawn", final.QuestionReceipt.Status)
}

type questionReplyLoss struct {
	Engine
	once bool
}

func (e *questionReplyLoss) Execute(ctx context.Context, q string) (any, error) {
	result, err := e.Engine.Execute(ctx, q)
	if err == nil && !e.once && strings.Contains(q, "saveCallsQuestion(") {
		e.once = true
		return nil, errors.New("synthetic committed reply loss")
	}
	return result, err
}
func TestQuestionsCommittedReplyLossReplaysOneVersionAndNoTextReceipts(t *testing.T) {
	cw, key, _, _, run := questionFixture(t)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host := *key.result.Session
	store := *cw.svc.store
	store.engine = &questionReplyLoss{Engine: store.engine}
	cw.svc.store = &store
	request := questionRequest(t, cw, host, run, "question_submit", "uncertain-submit", "benign unique private question fixture 349")
	_, e := cw.svc.Workflow(ctx, cw.ref, host.Token, request)
	require.Error(t, e)
	out, e := cw.svc.Workflow(ctx, cw.ref, host.Token, request)
	require.NoError(t, e)
	require.EqualValues(t, 1, out.QuestionReceipt.Revision)
	var versions, otherCopies int
	require.NoError(t, cw.db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE concept=? AND id=?`, questionConcept, questionConcept+":"+out.QuestionReceipt.ID).Scan(&versions))
	require.Equal(t, 1, versions)
	require.NoError(t, cw.db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE concept<>? AND payload::text LIKE ?`, questionConcept, "%benign unique private question fixture 349%").Scan(&otherCopies))
	require.Zero(t, otherCopies)
	row, e := cw.svc.store.question(ctx, cw.owner, out.QuestionReceipt.ID)
	require.NoError(t, e)
	metadata, e := json.Marshal(row.State)
	require.NoError(t, e)
	require.NotContains(t, string(metadata), request.Question.Text)
}
func TestQuestionsExplicitAgentScopesAndApprovedVersionVisibility(t *testing.T) {
	cw, key, _, _, run := questionFixture(t)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host := *key.result.Session
	out, e := cw.svc.Workflow(ctx, cw.ref, host.Token, questionRequest(t, cw, host, run, "question_submit", "human-question", "Private host question"))
	require.NoError(t, e)
	cleanupDelegationTestRows(t, cw)
	request := delegationTestRequest(cw)
	request.RoomIDs = []string{run.RoomID}
	request.SkillVersions = []callrules.SkillVersionAuthority{{SkillID: run.DefinitionID, Version: run.DefinitionVersion}}
	request.Scopes = []string{"read", "host", "message", "skills_read", "skills_control", "control"}
	_, grant := delegationTestGrant(t, cw, key, request)
	agent := context.WithValue(ctx, agentCallContext{}, grant)
	read := questionRequest(t, cw, host, run, "questions", "", "")
	_, e = cw.svc.Workflow(agent, cw.ref, host.Token, read)
	require.ErrorContains(t, e, "delegation_scope")
	request.Scopes = []string{"read", "host", "questions", "skills_read", "skills_control"}
	_, grant = delegationTestGrant(t, cw, key, request)
	agent = context.WithValue(ctx, agentCallContext{}, grant)
	choose := questionRequest(t, cw, host, run, "question_select", "agent-needs-control", "")
	choose.Question.ID = out.QuestionReceipt.ID
	choose.Question.Revision = 1
	choose.Question.ExpectedEpoch = run.ControlEpoch
	_, e = cw.svc.Workflow(agent, cw.ref, host.Token, choose)
	require.ErrorContains(t, e, "questions_host_required")
}

func TestQuestionsSelectionRechecksAuthorSession(t *testing.T) {
	for _, mode := range []string{"expired", "logged_out", "reentered"} {
		t.Run(mode, func(t *testing.T) {
			cw, key, _, start, run := questionFixture(t)
			if cw == nil {
				return
			}
			ctx := context.Background()
			_, guest, e := cw.join(start.Call.GuestToken, "Author")
			require.NoError(t, e)
			submitted, e := cw.svc.Workflow(ctx, cw.ref, guest.Token, questionRequest(t, cw, guest, run, "question_submit", "session-question", "Question before authority changes"))
			require.NoError(t, e)
			held, _, e := cw.svc.session(ctx, cw.ref, guest.Token, cw.clock.Now())
			require.NoError(t, e)
			switch mode {
			case "expired":
				require.NoError(t, cw.svc.store.updateAccountSession(ctx, cw.owner, held, map[string]any{"expiresAt": versionStamp(cw.clock.Now())}, cw.clock.Now()))
			case "logged_out":
				require.NoError(t, cw.svc.store.updateAccountSession(ctx, cw.owner, held, map[string]any{"endedAt": versionStamp(cw.clock.Now())}, cw.clock.Now()))
			case "reentered":
				require.NoError(t, cw.svc.Leave(ctx, guest))
				_, _, e = cw.svc.Join(ctx, cw.ref, &guest, callrules.JoinBody{Token: start.Call.GuestToken, Name: "Returned", Confirm: confirmed})
				require.NoError(t, e)
			}
			choose := questionRequest(t, cw, *key.result.Session, run, "question_select", "select-old-author", "")
			choose.Question.ID = submitted.QuestionReceipt.ID
			choose.Question.Revision = 1
			choose.Question.ExpectedEpoch = run.ControlEpoch
			_, e = cw.svc.Workflow(ctx, cw.ref, key.result.Session.Token, choose)
			require.ErrorContains(t, e, "questions_author_unavailable")
			require.Nil(t, questionRead(t, cw, *key.result.Session, run).Selected)
		})
	}
}
func TestQuestionsHumanHostRejectsRevokedGuestAgentAuthor(t *testing.T) {
	cw, host, _, start, run := questionFixture(t)
	if cw == nil {
		return
	}
	ctx := context.Background()
	cleanupDelegationTestRows(t, cw)
	t.Cleanup(func() {
		_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept=?", agentAdmissionConcept).Where("payload->>'siteId'=?", siteConcept+":"+cw.site).Exec(ctx)
	})
	owner := accountRegister(t, cw, newSoftwareAuthenticator(t))
	commerceVerified(t, cw, owner)
	grant := delegationTestRequest(cw)
	grant.Scopes = []string{"read", "join", "message"}
	token, id := delegationTestGrant(t, cw, owner, grant)
	denied := callrules.WorkflowRequest{Action: "questions", Question: &callrules.QuestionRequest{RoomID: start.Call.ID, Identity: "not-admitted", RunID: run.ID}}
	_, e := cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "workflow", Workflow: &denied})
	require.ErrorContains(t, e, "delegation_scope")
	proposalGrant := grant
	proposalGrant.Scopes = append(append([]string{}, grant.Scopes...), "questions")
	proposal := callrules.AgentProposalRequest{Label: "Moderated questions", Grant: proposalGrant}
	offered, e := cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "proposal", IdempotencyKey: "questions-explicit-proposal", Proposal: &proposal})
	require.NoError(t, e)
	pendingProposal := offered.(callrules.AgentProposal)
	require.Contains(t, pendingProposal.Grant.Scopes, "questions")
	_, e = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "workflow", Workflow: &denied})
	require.ErrorContains(t, e, "delegation_scope")
	_, e = cw.svc.Delegation(ctx, cw.ref, owner.result.Session.Token, &callrules.DelegationRequest{Action: "proposal_approve", ProposalID: pendingProposal.ID, ReviewDigest: pendingProposal.ReviewDigest})
	require.NoError(t, e)
	pending := requestGuestAgent(t, cw, token, start, "question-agent-request")
	hostState, e := cw.svc.SessionCall(ctx, *host.result.Session)
	require.NoError(t, e)
	require.Len(t, hostState.PendingAgents, 1)
	require.NoError(t, cw.svc.Host(ctx, *host.result.Session, callrules.HostActionRequest{Action: "agent_approve", RequestID: pending.RequestID, ReviewDigest: hostState.PendingAgents[0].ReviewDigest}))
	joined, e := cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "guest_join", GuestToken: start.Call.GuestToken, CallID: start.Call.ID, RequestID: pending.RequestID, IdempotencyKey: "question-agent-admit", Acceptance: acceptedAgent(t, cw, token)})
	require.NoError(t, e)
	identity := joined.(callrules.JoinResult).Identity
	request := callrules.WorkflowRequest{Action: "question_submit", Question: &callrules.QuestionRequest{RoomID: start.Call.ID, Identity: identity, RunID: run.ID, Text: "Agent's approved question", Publication: callrules.QuestionPublication, IdempotencyKey: "agent-question"}}
	result, e := cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "workflow", Workflow: &request})
	require.NoError(t, e)
	question := result.(callrules.WorkflowResult).QuestionReceipt
	_, e = cw.svc.Delegation(ctx, cw.ref, owner.result.Session.Token, &callrules.DelegationRequest{Action: "revoke", GrantID: id})
	require.NoError(t, e)
	choose := questionRequest(t, cw, *host.result.Session, run, "question_select", "select-revoked-agent", "")
	choose.Question.ID = question.ID
	choose.Question.Revision = 1
	choose.Question.ExpectedEpoch = run.ControlEpoch
	_, e = cw.svc.Workflow(ctx, cw.ref, host.result.Session.Token, choose)
	require.ErrorContains(t, e, "questions_author_unavailable")
}
func TestQuestionsAgentHostSelectsHumanWithoutBorrowingItsGrant(t *testing.T) {
	cw, key, _, start, run := questionFixture(t)
	if cw == nil {
		return
	}
	ctx := context.Background()
	cleanupDelegationTestRows(t, cw)
	_, guest, e := cw.join(start.Call.GuestToken, "Human author")
	require.NoError(t, e)
	submitted, e := cw.svc.Workflow(ctx, cw.ref, guest.Token, questionRequest(t, cw, guest, run, "question_submit", "human-submits", "Human's question"))
	require.NoError(t, e)
	grant := delegationTestRequest(cw)
	grant.Scopes = []string{"read", "host", "questions", "skills_read", "skills_control", "control"}
	grant.RoomIDs = []string{run.RoomID}
	grant.SkillVersions = []callrules.SkillVersionAuthority{{SkillID: run.DefinitionID, Version: run.DefinitionVersion}}
	_, id := delegationTestGrant(t, cw, key, grant)
	d, e := cw.svc.store.delegation(ctx, cw.owner, id)
	require.NoError(t, e)
	agent := context.WithValue(ctx, agentCallContext{}, id)
	agent = context.WithValue(agent, agentApprovalContext{}, delegationApprovalDigest(d))
	agent = context.WithValue(agent, agentScopeContext{}, "questions")
	choose := questionRequest(t, cw, *key.result.Session, run, "question_select", "agent-selects-human", "")
	choose.Question.ID = submitted.QuestionReceipt.ID
	choose.Question.Revision = 1
	choose.Question.ExpectedEpoch = run.ControlEpoch
	out, e := cw.svc.Workflow(agent, cw.ref, key.result.Session.Token, choose)
	require.NoError(t, e)
	require.Equal(t, "Human author", out.Questions.Selected.Author.Name)
}

type questionInventoryCorruption struct {
	Engine
	value any
}

func (e questionInventoryCorruption) Execute(ctx context.Context, q string) (any, error) {
	if strings.Contains(q, "callsQuestionInventory(") {
		return e.value, nil
	}
	return e.Engine.Execute(ctx, q)
}
func TestQuestionsInventoryMustBeCompleteAndWellFormed(t *testing.T) {
	cw, key, _, _, run := questionFixture(t)
	if cw == nil {
		return
	}
	request := questionRequest(t, cw, *key.result.Session, run, "question_submit", "must-not-write", "No partial inventory")
	missing := memql.NewResultWithOutput([]map[string]any{})
	missing.SetHasMore(true)
	malformed := memql.NewResultWithOutput("not a row list")
	for _, value := range []any{(*memql.ExecuteResult)(nil), missing, malformed} {
		store := *cw.svc.store
		store.engine = questionInventoryCorruption{Engine: store.engine, value: value}
		service := *cw.svc
		service.store = &store
		_, e := service.Workflow(context.Background(), cw.ref, key.result.Session.Token, request)
		require.ErrorContains(t, e, "questions_unavailable")
	}
}
func TestQuestionsNativeCursorPagesAndVisibleRevisionBinding(t *testing.T) {
	cw, key, _, _, run := questionFixture(t)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host := *key.result.Session
	submitted, e := cw.svc.Workflow(ctx, cw.ref, host.Token, questionRequest(t, cw, host, run, "question_submit", "paging-seed", "Benign question paging fixture"))
	require.NoError(t, e)
	original, e := cw.svc.store.question(ctx, cw.owner, submitted.QuestionReceipt.ID)
	require.NoError(t, e)
	// Bounded synthetic previous-admission inventory exercises actual engine cursors.
	for i := range 24 {
		q := *original
		q.ID = fmt.Sprintf("question-page-%02d", i)
		q.AuthorKeyID = callrules.HistoryDigest(nil, "fixture-key", fmt.Sprint(i/10))[:32]
		q.AuthorAdmission = callrules.HistoryDigest(nil, "fixture-admission", fmt.Sprint(i))
		q.Version = time.Time{}
		require.NoError(t, cw.svc.store.saveQuestion(ctx, cw.owner, &q, cw.clock.Now()))
	}
	first := questionRead(t, cw, host, run)
	require.Len(t, first.Items, 20)
	require.NotEmpty(t, first.NextCursor)
	read := questionRequest(t, cw, host, run, "questions", "", "")
	read.Question.Cursor = first.NextCursor
	read.Question.QueueRevision = first.QueueRevision
	next, e := cw.svc.Workflow(ctx, cw.ref, host.Token, read)
	require.NoError(t, e)
	require.Len(t, next.Questions.Items, 5)
	cw.clock.Advance(time.Second)
	dismiss := questionRequest(t, cw, host, run, "question_dismiss", "dismiss-paged", "")
	dismiss.Question.ID = first.Items[0].ID
	dismiss.Question.Revision = 1
	dismiss.Question.ExpectedEpoch = run.ControlEpoch
	_, e = cw.svc.Workflow(ctx, cw.ref, host.Token, dismiss)
	require.NoError(t, e)
	_, e = cw.svc.Workflow(ctx, cw.ref, host.Token, read)
	require.ErrorContains(t, e, "questions_changed")
}

func TestQuestionsFamilyExclusionAndNoImplicitSkillRun(t *testing.T) {
	cw, key, start, _ := familyFixture(t, "family-questions-excluded")
	if cw == nil {
		return
	}
	request := callrules.WorkflowRequest{Action: "questions", Question: &callrules.QuestionRequest{RoomID: start.Call.ID, Identity: start.Join.Identity, RunID: "unavailable-run"}}
	_, e := cw.svc.Workflow(context.Background(), cw.ref, key.result.Session.Token, request)
	require.ErrorContains(t, e, "questions_family_unavailable")
}
