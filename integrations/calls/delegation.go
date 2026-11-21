package calls

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

type agentCallContext struct{}
type agentScopeContext struct{}
type processingWithdrawalContext struct{}

func delegatedSeatKind(ctx context.Context) string {
	if ctx.Value(agentCallContext{}) != nil {
		return string(callrules.KindAgent)
	}
	return string(callrules.KindPerson)
}
func delegationGate(id string) string { return "calls.delegation." + id }

// Delegation is the human-only authority boundary. An agent cannot reach this
// method with its credential; only a recent user-verified passkey assertion can
// grant spending, saved-method charging, or revoke existing authority.
func (s *Service) Delegation(ctx context.Context, site callrules.SiteRef, token string, req *callrules.DelegationRequest) (callrules.DelegationResult, error) {
	out := callrules.DelegationResult{Grants: []callrules.DelegationInfo{}, Policy: callrules.CurrentAgentPolicy()}
	site, err := checkSite(site)
	if err != nil {
		return out, err
	}
	h, err := s.authenticatedKey(ctx, callrules.Session{Site: site, Token: token})
	if err != nil {
		return out, err
	}
	if req == nil {
		rows, e := s.store.delegations(memql.ContextWithFreshRead(ctx), site.OwnerUserID, site.SiteID, h.row.KeyID)
		if e != nil {
			return out, e
		}
		for _, d := range rows {
			out.Grants = append(out.Grants, delegationInfo(d, s.now()))
		}
		return out, nil
	}
	if req.Action == "portrait_upload" {
		return s.humanPortraitUpload(ctx, site, token, *req)
	}
	if req.PortraitData != "" {
		return out, callrules.ErrInvalid
	}
	if strings.HasPrefix(req.Action, "proposal_") {
		return s.reviewAgentProposal(ctx, site, token, h, *req)
	}
	if req.Action != "create" && req.Action != "revoke" {
		return out, callrules.ErrInvalid
	}
	if req.Action == "create" && !callrules.ValidDelegationApproval(*req, s.now()) {
		return out, callrules.ErrInvalid
	}
	locks := []string{accountGate(h.row.KeyID), keyGate(h.row.KeyID)}
	if req.Action == "revoke" {
		if len(req.GrantID) > 128 || req.GrantID == "" {
			return out, callrules.ErrInvalid
		}
		locks = append(locks, delegationGate(req.GrantID))
	}
	release, err := s.gates(ctx, locks...)
	if err != nil {
		return out, err
	}
	fresh := memql.ContextWithFreshRead(ctx)
	h, k, _, err := s.recentlyVerified(fresh, site, token)
	if err != nil {
		release()
		return out, err
	}
	if !keyActive(k.Status) {
		release()
		return out, accountRefusal("key_unavailable")
	}
	if req.Action == "create" {
		var customerID, paymentMethodID string
		if !s.commerceConfig().enabled() {
			release()
			return out, accountRefusal("commerce_disabled")
		}
		if err = s.commerceAdultAllowed(fresh, site, k.ID); err != nil {
			release()
			return out, err
		}
		if err = s.approvedPortrait(fresh, site, k.ID, *req); err != nil {
			release()
			return out, err
		}
		if req.Topup.Enabled {
			funds, e := s.store.commerceState(fresh, site.OwnerUserID, site.SiteID, k.ID)
			if e != nil {
				release()
				return out, e
			}
			if funds == nil || funds.CustomerID == "" || funds.PaymentMethodID == "" {
				release()
				return out, accountRefusal("saved_payment_method_required")
			}
			customerID, paymentMethodID = funds.CustomerID, funds.PaymentMethodID
		}
		rows, e := s.store.delegations(fresh, site.OwnerUserID, site.SiteID, k.ID)
		if e != nil {
			release()
			return out, e
		}
		if len(rows) >= 100 {
			release()
			return out, accountRefusal("delegation_limit")
		}
		secret, e := callrules.NewToken(s.random, callrules.TokenBytes)
		if e != nil {
			release()
			return out, e
		}
		grantID := id.NewShortId()
		credential := grantID + "." + secret
		d := &delegationRow{ID: grantID, SiteID: site.SiteID, KeyID: k.ID, TokenDigest: callrules.Digest(h.pepper, []byte(credential)), State: delegationState{Request: *req, ApprovalRevision: 1, ApprovalPolicyDigest: callrules.AgentConductDigest, CreatedAt: s.now(), CredentialID: h.row.CredentialID, Operations: []delegatedOperation{}}}
		d.State.CustomerID, d.State.PaymentMethodID = customerID, paymentMethodID
		err = s.store.saveDelegation(fresh, site.OwnerUserID, d, s.now())
		release()
		if err != nil {
			return out, err
		}
		out, err = s.Delegation(ctx, site, token, nil)
		out.Credential = credential
		return out, err
	}
	d, err := s.store.delegation(fresh, site.OwnerUserID, req.GrantID)
	if err != nil {
		release()
		return out, err
	}
	if d == nil || d.SiteID != site.SiteID || d.KeyID != k.ID {
		release()
		return out, callrules.ErrNotFound
	}
	if d.State.RevokedAt.IsZero() {
		d.State.RevokedAt = s.now()
		err = s.store.saveDelegation(fresh, site.OwnerUserID, d, s.now())
	}
	release()
	if err != nil {
		return out, err
	}
	_ = s.ReconcileResources(ctx)
	// Revocation is durable before best-effort media cleanup. Subsequent calls
	// and token renewals fail even if the media provider is temporarily down.
	sess := delegationSession(site, d, h.pepper)
	if err := s.Host(ctx, sess, callrules.HostActionRequest{Action: callrules.HostEnd}); err != nil {
		_ = s.Leave(ctx, sess) // Guest grants cannot end another host's call.
	}
	cancelled := map[string]bool{}
	for _, op := range d.State.Operations {
		if op.BookingID != "" && !cancelled[op.BookingID] {
			cancelled[op.BookingID] = true
			_ = s.Cancel(ctx, sess, op.BookingID)
		}
	}
	return s.Delegation(ctx, site, token, nil)
}

func delegationSession(site callrules.SiteRef, d *delegationRow, pepper []byte) callrules.Session {
	raw, _ := hex.DecodeString(callrules.HistoryDigest(pepper, "private-agent-session", site.SiteID, d.ID))
	token, _ := callrules.NewToken(bytes.NewReader(raw), callrules.TokenBytes)
	return callrules.Session{Site: site, Token: token, KeyID: d.KeyID, Kind: callrules.SessionGuest}
}

func (s *Service) delegationSessionReady(ctx context.Context, site callrules.SiteRef, d *delegationRow, pepper []byte) (callrules.Session, error) {
	sess := delegationSession(site, d, pepper)
	sid := callrules.HistoryDigest(nil, "private-agent-session-row", site.SiteID, d.ID)[:32]
	release, err := s.gates(ctx, delegationGate(d.ID))
	if err != nil {
		return sess, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	current, err := s.store.delegation(fresh, site.OwnerUserID, d.ID)
	if err != nil {
		return sess, err
	}
	if current == nil || !current.State.RevokedAt.IsZero() || !s.now().Before(current.State.Request.ExpiresAt) {
		return sess, accountRefusal("delegation_inactive")
	}
	cred, err := s.store.credentialByID(fresh, site.OwnerUserID, d.State.CredentialID)
	if err != nil {
		return sess, err
	}
	if cred == nil || !cred.RevokedAt.IsZero() || cred.KeyID != d.KeyID {
		return sess, accountRefusal("delegation_credential_revoked")
	}
	row, err := s.store.sessionByID(fresh, site.OwnerUserID, sid)
	if err != nil {
		return sess, err
	}
	expiry := minTime(s.now().Add(callrules.SessionLifetime), d.State.Request.ExpiresAt)
	if row == nil {
		err = s.store.createSession(fresh, site.OwnerUserID, newSession{ID: sid, SiteID: site.SiteID, KeyID: d.KeyID, Kind: callrules.SessionGuest, CredentialID: d.State.CredentialID, TokenDigest: callrules.Digest(pepper, []byte(sess.Token)), ExpiresAt: expiry}, s.now())
	} else if !row.EndedAt.IsZero() {
		return sess, callrules.ErrNoSession
	} else if row.ExpiresAt.Before(expiry.Add(-time.Hour)) {
		err = s.store.updateAccountSession(fresh, site.OwnerUserID, row, map[string]any{"expiresAt": versionStamp(expiry)}, s.now())
	}
	return sess, err
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func agentScope(action string) string {
	switch action {
	case "portrait_upload":
		return "portraits_write"
	case "portrait_get":
		return "read"
	case "authority", "leave":
		return "authority"
	case "request_join", "join_status", "guest_join":
		return "join"
	case "balance", "quote", "session", "history", "proposal", "proposal_status":
		return "read"
	case "start", "join":
		return "host"
	case "schedule", "reschedule", "cancel":
		return "schedule"
	case "host", "invitation_link", "rename", "archive_preview", "archive", "restore":
		return "control"
	case "invite":
		return "invite"
	case "topup":
		return "topup"
	}
	return ""
}

func (s *Service) agentAuthority(ctx context.Context, site callrules.SiteRef, token string) (*delegationRow, []byte, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || len(parts[0]) > 128 || !callrules.TokenWellFormed(parts[1]) {
		return nil, nil, callrules.ErrNoSession
	}
	pepper, err := s.pepperOr(ctx, callrules.ErrNoSession)
	if err != nil {
		return nil, nil, err
	}
	d, err := s.store.delegation(memql.ContextWithFreshRead(ctx), site.OwnerUserID, parts[0])
	if err != nil {
		return nil, nil, err
	}
	if d == nil || d.SiteID != site.SiteID || !hmac.Equal([]byte(d.TokenDigest), []byte(callrules.Digest(pepper, []byte(token)))) {
		return nil, nil, callrules.ErrNoSession
	}
	if !d.State.RevokedAt.IsZero() || !s.now().Before(d.State.Request.ExpiresAt) {
		return nil, nil, accountRefusal("delegation_inactive")
	}
	if err = s.commerceAdultAllowed(ctx, site, d.KeyID); err != nil {
		return nil, nil, err
	}
	return d, pepper, nil
}

// Agent composes exactly the same bounded operations the human surface invokes.
// A persisted reservation precedes every effect. Ambiguous outcomes retain their
// reservation and retry the same operation; no retry creates fresh authority.
func (s *Service) Agent(ctx context.Context, site callrules.SiteRef, token string, req callrules.AgentRequest) (any, error) {
	if req.Action == "portrait_upload" {
		if !reflect.DeepEqual(req, callrules.AgentRequest{Action: req.Action, PortraitData: req.PortraitData}) {
			return nil, callrules.ErrInvalid
		}
	} else if req.Action == "portrait_get" {
		if !reflect.DeepEqual(req, callrules.AgentRequest{Action: req.Action, PortraitID: req.PortraitID}) {
			return nil, callrules.ErrInvalid
		}
	} else if req.PortraitData != "" || req.PortraitID != "" {
		return nil, callrules.ErrInvalid
	}
	site, err := checkSite(site)
	if err != nil {
		return nil, err
	}
	if !s.commerceConfig().enabled() {
		return nil, accountRefusal("commerce_disabled")
	}
	scope := agentRequestScope(req)
	if scope == "" {
		return nil, callrules.ErrInvalid
	}
	d, pepper, err := s.agentAuthority(ctx, site, token)
	if err != nil {
		return nil, err
	}
	withdrawal := req.Action == "processing_consent" && req.Processing != nil && !req.Processing.Safety && !req.Processing.Transcription && !req.Processing.Insights
	if scope != "authority" && !slices.Contains(d.State.Request.Scopes, scope) && !withdrawal {
		return nil, accountRefusal("delegation_scope")
	}
	// A client cannot accept a later proposal by racing its old request through
	// a newly broadened grant. Native effects recheck this exact approval.
	ctx = context.WithValue(ctx, agentApprovalContext{}, delegationApprovalDigest(d))
	sess, err := s.delegationSessionReady(ctx, site, d, pepper)
	if err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, agentCallContext{}, d.ID)
	ctx = context.WithValue(ctx, agentScopeContext{}, scope)
	if withdrawal {
		ctx = context.WithValue(ctx, processingWithdrawalContext{}, true)
	}
	ctx = context.WithValue(ctx, agentPresentationContext{}, callrules.AgentParticipant{Purpose: d.State.Request.Purpose, ApprovalRevision: d.State.ApprovalRevision, ConductVersion: d.State.Request.ConductVersion, PortraitID: d.State.Request.PortraitID, PortraitDigest: d.State.Request.PortraitDigest})
	if req.Action == "authority" {
		return callrules.AgentAuthority{Grant: delegationInfo(d, s.now()), Policy: callrules.CurrentAgentPolicy()}, nil
	}
	if req.Action == "guest_join" || req.Action == "start" || req.Action == "join" || req.Action == "schedule" || req.Action == "reschedule" || req.Action == "topup" {
		if err = checkAgentAcceptance(d, req.Acceptance); err != nil {
			return nil, err
		}
		ctx = context.WithValue(ctx, agentAcceptedContext{}, *req.Acceptance)
	}
	if req.Action == "start" {
		release, e := s.gates(ctx, "calls.agent-admission."+d.ID)
		if e != nil {
			return nil, e
		}
		defer release()
	}
	switch req.Action {
	case "report_submit", "report_status", "report_list", "report_appeal":
		if req.Report == nil || !reflect.DeepEqual(req, callrules.AgentRequest{Action: req.Action, Report: req.Report}) {
			return nil, callrules.ErrInvalid
		}
		return s.report(ctx, site, sess.Token, req.Action, *req.Report)
	case "processing_consent":
		if !reflect.DeepEqual(req, callrules.AgentRequest{Action: req.Action, Processing: req.Processing}) {
			return nil, callrules.ErrInvalid
		}
		err := s.processingConsent(ctx, sess, callrules.HostActionRequest{Action: req.Action, Processing: req.Processing})
		if err != nil {
			return nil, err
		}
		current, e := s.SessionCall(ctx, sess)
		if e != nil {
			return nil, e
		}
		return current.Processing, nil
	case "portrait_upload":
		return s.uploadPortrait(ctx, sess, req.PortraitData)
	case "portrait_get":
		return s.Portrait(ctx, sess, req.PortraitID)
	case "request_join":
		return s.agentRequestJoin(ctx, site, token, d, req, pepper)
	case "join_status":
		return s.agentJoinStatus(ctx, site, d, req)
	case "guest_join":
		return s.agentGuestJoin(ctx, site, token, d, sess, req, pepper)
	case "invite":
		return s.Invite(ctx, sess, *req.Invitation)
	case "proposal", "proposal_status":
		return s.agentProposal(ctx, site, token, d, req)
	case "workflow":
		return s.agentWorkflow(ctx, site, token, d, sess, *req.Workflow)
	case "resource":
		return s.agentResource(ctx, site, token, d, sess, *req.Resource)
	case "balance":
		a, e := s.accountSnapshot(ctx, site, sess.Token)
		if e != nil {
			return nil, e
		}
		return a.Account, nil
	case "quote":
		if req.ParticipantLimit > d.State.Request.MaxParticipants || req.DurationMinutes > d.State.Request.MaxDurationMinutes {
			return nil, accountRefusal("delegation_call_limit")
		}
		a, e := s.quoteCall(ctx, site, sess.Token, callrules.AccountRequest{Action: "quote_call", AccountID: d.KeyID, ParticipantLimit: req.ParticipantLimit, DurationMinutes: req.DurationMinutes, Insights: req.Insights, Resources: req.Resources})
		if e != nil {
			return nil, e
		}
		if a.Quote.MaxCostCents > d.State.Request.MaxCallCents {
			return nil, accountRefusal("delegation_call_limit")
		}
		return a.Quote, nil
	case "session":
		return s.SessionCall(ctx, sess)
	case "join":
		return s.agentJoinAccepted(ctx, site, token, d, sess, req, pepper)
	case "invitation_link":
		current, e := s.SessionCall(ctx, sess)
		if e != nil {
			return nil, e
		}
		return s.InvitationLink(ctx, sess, callrules.InvitationLinkRequest{Identity: current.Identity})
	case "rename", "archive_preview", "archive", "restore":
		row, e := s.store.historyByID(memql.ContextWithFreshRead(ctx), site.OwnerUserID, req.CallID)
		if e != nil {
			return nil, e
		}
		if row == nil || row.KeyID != d.KeyID || row.SiteID != site.SiteID || !delegationOwnsSource(d, row.SourceKind, row.SourceID) {
			return nil, accountRefusal("delegation_call_scope")
		}
		action := callrules.HistoryActionRequest{Action: req.Action, ID: req.CallID, ConfirmationToken: req.ConfirmationToken, Acknowledged: req.Acknowledged}
		if req.Action == "rename" {
			action.Title = &req.Title
		}
		return s.HistoryAction(ctx, sess, action)
	case "history":
		return s.History(ctx, sess, callrules.HistoryRequest{Limit: 50, Period: 30})
	case "host":
		if req.Control == "spotlight" && !reflect.DeepEqual(req, callrules.AgentRequest{Action: req.Action, Control: req.Control, Spotlight: req.Spotlight}) {
			return nil, callrules.ErrInvalid
		}
		return map[string]bool{"ok": true}, s.Host(ctx, sess, callrules.HostActionRequest{Action: req.Control, Seat: req.Seat, Spotlight: req.Spotlight})
	case "leave":
		return map[string]bool{"ok": true}, s.Leave(ctx, sess)
	case "cancel":
		return map[string]bool{"ok": true}, s.Cancel(ctx, sess, req.CallID)
	}
	if !fundingOperationID.MatchString(req.IdempotencyKey) {
		return nil, callrules.ErrInvalid
	}
	if req.Action == "topup" {
		if err = s.recordAgentAcceptance(ctx, site, token, req); err != nil {
			return nil, err
		}
		return s.agentTopup(ctx, site, token, d, sess, req)
	}
	h, err := s.authenticatedKey(ctx, sess)
	if err != nil {
		return nil, err
	}
	config, err := s.readCallQuote(h, req.QuoteID, true)
	if err != nil {
		return nil, err
	}
	if config.ParticipantLimit > d.State.Request.MaxParticipants || config.DurationMinutes > d.State.Request.MaxDurationMinutes {
		return nil, accountRefusal("delegation_call_limit")
	}
	// A quote obtained through the human surface is still account-bound, but
	// cannot authorize an agent's excluded family features or their funding.
	if err = s.familyConfigurationAllowed(ctx, site, d.KeyID, config); err != nil {
		return nil, err
	}
	var confirm callrules.Confirmation
	if req.Action == "start" {
		// Refuse deterministic admission errors before reserving usage or charging
		// the saved payment method. Start still validates these values itself, and
		// the native gates retain their fresh authority checks. A fixed booking
		// grant keeps its existing scope refusal ahead of start-field validation.
		if !d.State.Request.ScheduleStart.IsZero() {
			return nil, accountRefusal("delegation_schedule_scope")
		}
		confirm = callrules.Confirmation{Adult: true, NoMinors: d.State.Request.Participation == "adults_only", Terms: d.State.Request.Terms, Participation: d.State.Request.Participation, AuthorityAccepted: d.State.Request.AuthorityAccepted}
		if (req.Terms != "" && req.Terms != confirm.Terms) || (req.Participation != "" && req.Participation != confirm.Participation) {
			return nil, accountRefusal("delegation_participation_scope")
		}
		if _, err = checkTitle(req.Title); err != nil {
			return nil, err
		}
		if _, _, err = checkJoiner(req.Name, confirm); err != nil {
			return nil, err
		}
	}
	if req.Action != "start" && !d.State.Request.ScheduleStart.IsZero() && !req.Start.Truncate(time.Second).Equal(d.State.Request.ScheduleStart.Truncate(time.Second)) {
		return nil, accountRefusal("delegation_schedule_scope")
	}
	if req.Action == "start" || req.Action == "schedule" || req.Action == "reschedule" {
		if err = s.recordAgentAcceptance(ctx, site, token, req); err != nil {
			return nil, err
		}
	}
	amount := callrules.PreviewReservation(config)
	op, err := s.reserveDelegated(ctx, site, token, req, "usage", amount)
	if err != nil {
		return nil, err
	}
	// Funding is a separate fixed mandate; no amount is taken from the agent.
	if d.State.Request.Topup.Enabled {
		a, e := s.accountSnapshot(ctx, site, sess.Token)
		if e != nil {
			return nil, e
		}
		if a.Account.AvailableCents != nil && *a.Account.AvailableCents < amount {
			top := callrules.AgentRequest{Action: "topup", IdempotencyKey: "automatic-" + op.ID}
			if _, e = s.agentTopup(ctx, site, token, d, sess, top); e != nil {
				return nil, e
			}
		}
	}
	operation := callrules.HistoryDigest(nil, "delegated-effect", d.ID, req.IdempotencyKey)[:32]
	var result any
	var roomID, bookingID string
	switch req.Action {
	case "start":
		r, e := s.Start(ctx, sess, callrules.StartBody{QuoteID: req.QuoteID, IdempotencyKey: operation, Name: req.Name, Title: req.Title, Confirm: confirm})
		if e == nil && (r.Join.Outcome == callrules.OutcomeAdmit || r.Join.Outcome == callrules.OutcomeWait || r.Join.Outcome == callrules.OutcomeQueue) {
			e = s.finishAgentAdmission(ctx, site, d, sess, req)
		}
		r.Call.HostToken = ""
		result, err, roomID = r, e, r.Call.ID
	case "schedule", "reschedule":
		request := callrules.ScheduleRequest{QuoteID: req.QuoteID, IdempotencyKey: operation, Start: req.Start, Minutes: config.DurationMinutes, Title: req.Title}
		var r callrules.ScheduledCall
		if req.Action == "schedule" {
			r, err = s.Schedule(ctx, sess, request)
		} else {
			r, err = s.Reschedule(ctx, sess, req.CallID, request)
		}
		bookingID = r.ID
		r.HostToken, r.HostLink = "", ""
		result = r
	default:
		return nil, callrules.ErrInvalid
	}
	if err != nil {
		return nil, err
	} // Retain uncertain holds; same operation may be retried.
	if err = s.finishDelegated(ctx, site, d.ID, op.ID, roomID, bookingID); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Service) reserveDelegated(ctx context.Context, site callrules.SiteRef, token string, req callrules.AgentRequest, kind string, amount int64) (delegatedOperation, error) {
	d, _, err := s.agentAuthority(ctx, site, token)
	if err != nil {
		return delegatedOperation{}, err
	}
	release, err := s.gates(ctx, delegationGate(d.ID))
	if err != nil {
		return delegatedOperation{}, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	d, _, err = s.agentAuthority(fresh, site, token)
	if err != nil {
		return delegatedOperation{}, err
	}
	if !slices.Contains(d.State.Request.Scopes, agentRequestScope(req)) {
		return delegatedOperation{}, accountRefusal("delegation_scope")
	}
	if kind == "usage" && !d.State.Request.ScheduleStart.IsZero() && (req.Action == "start" || !req.Start.Truncate(time.Second).Equal(d.State.Request.ScheduleStart.Truncate(time.Second))) {
		return delegatedOperation{}, accountRefusal("delegation_schedule_scope")
	}
	raw, _ := json.Marshal(req)
	digest := callrules.HistoryDigest(nil, "delegation-request", string(raw))
	operation := callrules.HistoryDigest(nil, "delegation-operation", d.ID, req.IdempotencyKey)[:32]
	// Reconcile completed call reservations from authoritative immutable settlement
	// receipts. Missing receipts keep their full maximum, including across periods.
	for i := range d.State.Operations {
		o := &d.State.Operations[i]
		if o.Kind == "usage" && o.BookingID != "" && o.RoomID == "" {
			booked, e := s.store.bookingByID(fresh, site.OwnerUserID, o.BookingID)
			if e != nil {
				return delegatedOperation{}, e
			}
			if booked != nil {
				o.RoomID = booked.RoomID
				if booked.status() == callrules.BookingCancelled {
					o.Amount = 0
					o.Status = "settled"
				}
			}
		}
		if o.Kind == "usage" && o.RoomID != "" {
			room, e := s.store.roomByID(fresh, site.OwnerUserID, o.RoomID)
			if e != nil {
				return delegatedOperation{}, e
			}
			if room != nil {
				receipt, e := s.store.simulatedWalletEntryByID(fresh, site.OwnerUserID, usageSettlementID(*room))
				if e != nil {
					return delegatedOperation{}, e
				}
				if receipt != nil && receipt.DebitedCents <= o.Amount {
					o.Amount = receipt.DebitedCents
					o.Status = "settled"
				}
			}
		}
	}
	for _, op := range d.State.Operations {
		if op.ID == operation {
			if op.Digest != digest || op.Kind != kind {
				return delegatedOperation{}, fundingConflict("idempotency_conflict")
			}
			return op, nil
		}
	}
	if len(d.State.Operations) >= 1000 {
		return delegatedOperation{}, accountRefusal("delegation_limit")
	}
	p, t := delegationUsage(d, kind, s.now())
	r := d.State.Request
	if kind == "usage" && r.MaxCalls > 0 {
		n := 0
		for _, o := range d.State.Operations {
			if o.Kind == "usage" {
				n++
			}
		}
		if n >= r.MaxCalls {
			return delegatedOperation{}, accountRefusal("delegation_call_count")
		}
	}
	if kind == "topup" {
		if !r.Topup.Enabled || !slices.Contains(r.Scopes, "topup") {
			return delegatedOperation{}, accountRefusal("delegation_topup_disabled")
		}
		r.MaxCallCents, r.PeriodLimitCents, r.LifetimeLimitCents = r.Topup.PerChargeLimitCents, r.Topup.PeriodLimitCents, r.Topup.LifetimeLimitCents
	}
	if !callrules.WithinDelegationBudget(amount, p, t, r.MaxCallCents, r.PeriodLimitCents, r.LifetimeLimitCents) {
		return delegatedOperation{}, accountRefusal("delegation_budget")
	}
	op := delegatedOperation{ID: operation, Digest: digest, Kind: kind, Period: callrules.DelegationPeriod(s.now()), At: s.now(), Amount: amount, Status: "pending"}
	d.State.Operations = append(d.State.Operations, op)
	err = s.store.saveDelegation(fresh, site.OwnerUserID, d, s.now())
	return op, err
}
func (s *Service) finishDelegated(ctx context.Context, site callrules.SiteRef, grant, operation, room, booking string) error {
	release, err := s.gates(ctx, delegationGate(grant))
	if err != nil {
		return err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	d, err := s.store.delegation(fresh, site.OwnerUserID, grant)
	if err != nil {
		return err
	}
	if d == nil {
		return callrules.ErrNotFound
	}
	for i := range d.State.Operations {
		if d.State.Operations[i].ID == operation {
			if d.State.Operations[i].Status == "settled" {
				return nil
			}
			d.State.Operations[i].Status = "completed"
			if d.State.Operations[i].Kind == "topup" {
				d.State.Operations[i].Status = "settled"
				d.State.Operations[i].Period = callrules.DelegationPeriod(s.now())
			}
			d.State.Operations[i].RoomID = room
			d.State.Operations[i].BookingID = booking
			return s.store.saveDelegation(fresh, site.OwnerUserID, d, s.now())
		}
	}
	return callrules.ErrNotFound
}
func (s *Service) agentTopup(ctx context.Context, site callrules.SiteRef, token string, d *delegationRow, sess callrules.Session, req callrules.AgentRequest) (any, error) {
	if !d.State.Request.Topup.Enabled {
		return nil, accountRefusal("delegation_topup_disabled")
	}
	op, err := s.reserveDelegated(ctx, site, token, req, "topup", d.State.Request.Topup.AmountCents)
	if err != nil {
		return nil, err
	}
	result, err := s.commerceAutoTopup(ctx, site, sess.Token, d.KeyID, d.State.Request.Topup.AmountCents, op.ID)
	if err != nil {
		return nil, err
	}
	if err = s.finishDelegated(ctx, site, d.ID, op.ID, "", ""); err != nil {
		return nil, err
	}
	return result, nil
}

// The start/schedule/payment native gates call this after taking the same payer
// lock as revocation. A request authenticated before revoke cannot create a new
// commitment afterwards. Already-dispatched provider work still reconciles.
func (s *Service) delegatedEffectAllowed(ctx context.Context, site callrules.SiteRef, key string) error {
	grant, ok := ctx.Value(agentCallContext{}).(string)
	if !ok {
		return nil
	}
	d, err := s.store.delegation(memql.ContextWithFreshRead(ctx), site.OwnerUserID, grant)
	if err != nil {
		return err
	}
	if d == nil || d.KeyID != key || d.SiteID != site.SiteID || !d.State.RevokedAt.IsZero() || !s.now().Before(d.State.Request.ExpiresAt) {
		return accountRefusal("delegation_inactive")
	}
	if digest, ok := ctx.Value(agentApprovalContext{}).(string); ok && digest != delegationApprovalDigest(d) {
		return accountRefusal("agent_approval_changed")
	}
	if accepted, ok := ctx.Value(agentAcceptedContext{}).(callrules.AgentAcceptance); ok {
		if err := checkAgentAcceptance(d, &accepted); err != nil {
			return err
		}
	}
	if scope, ok := ctx.Value(agentScopeContext{}).(string); ok && !slices.Contains(d.State.Request.Scopes, scope) && !(scope == "processing" && ctx.Value(processingWithdrawalContext{}) == true) {
		return accountRefusal("delegation_scope")
	}
	return nil
}

// The native start/schedule gates call this against the immutable quote, under
// the payer lock shared by approval/revocation. Checking only Agent's earlier
// snapshot would let an already-running request outlive a narrower approval.
func (s *Service) delegatedConfigurationAllowed(ctx context.Context, site callrules.SiteRef, key string, config callrules.CallConfiguration, start time.Time) error {
	grant, agent := ctx.Value(agentCallContext{}).(string)
	if !agent {
		return nil
	}
	if err := s.delegatedEffectAllowed(ctx, site, key); err != nil {
		return err
	}
	d, err := s.store.delegation(memql.ContextWithFreshRead(ctx), site.OwnerUserID, grant)
	if err != nil {
		return err
	}
	if d == nil {
		return accountRefusal("delegation_inactive")
	}
	r := d.State.Request
	scope := "host"
	if !start.IsZero() {
		scope = "schedule"
	}
	if !slices.Contains(r.Scopes, scope) {
		return accountRefusal("delegation_scope")
	}
	if !r.ScheduleStart.IsZero() && !start.Truncate(time.Second).Equal(r.ScheduleStart.Truncate(time.Second)) {
		return accountRefusal("delegation_schedule_scope")
	}
	if config.ParticipantLimit > r.MaxParticipants || config.DurationMinutes > r.MaxDurationMinutes || callrules.PreviewReservation(config) > r.MaxCallCents {
		return accountRefusal("delegation_call_limit")
	}
	return nil
}
func (s *Service) delegatedTopupAllowed(ctx context.Context, site callrules.SiteRef, key string, amount, available int64) error {
	grant, ok := ctx.Value(agentCallContext{}).(string)
	if !ok {
		return accountRefusal("delegation_required")
	}
	if err := s.delegatedEffectAllowed(ctx, site, key); err != nil {
		return err
	}
	d, err := s.store.delegation(memql.ContextWithFreshRead(ctx), site.OwnerUserID, grant)
	if err != nil {
		return err
	}
	if d == nil || !d.State.Request.Topup.Enabled || d.State.Request.Topup.AmountCents != amount || available > d.State.Request.Topup.ThresholdCents {
		return accountRefusal("topup_threshold_not_reached")
	}
	funds, err := s.store.commerceState(memql.ContextWithFreshRead(ctx), site.OwnerUserID, site.SiteID, key)
	if err != nil {
		return err
	}
	if funds == nil || d.State.CustomerID == "" || d.State.PaymentMethodID == "" || d.State.CustomerID != funds.CustomerID || d.State.PaymentMethodID != funds.PaymentMethodID {
		return accountRefusal("payment_method_changed")
	}
	return nil
}

func (s *Service) agentJoin(ctx context.Context, site callrules.SiteRef, d *delegationRow, sess callrules.Session, req callrules.AgentRequest, pepper []byte) (any, error) {
	if req.CallID == "" || len(req.CallID) > 128 {
		return nil, callrules.ErrInvalid
	}
	var reservation *delegatedOperation
	for i := range d.State.Operations {
		if d.State.Operations[i].BookingID == req.CallID {
			reservation = &d.State.Operations[i]
		}
	}
	if reservation == nil {
		return nil, accountRefusal("delegation_booking_scope")
	}
	booked, err := s.store.bookingByID(memql.ContextWithFreshRead(ctx), site.OwnerUserID, req.CallID)
	if err != nil {
		return nil, err
	}
	if booked == nil || booked.KeyID != d.KeyID || booked.SiteID != site.SiteID || booked.DelegationID != d.ID {
		return nil, callrules.ErrNotFound
	}
	config, err := decodeUsageConfig(booked.UsageConfig)
	if err != nil {
		return nil, err
	}
	if config.ParticipantLimit > d.State.Request.MaxParticipants || config.DurationMinutes > d.State.Request.MaxDurationMinutes || callrules.PreviewReservation(config) > reservation.Amount {
		return nil, accountRefusal("delegation_booking_changed")
	}
	links := configuredLinks(pepper, booked.UsageOperation)
	grant := d.State.Request
	confirm := callrules.Confirmation{Adult: true, NoMinors: grant.Participation == "adults_only", Terms: grant.Terms, Participation: grant.Participation, AuthorityAccepted: grant.AuthorityAccepted}
	result, _, err := s.Join(ctx, site, &sess, callrules.JoinBody{HostToken: links.HostToken, Name: req.Name, Confirm: confirm})
	return result, err
}

func delegationOwnsSource(d *delegationRow, kind, source string) bool {
	for _, op := range d.State.Operations {
		if source != "" && ((kind == "booking" && op.BookingID == source) || (kind == "room" && op.RoomID == source)) {
			return true
		}
	}
	return false
}

// Native booking mutations check this under their booking gate. An agent can
// manage only bookings still assigned to its grant, never another grant's or a
// human's booking merely because both spend the same private account balance.
func delegatedBookingAllowed(ctx context.Context, b *bookingRow) error {
	if grant, ok := ctx.Value(agentCallContext{}).(string); ok && (b == nil || b.DelegationID != grant) {
		return accountRefusal("delegation_booking_scope")
	}
	return nil
}
