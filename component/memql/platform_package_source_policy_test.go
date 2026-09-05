package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// platform_package_source_policy_test.go -- one source, once (2026-09-05
// design, D8). The PURE half here; the guard through the real mutation path
// is in platform_package_source_policy_db_test.go.

func TestNormalizeRepoSourceReadsEverySpellingOfOneRepository(t *testing.T) {
	same := []string{
		"https://github.com/Acme/Widget",
		"https://github.com/acme/widget/",
		"https://github.com/acme/widget.git",
		"http://github.com/acme/widget",
		"github.com/acme/widget",
		"https://www.github.com/acme/widget",
		"git@github.com:acme/widget.git",
		"  https://github.com/acme/widget  ",
	}
	for _, url := range same {
		got, _ := normalizeRepoSource(url, "")
		if got != "github.com/acme/widget" {
			t.Errorf("normalizeRepoSource(%q) = %q, want github.com/acme/widget", url, got)
		}
	}
	// Different repositories stay different.
	for _, url := range []string{"https://github.com/acme/widgets", "https://github.com/acme2/widget", "https://gitlab.com/acme/widget"} {
		got, _ := normalizeRepoSource(url, "")
		if got == "github.com/acme/widget" {
			t.Errorf("normalizeRepoSource(%q) collapsed onto a different repository", url)
		}
	}
}

func TestNormalizeRepoSourceKeepsTheRefAndTrimsIt(t *testing.T) {
	_, ref := normalizeRepoSource("https://github.com/acme/widget", " main ")
	if ref != "main" {
		t.Fatalf("ref %q, want main", ref)
	}
	// EMPTY IS ITS OWN VALUE: the guard cannot resolve the default branch's
	// name, so "" and "main" are two refs here. The OS closes that gap with
	// the probe's answer.
	_, empty := normalizeRepoSource("https://github.com/acme/widget", "")
	if empty != "" {
		t.Fatalf("an empty ref must stay empty, got %q", empty)
	}
}

// The guard judges only a write that CHOOSES a source. Every other write
// inherits the stored pair through the read-merge, and judging that would
// refuse a rename or an auto-deploy flip on a package created before the
// rule -- against itself, or against the package it already is.
func TestPackageSourceGuardSkipsWhatIsNotAClaim(t *testing.T) {
	e := &MemQLEngine{} // no database: any read attempt would error, which is the tell
	ctx := auth.ContextWithUserActor(context.Background(), "u-1")

	cases := []struct {
		name    string
		payload map[string]any
		prior   bool
		pUrl    string
		pRef    string
	}{
		{"a zip source", map[string]any{"sourceKind": "artifact", "artifactId": "a"}, false, "", ""},
		{"an archive write", map[string]any{"sourceKind": "repo", "repoUrl": "https://github.com/a/b", "status": "archived"}, true, "https://github.com/a/b", ""},
		{"an update inheriting its source", map[string]any{"sourceKind": "repo", "repoUrl": "https://github.com/a/b", "repoRef": "main", "status": "active"}, true, "https://github.com/A/B.git", "main"},
		{"a repo source with no URL", map[string]any{"sourceKind": "repo", "status": "active"}, false, "", ""},
	}
	for _, tc := range cases {
		if err := e.validatePackageSourceUnique(ctx, tc.payload, "p", "u-1", tc.prior, tc.pUrl, tc.pRef); err != nil {
			t.Errorf("%s: the guard read the database for a write that claims nothing: %v", tc.name, err)
		}
	}
}

// ...and DOES read for a write that claims one -- the reachable positive for
// the cases above. With no database the read fails CLOSED, naming the source.
func TestPackageSourceGuardFailsClosedWithoutADatabase(t *testing.T) {
	e := &MemQLEngine{}
	ctx := auth.ContextWithUserActor(context.Background(), "u-1")
	err := e.validatePackageSourceUnique(ctx,
		map[string]any{"sourceKind": "repo", "repoUrl": "https://github.com/a/b", "status": "active"},
		"p", "u-1", false, "", "")
	if err == nil || !strings.Contains(err.Error(), "cannot verify") {
		t.Fatalf("a create must be checked, and an unreachable database must refuse rather than admit: %v", err)
	}
	// A ref change on an existing package is a claim too.
	err = e.validatePackageSourceUnique(ctx,
		map[string]any{"sourceKind": "repo", "repoUrl": "https://github.com/a/b", "repoRef": "release", "status": "active"},
		"p", "u-1", true, "https://github.com/a/b", "main")
	if err == nil || !strings.Contains(err.Error(), "cannot verify") {
		t.Fatalf("changing the ref chooses a source and must be checked: %v", err)
	}
}

func TestCanonicalPackageStorageId(t *testing.T) {
	if got := canonicalPackageStorageId("abc"); got != "v1:platform:package:abc" {
		t.Fatalf("bare id: %q", got)
	}
	if got := canonicalPackageStorageId("v1:platform:package:abc"); got != "v1:platform:package:abc" {
		t.Fatalf("qualified id: %q", got)
	}
}
