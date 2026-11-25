package calls

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

type questionActor struct {
	session                  sessionRow
	seat                     seatRow
	room                     roomRow
	run                      *callrules.WorkflowRun
	grant                    *delegationRow
	pepper                   []byte
	actor, budget, admission string
}

func questionAdmission(seat seatRow) string {
	return callrules.HistoryDigest(nil, "question-admission", seat.ID, seat.Identity, versionStamp(seat.JoinedAt))
}
func questionOwn(q *questionRow, a questionActor) bool {
	return q.AuthorAdmission == a.admission && q.State.SessionID == a.session.ID && q.AuthorKeyID == a.session.KeyID
}
func questionPinned(a questionActor, skill string, version int) bool {
	return a.grant == nil || delegationAllowsSkill(a.grant, skill, version)
}
func questionHostRead(a questionActor, q *questionRow) bool {
	return a.seat.Host && (a.grant == nil || slices.Contains(a.grant.State.Request.Scopes, "skills_read") && questionPinned(a, q.State.SkillID, q.State.SkillVersion))
}
func questionRoomScope(a questionActor) bool {
	if a.grant == nil {
		return true
	}
	d := a.grant
	if len(d.State.Request.RoomIDs) > 0 {
		return slices.Contains(d.State.Request.RoomIDs, a.room.ID)
	}
	return delegationOwnsSource(d, "room", a.room.ID) || delegationOwnsSource(d, "booking", a.room.ScheduledCallID)
}
func questionModerate(a questionActor) bool {
	return a.seat.Host && questionRoomScope(a) && (a.grant == nil || a.run.Hold != "human" && slices.Contains(a.grant.State.Request.Scopes, "skills_control") && slices.Contains(a.grant.State.Request.Scopes, "control") && questionPinned(a, a.run.DefinitionID, a.run.DefinitionVersion))
}
func questionExpiry(r roomRow) time.Time {
	return minTime(r.OpenedAt.Add(25*time.Hour), r.FundingDeadline.Add(time.Hour))
}
func (s *Service) workflowQuestions(ctx context.Context, site callrules.SiteRef, token string, req callrules.WorkflowRequest) (callrules.WorkflowResult, error) {
	none := callrules.WorkflowResult{ServerNow: s.now()}
	if !callrules.ValidQuestionRequest(req) {
		return none, callrules.ErrInvalid
	}
	first, pepper, e := s.session(ctx, site, token, s.now())
	if e != nil {
		return none, e
	}
	q := req.Question
	firstSeat, firstRoom, e := s.seatAndRoom(ctx, site.OwnerUserID, first)
	if e != nil {
		return none, e
	}
	if firstSeat == nil || firstRoom == nil || firstRoom.ID != q.RoomID || firstSeat.Identity != q.Identity || !firstSeat.occupying() {
		return none, accountRefusal("questions_admission_changed")
	}
	if req.Action == "question_select" && !firstSeat.Host {
		return none, accountRefusal("questions_host_required")
	}
	if e = s.workflowAgentAllowed(ctx, site, first.KeyID, req.Action); e != nil {
		return none, e
	}
	// A fresh agent request does not inherit its old join handler's context.
	// Recover the exact native guest approval from this seat before acquiring the
	// reviewer gates; final checks below reread it under those gates.
	reviewerKey := ""
	if grant := agentContextGrant(ctx); grant != "" {
		if admissionID := seatAgentAdmissionID(firstSeat.AgentData); admissionID != "" {
			journal, e := s.store.agentAdmissions(ctx, site.OwnerUserID, firstRoom.ID)
			if e != nil {
				return none, e
			}
			a := journal.find(admissionID)
			if a == nil || a.GrantID != grant || a.ApprovalDigest != agentContextApproval(ctx) {
				return none, accountRefusal("questions_admission_changed")
			}
			reviewerKey = a.ReviewerKeyID
			ctx = context.WithValue(ctx, guestAgentContext{}, guestAgentContext{RoomID: firstRoom.ID, RequestID: a.ID, GrantID: a.GrantID, ApprovalDigest: a.ApprovalDigest, AdmissionKey: a.AdmissionKey})
		}
	}
	gates := []string{roomGate(q.RoomID), accountGate(first.KeyID), keyGate(first.KeyID)}
	if g := agentContextGrant(ctx); g != "" {
		gates = append(gates, delegationGate(g))
	}
	if reviewerKey != "" {
		gates = append(gates, accountGate(reviewerKey), keyGate(reviewerKey))
	}
	// Selection also fences the submitting admission against credential/grant revocation.
	var selectionTarget *questionRow
	if req.Action == "question_select" {
		if g := agentContextGrant(ctx); g != "" {
			d, e := s.store.delegation(ctx, site.OwnerUserID, g)
			if e != nil {
				return none, e
			}
			if d == nil || !slices.Contains(d.State.Request.Scopes, "control") || !slices.Contains(d.State.Request.Scopes, "skills_control") {
				return none, accountRefusal("questions_host_required")
			}
		}
		selectionTarget, e = s.store.question(ctx, site.OwnerUserID, q.ID)
		if e != nil {
			return none, e
		}
		if selectionTarget == nil || selectionTarget.SiteID != site.SiteID || selectionTarget.RoomID != q.RoomID {
			return none, accountRefusal("questions_not_found")
		}
		gates = append(gates, accountGate(selectionTarget.AuthorKeyID), keyGate(selectionTarget.AuthorKeyID))
		if selectionTarget.State.ReviewerKeyID != "" {
			gates = append(gates, accountGate(selectionTarget.State.ReviewerKeyID), keyGate(selectionTarget.State.ReviewerKeyID))
		}
		if selectionTarget.State.GrantID != "" {
			gates = append(gates, delegationGate(selectionTarget.State.GrantID))
		}
	}
	release, e := s.gates(ctx, gates...)
	if e != nil {
		return none, e
	}
	defer release()
	ctx = memql.ContextWithFreshRead(ctx)
	held, _, e := s.session(ctx, site, token, s.now())
	if e != nil {
		return none, e
	}
	if held.ID != first.ID || held.KeyID != first.KeyID || held.CredentialID != first.CredentialID {
		return none, callrules.ErrNoSession
	}
	seat, room, e := s.seatAndRoom(ctx, site.OwnerUserID, held)
	if e != nil {
		return none, e
	}
	if seat == nil || room == nil || room.ID != q.RoomID || room.SiteID != site.SiteID || seat.Identity != q.Identity || !seat.occupying() || !room.open() {
		return none, accountRefusal("questions_admission_changed")
	}
	if familyRoom(*room) {
		return none, accountRefusal("questions_family_unavailable")
	}
	if e = s.questionCurrentActor(ctx, site, token, *held, *seat, *room, req.Action); e != nil {
		return none, e
	}
	deadline, e := s.departureDeadline(ctx, *room, s.now())
	if e != nil {
		return none, e
	}
	if !deadline.IsZero() && !s.now().Before(deadline) {
		return none, accountRefusal("questions_admission_changed")
	}
	v, e := s.store.workflowState(ctx, site.OwnerUserID, workflowStateID(site.SiteID, "run", room.ID))
	if e != nil {
		return none, e
	}
	if v == nil || v.Run == nil || v.SiteID != site.SiteID || v.KeyID != room.KeyID || v.Run.RoomID != room.ID || v.Run.ID != q.RunID {
		return none, accountRefusal("questions_run_changed")
	}
	a := questionActor{session: *held, seat: *seat, room: *room, run: v.Run, pepper: pepper, admission: questionAdmission(*seat)}
	a.actor = callrules.HistoryDigest(pepper, "question-actor", site.SiteID, held.KeyID, held.ID, a.admission)
	a.budget = callrules.HistoryDigest(pepper, "question-budget", site.SiteID, held.KeyID)
	if g := agentContextGrant(ctx); g != "" {
		a.grant, e = s.store.delegation(ctx, site.OwnerUserID, g)
		if e != nil {
			return none, e
		}
		if a.grant == nil {
			return none, callrules.ErrNoSession
		}
	}
	if selectionTarget != nil {
		current, e := s.store.question(ctx, site.OwnerUserID, selectionTarget.ID)
		if e != nil {
			return none, e
		}
		if current == nil || !current.Version.Equal(selectionTarget.Version) {
			return none, accountRefusal("questions_revision_conflict")
		}
	}
	rows, e := s.store.questionInventory(ctx, site.OwnerUserID, site.SiteID, room.ID)
	if e != nil {
		return none, e
	}
	if req.Action != "questions" {
		receipt, e := s.questionMutate(ctx, site, token, a, rows, req)
		if e != nil {
			return none, e
		}
		none.QuestionReceipt = receipt
		rows, e = s.store.questionInventory(ctx, site.OwnerUserID, site.SiteID, room.ID)
		if e != nil {
			return callrules.WorkflowResult{ServerNow: s.now()}, e
		}
	}
	projection, e := s.questionProjection(ctx, site, a, rows, q, req.Action == "questions")
	if e != nil {
		return callrules.WorkflowResult{ServerNow: s.now()}, e
	}
	if e = s.questionCurrentActor(ctx, site, token, *held, *seat, *room, req.Action); e != nil {
		return callrules.WorkflowResult{ServerNow: s.now()}, e
	}
	none.Questions = projection
	none.ServerNow = s.now()
	return none, nil
}
func (s *Service) questionAvailability(ctx context.Context, site callrules.SiteRef, a questionActor) string {
	if e := s.workflowEntitlement(ctx, site, a.room); e != nil {
		return "questions_insights_required"
	}
	if a.run.Status != "running" && a.run.Status != "paused" {
		return "questions_run_inactive"
	}
	if !s.now().Before(questionExpiry(a.room)) {
		return "questions_expired"
	}
	return ""
}
func (s *Service) questionProjection(ctx context.Context, site callrules.SiteRef, a questionActor, rows []*questionRow, request *callrules.QuestionRequest, continued bool) (*callrules.Questions, error) {
	out := &callrules.Questions{RunID: a.run.ID, Publication: callrules.QuestionPublication, MaxTextLength: 500, MaxOpenPerAdmission: 2, MaxQuestionsPerRoom: 100, MaxQuestionsPerCallkey: 10, ExpiresAt: questionExpiry(a.room), Items: []callrules.QuestionItem{}, CanModerate: questionModerate(a), CanSeeAll: a.seat.Host && (a.grant == nil || slices.Contains(a.grant.State.Request.Scopes, "skills_read"))}
	out.Reason = s.questionAvailability(ctx, site, a)
	out.Available = out.Reason == ""
	ids := []string{}
	scope := []any{a.admission, a.session.ID, a.run.ID, a.run.ControlEpoch, a.run.Hold, a.run.DefinitionID, a.run.DefinitionVersion, out.CanSeeAll, out.CanModerate, out.Available, out.Reason}
	if a.grant != nil {
		scope = append(scope, delegationApprovalDigest(a.grant))
	}
	owned := 0
	for _, q := range rows {
		if q.AuthorKeyID == a.session.KeyID {
			owned++
		}
		if !s.now().Before(q.ExpiresAt) {
			continue
		}
		own := questionOwn(q, a)
		if own && questionActive(q) {
			out.OwnOpenCount++
		}
		visible := own || questionHostRead(a, q) || q.Status == "selected"
		if visible {
			scope = append(scope, []any{q.ID, q.Revision, q.Status})
		}
		if q.Status == "pending" && (own || questionHostRead(a, q)) {
			ids = append(ids, q.ID)
		}
		if q.Status == "selected" {
			if out.Selected != nil {
				return nil, accountRefusal("questions_unavailable")
			}
			full, e := s.store.question(ctx, site.OwnerUserID, q.ID)
			if e != nil {
				return nil, e
			}
			if full == nil || !full.Version.Equal(q.Version) {
				return nil, accountRefusal("questions_changed")
			}
			item := questionItem(full, a, false)
			out.Selected = &item
		}
	}
	raw, _ := json.Marshal(scope)
	out.QueueRevision = callrules.HistoryDigest(a.pepper, "question-visible-inventory", site.SiteID, string(raw))
	if continued && request.Cursor != "" && request.QueueRevision != out.QueueRevision {
		return nil, accountRefusal("questions_changed")
	}
	if out.Available {
		switch {
		case out.OwnOpenCount >= 2:
			out.Reason = "questions_open_limit"
		case owned >= 10:
			out.Reason = "questions_callkey_limit"
		case len(rows) >= 100:
			out.Reason = "questions_room_limit"
		default:
			out.CanSubmit = true
		}
	}
	cursor := ""
	if continued {
		cursor = request.Cursor
	}
	page, next, e := s.store.questionPage(ctx, site.OwnerUserID, ids, cursor)
	if e != nil {
		return nil, e
	}
	for _, q := range page {
		out.Items = append(out.Items, questionItem(q, a, out.Available && out.Selected == nil))
	}
	out.NextCursor = next
	return out, nil
}
func questionItem(q *questionRow, a questionActor, canSelect bool) callrules.QuestionItem {
	moderate := questionModerate(a) && questionPinned(a, q.State.SkillID, q.State.SkillVersion)
	return callrules.QuestionItem{CanSelect: moderate && canSelect && q.Status == "pending", CanDismiss: moderate && questionActive(q), CanResolve: moderate && q.Status == "selected", CanWithdraw: questionOwn(q, a) && questionActive(q), ID: q.ID, RunID: q.RunID, Revision: q.Revision, Status: q.Status, Text: q.Body, Author: q.State.Author, Own: questionOwn(q, a), SubmittedAt: q.SubmittedAt, ExpiresAt: q.ExpiresAt}
}
func (s *Service) questionMutate(ctx context.Context, site callrules.SiteRef, token string, a questionActor, rows []*questionRow, req callrules.WorkflowRequest) (*callrules.QuestionReceipt, error) {
	ask := req.Question
	now := s.now()
	raw, _ := json.Marshal(req)
	digest := callrules.HistoryDigest(a.pepper, "question-request", site.SiteID, a.room.ID, a.actor, string(raw))
	for _, q := range rows {
		for _, op := range q.State.Operations {
			if op.Actor == a.actor && op.Key == ask.IdempotencyKey {
				if op.Digest != digest {
					return nil, accountRefusal("questions_idempotency_conflict")
				}
				if !now.Before(q.ExpiresAt) {
					return nil, accountRefusal("questions_expired")
				}
				return &callrules.QuestionReceipt{ID: q.ID, Revision: op.Revision, Status: op.Status}, nil
			}
		}
	}
	var q *questionRow
	if req.Action == "question_submit" {
		if reason := s.questionAvailability(ctx, site, a); reason != "" {
			return nil, accountRefusal(reason)
		}
		owned, open := 0, 0
		for _, r := range rows {
			if r.AuthorKeyID == a.session.KeyID {
				owned++
			}
			if questionOwn(r, a) && questionActive(r) && now.Before(r.ExpiresAt) {
				open++
			}
		}
		if open >= 2 {
			return nil, accountRefusal("questions_open_limit")
		}
		if owned >= 10 {
			return nil, accountRefusal("questions_callkey_limit")
		}
		if len(rows) >= 100 {
			return nil, accountRefusal("questions_room_limit")
		}
		q = &questionRow{ID: callrules.HistoryDigest(a.pepper, "question-id", site.SiteID, a.room.ID, a.actor, ask.IdempotencyKey)[:32], SiteID: site.SiteID, RoomID: a.room.ID, RunID: a.run.ID, AuthorKeyID: a.session.KeyID, AuthorAdmission: a.admission, Status: "pending", Body: strings.TrimSpace(ask.Text), SubmittedAt: now, RoomDeadline: a.room.FundingDeadline, ExpiresAt: questionExpiry(a.room), State: questionState{Author: callrules.QuestionAuthor{Name: a.seat.DisplayName, Kind: a.seat.Kind, Identity: a.seat.Identity}, SeatID: a.seat.ID, SessionID: a.session.ID, CredentialID: a.session.CredentialID, JoinedAt: a.seat.JoinedAt, SkillID: a.run.DefinitionID, SkillVersion: a.run.DefinitionVersion, Operations: []questionOperation{}}}
		if a.grant != nil {
			q.State.GrantID = a.grant.ID
			q.State.ApprovalDigest = delegationApprovalDigest(a.grant)
			if guest, ok := ctx.Value(guestAgentContext{}).(guestAgentContext); ok {
				journal, e := s.store.agentAdmissions(ctx, site.OwnerUserID, a.room.ID)
				if e != nil {
					return nil, e
				}
				admission := journal.find(guest.RequestID)
				if admission == nil || admission.GrantID != a.grant.ID || admission.ApprovalDigest != q.State.ApprovalDigest {
					return nil, accountRefusal("questions_admission_changed")
				}
				q.State.GuestAdmissionID = admission.ID
				q.State.ReviewerKeyID = admission.ReviewerKeyID
			}
		}
	} else {
		for _, r := range rows {
			if r.ID == ask.ID {
				q = r
			}
		}
		if q == nil || !now.Before(q.ExpiresAt) {
			return nil, accountRefusal("questions_not_found")
		}
		if req.Action == "question_withdraw" {
			if !questionOwn(q, a) {
				return nil, accountRefusal("questions_author_required")
			}
		} else {
			if !questionModerate(a) || !questionPinned(a, q.State.SkillID, q.State.SkillVersion) {
				return nil, accountRefusal("questions_host_required")
			}
			if ask.ExpectedEpoch != a.run.ControlEpoch {
				return nil, accountRefusal("workflow_control_changed")
			}
		}
		if q.Revision != ask.Revision {
			return nil, accountRefusal("questions_revision_conflict")
		}
		if !questionActive(q) {
			return nil, accountRefusal("questions_terminal")
		}
		full, e := s.store.question(ctx, site.OwnerUserID, q.ID)
		if e != nil {
			return nil, e
		}
		if full == nil || !full.Version.Equal(q.Version) {
			return nil, accountRefusal("questions_changed")
		}
		q = full
		switch req.Action {
		case "question_select":
			if reason := s.questionAvailability(ctx, site, a); reason != "" {
				return nil, accountRefusal(reason)
			}
			if q.Status != "pending" {
				return nil, accountRefusal("questions_transition")
			}
			for _, r := range rows {
				if r.Status == "selected" && now.Before(r.ExpiresAt) {
					return nil, accountRefusal("questions_selection_active")
				}
			}
			if e := s.questionAuthorAllowed(ctx, site, a.room, q); e != nil {
				return nil, accountRefusal("questions_author_unavailable")
			}
			q.Status = "selected"
		case "question_withdraw":
			q.Status = "withdrawn"
			q.Body = ""
		case "question_dismiss":
			q.Status = "dismissed"
			q.Body = ""
		case "question_resolve":
			if q.Status != "selected" {
				return nil, accountRefusal("questions_transition")
			}
			q.Status = "resolved"
			q.Body = ""
		}
	}
	// Fixed successful-operation budgets are derived from at most300 receipts.
	recent := 0
	for _, r := range rows {
		for _, op := range r.State.Operations {
			if now.Sub(op.At) < time.Minute {
				recent++
			}
			if op.BudgetKey == a.budget && now.Sub(op.At) < 400*time.Millisecond || req.Action == "question_submit" && r.AuthorKeyID == a.session.KeyID && now.Sub(r.SubmittedAt) < 10*time.Second {
				return nil, accountRefusal("questions_rate_limited")
			}
		}
	}
	if recent >= 60 {
		return nil, accountRefusal("questions_rate_limited")
	}
	q.Revision++
	q.State.Operations = append(q.State.Operations, questionOperation{Key: ask.IdempotencyKey, Digest: digest, Actor: a.actor, BudgetKey: a.budget, Status: q.Status, Revision: q.Revision, At: now})
	if e := s.questionCurrentActor(ctx, site, token, a.session, a.seat, a.room, req.Action); e != nil {
		return nil, e
	}
	if e := s.store.saveQuestion(ctx, site.OwnerUserID, q, now); e != nil {
		return nil, e
	}
	return &callrules.QuestionReceipt{ID: q.ID, Revision: q.Revision, Status: q.Status}, nil
}

// Reconstruct the submitting admission, masking every caller-specific agent value.
// Selection holds author and reviewer authority gates alongside the room gate.
func (s *Service) questionAuthorAllowed(ctx context.Context, site callrules.SiteRef, room roomRow, q *questionRow) error {
	ctx = memql.ContextWithFreshRead(workflowEffectContext{ctx})
	held, e := s.store.sessionByID(ctx, site.OwnerUserID, q.State.SessionID)
	if e != nil {
		return e
	}
	if !held.usable(s.now()) || held.SiteID != site.SiteID || held.KeyID != q.AuthorKeyID || held.SeatID != q.State.SeatID || held.CredentialID != q.State.CredentialID || held.RoomID != room.ID {
		return callrules.ErrNoSession
	}
	author, e := s.store.seatByID(ctx, site.OwnerUserID, q.State.SeatID)
	if e != nil {
		return e
	}
	if author == nil || !author.occupying() || author.RoomID != room.ID || questionAdmission(*author) != q.AuthorAdmission || author.Kind != q.State.Author.Kind || author.Identity != q.State.Author.Identity {
		return callrules.ErrNoSession
	}
	if q.State.GrantID != "" {
		ctx = context.WithValue(ctx, agentCallContext{}, q.State.GrantID)
		ctx = context.WithValue(ctx, agentApprovalContext{}, q.State.ApprovalDigest)
		ctx = context.WithValue(ctx, agentScopeContext{}, "questions")
		if q.State.GuestAdmissionID != "" {
			j, e := s.store.agentAdmissions(ctx, site.OwnerUserID, room.ID)
			if e != nil {
				return e
			}
			a := j.find(q.State.GuestAdmissionID)
			if a == nil || a.ReviewerKeyID != q.State.ReviewerKeyID || a.GrantID != q.State.GrantID || a.ApprovalDigest != q.State.ApprovalDigest {
				return callrules.ErrNoSession
			}
			ctx = context.WithValue(ctx, guestAgentContext{}, guestAgentContext{RoomID: room.ID, RequestID: a.ID, GrantID: a.GrantID, ApprovalDigest: a.ApprovalDigest, AdmissionKey: a.AdmissionKey})
		}
		if e = s.workflowAgentAllowed(ctx, site, q.AuthorKeyID, "questions"); e != nil {
			return e
		}
		return s.agentSeatAllowed(ctx, room, *author)
	}
	if author.Kind != string(callrules.KindPerson) {
		return callrules.ErrNoSession
	}
	return nil
}

func (s *Service) questionCurrentActor(ctx context.Context, site callrules.SiteRef, token string, held sessionRow, seat seatRow, room roomRow, action string) error {
	if e := s.workflowCurrentActor(ctx, site, token, held, seat, room, action); e != nil {
		return e
	}
	if seat.Kind == string(callrules.KindAgent) || seat.Kind == string(callrules.KindAvatar) {
		var binding messageAgentBinding
		if len(seat.AgentData) > 4096 || json.Unmarshal([]byte(seat.AgentData), &binding) != nil || binding.GrantID == "" || binding.GrantID != agentContextGrant(ctx) || !callrules.QuestionDigest(binding.ApprovalDigest) || binding.ApprovalDigest != agentContextApproval(ctx) {
			return accountRefusal("questions_admission_changed")
		}
	}
	return nil
}
