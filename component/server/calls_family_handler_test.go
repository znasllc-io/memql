package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

type familyHandlerFake struct {
	*callsFakeService
	familyCalls   int
	familySite    calls.SiteRef
	familyToken   string
	familyRequest calls.FamilyRequest
	familyResult  calls.FamilyResult
	familyErr     error
	sessionCalls  int
	sessionToken  string
	familySession *calls.FamilySession
	sessionErr    error
	familyJoin    calls.JoinBody
	joinSession   *calls.Session
}

func (f *familyHandlerFake) Family(_ context.Context, site calls.SiteRef, token string, request calls.FamilyRequest) (calls.FamilyResult, error) {
	f.familyCalls++
	f.familySite, f.familyToken, f.familyRequest = site, token, request
	return f.familyResult, f.familyErr
}
func (f *familyHandlerFake) FamilySession(_ context.Context, _ calls.SiteRef, token string) (*calls.FamilySession, error) {
	f.sessionCalls++
	f.sessionToken = token
	return f.familySession, f.sessionErr
}
func (f *familyHandlerFake) Join(ctx context.Context, site calls.SiteRef, held *calls.Session, request calls.JoinBody) (calls.JoinResult, calls.Session, error) {
	if !request.Family {
		return f.callsFakeService.Join(ctx, site, held, request)
	}
	f.familyJoin, f.joinSession = request, held
	if held == nil {
		return calls.JoinResult{}, calls.Session{}, calls.ErrNoSession
	}
	return calls.JoinResult{Outcome: calls.OutcomeWait, Reason: "family_consent_required", RetryAfter: 3 * time.Second}, *held, nil
}
func newFamilyHandlerRig(t *testing.T) (*callsRig, *familyHandlerFake) {
	t.Helper()
	rig := newCallsRig(t)
	fake := &familyHandlerFake{callsFakeService: rig.svc, familyResult: calls.FamilyResult{Mode: "mock", Status: "pending", RetryAfter: 3}}
	rig.h = NewCallsHandler(fake, slog.New(slog.NewTextHandler(rig.logs, nil)), rig.clock.now)
	return rig, fake
}

func TestFamilyHandlerExactRouteAndSiteAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
		withoutStamp       bool
		foreignOrigin      bool
	}{
		{"get is not the family protocol", http.MethodGet, "/calls/family", false, false},
		{"put is not the family protocol", http.MethodPut, "/calls/family", false, false},
		{"trailing slash is not exempt", http.MethodPost, "/calls/family/", false, false},
		{"child path is not exempt", http.MethodPost, "/calls/family/approve", false, false},
		{"direct request has no trusted site stamp", http.MethodPost, "/calls/family", true, false},
		{"foreign direct request cannot manufacture a site", http.MethodPost, "/calls/family", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, fake := newFamilyHandlerRig(t)
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"action":"status"}`))
			req.Header.Set("Content-Type", "application/json")
			if !tc.withoutStamp {
				for k, v := range rig.stamp {
					req.Header.Set(k, v)
				}
			}
			if tc.foreignOrigin {
				req.Header.Set("Origin", "https://foreign.example")
			}
			rec := httptest.NewRecorder()
			rig.h.ServeHTTP(rec, req)
			require.Equal(t, http.StatusNotFound, rec.Code)
			require.Zero(t, fake.familyCalls)
			require.Empty(t, rig.svc.got, "unrecognized or unstamped requests must not read a site")
			require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
		})
	}
	t.Run("an installation without the family capability refuses the route", func(t *testing.T) {
		rig := newCallsRig(t)
		rec := rig.post("/calls/family", `{"action":"status"}`, rig.secrets.hostSession)
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Empty(t, rig.svc.got)
	})
	t.Run("the stamped owner must own the resolved deployable", func(t *testing.T) {
		rig, fake := newFamilyHandlerRig(t)
		rig.stamp[CallsStampOwnerHeader] = callsTestMember
		rec := rig.post("/calls/family", `{"action":"list"}`, rig.secrets.hostSession)
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Zero(t, fake.familyCalls)
	})
}

func TestFamilyHandlerRejectsMalformedBodiesBeforeReadingState(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"unknown consent authority field", `{"action":"mock_approve","isGuardian":true}`, http.StatusBadRequest},
		{"null cannot approve", `null`, http.StatusBadRequest},
		{"array cannot approve", `[]`, http.StatusBadRequest},
		{"trailing object", `{"action":"status"}{}`, http.StatusBadRequest},
		{"wrong typed authority", `{"action":"mock_approve","parentalAuthority":"yes"}`, http.StatusBadRequest},
		{"truncated object", `{"action":`, http.StatusBadRequest},
		{"body cap", `{"action":"status","invitationId":"` + strings.Repeat("x", int(memql.CallsSmallBodyBytes)) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, fake := newFamilyHandlerRig(t)
			rec := rig.post("/calls/family", tc.body, rig.secrets.hostSession)
			require.Equal(t, tc.status, rec.Code)
			require.Zero(t, fake.familyCalls)
			require.Empty(t, rig.svc.got)
			assertNoCallsCookie(t, rec)
		})
	}
	t.Run("browser form encodings cannot reach the ceremony", func(t *testing.T) {
		rig, fake := newFamilyHandlerRig(t)
		req := httptest.NewRequest(http.MethodPost, "/calls/family", strings.NewReader(`{"action":"mock_approve"}`))
		for k, v := range rig.stamp {
			req.Header.Set(k, v)
		}
		req.Header.Set("Content-Type", "text/plain")
		rec := httptest.NewRecorder()
		rig.h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
		require.Zero(t, fake.familyCalls)
		require.Empty(t, rig.svc.got)
	})
}

func TestFamilyHandlerClaimSetsOnlyPrivateSessionCookie(t *testing.T) {
	for _, partialFailure := range []bool{false, true} {
		name := "success"
		if partialFailure {
			name = "session written before a retryable failure"
		}
		t.Run(name, func(t *testing.T) {
			rig, fake := newFamilyHandlerRig(t)
			fake.familyResult.Session = &calls.Session{Token: rig.secrets.guestSession, Kind: calls.SessionGuest}
			fake.familyResult.Invitation = &calls.FamilyInvitation{ID: "invitation-one", Status: "pending", Claimed: true, ExpiresAt: callsT0.Add(30 * time.Minute)}
			if partialFailure {
				fake.familyErr = errors.New("the consent journal could not be saved")
			}
			rec := rig.post("/calls/family", callsJSON(t, calls.FamilyRequest{Action: "claim", InvitationToken: rig.secrets.guestLink}), "")
			if partialFailure {
				require.Equal(t, http.StatusBadGateway, rec.Code)
			} else {
				require.Equal(t, http.StatusOK, rec.Code)
				require.Contains(t, rec.Body.String(), `"status":"pending"`)
			}
			require.True(t, callsCookieFrom(t, rec) == rig.secrets.guestSession, "the committed session must survive a retry")
			require.False(t, strings.Contains(rec.Body.String(), rig.secrets.guestSession), "the session bearer belongs only in the HttpOnly cookie")
			require.NotContains(t, rec.Body.String(), `"Session"`)
			require.NotContains(t, rec.Body.String(), `"session"`)
			require.False(t, strings.Contains(rig.logs.String(), rig.secrets.guestSession), "no bearer in logs")
			require.False(t, strings.Contains(rig.logs.String(), rig.secrets.guestLink), "no invitation bearer in logs")
			require.Empty(t, fake.familyToken)
			require.Equal(t, 1, fake.familyCalls)
		})
	}
}

func TestFamilyHandlerPassesGuardianAdmissionBinding(t *testing.T) {
	rig, fake := newFamilyHandlerRig(t)
	request := calls.FamilyRequest{Action: "mock_approve", RoomID: callsTestRoomID, Identity: "guardian-admission", InvitationID: "invitation-one", IdempotencyKey: "family-approval-0001", ConsentVersion: calls.FamilyMockConsentVersion, ParentalAuthority: true} // gitleaks:allow -- public idempotency fixture, not an authorization credential.
	rec := rig.post("/calls/family", callsJSON(t, request), rig.secrets.hostSession)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, request, fake.familyRequest)
	require.Equal(t, calls.SiteRef{SiteID: callsTestSite, OwnerUserID: callsTestOwner, Deployable: callsTestDeployable}, fake.familySite)
	require.True(t, fake.familyToken == rig.secrets.hostSession, "the cookie is passed only to the native authority boundary")
	assertNoCallsCookie(t, rec)
}

func TestFamilyHandlerSessionRestoresPendingChildWithoutASeat(t *testing.T) {
	rig, fake := newFamilyHandlerRig(t)
	pending := calls.Session{Kind: calls.SessionGuest, KeyID: "child-key", ExpiresAt: callsT0.Add(time.Hour), Site: calls.SiteRef{SiteID: callsTestSite, OwnerUserID: callsTestOwner, Deployable: callsTestDeployable}}
	rig.svc.sessions[rig.secrets.guestSession] = pending
	fake.familySession = &calls.FamilySession{Mode: "mock", Role: "child", Status: "pending", InvitationID: "invitation-one"}
	rec := rig.get("/calls/session", rig.secrets.guestSession)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, fake.sessionCalls)
	require.True(t, fake.sessionToken == rig.secrets.guestSession)
	var reply map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reply))
	require.Equal(t, map[string]any{"mode": "mock", "role": "child", "status": "pending", "invitationId": "invitation-one"}, reply["family"])
	require.NotContains(t, reply, "seatId")
	require.NotContains(t, reply, "roomId")
	require.NotContains(t, reply, "host")
	require.NotContains(t, rig.svc.ops(), "SessionCall", "a pending child has no media seat to inspect")
	require.False(t, strings.Contains(rec.Body.String(), rig.secrets.guestSession))
	assertNoCallsCookie(t, rec)
}

func TestFamilyHandlerJoinCarriesCookieBoundInvitation(t *testing.T) {
	rig, fake := newFamilyHandlerRig(t)
	body := `{"family":true,"familyInvitationId":"invitation-one","name":"Guest"}`
	rec := rig.post("/calls/join", body, rig.secrets.guestSession)
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, fake.familyJoin.Family)
	require.Equal(t, "invitation-one", fake.familyJoin.FamilyInvitationID)
	require.Equal(t, "Guest", fake.familyJoin.Name)
	require.Empty(t, fake.familyJoin.FamilyToken)
	require.Equal(t, calls.Confirmation{}, fake.familyJoin.Confirm, "transport must not invent an adult declaration for a child")
	require.NotNil(t, fake.joinSession)
	require.True(t, fake.joinSession.Token == rig.secrets.guestSession)
	require.Contains(t, rec.Body.String(), `"outcome":"wait"`)
	assertNoCallsCookie(t, rec)
}

func TestFamilyHandlerRefusedSessionDoesNotExposeFamilyDetails(t *testing.T) {
	rig, fake := newFamilyHandlerRig(t)
	fake.familySession = &calls.FamilySession{Mode: "mock", Role: "child", Status: "approved", InvitationID: "private-invitation"}
	fake.sessionErr = calls.Refuse(calls.ErrForbidden, "family_state_unavailable", "private state must not be returned")
	rec := rig.get("/calls/session", rig.secrets.hostSession)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.NotContains(t, rec.Body.String(), "private-invitation")
	require.NotContains(t, rec.Body.String(), `"approved"`)
	require.NotContains(t, rec.Body.String(), `"family":`)
}
