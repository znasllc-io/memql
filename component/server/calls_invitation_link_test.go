package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/calls"
)

func TestCallsInvitationLinkReturnsOnlyGuestCapabilityWithoutCachingOrLoggingIt(t *testing.T) {
	r := newCallsRig(t)
	identity := callsToken(t, 0x74)[:16]
	body := callsJSON(t, map[string]any{"action": "invitation_link", "identity": identity})
	response := r.post("/calls/host", body, r.secrets.hostSession)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	var answer map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if len(answer) != 1 || answer["guestToken"] != r.secrets.guestLink {
		t.Fatal("answer must carry only the guest invitation capability")
	}
	last := r.svc.last()
	if last.op != "InvitationLink" || last.invitationLink.Identity != identity || last.site.OwnerUserID != callsTestOwner || last.session.Token != r.secrets.hostSession {
		t.Fatal("recovery did not bind identity, checked site and session")
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("an invitation capability may be cached")
	}
	if response.Header().Get("Set-Cookie") != "" {
		t.Fatal("link recovery must preserve the hosting session")
	}
	for name, secret := range r.secrets.all() {
		if strings.Contains(r.logs.String(), secret) {
			t.Fatalf("logged %s", name)
		}
		if secret != r.secrets.guestLink && strings.Contains(response.Body.String(), secret) {
			t.Fatalf("returned %s", name)
		}
	}
	for _, op := range r.svc.ops() {
		if op == "Invite" {
			t.Fatal("recovering a link sent mail")
		}
	}
}

func TestCallsInvitationLinkRejectsMixedOrMalformedRequestsBeforeDispatch(t *testing.T) {
	for _, which := range []string{"missing identity", "empty identity", "null identity", "bad identity", "too long", "email", "token", "seat", "url", "identity on invite", "identity on lock"} {
		t.Run(which, func(t *testing.T) {
			r := newCallsRig(t)
			body := map[string]any{"action": "invitation_link", "identity": callsToken(t, 0x74)[:16]}
			switch which {
			case "missing identity":
				delete(body, "identity")
			case "empty identity":
				body["identity"] = ""
			case "null identity":
				body["identity"] = nil
			case "bad identity":
				body["identity"] = "<script>bad</script>"
			case "too long":
				body["identity"] = strings.Repeat("a", 32)
			case "email":
				body["email"] = "recipient@example.test"
			case "token":
				body["token"] = r.secrets.guestLink
			case "seat":
				body["seat"] = callsTestSeatID
			case "url":
				body["url"] = "https://other.example.test/"
			case "identity on invite":
				body["action"] = "invite"
				body["email"] = "recipient@example.test"
				body["token"] = r.secrets.guestLink
			case "identity on lock":
				body["action"] = "lock"
			}
			response := r.post("/calls/host", callsJSON(t, body), r.secrets.hostSession)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status %d", response.Code)
			}
			if len(r.svc.ops()) != 0 {
				t.Fatal("malformed recovery made a service call")
			}
		})
	}
}

func TestCallsInvitationLinkPreservesAuthorityRefusals(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
	}{
		{"ended", calls.ErrEnded, http.StatusGone},
		{"not host", calls.ErrForbidden, http.StatusForbidden},
		{"expired", calls.ErrNoSession, http.StatusUnauthorized},
		{"gone", calls.ErrNotFound, http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := newCallsRig(t)
			r.svc.refusals["InvitationLink"] = callsFakeRefusal{err: test.err}
			response := r.post("/calls/host", callsJSON(t, map[string]any{"action": "invitation_link", "identity": callsToken(t, 0x74)[:16]}), r.secrets.hostSession)
			if response.Code != test.status {
				t.Fatalf("status %d, want %d", response.Code, test.status)
			}
			if strings.Contains(response.Body.String(), "guestToken") {
				t.Fatal("refused recovery returned a capability")
			}
		})
	}
	r := newCallsRig(t)
	body := callsJSON(t, map[string]any{"action": "invitation_link", "identity": callsToken(t, 0x74)[:16]})
	if response := r.post("/calls/host", body, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("no cookie: %d", response.Code)
	}
	if len(r.svc.ops()) != 0 {
		t.Fatal("a stranger's recovery reached the service")
	}
}
