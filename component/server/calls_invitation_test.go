package server

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/calls"
)

// Invitation mail extends only the existing host surface. No public relay, template,
// sender, host capability or mail-provider error is exposed by this boundary.
func TestCallsInvitationUsesHostSurfaceAndNamesNoCredentialInReply(t *testing.T) {
	r := newCallsRig(t)
	email := "invitee@example.test"
	response := r.post("/calls/host", callsJSON(t, map[string]any{
		"action": "invite", "email": email, "token": r.secrets.guestLink,
	}), r.secrets.hostSession)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"status":"captured","duplicate":false}` {
		t.Fatalf("invitation answer: %d %s", response.Code, response.Body.String())
	}
	last := r.svc.last()
	if last.op != "Invite" || last.invite.Email != email || last.invite.Token != r.secrets.guestLink || last.site.OwnerUserID != callsTestOwner {
		t.Fatal("invitation was not bound to the checked site and session")
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("invitation receipt may be cached")
	}
	for _, secret := range r.secrets.all() {
		if strings.Contains(response.Body.String(), secret) || strings.Contains(r.logs.String(), secret) {
			t.Fatal("credential escaped")
		}
	}
	if strings.Contains(r.logs.String(), email) {
		t.Fatal("recipient escaped into logs")
	}
}

func TestCallsInvitationRefusesMixedSchemasBeforeDispatch(t *testing.T) {
	for _, which := range []string{"missing email", "missing token", "seat on invite", "email on control", "token on control", "custom sender", "custom html", "malformed token"} {
		t.Run(which, func(t *testing.T) {
			r := newCallsRig(t)
			body := map[string]any{"action": "invite", "email": "invitee@example.test", "token": r.secrets.guestLink}
			switch which {
			case "missing email":
				delete(body, "email")
			case "missing token":
				delete(body, "token")
			case "seat on invite":
				body["seat"] = "seat1"
			case "email on control":
				body["action"] = "lock"
				delete(body, "token")
			case "token on control":
				body["action"] = "lock"
				delete(body, "email")
			case "custom sender":
				body["from"] = "someone@example.test"
			case "custom html":
				body["html"] = "<script>bad</script>"
			case "malformed token":
				body["token"] = "bad"
			}
			response := r.post("/calls/host", callsJSON(t, body), r.secrets.hostSession)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status %d", response.Code)
			}
			for _, op := range r.svc.ops() {
				if op == "Invite" || op == "Host" {
					t.Fatal("malformed action reached effect")
				}
			}
		})
	}
}

func TestCallsInvitationRequiresSessionAndHidesDeliveryErrors(t *testing.T) {
	r := newCallsRig(t)
	body := callsJSON(t, map[string]any{"action": "invite", "email": "invitee@example.test", "token": r.secrets.guestLink})
	if got := r.post("/calls/host", body, ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("no session: %d", got.Code)
	}
	r.svc.refusals["Invite"] = callsFakeRefusal{err: errors.Join(calls.ErrInvitationUnavailable, errors.New("private provider details"))}
	got := r.post("/calls/host", body, r.secrets.hostSession)
	if got.Code != http.StatusServiceUnavailable || strings.Contains(got.Body.String(), "private") || strings.Contains(r.logs.String(), "private") {
		t.Fatalf("delivery refusal was not scrubbed: %d", got.Code)
	}
}

func TestCallsInvitationLimitsKeepTheirActualWait(t *testing.T) {
	r := newCallsRig(t)
	body := callsJSON(t, map[string]any{"action": "invite", "email": "invitee@example.test", "token": r.secrets.guestLink})
	r.svc.refusals["Invite"] = callsFakeRefusal{err: &calls.InvitationLimit{RetryAfter: 2*time.Hour + 100*time.Millisecond}}
	got := r.post("/calls/host", body, r.secrets.hostSession)
	if got.Code != http.StatusTooManyRequests || got.Header().Get("Retry-After") != "7201" || !strings.Contains(got.Body.String(), `"retryAfterMs":7200100`) {
		t.Fatalf("timed limit: %d %s %s", got.Code, got.Header().Get("Retry-After"), got.Body.String())
	}
	r.svc.refusals["Invite"] = callsFakeRefusal{err: calls.Refuse(calls.ErrConflict, "invitation_full", "room has used its invitations")}
	got = r.post("/calls/host", body, r.secrets.hostSession)
	if got.Code != http.StatusConflict || got.Header().Get("Retry-After") != "" || !strings.Contains(got.Body.String(), `"reason":"invitation_full"`) {
		t.Fatalf("permanent limit: %d %s", got.Code, got.Body.String())
	}
}
