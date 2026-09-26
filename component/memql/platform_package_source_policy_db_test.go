package memql

import (
	"fmt"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// platform_package_source_policy_db_test.go -- the source-uniqueness guard
// (2026-09-05 design, D8) through the REAL mutation path.
//
// The pure tests beside this file would keep passing if executeWrite stopped
// calling the guard, so the property that matters is proven here through
// `eng.Execute` on createPackage, the way the hostname policy proves its own.
// Postgres-gated like its neighbours; CI's db-tests lane runs this package
// with MEMQL_REQUIRE_DB=1, so a skip there is a failure rather than a green.

func TestTheSameSourceCannotBeAddedTwice(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("pkg-source")
	repo := "https://github.com/acme/widget-" + suffix
	caller := userSiteCtx("user-pkg-" + suffix)
	account := "package-unique-account-" + suffix
	seedAccountOwnedBy(t, eng, "unique-operator-"+suffix, auth.RoleOwner, account, "Uniqueness test account")

	first := map[string]any{
		"packageId": "pkg-first-" + suffix, "name": "acme", "sourceKind": "repo", "accountId": account, "repoUrl": repo, "repoRef": "main",
	}
	if _, err := runSiteMutation(t, caller, eng, "createPackage", first); err != nil {
		t.Fatalf("the first registration must land: %v", err)
	}

	// The same owner and authorization path cannot register a spelling variant twice.
	other := userSiteCtx("user-pkg-other-" + suffix)
	second := map[string]any{
		"packageId": "pkg-second-" + suffix, "name": "acme again", "sourceKind": "repo", "accountId": account,
		"repoUrl": strings.ToUpper(repo) + ".git", "repoRef": " main ",
	}
	_, err := runSiteMutation(t, caller, eng, "createPackage", second)
	if err == nil {
		t.Fatal("the same repository at the same ref was registered twice")
	}
	if !strings.Contains(err.Error(), "already tracked") || !strings.Contains(err.Error(), "acme") {
		t.Fatalf("the refusal must say the source is already tracked and name the source: %v", err)
	}

	// Another owner has an independent path, even for the same tree/ref.
	if _, err := runSiteMutation(t, other, eng, "createPackage", second); err != nil {
		t.Fatalf("different owner must be independent: %v", err)
	}

	// Another REF of the same repository is a different source.
	tag := map[string]any{
		"packageId": "pkg-tag-" + suffix, "name": "acme v2", "sourceKind": "repo", "accountId": account, "repoUrl": repo, "repoRef": "v2.0.0",
	}
	if _, err := runSiteMutation(t, caller, eng, "createPackage", tag); err != nil {
		t.Fatalf("a different ref is a different source and must land: %v", err)
	}

	// A rename of the first package is not a claim, and must not refuse
	// against itself.
	if _, err := runSiteMutation(t, auth.ContextWithInternalOrigin(caller), eng, "recordPackageName",
		map[string]any{"packageId": "pkg-first-" + suffix, "name": "acme renamed"}); err != nil {
		t.Fatalf("a write that inherits its source must not be judged against itself: %v", err)
	}

	// ARCHIVING FREES THE SOURCE: that is what archiving a source is for.
	if _, err := runSiteMutation(t, auth.ContextWithInternalOrigin(caller), eng, "setPackageStatus",
		map[string]any{"packageId": "pkg-first-" + suffix, "status": "archived"}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := runSiteMutation(t, caller, eng, "createPackage", map[string]any{"packageId": "pkg-restored-" + suffix, "name": "re-added", "sourceKind": "repo", "accountId": account, "repoUrl": repo, "repoRef": "main"}); err != nil {
		t.Fatalf("an archived source holds nothing, so the same source must be addable again: %v", err)
	}
}

func TestPackageSourceClaimIsSerializedAcrossReplicas(t *testing.T) {
	first, db, _ := sharedReadMergeEngine(t)
	second, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	second.Logger = first.Logger
	if err := second.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	suffix := uniqueSuffix("source-race")
	account, owner := "account-"+suffix, "owner-"+suffix
	seedAccountOwnedBy(t, first, owner, auth.RoleOwner, account, "Source race account")
	caller := rankActorCtx(owner, auth.RoleOwner)
	start := make(chan struct{})
	errs := make(chan error, 2)
	for n, eng := range []*MemQLEngine{first, second} {
		go func(n int, eng *MemQLEngine) {
			<-start
			_, err := eng.Execute(caller, fmt.Sprintf(`mutation createPackage(packageId: %s, name: "Concurrent source", sourceKind: "repo", accountId: %s, repoUrl: %s, repoRef: "main")`, langparser.QuoteString(fmt.Sprintf("source-%s-%d", suffix, n)), langparser.QuoteString(account), langparser.QuoteString("https://github.com/acme/"+suffix)))
			errs <- err
		}(n, eng)
	}
	close(start)
	successes, collisions := 0, 0
	for n := 0; n < 2; n++ {
		err := <-errs
		if err == nil {
			successes++
		} else if strings.Contains(err.Error(), "already tracked") {
			collisions++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || collisions != 1 {
		t.Fatalf("concurrent claims: successes=%d collisions=%d", successes, collisions)
	}
}

func TestPackageSourceVisibilityRequiresOwnerAndCurrentOrganizationAuthority(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	installSiteOrganizationCapabilities(t)
	suffix := uniqueSuffix("source-visibility")
	owner, account := "owner-"+suffix, "account-"+suffix
	caller := siteOrganizationMemberCtx(t, eng, owner, account)
	packageID := "package-" + suffix
	if _, err := runSiteMutation(t, caller, eng, "createPackage", map[string]any{"packageId": packageID, "name": "Personal source", "sourceKind": "repo", "repoUrl": "https://github.com/acme/" + suffix, "accountId": account}); err != nil {
		t.Fatal(err)
	}
	q := fmt.Sprintf(`mutation setPackageSourceRemoved(packageId: %s, removed: true)`, langparser.QuoteString(packageID))
	peer := siteOrganizationMemberCtx(t, eng, "peer-"+suffix, account)
	if _, err := eng.Execute(peer, q); err == nil {
		t.Fatal("same organization peer removed another owner's personal source")
	}
	if _, err := eng.Execute(caller, q); err != nil {
		t.Fatalf("authorized owner refused: %v", err)
	}
	seedMembership(t, eng, "group-"+account, owner, "removed")
	restore := fmt.Sprintf(`mutation setPackageSourceRemoved(packageId: %s, removed: false)`, langparser.QuoteString(packageID))
	if _, err := eng.Execute(caller, restore); err == nil {
		t.Fatal("owner with revoked organization membership restored source")
	}
}
