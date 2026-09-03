package packages

import (
	"strings"
	"testing"
)

// archive_cascade_test.go -- packageArchive CASCADES (epic memql#4885,
// design sections A and F: "archive this source and every app it produced").
//
// The D10 law is unchanged and is what the cases below are about: a site
// that is LIVE is not archived by a cascade -- pausing is the step that
// gives anyone still using it a chance to notice, and it stays the person's
// decision -- so a package with a live app is refused before any site is
// touched. Draft and disabled apps are archived through the same stamped
// setSiteStatus the site archive uses, sites first, package last; an app
// already archived is left alone.

func archiveHarness(t *testing.T, sites ...map[string]any) (*Integration, *recordingEngine) {
	t.Helper()
	engine := &recordingEngine{rows: map[string][]map[string]any{
		"query packageById":     {ownerPackage()},
		"query sitesForPackage": sites,
	}}
	i := NewIntegration(engine, discardLogger())
	i.depsOnce.Do(func() {
		i.deps = &Deps{Store: &store{engine: engine}, Logger: discardLogger()}
	})
	return i, engine
}

func packageSite(name, hostname, status string) map[string]any {
	return map[string]any{
		"id":                    "v1:platform:site:" + name,
		"hostname":              hostname,
		"status":                status,
		"packageId":             "v1:platform:package:abc",
		"packageDeployableName": name,
	}
}

func archiveArgs() map[string]any {
	return map[string]any{"packageId": "v1:platform:package:abc", "confirmName": "acme"}
}

// statusWrites reads every lifecycle status write off the recorder, in order,
// as "<construct> <id> <status>".
func statusWrites(stmts []string) []string {
	var out []string
	for _, q := range stmts {
		if !strings.HasPrefix(q, "mutation setSiteStatus(") && !strings.HasPrefix(q, "mutation setPackageStatus(") {
			continue
		}
		name := callName(q)
		id := strings.SplitN(strings.SplitN(q, `: "`, 2)[1], `"`, 2)[0]
		status := strings.SplitN(strings.SplitN(q, `status: "`, 2)[1], `"`, 2)[0]
		out = append(out, name+" "+id+" "+status)
	}
	return out
}

func TestPackageArchiveCascadesToItsDisabledAppsSitesFirst(t *testing.T) {
	i, engine := archiveHarness(t,
		packageSite("storefront", "shop.example.com", siteStatusDisabled),
		packageSite("docs", "docs.example.com", siteStatusDisabled),
	)
	nodes, err := i.handleArchivePackage(callerCtx("v1:identity:user:someone"), archiveArgs(), 0)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	reply := replyPayload(t, nodes)
	if reply["status"] != "archived" {
		t.Fatalf("reply %v", reply)
	}

	// Three status writes, the sites FIRST and the package LAST: the package
	// row flips only once every app it produced is archived, so a failure
	// midway leaves the record and the reality agreeing -- some apps
	// archived, the package still active and still refusing.
	got := statusWrites(engine.statements())
	want := []string{
		"setSiteStatus v1:platform:site:storefront archived",
		"setSiteStatus v1:platform:site:docs archived",
		"setPackageStatus v1:platform:package:abc archived",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("want the sites archived first and the package last:\n got %v\nwant %v", got, want)
	}
	// Through the STAMPED store method, so the status guard beside
	// executeWrite still decides each write -- the handler decides nothing
	// the guard already decides.
	if len(reply["archivedSites"].([]any)) != 2 {
		t.Fatalf("the reply names the apps it archived: %v", reply)
	}
}

// A DRAFT app -- a first deploy that was never made live, the commonest state
// a composed source is abandoned in -- is walked through `disabled` on its
// way to `archived`: two writes, each one a transition the status guard
// admits. The guard refuses draft -> archived by name ("can only be archived
// from disabled"), and it is right to for a site somebody might be using; a
// draft resolves for nobody, so the pause it insists on is the law's own
// path with nobody to notice, and the cascade takes it rather than stopping
// at a refusal every never-published source would hit.
func TestPackageArchiveWalksADraftAppThroughDisabled(t *testing.T) {
	i, engine := archiveHarness(t, packageSite("docs", "docs.example.com", siteStatusDraft))
	if _, err := i.handleArchivePackage(callerCtx("v1:identity:user:someone"), archiveArgs(), 0); err != nil {
		t.Fatalf("archive: %v", err)
	}
	got := statusWrites(engine.statements())
	want := []string{
		"setSiteStatus v1:platform:site:docs disabled",
		"setSiteStatus v1:platform:site:docs archived",
		"setPackageStatus v1:platform:package:abc archived",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("want the draft walked through disabled, then archived, then the package:\n got %v\nwant %v", got, want)
	}
}

// A LIVE app refuses the whole call, naming ONLY the live hostnames, before
// any site is touched. "Pause first" stays the person's decision.
func TestPackageArchiveRefusesWhileAnAppIsStillServing(t *testing.T) {
	i, engine := archiveHarness(t,
		packageSite("storefront", "shop.example.com", siteStatusLive),
		packageSite("docs", "docs.example.com", siteStatusDisabled),
	)
	_, err := i.handleArchivePackage(callerCtx("v1:identity:user:someone"), archiveArgs(), 0)
	if got := RefusalCode(err); got != CodePackageHasActiveDeployables {
		t.Fatalf("want %s, got %s (%v)", CodePackageHasActiveDeployables, got, err)
	}
	if !strings.Contains(err.Error(), "shop.example.com") || strings.Contains(err.Error(), "docs.example.com") {
		t.Fatalf("the refusal names the LIVE hostnames and only those, got: %v", err)
	}
	if !strings.Contains(err.Error(), "still serving") {
		t.Fatalf("the refusal says what is wrong -- still serving -- got: %v", err)
	}
	if got := statusWrites(engine.statements()); len(got) != 0 {
		t.Fatalf("a refused cascade writes NOTHING, got %v", got)
	}
}

// An app already archived is left alone: rewriting it would put a second
// `archived` version on its timeline that archived nothing.
func TestPackageArchiveLeavesAnAlreadyArchivedAppAlone(t *testing.T) {
	i, engine := archiveHarness(t,
		packageSite("storefront", "shop.example.com", siteStatusArchived),
		packageSite("docs", "docs.example.com", siteStatusDisabled),
	)
	if _, err := i.handleArchivePackage(callerCtx("v1:identity:user:someone"), archiveArgs(), 0); err != nil {
		t.Fatalf("archive: %v", err)
	}
	got := statusWrites(engine.statements())
	want := []string{
		"setSiteStatus v1:platform:site:docs archived",
		"setPackageStatus v1:platform:package:abc archived",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %v, want %v", got, want)
	}

	// And a package whose apps are ALL already archived flips with no site
	// write at all -- the case the pre-cascade handler served.
	i, engine = archiveHarness(t, packageSite("storefront", "shop.example.com", siteStatusArchived))
	if _, err := i.handleArchivePackage(callerCtx("v1:identity:user:someone"), archiveArgs(), 0); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if got := statusWrites(engine.statements()); strings.Join(got, ",") != "setPackageStatus v1:platform:package:abc archived" {
		t.Fatalf("got %v", got)
	}
}

// A site write the guard refuses -- surfaced, and the cascade STOPS there:
// the package row is not flipped over an app that did not archive, so the
// record never claims more than the reality.
func TestPackageArchiveStopsWhenTheGuardRefusesASite(t *testing.T) {
	i, engine := archiveHarness(t,
		packageSite("storefront", "shop.example.com", siteStatusDisabled),
		packageSite("docs", "docs.example.com", siteStatusDisabled),
	)
	engine.fail = map[string]error{`setSiteStatus(siteId: "v1:platform:site:storefront"`: errRefusedByGuard}
	_, err := i.handleArchivePackage(callerCtx("v1:identity:user:someone"), archiveArgs(), 0)
	if err == nil || !strings.Contains(err.Error(), errRefusedByGuard.Error()) {
		t.Fatalf("the guard's refusal must surface, got %v", err)
	}
	if engine.sawStatement("mutation setPackageStatus") {
		t.Fatal("the package must not flip over an app that did not archive")
	}
	if engine.sawStatement(`setSiteStatus(siteId: "v1:platform:site:docs"`) {
		t.Fatal("the cascade stops at the first refusal")
	}
}

// The typed confirmation is verified before anything else, cascade included.
func TestPackageArchiveStillVerifiesTheTypedName(t *testing.T) {
	i, engine := archiveHarness(t, packageSite("docs", "docs.example.com", siteStatusDisabled))
	_, err := i.handleArchivePackage(callerCtx("v1:identity:user:someone"),
		map[string]any{"packageId": "v1:platform:package:abc", "confirmName": "acme-typo"}, 0)
	if RefusalCode(err) != "archive_confirmation_mismatch" {
		t.Fatalf("want archive_confirmation_mismatch, got %v", err)
	}
	if got := statusWrites(engine.statements()); len(got) != 0 {
		t.Fatalf("a mismatched name writes nothing, got %v", got)
	}
}

var errRefusedByGuard = refuse("site_status_refused", "v1:platform:site: a systemOwned row is exempt from the lifecycle")
