package web

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity"
)

type joiningSettings struct{ current Settings }

func TestSetupInitiallySelectsInvitationOnly(t *testing.T) {
	_, mux := nativeTestServer(t)
	response := nativeGET(mux, "/setup")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"PrefillMode":"invite_only"`) {
		t.Fatalf("initial setup must select invitation only: %s", response.Body.String())
	}
}

func (s *joiningSettings) Snapshot(context.Context) Settings { return s.current }

func TestLoginUsesEditedJoiningPolicyWithoutRestart(t *testing.T) {
	s, _, _ := newInviteStageServer(t)
	s.Cfg.RegistrationMode = identity.RegistrationModeOpen
	s.Cfg.RegistrationDomains = []string{"old.test"}
	live := &joiningSettings{}
	s.Live = live
	for _, test := range []struct{ mode, domain, email, stage string }{
		{"invite_only", "", "new@team.test", "needs_invite"},
		{"waitlist", "", "new@team.test", "waitlist_signup"},
		{"domain_restricted", "team.test", "new@team.test", ""},
		{"domain_restricted", "team.test", "new@old.test", "waitlist_signup"},
		{"open", "", "new@any.test", ""},
	} {
		live.current = Settings{RegistrationMode: identity.RegistrationMode(test.mode), RegistrationDomains: test.domain}
		page, existing := s.routeLoginEmail(httptest.NewRequest("GET", "/login", nil), test.email, "")
		stage := ""
		if page != nil {
			stage = page.Stage
		}
		if stage != test.stage || existing {
			t.Errorf("%+v: stage %q existing %v", test, stage, existing)
		}
	}
}

func TestAccessRequestReceivesReceiptInsteadOfMailPolling(t *testing.T) {
	s, captured, _ := newInviteStageServer(t)
	rec := postLogin(t, s, url.Values{"form": {"waitlist"}, "email": {"person@team.test"}, "name": {"Applicant"}, "additional_context": {"Join our team"}})
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Query().Get("action") != "access_request_created" || !captured.IsAccessRequest || captured.WaitlistName != "Applicant" || captured.WaitlistContext != "Join our team" {
		t.Fatalf("missing request receipt or applicant details: location=%s captured=%+v", location, captured)
	}
}
