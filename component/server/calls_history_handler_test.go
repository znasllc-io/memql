package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/calls"
)

type historyHTTPService struct {
	*callsFakeService
	requests []calls.HistoryRequest
	actions  []calls.HistoryActionRequest
	renames  int
	joins    []calls.JoinBody
}

func (s *historyHTTPService) Join(ctx context.Context, site calls.SiteRef, sess *calls.Session, req calls.JoinBody) (calls.JoinResult, calls.Session, error) {
	s.joins = append(s.joins, req)
	if sess == nil {
		return calls.JoinResult{}, calls.Session{}, calls.ErrNoSession
	}
	resolved, err := s.Session(ctx, site, sess.Token)
	if err != nil {
		return calls.JoinResult{}, calls.Session{}, err
	}
	return s.admission(true), resolved, nil
}

func TestCallsHistoryJoinRequiresCookieAndForwardsOnlyClosedJoinBody(t *testing.T) {
	r, svc := historyHTTPRig(t)
	id := strings.Repeat("a", 64)
	body := callsJSON(t, map[string]any{"historyId": id, "name": "Returned", "confirm": map[string]any{"adult": true, "noMinors": true, "terms": "preview"}})
	require.Equal(t, http.StatusUnauthorized, r.post("/calls/join", body, "").Code)
	require.Empty(t, svc.joins, "a private history id without a cookie never reaches the service")
	require.Equal(t, http.StatusOK, r.post("/calls/join", body, r.secrets.hostSession).Code)
	require.Len(t, svc.joins, 1)
	require.Equal(t, id, svc.joins[0].HistoryID)
	require.Empty(t, svc.joins[0].Token)
	require.Empty(t, svc.joins[0].HostToken)
	require.Equal(t, "Returned", svc.joins[0].Name)
	for _, extra := range []string{"keyId", "roomId", "host"} {
		bad := callsJSON(t, map[string]any{"historyId": id, "name": "Returned", extra: "forged"})
		require.Equal(t, http.StatusBadRequest, r.post("/calls/join", bad, r.secrets.hostSession).Code)
	}
	require.Len(t, svc.joins, 1)
	r.stamp[CallsStampOwnerHeader] = callsTestStranger
	require.Equal(t, http.StatusNotFound, r.post("/calls/join", body, r.secrets.hostSession).Code)
	require.Len(t, svc.joins, 1)
}

func (s *historyHTTPService) History(ctx context.Context, sess calls.Session, req calls.HistoryRequest) (calls.HistoryPage, error) {
	resolved, err := s.Session(ctx, sess.Site, sess.Token)
	if err != nil {
		return calls.HistoryPage{}, err
	}
	s.requests = append(s.requests, req)
	return calls.HistoryPage{AccountID: resolved.KeyID, Calls: []calls.HistoryCall{}, Overview: calls.HistoryOverview{Currency: "USD", PeriodDays: 30, Series: []calls.HistorySeries{}}}, nil
}
func (s *historyHTTPService) HistoryAction(ctx context.Context, sess calls.Session, req calls.HistoryActionRequest) (calls.HistoryActionResult, error) {
	if _, err := s.Session(ctx, sess.Site, sess.Token); err != nil {
		return calls.HistoryActionResult{}, err
	}
	s.actions = append(s.actions, req)
	return calls.HistoryActionResult{DeleteAt: "2026-12-20T00:00:00Z", ConfirmationToken: "opaque-server-confirmation"}, nil
}
func (s *historyHTTPService) RenameCall(ctx context.Context, sess calls.Session, title, identity string) (string, error) {
	if _, err := s.Session(ctx, sess.Site, sess.Token); err != nil {
		return "", err
	}
	s.renames++
	return title, nil
}
func historyHTTPRig(t *testing.T) (*callsRig, *historyHTTPService) {
	r := newCallsRig(t)
	svc := &historyHTTPService{callsFakeService: r.svc}
	r.h = NewCallsHandler(svc, slog.Default(), r.clock.now)
	return r, svc
}
func TestCallsHistoryHTTPRequiresSessionAndForwardsBoundedFilters(t *testing.T) {
	r, svc := historyHTTPRig(t)
	require.Equal(t, http.StatusUnauthorized, r.get("/calls/history", "").Code)
	require.Empty(t, svc.requests)
	require.Equal(t, http.StatusOK, r.get("/calls/history?archive=1&limit=50&period=7&status=cancelled&search=review", r.secrets.hostSession).Code)
	require.Len(t, svc.requests, 1)
	require.True(t, svc.requests[0].Archive)
	require.Equal(t, 7, svc.requests[0].Period)
	require.Equal(t, "cancelled", svc.requests[0].Status)
	for _, query := range []string{"?keyId=other", "?limit=51", "?limit=1&limit=2", "?token=secret"} {
		before := len(r.svc.got)
		require.Equal(t, http.StatusBadRequest, r.get("/calls/history"+query, r.secrets.hostSession).Code)
		require.Len(t, r.svc.got, before)
	}
	r.stamp[CallsStampOwnerHeader] = callsTestStranger
	require.Equal(t, http.StatusNotFound, r.get("/calls/history", r.secrets.hostSession).Code)
	require.Len(t, svc.requests, 1)
}
func TestCallsHistoryHTTPKeepsActionsClosedAndRenameBoundToIdentity(t *testing.T) {
	r, svc := historyHTTPRig(t)
	require.Equal(t, http.StatusOK, r.post("/calls/history", `{"action":"archive_preview","id":"opaque"}`, r.secrets.hostSession).Code)
	require.Len(t, svc.actions, 1)
	for _, body := range []string{`{"action":"archive","id":"opaque","unknown":true}`, `{"action":"rename","id":"opaque","title":7}`} {
		require.Equal(t, http.StatusBadRequest, r.post("/calls/history", body, r.secrets.hostSession).Code)
	}
	require.Equal(t, http.StatusBadRequest, r.post("/calls/host", `{"action":"rename","title":"Review"}`, r.secrets.hostSession).Code)
	identity := strings.Repeat("a", calls.MediaIdentityBytes*8/5)
	require.Equal(t, http.StatusOK, r.post("/calls/host", callsJSON(t, map[string]any{"action": "rename", "title": "Review", "identity": identity}), r.secrets.hostSession).Code)
	require.Equal(t, 1, svc.renames)
	require.Equal(t, http.StatusBadRequest, r.post("/calls/host", callsJSON(t, map[string]any{"action": "rename", "title": "Review", "identity": identity, "seat": "other"}), r.secrets.hostSession).Code)
}
