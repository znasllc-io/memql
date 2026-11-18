package edge

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// csp_calls_test.go -- the page policy of a deployable whose calls switch is on
// (csp_calls.go): its connect-src names the media servers, and only them, and its
// Permissions-Policy lets the page ask for the camera.

// callsMediaEnv is an environment holding the media host suffix and nothing else.
func callsMediaEnv(suffix string) func(string) string {
	return func(key string) string {
		if key == CallsMediaHostSuffixEnv {
			return suffix
		}
		return ""
	}
}

// callsMediaSourcesFor is the four sources a calls page's connect-src gains for a suffix,
// spelled out rather than built by the code under test.
func callsMediaSourcesFor(suffix string) string {
	return "wss://" + suffix + ":* wss://*." + suffix + ":* https://" + suffix + ":* https://*." + suffix + ":*"
}

// THE MEDIA SERVERS AND NOTHING ELSE. A calls page connects to a media server for
// signalling (wss) and its connection check (https), on whatever port the server answers,
// at the suffix's own host or any host under it; that is the whole of what the switch adds
// to the policy, byte for byte, and every other directive is the one every site gets. A
// site with calls off, an unset suffix and a suffix that is not a host name all serve the
// policy this cluster already serves, so an operator's typo can never widen a policy into
// a wildcard or inject a directive.
func TestCallsPagePolicyAdmitsOnlyTheMediaSuffix(t *testing.T) {
	calls := testSite()
	calls.Calls = true
	req := httptest.NewRequest("GET", "/", nil)

	t.Run("a calls page names the media servers in connect-src and nowhere else", func(t *testing.T) {
		got := policyForSite(req, calls, callsMediaEnv("media.example.com"), "")

		want := strings.Replace(policyWithoutHashes,
			"connect-src 'self' https://shop.example.com wss://shop.example.com;",
			"connect-src 'self' https://shop.example.com wss://shop.example.com "+callsMediaSourcesFor("media.example.com")+";", 1)
		if got != want {
			t.Fatalf("policy wrong.\n got: %q\nwant: %q", got, want)
		}
	})

	t.Run("the suffix alone changes no other site's policy", func(t *testing.T) {
		if got := policyForSite(req, testSite(), callsMediaEnv("media.example.com"), ""); got != policyWithoutHashes {
			t.Fatalf("a site with calls off gained a source: %q", got)
		}
	})

	t.Run("the suffix is normalised", func(t *testing.T) {
		got := directive(policyForSite(req, calls, callsMediaEnv("  Media.Example.COM.  "), ""), "connect-src")
		if !strings.HasSuffix(got, " "+callsMediaSourcesFor("media.example.com")) {
			t.Fatalf("connect-src = %q, want the suffix in lower case without its final dot", got)
		}
	})

	t.Run("a suffix at the limits of a name is still one", func(t *testing.T) {
		label := strings.Repeat("a", 63)
		long := strings.TrimSuffix(strings.Repeat(label+".", 3)+strings.Repeat("b", 61), ".")
		for _, suffix := range []string{"m-1.media.example.com", "xn--bcher-kva.example", label + ".example.com", long} {
			got := directive(policyForSite(req, calls, callsMediaEnv(suffix), ""), "connect-src")
			if !strings.HasSuffix(got, " "+callsMediaSourcesFor(suffix)) {
				t.Errorf("%q (%d characters) was not admitted: %q", suffix, len(suffix), got)
			}
		}
	})

	unusable := []string{
		"",
		"   ",
		"localhost",
		"*.media.example.com",
		"*",
		"https://media.example.com",
		"wss://media.example.com",
		"media.example.com:7443",
		"media.example.com:*",
		"media.example.com/rtc",
		"user@media.example.com",
		"media example.com",
		"media.example.com;default-src *",
		"media.example.com 'unsafe-inline'",
		"media.example.com\r\nX-Injected: yes",
		"-media.example.com",
		"media-.example.com",
		"media..example.com",
		".media.example.com",
		"media.example.com..",
		"media_server.example.com",
		"10.0.0.1",
		"192.0.2.1.",
		"[::1]",
		"2001:db8::1",
		strings.Repeat("a", 64) + ".example.com",
		strings.Repeat("abcdefghij.", 25) + "example.com",
	}
	for _, suffix := range unusable {
		t.Run("unusable/"+suffix, func(t *testing.T) {
			if got := policyForSite(req, calls, callsMediaEnv(suffix), ""); got != policyWithoutHashes {
				t.Fatalf("suffix %q changed the policy: %q", suffix, got)
			}
			if _, ok := ParseCallsMediaHostSuffix(suffix); ok {
				t.Fatalf("suffix %q parsed as a host name", suffix)
			}
		})
	}

	// THE SERVED PAGE CARRIES IT, read from the environment the edge runs in: a document, the
	// 404 tail, and nothing for a site with calls off.
	t.Run("the served page carries it", func(t *testing.T) {
		t.Setenv(CallsMediaHostSuffixEnv, "media.example.com")
		files := map[string]string{"index.html": "CALLS"}
		for _, path := range []string{"/", "/no/such/page.txt"} {
			got := directive(serve(t, calls, files, path).Header().Get("Content-Security-Policy"), "connect-src")
			if !strings.HasSuffix(got, " "+callsMediaSourcesFor("media.example.com")) {
				t.Errorf("%s: connect-src = %q, want the media servers", path, got)
			}
		}
		if got := serve(t, testSite(), files, "/").Header().Get("Content-Security-Policy"); strings.Contains(got, "media.example.com") {
			t.Errorf("a site with calls off was served the media servers: %q", got)
		}
	})
}

// A CALLS PAGE MAY ASK FOR THE CAMERA; NO OTHER PAGE MAY. camera=() rejects getUserMedia
// before the browser shows its prompt, which reads to the page exactly like a person
// declining (TestHostedSitesMayAskForTheMicrophoneAndNothingElse says why that matters), so
// a video call would be dead in every deployed cluster while passing every other test. The
// switch is what opens it, whether or not a media server is configured, and geolocation
// stays shut on every page.
func TestOnlyACallsPageMayAskForTheCamera(t *testing.T) {
	files := map[string]string{"index.html": "ROOT"}

	calls := testSite()
	calls.Calls = true
	for _, path := range []string{"/", "/no/such/page.txt"} {
		if got := serve(t, calls, files, path).Header().Get("Permissions-Policy"); got != "geolocation=(), microphone=(self), camera=(self)" {
			t.Errorf("a calls page at %s got Permissions-Policy %q, want the camera open to itself", path, got)
		}
	}

	if got := serve(t, testSite(), files, "/").Header().Get("Permissions-Policy"); got != "geolocation=(), microphone=(self), camera=()" {
		t.Errorf("a page with calls off got Permissions-Policy %q, want the camera shut", got)
	}
}
