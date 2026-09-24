package packages

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// refusal_identity_parity_test.go -- the codes "raised on the identity node,
// catalogued here" are spelled TWICE, and this is what reads both.
//
// component/identity and integrations/identity sit below this package in the
// module order, so they cannot import the catalogue and spell each code as a
// string literal instead (their comments say so). A literal that drifts from
// the constant does not fail anything on its own: the identity node goes on
// answering a code, MemQL OS has copy for a different one, and the person is
// shown "the cluster answered \"...\"" where a sentence should be. The OS's own
// gate (clients/os/test/deployables/refusals.test.ts) compares the catalogue to
// the client and never sees the identity side at all.
//
// Read as TEXT, because there is no import to read it through.

func repoRoot(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "..", "..")
}

// identityRaiseSites are the files that answer a code a person can see during
// GitHub Connect, while the cluster's GitHub App is registered, or during
// Connect Shopify -- whose four builtins answer their reasons over the stream
// as githubConnectBegin does (integrations/shopify/connect.go), and whose
// callback answers its own tokens and the Shopify half's
// (integrations/shopify/connect_callback.go, reached through
// identity.ShopifyConnect).
var identityRaiseSites = []string{
	"integrations/identity/githubconnect.go",
	"integrations/identity/githubapp.go",
	"component/identity/http/github_callback.go",
	"component/identity/http/github_app_callback.go",
	"component/identity/web/github_app_setup.go",
	"component/identity/http/shopify_callback.go",
	"integrations/shopify/connect.go",
	"integrations/shopify/connect_callback.go",
}

// resultConst matches the constants those files keep their outcomes in. The
// naming is the convention this test rests on: a code a person can see is a
// `result…`, a `…Reason…` or an `…Result…` constant, which is what separates it
// from an audit ACTION (`github_app_registered` the action, say) that shares a
// prefix and is nobody's refusal.
var resultConst = regexp.MustCompile(`(?m)^\s*(?:const\s+)?((?:result|connectReason|appReason|appSetupResult)\w*)\s*=\s*"([a-z_]+)"`)

// notRefusals are outcomes those constants also hold that are SUCCESSES, and so
// have no place in a catalogue of refusals. The OS reads them as markers.
var notRefusals = map[string]bool{
	"ok": true, "connected": true, "reconnected": true, "installed": true,
	"github_app_registered": true,
}

func TestEveryCodeTheIdentityNodeAnswersIsCatalogued(t *testing.T) {
	root := repoRoot(t)
	catalogue, err := os.ReadFile(filepath.Join(root, "component", "packages", "refusal.go"))
	if err != nil {
		t.Fatal(err)
	}
	catalogued := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*Code\w+\s*=\s*"([a-z_]+)"`).FindAllStringSubmatch(string(catalogue), -1) {
		catalogued[m[1]] = true
	}
	if !catalogued["connect_state_invalid"] {
		t.Fatal("the catalogue was not read: connect_state_invalid is missing, so every assertion below would be vacuous")
	}

	seen := map[string]string{}
	for _, rel := range identityRaiseSites {
		src, rerr := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if rerr != nil {
			t.Fatalf("%s: %v -- if the file moved, move this list with it rather than letting it stop checking", rel, rerr)
		}
		for _, m := range resultConst.FindAllStringSubmatch(string(src), -1) {
			seen[m[2]] = rel + " (" + m[1] + ")"
		}
	}
	if len(seen) < 8 {
		t.Fatalf("only %d outcome constants were found across the identity raise sites; the naming convention this test reads has changed: %v", len(seen), seen)
	}

	var uncatalogued []string
	for code, where := range seen {
		if !notRefusals[code] && !catalogued[code] {
			uncatalogued = append(uncatalogued, code+" -- "+where)
		}
	}
	sort.Strings(uncatalogued)
	if len(uncatalogued) > 0 {
		t.Errorf("the identity node answers codes this catalogue does not hold, so MemQL OS has no copy for them:\n  %s",
			strings.Join(uncatalogued, "\n  "))
	}

	// AND THE OTHER WAY: a catalogue entry the identity node never answers is
	// copy nobody can reach.
	for _, code := range []string{
		CodeGithubAppManagedByEnvironment, CodeGithubAppSetupForbidden, CodeGithubAppSetupInvalid,
		CodeGithubAppSetupStateInvalid, CodeGithubAppSetupFailed, CodeConnectStateInvalid, CodeGithubAppNotConfigured,
		CodeExchangeFailed, CodeSignatureInvalid, CodePermissionLost, CodeScopesMissing, CodeStorefrontTokenFailed,
		CodeSiteNotWritable, CodeNotAStorefront, CodeStoreNotNamed, CodeStoreRedacted, CodeAppCredentialsInvalid,
		CodeSecretNameAmbiguous, CodeStoreNotConnected, CodeStoreInUse, CodeStorefrontTokenRequired,
		CodeStorefrontTokenInvalid, CodeShopifyAppNotSaved,
	} {
		if _, ok := seen[code]; !ok {
			t.Errorf("%s is catalogued as raised on the identity node, and no identity raise site answers it", code)
		}
	}
}
