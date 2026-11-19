package edge

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// csp_calls_test.go -- the page policy of a deployable whose calls switch is on
// (csp_calls.go): its connect-src names the media servers, and only them, its script-src
// lets it compile WebAssembly and nothing more, and its Permissions-Policy lets the page
// ask for the camera.

// callsScriptSrc is a calls page's script-src, spelled out rather than built by the code
// under test: its own scripts, and the WebAssembly it may compile.
const callsScriptSrc = "script-src 'self' 'wasm-unsafe-eval'"

// callsPolicyWithoutMedia is what a calls page is served when no media host can be named:
// the policy every site gets, with a calls page's script-src.
var callsPolicyWithoutMedia = strings.Replace(policyWithoutHashes, "script-src 'self';", callsScriptSrc+";", 1)

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
// at the suffix's own host or any host under it; that is the whole of what the suffix adds
// to the policy, byte for byte, and every other directive is the one every site gets but
// script-src, which TestOnlyACallsPageMayCompileWebAssembly holds. A site with calls off
// serves the policy this cluster already serves, and an unset suffix or one that is not a
// host name adds no media host, so an operator's typo can never widen a policy into a
// wildcard or inject a directive.
func TestCallsPagePolicyAdmitsOnlyTheMediaSuffix(t *testing.T) {
	calls := testSite()
	calls.Calls = true
	req := httptest.NewRequest("GET", "/", nil)

	t.Run("a calls page names the media servers in connect-src and nowhere else", func(t *testing.T) {
		got := policyForSite(req, calls, callsMediaEnv("media.example.com"), "")

		want := strings.Replace(callsPolicyWithoutMedia,
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
			if got := policyForSite(req, calls, callsMediaEnv(suffix), ""); got != callsPolicyWithoutMedia {
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

// A CALLS PAGE MAY COMPILE WEBASSEMBLY; NO OTHER PAGE MAY, AND NO PAGE MAY EVAL. A call's
// background blur runs its segmentation model in WebAssembly, and a browser compiles none
// under script-src 'self' alone, so without 'wasm-unsafe-eval' the blur could start on no
// deployed cluster while every other test passed. That source admits compiling WebAssembly
// and nothing else: JavaScript eval stays refused ('unsafe-eval' never appears), no other
// source is added, and it is the switch's, whether or not a media host can be named. A
// page with calls off keeps script-src 'self', and a calls document's inline hashes follow
// the calls page's sources.
func TestOnlyACallsPageMayCompileWebAssembly(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	calls := testSite()
	calls.Calls = true

	for name, env := range map[string]func(string) string{"with a media host": callsMediaEnv("media.example.com"), "with none": noEnv} {
		t.Run("a calls page "+name, func(t *testing.T) {
			policy := policyForSite(req, calls, env, "")
			if got := directive(policy, "script-src"); got != callsScriptSrc {
				t.Fatalf("script-src = %q, want %q", got, callsScriptSrc)
			}
			if strings.Contains(policy, "'unsafe-eval'") {
				t.Fatalf("a calls page may eval JavaScript: %q", policy)
			}
		})
	}

	t.Run("a page with calls off keeps script-src 'self'", func(t *testing.T) {
		if got := directive(policyForSite(req, testSite(), callsMediaEnv("media.example.com"), ""), "script-src"); got != "script-src 'self'" {
			t.Fatalf("script-src = %q", got)
		}
	})

	t.Run("a calls document's inline hashes follow its sources", func(t *testing.T) {
		if got, want := directive(policyForSite(req, calls, noEnv, hashAlert1), "script-src"), callsScriptSrc+" "+hashAlert1; got != want {
			t.Fatalf("script-src = %q, want %q", got, want)
		}
	})

	t.Run("the served page carries it", func(t *testing.T) {
		files := map[string]string{"index.html": "CALLS"}
		for _, path := range []string{"/", "/no/such/page.txt"} {
			if got := directive(serve(t, calls, files, path).Header().Get("Content-Security-Policy"), "script-src"); got != callsScriptSrc {
				t.Errorf("%s: script-src = %q, want %q", path, got, callsScriptSrc)
			}
		}
		if got := directive(serve(t, testSite(), files, "/").Header().Get("Content-Security-Policy"), "script-src"); got != "script-src 'self'" {
			t.Errorf("a page with calls off was served script-src %q", got)
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
