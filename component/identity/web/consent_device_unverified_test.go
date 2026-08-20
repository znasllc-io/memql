package web

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// consent_device_unverified_test.go -- memql#3824.
//
// The device approval page is a consent screen, and it had the same gap
// /authorize did (memql#3794): it renders a client's human name under
// "Application", and on the self-registered path that name was chosen by
// whoever called the unauthenticated POST /register. Nothing said so.
//
// WHY IT IS LESS BAD HERE, AND STILL WORTH FIXING. This page was already
// better than /authorize was: it shows the client_id beside the name, plus
// SourceIP and the device's self-reported User-Agent, so an approver has
// corroborating evidence. The struct comment on DevicePending.ClientId even
// says why -- "a friendly name is self-asserted at registration, the id is what
// the session is actually bound to".
//
// So the author knew, and the page ACTED on it, and never SAID it. A reader had
// to infer from the presence of a second field that the first one was
// untrustworthy. That is a strictly weaker signal than a sentence, and it is
// invisible to anyone who does not already know what a client_id is -- which is
// most people approving a device.
//
// Assertions are on RENDERED OUTPUT, for the reason memql#3794 gives: a test
// that confirms the handler ran has not confirmed the user can see anything.

// deviceSelfRegisteredServer rewires the shared harness's client resolver to
// report the client as self-registered, leaving everything else identical --
// so a difference in the rendered page is attributable to that one bit.
func deviceSelfRegisteredServer(t *testing.T, name string) (*Server, string) {
	t.Helper()
	s, _, token := newDeviceWebServer(t)
	s.deviceFlow.ClientDisplay = func(_ context.Context, clientId string) (string, bool) {
		if clientId == "vscode-memql" {
			return name, true
		}
		return "", false
	}
	return s, token
}

func renderDeviceApproval(t *testing.T, s *Server, token string) string {
	t.Helper()
	rec := deviceGet(s, token, "?user_code="+url.QueryEscape(deviceTestUserCode))
	if rec.Code != 200 {
		t.Fatalf("GET /device: status = %d, want 200; body=%.400s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestDeviceApprovalMarksASelfRegisteredClient is the regression.
func TestDeviceApprovalMarksASelfRegisteredClient(t *testing.T) {
	// The impostor name matters: this is the shape the marker exists for --
	// a plausible name an attacker picked, on a page asking someone to
	// approve a sign-in they did not start.
	s, token := deviceSelfRegisteredServer(t, "MemQL for VS Code")

	body := renderDeviceApproval(t, s, token)

	if !strings.Contains(body, "NOT VERIFIED") {
		t.Error("the device approval page does not mark a self-registered client.\n" +
			"Its name was chosen by whoever called the unauthenticated POST /register " +
			"(memql#3824), and the page presents it under \"Application\" as though it " +
			"identified anything.")
	}
	if !strings.Contains(body, "registered itself") {
		t.Error("the page does not say the application registered itself. The badge alone " +
			"tells an approver that something differs without telling them what to do " +
			"about it; the sentence points them at the code and the request details, " +
			"which are the things on this page nobody self-asserted.")
	}
}

// TestDeviceApprovalLeavesAnOperatorConfiguredClientUnmarked is the other side,
// and it is what keeps the badge meaningful. A marker every client carried
// would be furniture.
func TestDeviceApprovalLeavesAnOperatorConfiguredClientUnmarked(t *testing.T) {
	// The shared harness wires ClientDisplay with selfRegistered=false.
	s, _, token := newDeviceWebServer(t)

	body := renderDeviceApproval(t, s, token)

	if !strings.Contains(body, "MemQL for VS Code") {
		t.Fatalf("the harness's client name is not on the page, so this test is not "+
			"measuring the case it names.\nbody: %.600s", body)
	}
	if strings.Contains(body, "NOT VERIFIED") {
		t.Error("an operator-configured client was marked NOT VERIFIED. That name came " +
			"from MEMQL_IDENTITY_REGISTERED_CLIENTS -- somebody with access to the " +
			"deployment chose it, which is exactly the provenance the marker exists to " +
			"distinguish.")
	}
}

// TestDeviceApprovalDoesNotMarkAnUnresolvedClient pins the decision that a
// FALLBACK to the raw client_id carries no badge.
//
// This is the case worth arguing about, so it is worth pinning. When the
// resolver returns nothing -- a nil hook, an unknown id, a client with no name
// -- the page shows the client_id. It would be easy to call that "unverified"
// too, since nobody vouched for it either.
//
// That would be wrong. The badge marks a SELF-ASSERTED NAME: someone chose a
// label and is presenting it as identification. An opaque client_id is nobody's
// claim about anything -- it is the value the session is actually bound to, and
// it is the thing the marked case points people TOWARD. Marking it as well
// would put the warning on the page's most trustworthy field and teach
// approvers to ignore the word where it means most.
func TestDeviceApprovalDoesNotMarkAnUnresolvedClient(t *testing.T) {
	s, _, token := newDeviceWebServer(t)
	s.deviceFlow.ClientDisplay = func(_ context.Context, _ string) (string, bool) {
		return "", false // unknown client: fall back to the raw id
	}

	body := renderDeviceApproval(t, s, token)

	if !strings.Contains(body, "vscode-memql") {
		t.Fatalf("the raw client_id is not shown when the name cannot be resolved, so the "+
			"fallback this test is about did not happen.\nbody: %.600s", body)
	}
	if strings.Contains(body, "NOT VERIFIED") {
		t.Error("an UNRESOLVED client was marked NOT VERIFIED. The badge marks a " +
			"self-asserted name, not an unknown client: the raw client_id is nobody's " +
			"assertion, and it is what the marked case tells people to look at instead " +
			"(memql#3824).")
	}
}

// TestDeviceApprovalNilHookDoesNotPanicOrMark covers the documented nil case,
// which is a real configuration -- DeviceFlow.ClientDisplay is optional, and a
// deployment that leaves it unset gets the raw id.
func TestDeviceApprovalNilHookDoesNotPanicOrMark(t *testing.T) {
	s, _, token := newDeviceWebServer(t)
	s.deviceFlow.ClientDisplay = nil

	body := renderDeviceApproval(t, s, token)

	if !strings.Contains(body, "vscode-memql") {
		t.Errorf("with no resolver wired the page must fall back to the raw client_id.\n"+
			"body: %.600s", body)
	}
	if strings.Contains(body, "NOT VERIFIED") {
		t.Error("a nil resolver produced a NOT VERIFIED badge. Nothing was asserted, " +
			"so there is nothing to distrust.")
	}
}
