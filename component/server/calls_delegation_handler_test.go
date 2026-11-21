package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

type delegatedHandlerFake struct {
	*callsFakeService
	invoked int
	token   string
	result  any
}

func (f *delegatedHandlerFake) Delegation(context.Context, calls.SiteRef, string, *calls.DelegationRequest) (calls.DelegationResult, error) {
	f.invoked++
	return calls.DelegationResult{}, nil
}
func (f *delegatedHandlerFake) Agent(_ context.Context, _ calls.SiteRef, token string, _ calls.AgentRequest) (any, error) {
	f.invoked++
	f.token = token
	if f.result != nil {
		return f.result, nil
	}
	return map[string]bool{"ok": true}, nil
}
func (f *delegatedHandlerFake) AgentManual(context.Context, calls.SiteRef) (any, error) {
	return calls.UTCPManual("https://calls.example/_memql/calls/agent"), nil
}

func TestMachineRouteDoesNotAcceptHumanSessionOrArbitraryFields(t *testing.T) {
	rig := newCallsRig(t)
	svc := &delegatedHandlerFake{callsFakeService: rig.svc}
	h := NewCallsHandler(svc, nil, rig.clock.now)
	for _, tc := range []struct {
		token, body string
		status      int
	}{
		{"", `{"action":"balance"}`, 401},
		{"grant." + rig.secrets.guestSession, `{"action":"balance","url":"https://other.example"}`, 400},
		{"grant." + rig.secrets.guestSession, `{"action":"balance"}`, 200},
	} {
		req := httptest.NewRequest(http.MethodPost, "/calls/agent", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range rig.stamp {
			req.Header.Set(k, v)
		}
		req.Header.Set(memql.CallsAgentCredentialHeader, tc.token)
		req.AddCookie(&http.Cookie{Name: CallsCookieName, Value: rig.secrets.hostSession})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, tc.status, rec.Code, rec.Body.String())
		require.Empty(t, rec.Header().Get("Set-Cookie"))
	}
	require.Equal(t, 1, svc.invoked)
}

func TestMachineResultsUseHumanWireShapesWithoutHostBearerOrCookie(t *testing.T) {
	at := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	reserved, cost := int64(40), int64(12)
	join := calls.JoinResult{Outcome: calls.OutcomeAdmit, URL: "wss://media.example", Pass: "media-pass", Identity: "agent-seat", Host: true, ExpiresIn: 1500 * time.Millisecond}
	start := calls.StartResult{Call: calls.CallLinks{ID: callsTestRoomID, GuestToken: "guest-shareable", HostToken: "private-host-bearer"}, Join: join}
	config := calls.CallConfiguration{ParticipantLimit: 2, DurationMinutes: 60, RateVersion: "test-rate"}
	for _, tc := range []struct {
		name, action string
		result       any
		want         map[string]any
	}{
		{
			name: "start", action: "start", result: start,
			want: map[string]any{"call": map[string]any{"id": callsTestRoomID, "guestToken": "guest-shareable", "hostToken": ""}, "join": map[string]any{"outcome": "admit", "url": "wss://media.example", "pass": "media-pass", "identity": "agent-seat", "host": true, "expiresInMs": 1500}},
		},
		{
			name: "start pointer", action: "start", result: &start,
			want: map[string]any{"call": map[string]any{"id": callsTestRoomID, "guestToken": "guest-shareable", "hostToken": ""}, "join": map[string]any{"outcome": "admit", "url": "wss://media.example", "pass": "media-pass", "identity": "agent-seat", "host": true, "expiresInMs": 1500}},
		},
		{
			name: "schedule", action: "schedule",
			result: calls.ScheduledCall{Configuration: &config, ID: callsTestRoomID, Start: at, End: at.Add(time.Hour), Title: "Lesson", GuestToken: "guest-shareable", HostToken: "private-host-bearer", GuestLink: "https://calls.example/?g=guest-shareable", HostLink: "https://calls.example/?h=private-host-bearer", ICS: []byte("BEGIN:VCALENDAR\nEND:VCALENDAR")},
			want:   map[string]any{"participantLimit": 2, "durationMinutes": 60, "insights": false, "resources": false, "rateVersion": "test-rate", "id": callsTestRoomID, "start": "2026-11-20T12:00:00Z", "end": "2026-11-20T13:00:00Z", "title": "Lesson", "guestToken": "guest-shareable", "hostToken": "", "guestLink": "https://calls.example/?g=guest-shareable", "hostLink": "", "ics": "BEGIN:VCALENDAR\nEND:VCALENDAR"},
		},
		{
			name: "join", action: "join", result: join,
			want: map[string]any{"outcome": "admit", "url": "wss://media.example", "pass": "media-pass", "identity": "agent-seat", "host": true, "expiresInMs": 1500},
		},
		{
			name: "session", action: "session",
			result: calls.SeatCall{Identity: "agent-seat", LeaveToken: "leave-seat", Host: true, Title: "Lesson", OpenedAt: at, AllowanceEndsAt: at.Add(time.Hour), CountdownEndsAt: at.Add(5 * time.Minute), CountdownKind: "wrap_up", FundingMode: "prepaid", ParticipantLimit: 2, DurationMinutes: 60, ReservedCents: &reserved, CostCents: &cost, Roster: &calls.Roster{InCall: []calls.RosterSeat{{Seat: callsTestSeatID, DisplayName: "Assistant", Kind: "agent", Host: true, Status: "in_call", Identity: "agent-seat"}}}},
			want:   map[string]any{"identity": "agent-seat", "leaveToken": "leave-seat", "host": true, "title": "Lesson", "openedAt": "2026-11-20T12:00:00Z", "allowanceEndsAt": "2026-11-20T13:00:00Z", "countdownEndsAt": "2026-11-20T12:05:00Z", "countdownKind": "wrap_up", "fundingMode": "prepaid", "participantLimit": 2, "durationMinutes": 60, "reservedCents": 40, "costCents": 12, "roster": map[string]any{"locked": false, "inCall": []any{map[string]any{"seat": callsTestSeatID, "displayName": "Assistant", "kind": "agent", "host": true, "status": "in_call", "identity": "agent-seat"}}, "waiting": []any{}}},
		},
		{
			name: "invitation_link", action: "invitation_link", result: calls.InvitationLinkResult{GuestToken: "guest-shareable"},
			want: map[string]any{"guestToken": "guest-shareable"},
		},
		{
			name: "balance keeps tagged account", action: "balance", result: calls.AccountInfo{AccountID: "account", KeyID: "key", KeyLabel: "Primary", CreatedAt: at, BalanceCents: &reserved, CredentialCount: 2, CanHost: true, FundingMode: "prepaid"},
			want: map[string]any{"accountId": "account", "keyId": "key", "keyLabel": "Primary", "createdAt": "2026-11-20T12:00:00Z", "balanceCents": 40, "credentialCount": 2, "unclaimed": false, "legacy": false, "canHost": true, "fundingMode": "prepaid"},
		},
		{
			name: "quote keeps tagged pointer", action: "quote", result: &calls.CallQuote{CallConfiguration: config, QuoteID: "quote", MaxCostCents: 40, RateMicroDollarsPerMinute: 5000, ExpiresAt: at, ExpiresInMs: 30000},
			want: map[string]any{"participantLimit": 2, "durationMinutes": 60, "insights": false, "resources": false, "rateVersion": "test-rate", "quoteId": "quote", "maxCostCents": 40, "rateMicroDollarsPerMinute": 5000, "expiresAt": "2026-11-20T12:00:00Z", "expiresInMs": 30000},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newCallsRig(t)
			svc := &delegatedHandlerFake{callsFakeService: rig.svc, result: tc.result}
			h := NewCallsHandler(svc, nil, rig.clock.now)
			req := httptest.NewRequest(http.MethodPost, "/calls/agent", strings.NewReader(callsJSON(t, map[string]any{"action": tc.action})))
			req.Header.Set("Content-Type", "application/json")
			for k, v := range rig.stamp {
				req.Header.Set(k, v)
			}
			req.Header.Set(memql.CallsAgentCredentialHeader, "grant."+rig.secrets.guestSession)
			req.AddCookie(&http.Cookie{Name: CallsCookieName, Value: rig.secrets.hostSession})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.JSONEq(t, callsJSON(t, tc.want), rec.Body.String())
			require.Empty(t, rec.Header().Get("Set-Cookie"))
			require.NotContains(t, rec.Body.String(), "private-host-bearer")
			require.Equal(t, "private-host-bearer", start.Call.HostToken, "wire conversion must not mutate the native result")
		})
	}
}
