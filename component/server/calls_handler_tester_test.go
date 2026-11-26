package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/calls"
)

func TestCallsAnswerForTesterCodePaused(t *testing.T) {
	a := callsAnswerFor(callsOpAccount, &calls.TesterCodePaused{RetryAfter: 14*time.Minute + time.Second}, calls.JoinResult{})
	require.Equal(t, http.StatusTooManyRequests, a.status)
	require.Equal(t, callsErrorReply{Error: "limited", Reason: "tester_code_paused", RetryAfterMs: 841000}, a.body)
	require.True(t, a.known) // known answers are never logged with the request
}

func TestCallsAnswerForTesterCodeIncorrect(t *testing.T) {
	a := callsAnswerFor(callsOpAccount, &calls.TesterCodeIncorrect{AttemptsLeft: 3}, calls.JoinResult{})
	require.Equal(t, http.StatusForbidden, a.status)
	require.Equal(t, callsErrorReply{Error: "forbidden", Reason: "tester_code_incorrect", AttemptsLeft: 3}, a.body)
	require.True(t, a.known)
}

func TestCallsAnswerForTesterCodeReasons(t *testing.T) {
	for reason, status := range map[string]int{
		calls.ReasonTesterAccessUnavailable:   http.StatusForbidden,
		calls.ReasonAutomaticTopupUnavailable: http.StatusForbidden,
		calls.ReasonTesterCodeExpired:         http.StatusForbidden,
	} {
		a := callsAnswerFor(callsOpAccount, calls.Refuse(calls.ErrForbidden, reason, "refused"), calls.JoinResult{})
		require.Equal(t, status, a.status, reason)
		require.Equal(t, reason, a.body.Reason)
	}
	a := callsAnswerFor(callsOpAccount, calls.Refuse(calls.ErrInvalid, calls.ReasonTesterCodeRequired, "missing"), calls.JoinResult{})
	require.Equal(t, http.StatusBadRequest, a.status)
	require.Equal(t, "tester_code_required", a.body.Reason)
}

func TestCallsRefuseTesterCodePausedSetsRetryAfter(t *testing.T) {
	h := &callsHandler{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now}
	rec := httptest.NewRecorder()
	h.refuse(rec, callsCall{route: callsRoute{op: callsOpAccount}}, &calls.TesterCodePaused{RetryAfter: 14*time.Minute + time.Second}, calls.JoinResult{})
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "841", rec.Header().Get("Retry-After"))
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, map[string]any{"error": "limited", "reason": "tester_code_paused", "retryAfterMs": float64(841000)}, body)
}
