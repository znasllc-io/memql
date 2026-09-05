package packages

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/edge"
)

// delete_cancel_skip_test.go -- the three verbs epic memql#4937 adds.
//
// Each of the three is asserted on the thing the design says PROVES it, not
// on the mechanism that gets there:
//
//	siteDelete   the name becomes reusable, and the domains came down first
//	cancel       the BOUNDARY holds -- not a race between two goroutines
//	skip         a partial deploy is legible, and does not trip binding_missing
//
// The negative cases carry as much of the design as the positive ones: a live
// site is not deletable, a rolling run is not cancellable, and a skipped app
// that has never been deployed must NOT be refused for the hostname it
// deliberately does not have.

// ---------------------------------------------------------------------------
// siteDelete
// ---------------------------------------------------------------------------

func deleteHarness(t *testing.T, site map[string]any, siblings ...map[string]any) (*Integration, *recordingEngine) {
	t.Helper()
	engine := &recordingEngine{rows: map[string][]map[string]any{
		"query siteById":        {site},
		"query sitesForPackage": append([]map[string]any{site}, siblings...),
		"builtin customDomainReleaseForSite": {{
			"siteId": site["id"], "requested": float64(2), "alreadyRemoved": float64(0),
		}},
	}}
	i := NewIntegration(engine, discardLogger())
	i.depsOnce.Do(func() {
		i.deps = &Deps{Store: &store{engine: engine}, Logger: discardLogger()}
	})
	return i, engine
}

func deletableSite(status string) map[string]any {
	return map[string]any{
		"id":                    "v1:platform:site:shop",
		"hostname":              "shop.example.com",
		"status":                status,
		"packageId":             "v1:platform:package:abc",
		"packageDeployableName": "storefront",
	}
}

func deleteArgs(hostname string) map[string]any {
	return map[string]any{"siteId": "v1:platform:site:shop", "confirmHostname": hostname}
}

// THE ASSERTION THAT PROVES THE FEATURE. `deleted` is the only field
// liveSiteIdsForHostname excludes on -- it never reads `status` -- so this
// write, and nothing before it in the cascade, is what makes the name
// reusable. Archiving does none of it, which is the bug the epic exists for.
func TestDeleteReleasesTheNameAndTakesTheDomainsDownFirst(t *testing.T) {
	i, engine := deleteHarness(t, deletableSite(siteStatusArchived))
	nodes, err := i.handleDeleteSite(callerCtx("v1:identity:user:someone"), deleteArgs("shop.example.com"), 0)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	reply := replyPayload(t, nodes)
	if reply["hostname"] != "shop.example.com" {
		t.Fatalf("reply %v", reply)
	}

	stmts := engine.statements()
	domainsAt, deleteAt := -1, -1
	for n, q := range stmts {
		if strings.HasPrefix(q, "builtin customDomainReleaseForSite(") {
			domainsAt = n
		}
		if strings.HasPrefix(q, "mutation deleteSite(") {
			deleteAt = n
		}
	}
	if deleteAt < 0 {
		t.Fatalf("the site was never stamped deleted, so the hostname is still held: %v", stmts)
	}
	if domainsAt < 0 {
		t.Fatalf("the domains were never released, so the Ingress and Certificate stay applied: %v", stmts)
	}
	// THE ORDER IS THE DESIGN. The site is stamped LAST, so a failure part-way
	// leaves a deployable that is still findable and still says what state it
	// is in -- rather than an invisible row holding a name nobody can reclaim
	// and nobody can see.
	if domainsAt > deleteAt {
		t.Fatalf("the domains must come down BEFORE the site row is stamped, got release at %d and delete at %d", domainsAt, deleteAt)
	}
}

// A source whose apps are all gone should stop fetching on a timer.
func TestDeleteDisarmsAutoDeployOnTheLastServableApp(t *testing.T) {
	i, engine := deleteHarness(t, deletableSite(siteStatusArchived))
	if _, err := i.handleDeleteSite(callerCtx("v1:identity:user:someone"), deleteArgs("shop.example.com"), 0); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !hasCall(engine.statements(), "mutation setPackageAutoDeploy(") {
		t.Fatal("the last app of a source was deleted and auto-deploy was left armed")
	}
}

// ...and a source that still has one does NOT, because it still has something
// to deploy.
func TestDeleteLeavesAutoDeployArmedWhileASiblingCanStillServe(t *testing.T) {
	sibling := map[string]any{
		"id": "v1:platform:site:web", "hostname": "web.example.com",
		"status": siteStatusLive, "packageId": "v1:platform:package:abc",
	}
	i, engine := deleteHarness(t, deletableSite(siteStatusArchived), sibling)
	if _, err := i.handleDeleteSite(callerCtx("v1:identity:user:someone"), deleteArgs("shop.example.com"), 0); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if hasCall(engine.statements(), "mutation setPackageAutoDeploy(") {
		t.Fatal("a sibling is still serving, so the source still has work -- auto-deploy must stay armed")
	}
}

// Delete runs only from the two states nothing is served from. `live` and
// `disabled` are refused NAMING THE NEXT STEP, because "pause it, then
// archive it" is the whole answer rather than a scolding.
func TestDeleteRefusesWhatIsStillServing(t *testing.T) {
	for _, status := range []string{siteStatusLive, siteStatusDisabled} {
		i, engine := deleteHarness(t, deletableSite(status))
		_, err := i.handleDeleteSite(callerCtx("v1:identity:user:someone"), deleteArgs("shop.example.com"), 0)
		if RefusalCode(err) != CodeSiteNotDeletable {
			t.Fatalf("status %q: want %s, got %v", status, CodeSiteNotDeletable, err)
		}
		// NOTHING WAS WRITTEN. A refusal that had already taken the domains
		// down would leave the deployable serving at its cluster address with
		// its client's domain gone.
		if hasCall(engine.statements(), "builtin customDomainReleaseForSite(") ||
			hasCall(engine.statements(), "mutation deleteSite(") {
			t.Fatalf("status %q: a refused delete wrote something: %v", status, engine.statements())
		}
	}
}

// A draft skips the disable-then-archive ceremony entirely (D5): it resolves
// for nobody, so the pause that exists to let people notice has nobody to
// notify. This is the dead end the epic was reported for.
func TestDraftIsDeletableWithoutBeingArchivedFirst(t *testing.T) {
	i, _ := deleteHarness(t, deletableSite(siteStatusDraft))
	if _, err := i.handleDeleteSite(callerCtx("v1:identity:user:someone"), deleteArgs("shop.example.com"), 0); err != nil {
		t.Fatalf("a draft must be discardable in one step, got %v", err)
	}
}

func TestDeleteRefusesAMistypedHostnameAndASystemOwnedRow(t *testing.T) {
	i, engine := deleteHarness(t, deletableSite(siteStatusArchived))
	_, err := i.handleDeleteSite(callerCtx("v1:identity:user:someone"), deleteArgs("shop.example.co"), 0)
	if RefusalCode(err) != CodeDeleteConfirmationMismatch {
		t.Fatalf("want %s, got %v", CodeDeleteConfirmationMismatch, err)
	}
	if hasCall(engine.statements(), "mutation deleteSite(") {
		t.Fatal("a mistyped confirmation deleted the site")
	}

	owned := deletableSite(siteStatusArchived)
	owned["systemOwned"] = true
	j, jengine := deleteHarness(t, owned)
	_, err = j.handleDeleteSite(callerCtx("v1:identity:user:someone"), deleteArgs("shop.example.com"), 0)
	if RefusalCode(err) != CodeSiteSystemOwned {
		t.Fatalf("want %s, got %v", CodeSiteSystemOwned, err)
	}
	if hasCall(jengine.statements(), "mutation deleteSite(") {
		t.Fatal("one of the cluster's own surfaces was deleted")
	}
}

// ---------------------------------------------------------------------------
// cancel
// ---------------------------------------------------------------------------

func cancelHarness(t *testing.T, status string) (*Integration, *recordingEngine) {
	t.Helper()
	engine := &recordingEngine{rows: map[string][]map[string]any{
		"query packageDeploymentById": {{
			"id":        "v1:platform:packageDeployment:run",
			"packageId": "v1:platform:package:abc",
			"status":    status,
		}},
	}}
	i := NewIntegration(engine, discardLogger())
	i.depsOnce.Do(func() {
		i.deps = &Deps{Store: &store{engine: engine}, Logger: discardLogger()}
	})
	return i, engine
}

func cancelArgs() map[string]any {
	return map[string]any{
		"packageId":    "v1:platform:package:abc",
		"deploymentId": "v1:platform:packageDeployment:run",
	}
}

// THE BOUNDARY, ASSERTED AS A BOUNDARY. Everything before the roll can be
// stopped; from staging_dsl on the ASK is refused, so a person is told rather
// than left watching a Cancel that will never be read. Sequencing goroutines
// to try to catch a run mid-roll would assert timing rather than the guard.
func TestCancelIsOfferedUpToTheRollAndRefusedFromItOn(t *testing.T) {
	cancellable := []string{StatusAnalyzing, StatusAwaitingConfirm, StatusBuilding}
	for _, status := range cancellable {
		if !CancellableStage(status) {
			t.Fatalf("%q is before the roll and must be cancellable", status)
		}
	}
	for _, status := range []string{StatusStagingDsl, StatusRolling, StatusPublishing} {
		if CancellableStage(status) {
			t.Fatalf("%q is at or past the roll -- stopping there leaves the cluster half-rolled", status)
		}
		i, engine := cancelHarness(t, status)
		_, err := i.handleCancelDeployment(callerCtx("v1:identity:user:someone"), cancelArgs(), 0)
		if RefusalCode(err) != CodeDeploymentNotCancellable {
			t.Fatalf("status %q: want %s, got %v", status, CodeDeploymentNotCancellable, err)
		}
		if hasCall(engine.statements(), "mutation requestPackageDeploymentCancel(") {
			t.Fatalf("status %q: a refused cancel set the flag anyway", status)
		}
	}
}

// A RUNNING run is FLAGGED and not closed: the node running it is the only
// writer that closes the row, so the timeline can never claim a run stopped
// while its build is still going somewhere.
func TestCancellingARunningDeploymentFlagsItRatherThanClosingIt(t *testing.T) {
	i, engine := cancelHarness(t, StatusBuilding)
	if _, err := i.handleCancelDeployment(callerCtx("v1:identity:user:someone"), cancelArgs(), 0); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !hasCall(engine.statements(), "mutation requestPackageDeploymentCancel(") {
		t.Fatal("a running run must be flagged")
	}
	if hasCall(engine.statements(), "mutation closePackageDeployment(") {
		t.Fatal("the capability closed a row the running node owns")
	}
}

// A PARKED run is the one exception, and its premise is why: nothing is
// running, so nothing would ever read the flag. Left flagged it would stay
// non-terminal until the abandoned sweep closed it as a LOST run -- blaming
// the cluster for a person's decision.
func TestCancellingAParkedRunClosesItCancelledRatherThanLeavingItToTheSweep(t *testing.T) {
	i, engine := cancelHarness(t, StatusAwaitingConfirm)
	nodes, err := i.handleCancelDeployment(callerCtx("v1:identity:user:someone"), cancelArgs(), 0)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := replyPayload(t, nodes)["status"]; got != StatusCancelled {
		t.Fatalf("want %q, got %v", StatusCancelled, got)
	}
	closed := ""
	for _, q := range engine.statements() {
		if strings.HasPrefix(q, "mutation closePackageDeployment(") {
			closed = q
		}
	}
	if closed == "" {
		t.Fatalf("a parked run must be closed here: %v", engine.statements())
	}
	if !strings.Contains(closed, `status: "`+StatusCancelled+`"`) {
		t.Fatalf("a parked run must close as %q, not as a failure: %s", StatusCancelled, closed)
	}
}

func TestCancellingATerminalRunIsRefused(t *testing.T) {
	for _, status := range []string{StatusSucceeded, StatusFailed, StatusRefused, StatusAbandoned, StatusCancelled} {
		i, _ := cancelHarness(t, status)
		_, err := i.handleCancelDeployment(callerCtx("v1:identity:user:someone"), cancelArgs(), 0)
		if RefusalCode(err) != CodeDeploymentNotCancellable {
			t.Fatalf("status %q: want %s, got %v", status, CodeDeploymentNotCancellable, err)
		}
	}
}

// `cancelled` is terminal, and NOT a flavour of failed. A surface that read it
// as a failure would report a person's own click back to them as a fault --
// the same distinction `abandoned` draws, for a sharper reason.
func TestCancelledIsItsOwnTerminalStatus(t *testing.T) {
	if !IsTerminal(StatusCancelled) {
		t.Fatal("a cancelled run is finished")
	}
	if StatusCancelled == StatusFailed || StatusCancelled == StatusAbandoned {
		t.Fatal("cancelled must be its own word")
	}
}

// ---------------------------------------------------------------------------
// skip (memql#4930)
// ---------------------------------------------------------------------------

// The case memql#4930 was raised from: a source declaring a storefront and a
// starter SPA, where the operator wanted only the storefront.
//
// DRIVEN THROUGH publish() ITSELF rather than asserted on the Placement
// struct: what matters is that the stage records the skip and does not refuse
// the run, and a test of the struct would pass against a pipeline that ignored
// the field entirely.
func TestASkippedAppIsRecordedAndDoesNotTripBindingMissing(t *testing.T) {
	pub := &fakePublisher{}
	engine := &recordingEngine{rows: map[string][]map[string]any{
		"query sitesForPackage": nil, // NEITHER app has ever been deployed
	}}
	d := &Deps{Store: &store{engine: engine}, Publisher: pub, Logger: discardLogger()}
	req := DeployRequest{
		PackageId: "v1:platform:package:abc",
		Placements: map[string]Placement{
			"storefront": {Hostname: "shop.example.com"},
			"web":        {Skip: true}, // no hostname, deliberately
		},
	}
	rep := &Report{Deployables: []DeployableReport{{Name: "storefront"}, {Name: "web"}}}
	bundles := map[string]edge.Bundle{"storefront": {}}

	outcomes, err := d.publish(callerCtx("v1:identity:user:someone"), req, map[string]any{}, rep, bundles)
	if err != nil {
		// The failure this guards against: `web` has never been deployed and
		// has no hostname, so without the skip short-circuit the stage refuses
		// the WHOLE run for deployable_binding_missing -- which would make
		// "deploy only the storefront" impossible on a first deploy, exactly
		// when somebody wants it.
		t.Fatalf("a skipped app must not refuse the run: %v (code %q)", err, RefusalCode(err))
	}

	// ONE ENTRY PER MANIFEST DEPLOYABLE. The row's `deployables` promises it,
	// and a missing entry reads as "nothing happened" where the truth is "you
	// chose not to".
	if len(outcomes) != 2 {
		t.Fatalf("want an outcome for each declared app, got %d: %+v", len(outcomes), outcomes)
	}
	byName := map[string]DeployableOutcome{}
	for _, o := range outcomes {
		byName[o.Name] = o
	}
	skipped, ok := byName["web"]
	if !ok || skipped.Refusal == nil || skipped.Refusal.Code != CodeDeployableSkipped {
		t.Fatalf("the skipped app must be recorded as skipped, got %+v", byName["web"])
	}
	if skipped.Refusal.Fatal {
		t.Fatal("a partial deploy is a complete run -- a deliberate skip is not fatal")
	}
	if skipped.SiteId != "" {
		t.Fatal("a skipped app got a site")
	}
	// ...and the one that was NOT skipped went out.
	if got := byName["storefront"]; got.SiteId == "" {
		t.Fatalf("the app that was chosen must still deploy, got %+v", got)
	}
	if len(pub.published) != 1 {
		t.Fatalf("exactly one app should have been published, got %v", pub.published)
	}
	if CodeDeployableSkipped == CodeDeployableHostnameUnchosen {
		t.Fatal("a deliberate skip and a missing binding are different answers")
	}
}

// THE REACHABLE POSITIVE for the test above: with the SAME two apps and no
// skip, the run IS refused for the missing binding. Without this, the test
// above would pass against a publish stage that had stopped refusing anything.
func TestWithoutTheSkipTheSameRunIsRefusedForTheMissingBinding(t *testing.T) {
	pub := &fakePublisher{}
	engine := &recordingEngine{rows: map[string][]map[string]any{"query sitesForPackage": nil}}
	d := &Deps{Store: &store{engine: engine}, Publisher: pub, Logger: discardLogger()}
	req := DeployRequest{
		PackageId: "v1:platform:package:abc",
		Placements: map[string]Placement{
			"storefront": {Hostname: "shop.example.com"},
			"web":        {}, // not skipped, and still no hostname
		},
	}
	rep := &Report{Deployables: []DeployableReport{{Name: "storefront"}, {Name: "web"}}}
	bundles := map[string]edge.Bundle{"storefront": {}, "web": {}}

	_, err := d.publish(callerCtx("v1:identity:user:someone"), req, map[string]any{}, rep, bundles)
	if RefusalCode(err) != CodeDeployableHostnameUnchosen {
		t.Fatalf("want %s so the skip test is measuring something, got %v", CodeDeployableHostnameUnchosen, err)
	}
}

// The wire shape carries it, so a browser can express the choice at all.
func TestPlacementsArgReadsSkip(t *testing.T) {
	got := placementsArg(map[string]any{"placements": map[string]any{
		"storefront": map[string]any{"hostname": "shop.example.com"},
		"web":        map[string]any{"skip": true},
	}}, "placements")
	if got["storefront"].Skip {
		t.Fatal("an app with a hostname is not skipped")
	}
	if !got["web"].Skip {
		t.Fatal("skip:true did not survive the wire shape")
	}
}

// hasCall reports whether any recorded statement starts with prefix.
func hasCall(stmts []string, prefix string) bool {
	for _, q := range stmts {
		if strings.HasPrefix(q, prefix) {
			return true
		}
	}
	return false
}
