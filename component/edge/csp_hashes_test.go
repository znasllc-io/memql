// component/edge/csp_hashes_test.go
package edge

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func noEnv(string) string { return "" }

func testSite() *Site {
	return &Site{ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "static"}
}

// THE POLICY WITH NO HASHES IS THE POLICY THIS CLUSTER ALREADY SERVES.
//
// Hardcoded in full, byte for byte, rather than compared against a
// recomputed value: this is the assertion protecting every bundle with no
// inline scripts -- MemQL OS above all, a Vite build with zero of them --
// from a change that is supposed to be invisible to it. A drifting
// expectation computed the same way the code computes it would not notice
// the drift.
const policyWithoutHashes = "default-src 'self'; " +
	"connect-src 'self' https://shop.example.com wss://shop.example.com; " +
	"img-src 'self' data: blob:; " +
	"style-src 'self' 'unsafe-inline'; " +
	"script-src 'self'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'"

func TestPolicyForSiteWithNoHashesIsUnchanged(t *testing.T) {
	got := policyForSite(httptest.NewRequest("GET", "/", nil), testSite(), noEnv, "")
	if got != policyWithoutHashes {
		t.Errorf("policy changed for a site with no inline scripts.\n got: %q\nwant: %q", got, policyWithoutHashes)
	}
}

// The hashes land in script-src and nowhere else, after 'self', separated by
// a single space.
func TestPolicyForSiteAppendsHashesToScriptSrc(t *testing.T) {
	got := policyForSite(httptest.NewRequest("GET", "/", nil), testSite(), noEnv, hashAlert1+" "+hashConsole)

	want := strings.Replace(policyWithoutHashes,
		"script-src 'self';",
		"script-src 'self' "+hashAlert1+" "+hashConsole+";", 1)
	if got != want {
		t.Errorf("policy with hashes wrong.\n got: %q\nwant: %q", got, want)
	}
}

// 'unsafe-inline' must never appear in script-src. A browser IGNORES
// 'unsafe-inline' when a hash is present, so adding it would be inert here
// and catastrophic on any page that fell back to no hashes -- which is
// exactly what the overflow path does.
func TestPolicyForSiteNeverAdmitsUnsafeInlineScript(t *testing.T) {
	for name, hashes := range map[string]string{"with hashes": hashAlert1, "without hashes": ""} {
		t.Run(name, func(t *testing.T) {
			got := policyForSite(httptest.NewRequest("GET", "/", nil), testSite(), noEnv, hashes)
			scriptSrc := directive(got, "script-src")
			if strings.Contains(scriptSrc, "unsafe-inline") {
				t.Errorf("script-src admits unsafe-inline: %q", scriptSrc)
			}
		})
	}
}

// directive returns the named directive's full text from a CSP, or "".
func directive(csp, name string) string {
	for _, d := range strings.Split(csp, ";") {
		d = strings.TrimSpace(d)
		if strings.HasPrefix(d, name+" ") || d == name {
			return d
		}
	}
	return ""
}
