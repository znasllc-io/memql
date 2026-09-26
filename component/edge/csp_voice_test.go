package edge

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServedAPIClientAdmitsConfiguredVoiceSignaling(t *testing.T) {
	t.Setenv("MEMQL_LIVEKIT_PUBLIC_URL", "wss://voice.example.com:8443/signaling")
	for name, site := range map[string]*Site{
		"cluster client": {ID: "client", Hostname: "client.example.com", Kind: "spa", Status: "live", APIProxy: true},
		"account door":   osSiteThroughDoor(),
	} {
		t.Run(name, func(t *testing.T) {
			response := serve(t, site, map[string]string{"index.html": "VOICE CLIENT"}, "/")
			policy := response.Header().Get("Content-Security-Policy")
			connect := strings.Fields(directive(policy, "connect-src"))
			for _, want := range []string{"https://voice.example.com:8443", "wss://voice.example.com:8443"} {
				found := false
				for _, source := range connect {
					found = found || source == want
				}
				if !found {
					t.Errorf("served page blocks LiveKit signaling %q: %s", want, policy)
				}
			}
			if got := directive(policy, "script-src"); got != "script-src 'self'" {
				t.Errorf("voice changed script policy: %s", got)
			}
		})
	}
}

func TestVoiceSignalingPolicyRequiresAPIAndValidSecureConfiguration(t *testing.T) {
	site := testSite()
	site.APIProxy = true
	for _, raw := range []string{"", "ws://voice.example.com", "https://", "wss://*.example.com", "wss://voice.example.com;default-src", "wss://user:secret@voice.example.com", "wss://voice.example.com\r\nX-Test: injected"} {
		t.Run(raw, func(t *testing.T) {
			env := func(key string) string {
				if key == "MEMQL_LIVEKIT_PUBLIC_URL" {
					return raw
				}
				return ""
			}
			if got := policyForSite(httptest.NewRequest("GET", "/", nil), site, env, ""); got != policyWithoutHashes {
				t.Errorf("invalid voice configuration changed the policy: %s", got)
			}
		})
	}
	site.APIProxy = false
	env := func(key string) string {
		if key == "MEMQL_LIVEKIT_PUBLIC_URL" {
			return "wss://voice.example.com"
		}
		return ""
	}
	if got := policyForSite(httptest.NewRequest("GET", "/", nil), site, env, ""); got != policyWithoutHashes {
		t.Errorf("site without cluster API access gained a voice source: %s", got)
	}
}
