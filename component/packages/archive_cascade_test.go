package packages

import (
	"strings"
	"testing"
)

// archive_cascade_test.go -- packageArchive DEACTIVATES every app the source
// produced, then archives the source (design D3 of
// 2026-09-05-deployables-states-activation-and-source-archive-design.md).
//
// The cascade used to ARCHIVE each site, and the cluster-wide uniqueness probe
// reads `deleted` alone -- so an archived source went on holding every
// address it had ever claimed, and a re-added source could not take them
// back. Deactivating is what frees a name: the domains come down, the site is
// stamped deleted, and the app goes on the source's off-list so a restored
// source comes back with nothing deploying itself.
//
// A LIVE app no longer refuses the call. The owner decided that archiving a
// source is allowed while apps are serving, behind a confirmation that names
// them; the refusal that stood in for that confirmation is retired.

func archiveHarness(t *testing.T, sites ...map[string]any) (*Integration, *recordingEngine) {
	t.Helper()
	engine := &recordingEngine{rows: map[string][]map[string]any{
		"query packageById":     {ownerPackageDeclaring("storefront", "docs")},
		"query sitesForPackage": sites,
		"builtin customDomainReleaseForSite": {{
			"requested": float64(1), "alreadyRemoved": float64(0),
		}},
	}}
	i := NewIntegration(engine, discardLogger())
	i.depsOnce.Do(func() {
		i.deps = &Deps{Store: &store{engine: engine}, Logger: discardLogger()}
	})
	return i, engine
}

// ownerPackageDeclaring is ownerPackage with a manifest catalogue, which is
// what the cascade puts on the off-list.
func ownerPackageDeclaring(names ...string) map[string]any {
	pkg := ownerPackage()
	declares := make([]any, 0, len(names))
	for _, n := range names {
		declares = append(declares, map[string]any{"name": n, "kind": "spa"})
	}
	pkg["declares"] = declares
	return pkg
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

// cascadeWrites reads every write the cascade makes off the recorder, in
// order, reduced to "<verb> <id>" so the ORDER can be asserted as a whole.
func cascadeWrites(stmts []string) []string {
	var out []string
	for _, q := range stmts {
		switch {
		case strings.HasPrefix(q, "builtin customDomainReleaseForSite("):
			out = append(out, "release "+argOf(q, "siteId"))
		case strings.HasPrefix(q, "mutation deleteSite("):
			out = append(out, "delete "+argOf(q, "siteId"))
		case strings.HasPrefix(q, "mutation disablePackageDeployables("):
			out = append(out, "disable "+argOf(q, "deployableNames"))
		case strings.HasPrefix(q, "mutation setPackageAutoDeploy("):
			out = append(out, "autodeploy "+argOf(q, "autoDeploy"))
		case strings.HasPrefix(q, "mutation setPackageStatus("):
			out = append(out, "package "+argOf(q, "status"))
		case strings.HasPrefix(q, "mutation setSiteStatus("):
			out = append(out, "status "+argOf(q, "siteId")+" "+argOf(q, "status"))
		}
	}
	return out
}

// argOf pulls one named argument's rendered value out of a call string.
func argOf(q, name string) string {
	i := strings.Index(q, name+": ")
	if i < 0 {
		return ""
	}
	rest := q[i+len(name)+2:]
	if strings.HasPrefix(rest, `"`) {
		return strings.SplitN(rest[1:], `"`, 2)[0]
	}
	if strings.HasPrefix(rest, "[") {
		return strings.SplitN(rest, "]", 2)[0] + "]"
	}
	end := strings.IndexAny(rest, ",)")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// THE ASSERTION THAT PROVES THE FEATURE. Every app is DELETED rather than
// archived -- `deleted` is the only field the uniqueness probe reads -- with
// its domains released first, the names go on the off-list, and the package
// flips LAST so a failure part-way leaves a source that is still active and
// still says what happened.
func TestPackageArchiveDeactivatesEveryAppAndFreesItsName(t *testing.T) {
	i, engine := archiveHarness(t,
		packageSite("storefront", "shop.example.com", siteStatusLive),
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
	got := cascadeWrites(engine.statements())
	want := []string{
		"release v1:platform:site:storefront",
		"delete v1:platform:site:storefront",
		"release v1:platform:site:docs",
		"delete v1:platform:site:docs",
		`disable ["storefront","docs"]`,
		"package archived",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("want each app released then deleted, the off-list, then the package:\n got %v\nwant %v", got, want)
	}
	// NOTHING IS ARCHIVED ANY MORE. A site at `archived` holds its name.
	if engine.sawStatement("mutation setSiteStatus(") {
		t.Fatal("the cascade must not archive a site -- an archived site still holds its hostname")
	}
	released, _ := reply["releasedSites"].([]any)
	if len(released) != 2 {
		t.Fatalf("the reply names the addresses it released: %v", reply)
	}
}

// A LIVE app is deactivated like any other. The refusal that used to stand
// here is the owner's decision to retire: the confirmation in the OS names
// the live addresses instead.
func TestPackageArchiveNoLongerRefusesALiveApp(t *testing.T) {
	i, engine := archiveHarness(t, packageSite("storefront", "shop.example.com", siteStatusLive))
	if _, err := i.handleArchivePackage(callerCtx("v1:identity:user:someone"), archiveArgs(), 0); err != nil {
		t.Fatalf("a live app must not refuse the archive any more, got %v", err)
	}
	if !engine.sawStatement(`mutation deleteSite(siteId: "v1:platform:site:storefront"`) {
		t.Fatal("the live app was not deleted")
	}
}

// Auto-deploy is disarmed when it was armed, and left alone when it was not:
// an archived source must not fetch on a timer, and a write that changes
// nothing is a version that records nothing.
func TestPackageArchiveDisarmsAutoDeployOnlyWhenArmed(t *testing.T) {
	i, engine := archiveHarness(t, packageSite("docs", "docs.example.com", siteStatusDisabled))
	armed := ownerPackageDeclaring("docs")
	armed["autoDeploy"] = true
	engine.rows["query packageById"] = []map[string]any{armed}
	if _, err := i.handleArchivePackage(callerCtx("v1:identity:user:someone"), archiveArgs(), 0); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if !engine.sawStatement("mutation setPackageAutoDeploy(") {
		t.Fatal("an armed source was archived with auto-deploy left on")
	}

	j, jengine := archiveHarness(t, packageSite("docs", "docs.example.com", siteStatusDisabled))
	if _, err := j.handleArchivePackage(callerCtx("v1:identity:user:someone"), archiveArgs(), 0); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if jengine.sawStatement("mutation setPackageAutoDeploy(") {
		t.Fatal("a source that was never armed got a disarm write")
	}
}

// A source with no sites at all -- never deployed, or every app already
// deactivated -- still puts its catalogue on the off-list and archives.
func TestPackageArchiveWithNoSitesStillDeactivatesTheCatalogue(t *testing.T) {
	i, engine := archiveHarness(t)
	if _, err := i.handleArchivePackage(callerCtx("v1:identity:user:someone"), archiveArgs(), 0); err != nil {
		t.Fatalf("archive: %v", err)
	}
	got := cascadeWrites(engine.statements())
	want := []string{`disable ["storefront","docs"]`, "package archived"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// A site write the guard refuses -- surfaced, and the cascade STOPS there:
// the package row is not flipped over an app that did not come down, so the
// record never claims more than the reality.
func TestPackageArchiveStopsWhenTheGuardRefusesASite(t *testing.T) {
	i, engine := archiveHarness(t,
		packageSite("storefront", "shop.example.com", siteStatusDisabled),
		packageSite("docs", "docs.example.com", siteStatusDisabled),
	)
	engine.fail = map[string]error{`deleteSite(siteId: "v1:platform:site:storefront"`: errRefusedByGuard}
	_, err := i.handleArchivePackage(callerCtx("v1:identity:user:someone"), archiveArgs(), 0)
	if err == nil || !strings.Contains(err.Error(), errRefusedByGuard.Error()) {
		t.Fatalf("the guard's refusal must surface, got %v", err)
	}
	if engine.sawStatement("mutation setPackageStatus") {
		t.Fatal("the package must not flip over an app that did not come down")
	}
	if engine.sawStatement(`deleteSite(siteId: "v1:platform:site:docs"`) {
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
	if got := cascadeWrites(engine.statements()); len(got) != 0 {
		t.Fatalf("a mismatched name writes nothing, got %v", got)
	}
}

var errRefusedByGuard = refuse("site_status_refused", "v1:platform:site: a systemOwned row is exempt from the lifecycle")

// ---------------------------------------------------------------------------
// packageDeactivateDeployable (design D2)
// ---------------------------------------------------------------------------

func deactivateArgs(name, confirm string) map[string]any {
	return map[string]any{"packageId": "v1:platform:package:abc", "deployableName": name, "confirmName": confirm}
}

// Deactivating ONE app: its domains come down, its site is deleted (the name
// is free at that write), and the name goes on the off-list -- in that order,
// the site before the preference, because the deletion is the act and a
// failure to record the preference must not leave a deployable nobody asked
// to keep.
func TestDeactivateReleasesTheAppAndPutsItOnTheOffList(t *testing.T) {
	i, engine := archiveHarness(t,
		packageSite("storefront", "shop.example.com", siteStatusLive),
		packageSite("docs", "docs.example.com", siteStatusLive),
	)
	nodes, err := i.handleDeactivateDeployable(callerCtx("v1:identity:user:someone"), deactivateArgs("storefront", "storefront"), 0)
	if err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	reply := replyPayload(t, nodes)
	if reply["hostname"] != "shop.example.com" || reply["deployableName"] != "storefront" {
		t.Fatalf("reply %v", reply)
	}
	got := cascadeWrites(engine.statements())
	want := []string{
		"release v1:platform:site:storefront",
		"delete v1:platform:site:storefront",
		`disable ["storefront"]`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	// The sibling is untouched, and auto-deploy stays armed while it serves.
	if engine.sawStatement(`siteId: "v1:platform:site:docs"`) {
		t.Fatal("deactivating one app touched its sibling")
	}
	if engine.sawStatement("mutation setPackageAutoDeploy(") {
		t.Fatal("a sibling still serves, so auto-deploy must stay armed")
	}
}

// The last app of a source disarms auto-deploy, exactly as its delete did.
func TestDeactivateDisarmsAutoDeployOnTheLastApp(t *testing.T) {
	i, engine := archiveHarness(t, packageSite("storefront", "shop.example.com", siteStatusLive))
	if _, err := i.handleDeactivateDeployable(callerCtx("v1:identity:user:someone"), deactivateArgs("storefront", "storefront"), 0); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if !engine.sawStatement("mutation setPackageAutoDeploy(") {
		t.Fatal("the last app of a source was deactivated and auto-deploy was left armed")
	}
}

// An app the source only DECLARES -- no site -- deactivates too: nothing to
// release, nothing to delete, and the name still goes on the off-list. That
// is what skipping at the gate writes, reached from the app's own page.
func TestDeactivateAnAppWithNoSiteOnlyWritesTheOffList(t *testing.T) {
	i, engine := archiveHarness(t, packageSite("storefront", "shop.example.com", siteStatusLive))
	nodes, err := i.handleDeactivateDeployable(callerCtx("v1:identity:user:someone"), deactivateArgs("docs", "docs"), 0)
	if err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	reply := replyPayload(t, nodes)
	if reply["siteId"] != "" {
		t.Fatalf("no site was involved, reply %v", reply)
	}
	got := cascadeWrites(engine.statements())
	if strings.Join(got, ",") != `disable ["docs"]` {
		t.Fatalf("got %v", got)
	}
}

// The confirmation is the APP'S NAME, not its hostname, and it is verified
// server-side before anything is written.
func TestDeactivateVerifiesTheTypedAppName(t *testing.T) {
	i, engine := archiveHarness(t, packageSite("storefront", "shop.example.com", siteStatusLive))
	_, err := i.handleDeactivateDeployable(callerCtx("v1:identity:user:someone"), deactivateArgs("storefront", "shop.example.com"), 0)
	if RefusalCode(err) != CodeDeactivateConfirmationMismatch {
		t.Fatalf("want %s, got %v", CodeDeactivateConfirmationMismatch, err)
	}
	if got := cascadeWrites(engine.statements()); len(got) != 0 {
		t.Fatalf("a mismatched name writes nothing, got %v", got)
	}
	// A name the source does not declare is refused by name, not written to
	// the off-list as a typo that matches nothing.
	_, err = i.handleDeactivateDeployable(callerCtx("v1:identity:user:someone"), deactivateArgs("nope", "nope"), 0)
	if RefusalCode(err) != CodeSourceUnreadable {
		t.Fatalf("an undeclared app: want %s, got %v", CodeSourceUnreadable, err)
	}
}

// One of the cluster's own surfaces is refused, whoever asks.
func TestDeactivateRefusesASystemOwnedSite(t *testing.T) {
	owned := packageSite("storefront", "shop.example.com", siteStatusLive)
	owned["systemOwned"] = true
	i, engine := archiveHarness(t, owned)
	_, err := i.handleDeactivateDeployable(callerCtx("v1:identity:user:someone"), deactivateArgs("storefront", "storefront"), 0)
	if RefusalCode(err) != CodeSiteSystemOwned {
		t.Fatalf("want %s, got %v", CodeSiteSystemOwned, err)
	}
	if engine.sawStatement("mutation deleteSite(") || engine.sawStatement("mutation disablePackageDeployables(") {
		t.Fatal("a system-owned surface was written")
	}
}

// ---------------------------------------------------------------------------
// The confirmation is the thing's NAME (design D9)
// ---------------------------------------------------------------------------

// The label under the cluster's domain is enough; the whole hostname still
// works, and anything else is refused.
func TestConfirmationMatchesTheLabelOrTheWholeHostname(t *testing.T) {
	for _, typed := range []string{"shop", "shop.example.com", " shop "} {
		if !confirmationMatches(typed, "shop.example.com") {
			t.Errorf("%q should confirm shop.example.com", typed)
		}
	}
	for _, typed := range []string{"", "sho", "shop.example", "example.com", "shop.example.co"} {
		if confirmationMatches(typed, "shop.example.com") {
			t.Errorf("%q must not confirm shop.example.com", typed)
		}
	}
	// A bare apex has one label and it IS the hostname.
	if !confirmationMatches("example.com", "example.com") || confirmationMatches("example", "example.com") {
		t.Error("an apex confirms as itself and its first label is not a name on its own")
	}
}

func TestArchiveAndDeleteAcceptTheLabel(t *testing.T) {
	i, engine := deleteHarness(t, deletableSite(siteStatusArchived))
	if _, err := i.handleDeleteSite(callerCtx("v1:identity:user:someone"), deleteArgs("shop"), 0); err != nil {
		t.Fatalf("the label must confirm a delete, got %v", err)
	}
	if !hasCall(engine.statements(), "mutation deleteSite(") {
		t.Fatal("the label confirmed and nothing was deleted")
	}

	j, jengine := deleteHarness(t, deletableSite(siteStatusDisabled))
	if _, err := j.handleArchiveSite(callerCtx("v1:identity:user:someone"), deleteArgs("shop"), 0); err != nil {
		t.Fatalf("the label must confirm an archive, got %v", err)
	}
	if !hasCall(jengine.statements(), "mutation setSiteStatus(") {
		t.Fatal("the label confirmed and nothing was archived")
	}
}
