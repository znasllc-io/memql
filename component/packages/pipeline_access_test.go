package packages

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/secret"
)

// pipeline_access_test.go -- what component/pipelinerun borrows from
// Deployables (epic memql#5477): the node's GitHub App client, and an
// installation token minted through the source owner's grant.
//
// The claim carrying the weight is the security invariant: a token is minted
// ONLY after the owner's grant is verified to still reach the repository, on
// every call -- including the call a replica could have answered from its
// token cache.

// firstHit is the position of the first request to path in the fake's log,
// or -1.
func firstHit(hub *grantHub, path string) int {
	for n, r := range hub.seen() {
		if r.URL.Path == path {
			return n
		}
	}
	return -1
}

func TestInstallationTokenMintsThroughTheOwnersVerifiedGrant(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	hub := installedRepo(newGrantHub(), "acme", "widget")
	i, _, engine, deps := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))

	// The caller is somebody else -- the driver acts for the source's owner,
	// and the credential must resolve under that owner, not under the caller.
	token, installation, err := i.InstallationToken(callerCtx("v1:identity:user:operator"),
		grantCredentialId, grantOwner, "acme", "widget")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if token != grantInstallToken || installation != 42 {
		t.Fatalf("want the installation token for installation 42, got %q / %d", token, installation)
	}
	if got := engine.actors["sourceCredentialSealedById"]; got != grantOwner {
		t.Fatalf("the credential must resolve under the OWNER's actor, got %q", got)
	}

	// THE GRANT WAS VERIFIED, under the person's own token, BEFORE the mint.
	if got := hub.bearerOn("/user/installations"); got != grantUserToken {
		t.Fatalf("the installation list must be read under the person's token, got %q", got)
	}
	if got := hub.bearerOn("/repos/acme/widget"); got != grantUserToken {
		t.Fatalf("the repository must be read under the person's token, got %q", got)
	}
	verified, minted := firstHit(hub, "/repos/acme/widget"), firstHit(hub, "/app/installations/42/access_tokens")
	if verified < 0 || minted < 0 || verified > minted {
		t.Fatalf("the grant must be verified before the mint: verify at %d, mint at %d", verified, minted)
	}

	// MEMORY ONLY: neither the minted token nor the grant's own reaches a row.
	for _, q := range engine.statements() {
		if strings.Contains(q, grantInstallToken) || strings.Contains(q, grantUserToken) {
			t.Fatalf("a token reached a statement: %s", q)
		}
	}

	// And the client is the node's one client -- one installation-token cache.
	if i.GitHub() != deps.GitHubApp {
		t.Fatal("GitHub() must answer the client Deps holds, not a second one")
	}
}

// TestNoTokenIsMintedForAGrantThatNoLongerReachesTheRepository is the
// invariant measured three ways. The third is the one a cache would break:
// a token minted a moment ago sits in this replica's cache, the person loses
// access, and the next call must refuse rather than answer from the cache.
func TestNoTokenIsMintedForAGrantThatNoLongerReachesTheRepository(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)

	t.Run("the person cannot read the repository", func(t *testing.T) {
		hub := installedRepo(newGrantHub(), "acme", "widget")
		hub.body("/repos/acme/widget", http.StatusNotFound, `{"message":"Not Found"}`)
		i, _, _, _ := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
		token, _, err := i.InstallationToken(context.Background(), grantCredentialId, grantOwner, "acme", "widget")
		if got := RefusalCode(err); got != "repository_not_accessible" || token != "" {
			t.Fatalf("want repository_not_accessible and no token, got %q / %q (%v)", got, token, err)
		}
		if n := hub.hits("/app/installations/42/access_tokens"); n != 0 {
			t.Fatalf("%d token(s) minted for a grant that cannot read the repository", n)
		}
	})

	t.Run("the installation is not one the person reaches", func(t *testing.T) {
		hub := installedRepo(newGrantHub(), "acme", "widget")
		hub.body("/user/installations", http.StatusOK,
			`{"total_count":1,"installations":[{"id":77,"account":{"login":"other"},"suspended_at":null}]}`)
		i, _, _, _ := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
		token, _, err := i.InstallationToken(context.Background(), grantCredentialId, grantOwner, "acme", "widget")
		if got := RefusalCode(err); got != "repository_not_accessible" || token != "" {
			t.Fatalf("want repository_not_accessible and no token, got %q / %q (%v)", got, token, err)
		}
		if n := hub.hits("/app/installations/42/access_tokens"); n != 0 {
			t.Fatalf("%d token(s) minted against an installation the person does not reach", n)
		}
	})

	t.Run("a token is cached and the grant has since lost the repository", func(t *testing.T) {
		hub := installedRepo(newGrantHub(), "acme", "widget")
		i, _, _, _ := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
		ctx := context.Background()
		if _, _, err := i.InstallationToken(ctx, grantCredentialId, grantOwner, "acme", "widget"); err != nil {
			t.Fatalf("first mint: %v", err)
		}
		hub.body("/repos/acme/widget", http.StatusNotFound, `{"message":"Not Found"}`)
		token, _, err := i.InstallationToken(ctx, grantCredentialId, grantOwner, "acme", "widget")
		if got := RefusalCode(err); got != "repository_not_accessible" || token != "" {
			t.Fatalf("a cached token must not outlive the grant's access: got %q / %q (%v)", got, token, err)
		}
		// The control that the cache was really there: one mint across both
		// calls, so the refusal above was the verification and not a cold mint
		// that happened to fail.
		if n := hub.hits("/app/installations/42/access_tokens"); n != 1 {
			t.Fatalf("want exactly one mint, got %d", n)
		}
	})
}

// A pasted token is not a grant: there is no installation to mint for, and the
// app's authority is never borrowed on a pasted token's say-so. Refused by
// name, with no call to the app at all.
func TestInstallationTokenRefusesAPastedTokenCredential(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	hub := installedRepo(newGrantHub(), "acme", "widget")
	i, _, _, _ := grantHarness(t, hub, sealedRow(t, credentialStatusActive))

	token, installation, err := i.InstallationToken(context.Background(),
		"v1:platform:sourceCredential:abc", "v1:identity:user:alice", "acme", "widget")
	if got := RefusalCode(err); got != CodeCredentialNotFound || token != "" || installation != 0 {
		t.Fatalf("want %s and nothing minted, got %q / %q / %d (%v)", CodeCredentialNotFound, got, token, installation, err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatal("the refusal carries the pasted token")
	}
	for _, path := range []string{"/user/installations", "/repos/acme/widget/installation", "/app/installations/42/access_tokens"} {
		if n := hub.hits(path); n != 0 {
			t.Fatalf("%d call(s) to %s for a pasted token", n, path)
		}
	}
}

// No owner, no grant: a source with nobody to resolve the credential under is
// refused before anything is read -- never resolved under whoever is calling.
func TestInstallationTokenRefusesAnOwnerlessCall(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	hub := installedRepo(newGrantHub(), "acme", "widget")
	i, _, engine, _ := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))

	for name, args := range map[string][4]string{
		"no owner":      {grantCredentialId, "  ", "acme", "widget"},
		"no credential": {"", grantOwner, "acme", "widget"},
		"no repository": {grantCredentialId, grantOwner, "acme", ""},
	} {
		t.Run(name, func(t *testing.T) {
			token, _, err := i.InstallationToken(callerCtx("v1:identity:user:operator"), args[0], args[1], args[2], args[3])
			if err == nil || RefusalCode(err) == "" || token != "" {
				t.Fatalf("want a typed refusal and no token, got %q (%v)", token, err)
			}
		})
	}
	if n := len(engine.statements()); n != 0 {
		t.Fatalf("%d statement(s) ran for calls that could not name an owner's grant: %v", n, engine.statements())
	}
	if n := len(hub.seen()); n != 0 {
		t.Fatalf("%d request(s) left the cluster", n)
	}
}

func TestInstallationTokenOnANodeWithNoAppRefusesAsNotConfigured(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	hub := installedRepo(newGrantHub(), "acme", "widget")
	i, _, _, deps := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
	deps.GitHubApp = githubapp.New(githubapp.Config{}, githubapp.WithHTTPClient(&http.Client{Transport: hub}))

	_, _, err := i.InstallationToken(context.Background(), grantCredentialId, grantOwner, "acme", "widget")
	if got := RefusalCode(err); got != CodeGithubAppNotConfigured {
		t.Fatalf("want %s, got %q (%v)", CodeGithubAppNotConfigured, got, err)
	}
	if n := hub.hits("/app/installations/42/access_tokens"); n != 0 {
		t.Fatalf("%d mint(s) on a node with no app", n)
	}
}

// An integration that cannot resolve at all has no client to hand out and no
// token to mint, and says so rather than panicking.
func TestAnUnresolvableIntegrationHasNoClient(t *testing.T) {
	i := NewIntegration(nil, discardLogger())
	if i.GitHub() != nil {
		t.Fatal("an integration with no engine has no client")
	}
	if _, _, err := i.InstallationToken(context.Background(), grantCredentialId, grantOwner, "acme", "widget"); err == nil {
		t.Fatal("an integration with no engine cannot mint")
	}
}
