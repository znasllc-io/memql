package memql

import (
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

	first := map[string]any{
		"packageId": "pkg-first-" + suffix, "name": "acme", "sourceKind": "repo", "repoUrl": repo, "repoRef": "main",
	}
	if _, err := runSiteMutation(t, caller, eng, "createPackage", first); err != nil {
		t.Fatalf("the first registration must land: %v", err)
	}

	// The SAME source under a different spelling, by a different person: a
	// collision is cluster-wide, and the refusal names the holder.
	other := userSiteCtx("user-pkg-other-" + suffix)
	second := map[string]any{
		"packageId": "pkg-second-" + suffix, "name": "acme again", "sourceKind": "repo",
		"repoUrl": strings.ToUpper(repo) + ".git", "repoRef": " main ",
	}
	_, err := runSiteMutation(t, other, eng, "createPackage", second)
	if err == nil {
		t.Fatal("the same repository at the same ref was registered twice")
	}
	if !strings.Contains(err.Error(), "already tracked") || !strings.Contains(err.Error(), "acme") {
		t.Fatalf("the refusal must say the source is already tracked and name the source: %v", err)
	}

	// Another REF of the same repository is a different source.
	tag := map[string]any{
		"packageId": "pkg-tag-" + suffix, "name": "acme v2", "sourceKind": "repo", "repoUrl": repo, "repoRef": "v2.0.0",
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
	if _, err := runSiteMutation(t, other, eng, "createPackage", second); err != nil {
		t.Fatalf("an archived source holds nothing, so the same source must be addable again: %v", err)
	}
}
