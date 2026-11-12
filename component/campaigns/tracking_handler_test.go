package campaigns

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// tracking_handler_test.go -- what the two endpoints answer, and what they
// write (memql#4823).
//
// The assertions that matter most are about what a RECIPIENT sees. A pixel
// that errors is a broken-image icon inside somebody's message; a click
// endpoint that 500s is our stack trace where a person expected a shop. So
// every failure branch is driven and every one is checked for the answer
// rather than only for the absence of a write.

func trackingHandlerFixture(t *testing.T) (*TrackingHandler, *fakeEngine) {
	t.Helper()
	engine := &fakeEngine{jobs: []map[string]any{jobRow()}}
	h := NewTrackingHandler(engine, trackingConfig(), quietLogger())
	return h, engine
}

func trackedGet(t *testing.T, h *TrackingHandler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, path, nil))
	return rw
}

func mintFor(t *testing.T, kind, url string) string {
	t.Helper()
	token, err := MintTrackingToken(trackSecretA, TrackingPayload{
		DeliveryID: "v1:campaigns:delivery:d-1",
		CampaignID: testCampaign,
		Kind:       kind,
		URL:        url,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return token
}

func TestOpenPixelIsAnsweredAndRecorded(t *testing.T) {
	h, engine := trackingHandlerFixture(t)
	rw := trackedGet(t, h, TrackingOpenPath+mintFor(t, EngagementOpen, ""))

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	if got := rw.Header().Get("Content-Type"); got != "image/gif" {
		t.Errorf("Content-Type = %q, want image/gif", got)
	}
	if rw.Body.Len() == 0 {
		t.Error("the pixel body is empty")
	}
	if !wroteContaining(engine, "mutation recordEngagementEvent", `kind: "open"`) {
		t.Errorf("no engagement event was recorded.\ncalls:\n%s",
			strings.Join(callsWithPrefix(engine, "mutation recordEngagementEvent"), "\n"))
	}
}

// TestTheOpenPixelAnswersEvenWhenNothingElseWorks is the posture stated as a
// test. Each of these would, in the obvious implementation, produce an error
// response -- and every one of them would render as a broken image inside a
// campaign the operator sent.
func TestTheOpenPixelAnswersEvenWhenNothingElseWorks(t *testing.T) {
	cases := map[string]string{
		"a forged token":       TrackingOpenPath + "t1.deadbeef.a.b.c.d.e.f",
		"an empty token":       TrackingOpenPath,
		"a token with a slash": TrackingOpenPath + "a/b",
		"a CLICK token":        TrackingOpenPath + mintFor(t, EngagementClick, "https://acme.test/x"),
		"an unsigned string":   TrackingOpenPath + "not-a-token",
	}
	for name, path := range cases {
		h, engine := trackingHandlerFixture(t)
		rw := trackedGet(t, h, path)
		if rw.Code != http.StatusOK || rw.Header().Get("Content-Type") != "image/gif" {
			t.Errorf("%s: answered %d %q, want 200 image/gif. A tracking pixel that errors is a broken "+
				"image icon in the middle of somebody's message", name, rw.Code, rw.Header().Get("Content-Type"))
		}
		if len(callsWithPrefix(engine, "mutation recordEngagementEvent")) != 0 {
			t.Errorf("%s: recorded an engagement event anyway", name)
		}
	}
}

func TestClickRedirectsToTheSignedTarget(t *testing.T) {
	h, engine := trackingHandlerFixture(t)
	const target = "https://acme.test/spring?a=1&b=2"
	rw := trackedGet(t, h, TrackingClickPath+mintFor(t, EngagementClick, target))

	if rw.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302. A 301 is CACHED by the browser, so a second click on the same "+
			"link would never reach us and the count would silently stop at one per device", rw.Code)
	}
	if got := rw.Header().Get("Location"); got != target {
		t.Errorf("Location = %q, want the signed target %q", got, target)
	}
	if !wroteContaining(engine, "mutation recordEngagementEvent", `kind: "click"`) {
		t.Error("no click event was recorded")
	}
}

// TestAnUnverifiedTargetIsNeverRedirectedTo is the open-redirect assertion,
// driven through every shape that could reach the Location header.
func TestAnUnverifiedTargetIsNeverRedirectedTo(t *testing.T) {
	tampered := func() string {
		parts := strings.Split(mintFor(t, EngagementClick, "https://acme.test/spring"), ".")
		// base64url("https://evil.test")
		parts[6] = "aHR0cHM6Ly9ldmlsLnRlc3Q"
		return strings.Join(parts, ".")
	}()
	unsignedScheme, err := MintTrackingToken(trackSecretA, TrackingPayload{
		DeliveryID: "d-1", CampaignID: testCampaign, Kind: EngagementClick,
		URL: "javascript:alert(1)",
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	cases := map[string]string{
		"a tampered target":           TrackingClickPath + tampered,
		"a forged token":              TrackingClickPath + "t1.deadbeef.a.b.c.d.e.f",
		"an OPEN token":               TrackingClickPath + mintFor(t, EngagementOpen, ""),
		"no token":                    TrackingClickPath,
		"a signed javascript: scheme": TrackingClickPath + unsignedScheme,
	}
	for name, path := range cases {
		h, engine := trackingHandlerFixture(t)
		rw := trackedGet(t, h, path)
		if loc := rw.Header().Get("Location"); loc != "" {
			t.Errorf("%s: redirected to %q. The signature over the url IS the open-redirect defence; a "+
				"target that does not verify must never reach a Location header", name, loc)
		}
		if rw.Code >= 500 {
			t.Errorf("%s: answered %d. The person clicked a link in an email and is owed a page, not a "+
				"stack of ours", name, rw.Code)
		}
		if !strings.Contains(rw.Body.String(), "not valid") {
			t.Errorf("%s: did not render the invalid-link page.\ngot: %s", name, rw.Body.String())
		}
		if len(callsWithPrefix(engine, "mutation recordEngagementEvent")) != 0 {
			t.Errorf("%s: recorded an engagement event for a target it refused to follow", name)
		}
	}
}

// TestTheEngagementRowIsWrittenUnderTheCampaignOwnersActor is the
// authorization claim: the request carries no actor at all, so the owner has
// to come out of the signed payload's campaign by way of the engine's own
// send-job row.
func TestTheEngagementRowIsWrittenUnderTheCampaignOwnersActor(t *testing.T) {
	h, engine := trackingHandlerFixture(t)
	trackedGet(t, h, TrackingOpenPath+mintFor(t, EngagementOpen, ""))

	engine.mu.Lock()
	defer engine.mu.Unlock()
	var found bool
	for _, c := range engine.calls {
		if !strings.HasPrefix(c.query, "mutation recordEngagementEvent(") {
			continue
		}
		found = true
		if c.actorID != testOwner {
			t.Errorf("the engagement row was written under actor %q, want the campaign owner %q. The "+
				"row is owner-tier and the request has no actor, so an owner that did not come out of "+
				"the signed payload is a row owned by the wrong person -- or by nobody", c.actorID, testOwner)
		}
	}
	if !found {
		t.Fatal("no engagement write was issued")
	}
}

// TestACampaignWithNoSendJobRecordsNothing: a signed token whose campaign has
// no send job cannot have its owner resolved, and a row written under a blank
// actor is a row nobody -- including the operator asking where their numbers
// went -- can read.
func TestACampaignWithNoSendJobRecordsNothing(t *testing.T) {
	engine := &fakeEngine{}
	h := NewTrackingHandler(engine, trackingConfig(), quietLogger())
	rw := trackedGet(t, h, TrackingOpenPath+mintFor(t, EngagementOpen, ""))

	if rw.Code != http.StatusOK {
		t.Errorf("status = %d, want the pixel anyway", rw.Code)
	}
	if len(callsWithPrefix(engine, "mutation recordEngagementEvent")) != 0 {
		t.Error("an engagement row was written with no resolvable owner. Row authz would then make it " +
			"readable by nobody, which presents as the numbers silently not moving")
	}
}

func TestTrackingCarriesTheSecurityHeaders(t *testing.T) {
	h, _ := trackingHandlerFixture(t)
	for _, path := range []string{
		TrackingOpenPath + mintFor(t, EngagementOpen, ""),
		TrackingClickPath + mintFor(t, EngagementClick, "https://acme.test/x"),
		TrackingClickPath + "forged",
	} {
		rw := trackedGet(t, h, path)
		for header, want := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"Referrer-Policy":        "no-referrer",
		} {
			if got := rw.Header().Get(header); got != want {
				t.Errorf("%s: %s = %q, want %q", path, header, got, want)
			}
		}
		if csp := rw.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: CSP = %q, want frame-ancestors 'none'", path, csp)
		}
		if cc := rw.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
			t.Errorf("%s: Cache-Control = %q, want no-store -- a cached pixel is an open counted once "+
				"and never again", path, cc)
		}
	}
}

func TestNonGetIsRefused(t *testing.T) {
	h, _ := trackingHandlerFixture(t)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, TrackingOpenPath+"whatever", nil))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rw.Code)
	}
	if got := rw.Header().Get("Allow"); got != "GET" {
		t.Errorf("Allow = %q, want GET", got)
	}
}

// TestABasePrefixedPathStillResolves covers a deployment served under
// MEMQL_SERVER_PUBLIC_PATH, whose routes carry a prefix.
func TestABasePrefixedPathStillResolves(t *testing.T) {
	h, engine := trackingHandlerFixture(t)
	rw := trackedGet(t, h, "/api"+TrackingOpenPath+mintFor(t, EngagementOpen, ""))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d", rw.Code)
	}
	if !wroteContaining(engine, "mutation recordEngagementEvent", `kind: "open"`) {
		t.Error("a base-prefixed path did not reach the recording branch")
	}
}
