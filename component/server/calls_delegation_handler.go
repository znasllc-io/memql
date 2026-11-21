package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

type CallsDelegationService interface {
	Delegation(context.Context, calls.SiteRef, string, *calls.DelegationRequest) (calls.DelegationResult, error)
	Agent(context.Context, calls.SiteRef, string, calls.AgentRequest) (any, error)
	AgentManual(context.Context, calls.SiteRef) (any, error)
}
type CallsCommerceWebhookService interface {
	CommerceWebhook(context.Context, calls.SiteRef, string, http.Header, []byte) error
}

func (h *callsHandler) serveDelegated(w http.ResponseWriter, r *http.Request, c callsCall) {
	svc, ok := h.svc.(CallsDelegationService)
	if !ok {
		h.refuse(w, c, errCallsNotHere, calls.JoinResult{})
		return
	}
	var result any
	var err error
	switch c.route.op {
	case callsOpDelegation:
		if !h.requireSession(w, c) {
			return
		}
		var req *calls.DelegationRequest
		if r.Method == http.MethodPost {
			var body calls.DelegationRequest
			if !h.decode(w, r, c, &body) {
				return
			}
			req = &body
		}
		if !h.resolveSite(w, r, &c) {
			return
		}
		result, err = svc.Delegation(r.Context(), c.site, c.cookie.token, req)
	case callsOpAgent:
		token := r.Header.Get(memql.CallsAgentCredentialHeader)
		parts := strings.Split(token, ".")
		if len(parts) != 2 || len(parts[0]) > 128 || !calls.TokenWellFormed(parts[1]) {
			h.refuse(w, c, calls.ErrNoSession, calls.JoinResult{})
			return
		}
		var req calls.AgentRequest
		if !h.decode(w, r, c, &req) {
			return
		}
		if !h.resolveSite(w, r, &c) {
			return
		}
		result, err = svc.Agent(r.Context(), c.site, token, req)
	case callsOpUTCP:
		if !h.resolveSite(w, r, &c) {
			return
		}
		result, err = svc.AgentManual(r.Context(), c.site)
	}
	if err != nil {
		h.refuse(w, c, err, calls.JoinResult{})
		return
	}
	if c.route.op == callsOpAgent {
		result = h.agentReply(result)
	}
	h.write(w, http.StatusOK, result)
}

// agentReply shares the human transport's field names, time units and roster
// filtering. Delegated authority never receives a reusable host-link bearer.
func (h *callsHandler) agentReply(result any) any {
	switch v := result.(type) {
	case calls.StartResult:
		return callsStartReply{Call: callsLinksReply{ID: memql.BareShortId(v.Call.ID), GuestToken: v.Call.GuestToken}, Join: joinReply(v.Join)}
	case calls.ScheduledCall:
		return callsScheduledReply{CallConfiguration: v.Configuration, ID: memql.BareShortId(v.ID), Start: v.Start.UTC().Format(time.RFC3339Nano), End: v.End.UTC().Format(time.RFC3339Nano), Title: v.Title, GuestToken: v.GuestToken, GuestLink: v.GuestLink, ICS: string(v.ICS)}
	case calls.JoinResult:
		return joinReply(v)
	case calls.SeatCall:
		// The native agent result contains call state only, not a human session.
		// Reuse its normalization without inventing an expiry or cookie identity.
		r := h.sessionReply(calls.Session{}, v)
		return callsAgentSessionReply{Reports: r.Reports, Processing: r.Processing, RoomID: memql.BareShortId(v.RoomID), Identity: r.Identity, LeaveToken: r.LeaveToken, CountdownKind: r.CountdownKind, ParticipantLimit: r.ParticipantLimit, DurationMinutes: r.DurationMinutes, Insights: r.Insights, Resources: r.Resources, ReservedCents: r.ReservedCents, CostCents: r.CostCents, Host: r.Host, OpenedAt: r.OpenedAt, Title: r.Title, CountdownEndsAt: r.CountdownEndsAt, AllowanceEndsAt: r.AllowanceEndsAt, FundingMode: r.FundingMode, Roster: r.Roster, Agents: r.Agents}
	case calls.InvitationLinkResult:
		return struct {
			GuestToken string `json:"guestToken"`
		}{v.GuestToken}
	case *calls.StartResult:
		if v != nil {
			return h.agentReply(*v)
		}
	case *calls.ScheduledCall:
		if v != nil {
			return h.agentReply(*v)
		}
	case *calls.JoinResult:
		if v != nil {
			return h.agentReply(*v)
		}
	case *calls.SeatCall:
		if v != nil {
			return h.agentReply(*v)
		}
	case *calls.InvitationLinkResult:
		if v != nil {
			return h.agentReply(*v)
		}
	}
	// Account, quote, history and commerce results already have wire JSON tags.
	return result
}

type callsAgentSessionReply struct {
	Reports          calls.ReportCapabilities `json:"reports"`
	Processing       *calls.ProcessingStatus  `json:"processing,omitempty"`
	Agents           []calls.AgentParticipant `json:"agents,omitempty"`
	RoomID           string                   `json:"roomId,omitempty"`
	Identity         string                   `json:"identity,omitempty"`
	LeaveToken       string                   `json:"leaveToken,omitempty"`
	CountdownKind    string                   `json:"countdownKind,omitempty"`
	ParticipantLimit int                      `json:"participantLimit,omitempty"`
	Insights         bool                     `json:"insights,omitempty"`
	Resources        bool                     `json:"resources,omitempty"`
	DurationMinutes  int                      `json:"durationMinutes,omitempty"`
	ReservedCents    *int64                   `json:"reservedCents,omitempty"`
	CostCents        *int64                   `json:"costCents,omitempty"`
	Host             bool                     `json:"host,omitempty"`
	OpenedAt         string                   `json:"openedAt,omitempty"`
	Title            string                   `json:"title,omitempty"`
	CountdownEndsAt  string                   `json:"countdownEndsAt,omitempty"`
	AllowanceEndsAt  string                   `json:"allowanceEndsAt,omitempty"`
	FundingMode      string                   `json:"fundingMode,omitempty"`
	Roster           *callsRosterReply        `json:"roster,omitempty"`
}

func (h *callsHandler) serveCommerceWebhook(w http.ResponseWriter, r *http.Request, c callsCall) {
	svc, ok := h.svc.(CallsCommerceWebhookService)
	if !ok {
		h.refuse(w, c, errCallsNotHere, calls.JoinResult{})
		return
	}
	provider := "stripe"
	signature := r.Header.Get("Stripe-Signature")
	if c.route.op == callsOpDiditWebhook {
		provider = "didit"
		signature = r.Header.Get("X-Signature-V2") + r.Header.Get("X-Signature")
	}
	if signature == "" || len(signature) > 4096 {
		h.refuse(w, c, calls.ErrForbidden, calls.JoinResult{})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.refuse(w, c, errCallsTooLarge, calls.JoinResult{})
		return
	}
	if !h.resolveSite(w, r, &c) {
		return
	}
	if err = svc.CommerceWebhook(r.Context(), c.site, provider, r.Header, body); err != nil {
		h.refuse(w, c, err, calls.JoinResult{})
		return
	}
	h.write(w, http.StatusOK, map[string]bool{"received": true})
}
