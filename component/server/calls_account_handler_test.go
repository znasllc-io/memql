package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/calls"
)

type accountHandlerFake struct {
	*callsFakeService
	result  calls.AccountResult
	err     error
	request *calls.AccountRequest
	calls   int
}

func (f *accountHandlerFake) Account(_ context.Context, site calls.SiteRef, token string, r *calls.AccountRequest) (calls.AccountResult, error) {
	f.calls++
	f.request = r
	require.Equal(f.t, callsTestSite, site.SiteID)
	return f.result, f.err
}
func accountHandlerRig(t *testing.T) (*callsRig, *accountHandlerFake) {
	rig := newCallsRig(t)
	svc := &accountHandlerFake{callsFakeService: rig.svc, result: calls.AccountResult{Capabilities: calls.AccountCapabilities{Passkeys: true}, Credentials: []calls.AccountCredential{}}}
	rig.h = NewCallsHandler(svc, nil, rig.clock.now)
	return rig, svc
}

func TestCallsAccountChecksStampAndCapsBeforeCeremony(t *testing.T) {
	for _, attack := range []string{"missing_stamp", "forged_owner", "oversized", "unknown_field", "not_json_object"} {
		t.Run(attack, func(t *testing.T) {
			rig, svc := accountHandlerRig(t)
			body := `{"action":"register_begin"}`
			status := http.StatusNotFound
			switch attack {
			case "missing_stamp":
				rig.stamp = map[string]string{}
			case "forged_owner":
				rig.stamp[CallsStampOwnerHeader] = callsTestStranger
			case "oversized":
				body = `{"action":"register_finish","credential":"` + strings.Repeat("x", 65*1024) + `"}`
				status = http.StatusRequestEntityTooLarge
			case "unknown_field":
				body = `{"action":"register_begin","tenantId":"other"}`
				status = http.StatusBadRequest
			case "not_json_object":
				body = `null`
				status = http.StatusBadRequest
			}
			rec := rig.send(http.MethodPost, "/calls/account", body, "")
			require.Equal(t, status, rec.Code, rec.Body.String())
			require.Zero(t, svc.calls)
			require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
		})
	}
}

func TestCallsAccountFundingDecodesOnlyIntegerCentsAndExpectedAccount(t *testing.T) {
	rig, svc := accountHandlerRig(t)
	body := `{"action":"simulate_topup","accountId":"expected-account","amountCents":1000,"idempotencyKey":"operation-00000001"}`
	rec := rig.send(http.MethodPost, "/calls/account", body, rig.secrets.guestSession)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "expected-account", svc.request.AccountID)
	require.EqualValues(t, 1000, svc.request.AmountCents)
	require.Equal(t, "operation-00000001", svc.request.IdempotencyKey)
	for _, amount := range []string{`1000.5`, `"1000"`, `1000000000000000000000000`} {
		rig, svc := accountHandlerRig(t)
		rec := rig.send(http.MethodPost, "/calls/account", strings.Replace(body, `1000`, amount, 1), rig.secrets.guestSession)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.Zero(t, svc.calls)
	}
}
func TestCallsAccountCookieIsHttpOnlyAndNeverInJSON(t *testing.T) {
	rig, svc := accountHandlerRig(t)
	svc.result.Session = &calls.Session{Token: rig.secrets.guestSession}
	rec := rig.send(http.MethodPost, "/calls/account", `{"action":"register_finish","challengeId":"one-use","credential":{}}`, "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), rig.secrets.guestSession)
	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	require.True(t, cookies[0].HttpOnly)
	require.True(t, cookies[0].Secure)
	require.Equal(t, http.SameSiteStrictMode, cookies[0].SameSite)
	require.Equal(t, "/", cookies[0].Path)
	require.Equal(t, "register_finish", svc.request.Action)
	svc.result.Session = nil
	svc.result.ClearSession = true
	rec = rig.send(http.MethodPost, "/calls/account", `{"action":"logout"}`, rig.secrets.guestSession)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, -1, rec.Result().Cookies()[0].MaxAge)
}
func TestCallsAccountSnapshotSupportsAnonymousAndLegacySessions(t *testing.T) {
	rig, svc := accountHandlerRig(t)
	rec := rig.send(http.MethodGet, "/calls/account", "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Nil(t, svc.request)
	require.Contains(t, rec.Body.String(), `"authenticated":false`)
	svc.err = calls.Refuse(calls.ErrForbidden, "legacy_key", "use the legacy route")
	rec = rig.send(http.MethodPost, "/calls/account", `{"action":"login_begin","key":"legacy"}`, "")
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), `"reason":"legacy_key"`)
}
