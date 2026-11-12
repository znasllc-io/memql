package wholesalepack_test

// live_e2e_test.go -- THE WHOLE LIFECYCLE AGAINST A REAL DATABASE AND A
// REAL ENGINE (epic memql#5533).
//
// The pack's other tests prove its decisions are right as functions over
// rows, behind the rowReader and Caller seams. Those seams are what make
// the state machine testable and they are also what hides everything below:
//
//	PROOF 1 -- A BUILTIN'S NODES ARE PERSISTED. The whole pack rests on
//	  this. Every write it makes is a builtin returning MemoryNodes, because
//	  every one of them needs a cross-row read first -- so if a builtin's
//	  reply were not stored, nothing in this pack would work and every
//	  fixture-backed test would still pass.
//
//	PROOF 2 -- THE applicationsOpen GATE HOLDS THROUGH THE REAL ENGINE, and
//	  absent settings really do refuse. This is a shopper-reachable write
//	  endpoint; "fail closed" has to be measured rather than asserted.
//
//	PROOF 3 -- storeId SCOPES THE READ (design D9), through the real query
//	  and the real row-authz gate. This is what keeps an application
//	  submitted while previewing a storefront out of the live store's queue.
//
//	PROOF 4 -- THE DECISION LOG FOLDS. Append-only storage plus a derived
//	  state is the pack's central decision, and this is the only place both
//	  halves meet real persistence: two decisions written, the newest
//	  winning, and the first one's reason still readable afterwards.
//
//	PROOF 5 -- AN ILLEGAL TRANSITION IS REFUSED AGAINST THE STORED LOG,
//	  rather than against a fixture the test handed itself.
//
//	PROOF 6 -- ownerUserId IS THE MERCHANT, so the borrowed-authority path
//	  really does make the site owner the owner. If it stamped the applicant
//	  or an empty string the rows would be owned by nobody and every
//	  merchant read would answer zero.
//
// Postgres-gated like every *_db_test.go: skips when no database is
// reachable, and MEMQL_REQUIRE_DB=1 turns that skip into a failure.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	memqldsl "github.com/znasllc-io/memql/dsl"
	"github.com/znasllc-io/memql/integrations/shopify"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
	"github.com/znasllc-io/memql/integrations/shopify/shopifylive"
	wholesalepack "github.com/znasllc-io/memql/packs/wholesalepack"
)

// registerPackOnce guards the registrations a BOOT makes exactly once.
var registerPackOnce sync.Once

// EVERY IDENTIFIER IS UNIQUE PER RUN. The db-gated tests do not clean up, so
// a throwaway database accumulates rows across runs and a test asserting an
// EXACT count against a fixed storeId fails on its second run against code
// that is perfectly correct.
func wholesaleScope() (merchant, storeLive, storeDev string) {
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	return "user-wholesale-" + n, "store-wholesale-live-" + n, "store-wholesale-dev-" + n
}

func liveWholesaleEngine(t *testing.T, disabled ...string) *memql.MemQLEngine {
	t.Helper()
	dsn := dbtest.DSN()
	probe := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	if err := probe.PingContext(context.Background()); err != nil {
		_ = probe.Close()
		dbtest.Unreachable(t, "wholesale live e2e", dsn, err)
	}
	_ = probe.Close()

	_ = os.Setenv("MEMQL_DATABASE_DSN", dsn)
	mnd, err := memoryNodes.NewMemoryNodesDatabase()
	if err != nil {
		t.Fatalf("NewMemoryNodesDatabase: %v", err)
	}
	mnd.Start(context.Background())
	select {
	case <-mnd.Ready():
	case <-time.After(60 * time.Second):
		t.Fatal("database did not become ready within 60s")
	}
	bunDB := mnd.BunDB()
	if bunDB == nil {
		t.Fatal("nil bun.DB after migrate")
	}

	// REGISTERED THE WAY app/anchor_storefront_packs.go DOES IT, so the
	// adapters are in the registry and the shopper surface is declared --
	// which is what makes this a test of the pack rather than of its DSL.
	//
	// ONCE PER PROCESS, and the engine is RIGHT to insist: a second
	// RegisterShopperForm for one route panics rather than winning, because
	// "the surface a shopper reaches must be readable off the declarations,
	// and last-wins makes which one is live depend on init order". A boot
	// registers once, so the harness does too -- the tree is re-mounted per
	// test because each one unregisters it on cleanup.
	// AND THE TREE STAYS MOUNTED for the process, which is also what a boot
	// does: RegisterTree panics on a second registration of one namespace,
	// for the same reason the shopper route does. The only per-test state is
	// the disabled set below.
	registerPackOnce.Do(func() { wholesalepack.Register(wholesalepack.Domain) })

	memqldsl.SetDisabledPackDomains(disabled)
	t.Cleanup(func() { memqldsl.SetDisabledPackDomains(nil) })

	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memql.New(bunDB)
	if err != nil {
		t.Fatalf("engine New: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("engine.Init: %v", err)
	}
	// GRANTS ARE READ ONLY THROUGH AN INSTALLED SOURCE, which an engine that
	// never Starts must install itself -- or the wholesale part a store's
	// person is granted would be held by nobody (packs/reviewspack's live
	// harness does the same).
	eng.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(nil); auth.SetMembershipSource(nil) })

	// MATERIALIZE THE GO HALF, which app.materializePlugins does at boot and
	// which reviewspack's own live harness never needed: that pack's live
	// proofs go through a MUTATION and a query, so its executors were never
	// asked for. Every write THIS pack makes is a builtin, so without this
	// step every call answers `unknown builtin executor` -- which is exactly
	// what the first run of this file reported, and is the reason it exists.
	//
	// A DISABLED PACK IS NOT MATERIALIZED, which is app's own rule (module
	// registry, 4.2) and the Go half of the same switch the DSL layer flips:
	// the pack's concepts stay, and its behaviour goes.
	if !slices.Contains(disabled, wholesalepack.Domain) {
		prov, err := wholesalepack.NewProvider(memql.PluginContext{
			Engine: eng,
			Logger: eng.Logger,
		})
		if err != nil {
			t.Fatalf("NewProvider: %v", err)
		}
		if err := eng.RegisterIntegration(prov); err != nil {
			t.Fatalf("RegisterIntegration: %v", err)
		}
	}
	return eng
}

// asSiteOwner is the borrowed authority the bff applies on the shopper path:
// the SITE OWNER'S actor, exactly as auth.ContextWithUserActor builds it.
//
// IN THE FORM A REAL ACTOR CARRIES: a signed-in person's actor.userId is the
// canonical v1:identity:user:<id> (component/auth/identity_resolver.go), and
// the owner the edge stamps is the site row's stored, canonical owner. The
// read predicate compares ownerUserId with actor.userId as written, so a test
// actor in any other form would read rows no real one does.
func asSiteOwner(userID string) context.Context {
	return auth.ContextWithUserActor(context.Background(), userRef(userID))
}

// userRef is a person's id in canonical form.
func userRef(userID string) string {
	if strings.HasPrefix(userID, "v1:identity:user:") {
		return userID
	}
	return "v1:identity:user:" + userID
}

func wsQuote(s string) string { return `"` + s + `"` }

// asDecider is the store's own person deciding: the merchant, signed in as
// themselves and granted the wholesale part the way a store's person is granted
// it. A decision is gated by the part, and the cluster's operators decide none.
func asDecider(t *testing.T, eng *memql.MemQLEngine, userID string) context.Context {
	t.Helper()
	grantWholesalePart(t, eng, userID)
	return signedIn(userID)
}

// grantWholesalePart gives one person the wholesale part, at the id the grant
// writer derives.
func grantWholesalePart(t *testing.T, eng *memql.MemQLEngine, userID string) {
	t.Helper()
	call := `writeGrant(grantId: ` + quote(auth.GrantRowID(auth.SubjectKindUser, userRef(userID), "execute", "app:deployables/wholesale")) +
		`, subjectKind: "user", subjectId: ` + quote(userRef(userID)) +
		`, verb: "execute", resourceType: "app:deployables/wholesale", effect: "allow", grantedBy: "wholesale-e2e-seeder")`
	if _, err := eng.Execute(seedCtx(), call); err != nil {
		t.Fatalf("grant the wholesale part: %v", err)
	}
}

func wsRows(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, q string) []map[string]any {
	t.Helper()
	result, err := eng.Execute(ctx, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return memql.MaterializeRows(result)
}

func wsExec(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, call string) {
	t.Helper()
	if _, err := eng.Execute(ctx, call); err != nil {
		t.Fatalf("%s: %v", call, err)
	}
}

// openApplications sets a store's settings as trusted server code acting as
// ctx's actor: wholesaleSetSettings admits internal origin or an operator.
//
// A SET-UP ALWAYS TIES THE STORE TO AN ORGANIZATION (ruling 55), through the
// one site bound to the store or a site named. A store a test bound to its own
// site is set up through it; a store with no site bound is given one here --
// owned by ctx's actor, the store's merchant, and of an organization of its own
// in which the merchant holds the wholesale part -- and set up naming it, so
// the row is the merchant's, as it was before.
func openApplications(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, storeID, adapter string) {
	t.Helper()
	call := func(site string) error {
		named := ""
		if site != "" {
			named = `, siteId: ` + wsQuote(site)
		}
		_, err := eng.Execute(auth.ContextWithInternalOrigin(ctx), `builtin wholesaleSetSettings(applicationsOpen: true, `+
			`entitlementAdapter: `+wsQuote(adapter)+`, storeId: `+wsQuote(storeID)+named+`)`)
		return err
	}
	err := call("")
	if err != nil && strings.Contains(err.Error(), wholesalepack.NameTheSiteSentence) {
		ac, _ := auth.AccessFromContext(ctx)
		owner := strings.TrimPrefix(ac.UserId, "v1:identity:user:")
		site, account := "site-of-"+storeID, "acct-of-"+storeID
		// THE MERCHANT IS THE STORE'S PERSON in its organization, holding the
		// part: applications are written tied to the site's organization
		// (ruling 56), so deciding them is judged there.
		group := seedOrganization(t, eng, account)
		grantGroup(t, eng, group, "read", "app:deployables")
		grantGroup(t, eng, group, "execute", "app:deployables/wholesale")
		seedPersonIn(t, eng, owner, owner, "writer", true, group, account, "added")
		seedSiteIn(t, eng, site, owner, storeID, account)
		err = call(site)
	}
	if err != nil {
		t.Fatalf("set up %s: %v", storeID, err)
	}
}

// storeSiteArgs is the one site bound to store, as the call arguments the shopper
// handler would have stamped: siteId and the site's accountId. A store takes
// applications only through its admitted site, carrying that site's
// organization (ruling 56), so a direct submission names both.
func storeSiteArgs(t *testing.T, eng *memql.MemQLEngine, storeID string) string {
	t.Helper()
	rows := wsRows(t, eng, asOperator("operator-"+storeID), `query sitesBoundToStore(storeIds: [`+wsQuote(storeID)+`])`)
	if len(rows) != 1 {
		t.Fatalf("store %s has %d sites bound, want one", storeID, len(rows))
	}
	return `siteId: ` + wsQuote(bareID(asString(rows[0]["id"]))) + `, accountId: ` + wsQuote(asString(rows[0]["accountId"]))
}

// submit stands in for component/server/shopper_handler.go: the pack's
// declared fields plus the three it stamps -- storeId, siteId, and the
// submission id it mints for this POST (design record 2026-09-21, D4).
//
// It RETURNS the submission id, because the application is written AT it and
// that is what a client's @shopperFormExtension relates its own row to.
func submit(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, storeID, company, email string) string {
	t.Helper()
	submissionID := "sub-" + id.NewShortId()
	wsExec(t, eng, ctx, `builtin wholesaleSubmitApplication(applicantEmail: `+wsQuote(email)+
		`, applicantName: "Sam Rivers", companyName: `+wsQuote(company)+
		`, `+storeSiteArgs(t, eng, storeID)+`, storeId: `+wsQuote(storeID)+
		`, submissionId: `+wsQuote(submissionID)+`)`)
	return submissionID
}

// ---------------------------------------------------------------------------

func TestLiveE2E_TheApplicationLifecycle(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, storeLive, storeDev := wholesaleScope()
	ctx := asSiteOwner(merchant)

	// ---- PROOF 2: absent settings refuse -------------------------------
	//
	// BEFORE any settings row exists. This endpoint is reachable by the
	// public, so "fail closed" is measured rather than asserted.
	if _, err := eng.Execute(ctx, `builtin wholesaleSubmitApplication(applicantEmail: "a@b.c"`+
		`, applicantName: "A", companyName: "Acme", storeId: `+wsQuote(storeLive)+
		`, submissionId: "sub-no-settings")`); err == nil {
		t.Fatal("a store with NO wholesale settings accepted an application: absent " +
			"settings must be closed, because a merchant who has never touched them has " +
			"not asked the internet for their customers' business details")
	}

	openApplications(t, eng, ctx, storeLive, wholesalepack.AdapterCustomerTag)

	// ---- PROOF 1: a builtin's nodes are persisted ----------------------
	liveSubmission := submit(t, eng, ctx, storeLive, "Acme Trading", "sam@acme.example")
	live := wsRows(t, eng, ctx, `query applicationsForStore(storeId: `+wsQuote(storeLive)+`)`)
	if len(live) != 1 {
		t.Fatalf("applicationsForStore returned %d rows, want 1 -- every write this pack "+
			"makes is a builtin returning nodes, so if those are not persisted nothing in "+
			"the pack works and every fixture-backed test still passes", len(live))
	}
	appID, _ := live[0]["id"].(string)
	// AND IT IS THE SUBMISSION. Not merely "it has an id": a client's
	// @shopperFormExtension relates its own row to the id the bff stamped,
	// so an application written anywhere else is one nothing can find
	// (design record 2026-09-21, D4).
	if !strings.HasSuffix(appID, liveSubmission) {
		t.Fatalf("the application landed at id %q, which does not carry the submission id %q "+
			"it was written under", appID, liveSubmission)
	}
	if appID == "" {
		t.Fatalf("the application carries no id; the engine must derive one because a "+
			"shopper may not choose one: %+v", live[0])
	}

	// ---- PROOF 6: ownerUserId is the MERCHANT --------------------------
	if got, _ := live[0]["ownerUserId"].(string); got != userRef(merchant) {
		t.Fatalf("ownerUserId = %q, want the site owner %q -- the applicant is "+
			"applicantName and applicantEmail, never an identity", got, merchant)
	}
	if got, _ := live[0]["applicantEmail"].(string); got != "sam@acme.example" {
		t.Fatalf("applicantEmail = %q", got)
	}

	// ---- PROOF 3: storeId scopes the read (design D9) ------------------
	//
	// The development store takes an application too, and the live store's
	// queue must not show it. This is what keeps an application submitted
	// while exercising a candidate version out of the live store's inbox.
	openApplications(t, eng, ctx, storeDev, wholesalepack.AdapterCustomerTag)
	_ = submit(t, eng, ctx, storeDev, "Written while previewing", "dev@acme.example")
	if again := wsRows(t, eng, ctx,
		`query applicationsForStore(storeId: `+wsQuote(storeLive)+`)`); len(again) != 1 {
		t.Fatalf("the live store's queue returned %d applications after one was written "+
			"against the development store; it must still return exactly the one written "+
			"against it", len(again))
	}

	// ---- PROOF 4: the decision log folds -------------------------------
	//
	// Rejected first, then approved. The newest decision wins AND the
	// first one's reason is still there, which is the whole argument for
	// append-only storage.
	state := stateOf(t, eng, ctx, appID)
	if state != wholesalepack.StateSubmitted {
		t.Fatalf("a fresh application is %q, want %q", state, wholesalepack.StateSubmitted)
	}
	decider := asDecider(t, eng, merchant)
	decide(t, eng, decider, appID, wholesalepack.TransitionReject, merchant, "no trading history")
	if state = stateOf(t, eng, ctx, appID); state != wholesalepack.StateRejected {
		t.Fatalf("after a reject the state is %q, want %q", state, wholesalepack.StateRejected)
	}

	// ---- PROOF 5: an illegal transition is refused against the LOG -----
	//
	// Nothing leads out of rejected. A caller that could say what state it
	// believed the application was in would be the one deciding.
	if _, err := eng.Execute(decider, `builtin wholesaleRecordDecision(applicationId: `+
		wsQuote(appID)+`, decidedBy: `+wsQuote(merchant)+
		`, principalKind: "client", transition: "approve")`); err == nil {
		t.Fatal("approving a REJECTED application was allowed; nothing leads out of " +
			"rejected, because re-deciding in place would overwrite why the first " +
			"decision was made")
	}

	// THE CLUSTER'S OPERATORS DECIDE NONE (X4), through the real engine and
	// not just the Go guard -- whatever principalKind the call names.
	if _, err := eng.Execute(asOperator("operator-"+merchant), `builtin wholesaleRecordDecision(applicationId: `+
		wsQuote(appID)+`, principalKind: "client", transition: "reject")`); err == nil ||
		!strings.Contains(err.Error(), "do not decide them") {
		t.Fatalf("a cluster operator's decision was not refused as one: %v", err)
	}

	// The reason for the refusal is STILL READABLE, which is the property
	// that would be lost to an overwritten state field.
	decisions := wsRows(t, eng, ctx,
		`query decisionsForApplication(applicationId: `+wsQuote(appID)+`)`)
	if len(decisions) != 1 {
		t.Fatalf("decisionsForApplication returned %d rows, want 1", len(decisions))
	}
	if note, _ := decisions[0]["note"].(string); note != "no trading history" {
		t.Fatalf("the decision's reason is %q; it must survive", note)
	}
	// AND IT IS SCOPED TO THE APPLICATION'S STORE, copied rather than
	// supplied.
	if got, _ := decisions[0]["storeId"].(string); got != storeLive {
		t.Fatalf("the decision's storeId is %q, want %q -- it is copied off the "+
			"application being decided", got, storeLive)
	}
}

// THE APPROVED PATH, ITS ENTITLEMENT, AND WHAT A FAILED PUSH LEAVES BEHIND.
//
// Split from the lifecycle above because it needs its own application: the
// transition table deliberately allows no way from rejected to approved.
func TestLiveE2E_ApprovalProvisionsAndAFailedPushKeepsTheApproval(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, storeLive, _ := wholesaleScope()
	ctx := asSiteOwner(merchant)

	openApplications(t, eng, ctx, storeLive, wholesalepack.AdapterCustomerTag)
	_ = submit(t, eng, ctx, storeLive, "Northwind Supply", "buyer@northwind.example")
	rows := wsRows(t, eng, ctx, `query applicationsForStore(storeId: `+wsQuote(storeLive)+`)`)
	if len(rows) != 1 {
		t.Fatalf("want 1 application, got %d", len(rows))
	}
	appID, _ := rows[0]["id"].(string)

	// THE PROVISIONING BUILTINS ARE THE STORE'S AUTOMATION'S, which calls
	// with internal origin; a direct call here arrives the same way.
	automation := auth.ContextWithInternalOrigin(ctx)

	// PROVISIONING AN UNAPPROVED APPLICATION IS REFUSED. The decision log
	// decides, not an argument.
	if _, err := eng.Execute(automation, `builtin wholesaleProvisionEntitlement(applicationId: `+
		wsQuote(appID)+`)`); err == nil {
		t.Fatal("an application nobody approved was provisioned")
	}

	decider := asDecider(t, eng, merchant)
	decide(t, eng, decider, appID, wholesalepack.TransitionApprove, merchant, "vouched for")
	if got := stateOf(t, eng, ctx, appID); got != wholesalepack.StateApproved {
		t.Fatalf("state = %q, want %q", got, wholesalepack.StateApproved)
	}

	// THE PUSH WILL FAIL: no Shopify connector is configured in this test
	// engine. That is the case worth measuring, because it is the one the
	// design speaks to -- "a provisioning failure leaves the application
	// approved and the entitlement pending with its error".
	wsExec(t, eng, automation, `builtin wholesaleProvisionEntitlement(applicationId: `+wsQuote(appID)+`)`)

	ents := wsRows(t, eng, ctx,
		`query entitlementForApplication(applicationId: `+wsQuote(appID)+`)`)
	if len(ents) == 0 {
		t.Fatal("a failed push left NO entitlement row. The merchant would have an " +
			"approved application, no wholesale prices, and nothing anywhere saying a " +
			"push had been attempted -- which is the state the row exists to prevent")
	}
	entState, _ := ents[0]["state"].(string)
	if entState == wholesalepack.EntitlementGranted {
		t.Fatal("the entitlement is recorded as granted although no connector answered")
	}
	if entState != wholesalepack.EntitlementPending && entState != wholesalepack.EntitlementFailed {
		t.Fatalf("entitlement state = %q, want pending or failed", entState)
	}
	if errText, _ := ents[0]["error"].(string); strings.TrimSpace(errText) == "" {
		t.Fatal("a non-granted entitlement must carry WHY")
	}
	if got, _ := ents[0]["adapter"].(string); got != wholesalepack.AdapterCustomerTag {
		t.Fatalf("adapter = %q, want the one settings named", got)
	}

	// AND THE APPROVAL STANDS. This is the assertion the whole
	// pending-versus-granted distinction exists for: a push that failed is
	// a fact about somebody else's API, not a reason to un-approve.
	if got := stateOf(t, eng, ctx, appID); got != wholesalepack.StateApproved {
		t.Fatalf("the application is %q after a failed push; it must still be %q",
			got, wholesalepack.StateApproved)
	}

	// A REVOKE NEEDS ITS DECISION FIRST, so the state and the history agree.
	if _, err := eng.Execute(automation, `builtin wholesaleRevokeEntitlement(applicationId: `+
		wsQuote(appID)+`)`); err == nil {
		t.Fatal("an entitlement was revoked with no revoke decision behind it")
	}
	decide(t, eng, decider, appID, wholesalepack.TransitionRevoke, merchant, "account closed")
	if got := stateOf(t, eng, ctx, appID); got != wholesalepack.StateRevoked {
		t.Fatalf("state = %q, want %q", got, wholesalepack.StateRevoked)
	}
}

// A DISABLED PACK IS MOUNTED-INERT, which is what makes "this pack ships
// disabled" SAFE rather than merely quiet. Concepts stay, so cross-domain
// imports and @relationship targets keep resolving and rows written before
// the flip stay browsable; behaviour goes, so the shopper surface reaches a
// construct that is not in the registry at all.
func TestLiveE2E_ADisabledWholesalePackIsInert(t *testing.T) {
	eng := liveWholesaleEngine(t, wholesalepack.Domain)
	merchant, storeLive, _ := wholesaleScope()
	ctx := asSiteOwner(merchant)

	// A COMPLETE call, so the refusal is "the construct is not loaded" rather
	// than "an argument is missing". An inert pack has to be what stops this.
	if _, err := eng.Execute(ctx, `builtin wholesaleSubmitApplication(applicantEmail: "a@b.c"`+
		`, applicantName: "A", companyName: "Acme", storeId: `+wsQuote(storeLive)+
		`, submissionId: "sub-inert")`); err == nil {
		t.Fatal("the shopper write ran on a DISABLED pack: a mounted-inert pack loads no " +
			"behavioural construct, so the declared form has nothing to reach")
	}
	if _, err := eng.Execute(ctx,
		`query applicationsForStore(storeId: `+wsQuote(storeLive)+`)`); err == nil {
		t.Fatal("a query on a DISABLED pack still resolved")
	}
	if _, err := memoryNodes.DefaultRegistry().Get("v1:wholesale:application"); err != nil {
		t.Fatalf("a disabled pack's concepts must still load -- mounted-inert, not "+
			"unmounted: %v", err)
	}
}

// ---------------------------------------------------------------------------

func decide(t *testing.T, eng *memql.MemQLEngine, ctx context.Context,
	appID, transition, decidedBy, note string) {
	t.Helper()
	wsExec(t, eng, ctx, `builtin wholesaleRecordDecision(applicationId: `+wsQuote(appID)+
		`, decidedBy: `+wsQuote(decidedBy)+`, note: `+wsQuote(note)+
		`, principalKind: "client", transition: `+wsQuote(transition)+`)`)
}

// stateOf folds the STORED decision log, which is what the pack itself does
// and what has to be true.
//
// IT DOES NOT READ THE ANSWER OF wholesaleApplicationState, and the reason
// is an engine behaviour rather than anything about this pack: a top-level
// `builtin X(...)` call returns an EMPTY bundle -- measured here, with a
// registered concept on the node and with an unregistered one, both giving
// {"Bundle":null}. packs/reviewspack's public read is a builtin too and
// shares it exactly, so it is neither new nor this epic's to change:
// surfacing a builtin's nodes at top level is component/memql's result
// handling, which every pack and every integration sits on.
//
// The builtin is still exercised below -- it must run and must not error --
// and the STATE is asserted against the rows, so this test measures what it
// can actually see.
func stateOf(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, appID string) string {
	t.Helper()
	// The builtin must RUN. Its answer is the engine's to surface.
	if _, err := eng.Execute(ctx,
		`builtin wholesaleApplicationState(applicationId: `+wsQuote(appID)+`)`); err != nil {
		t.Fatalf("wholesaleApplicationState: %v", err)
	}
	rows := wsRows(t, eng, ctx,
		`query decisionsForApplication(applicationId: `+wsQuote(appID)+`)`)
	return wholesalepack.FoldState(rows)
}

// THE TWO-CLIENT TEST, BOOTED (epic memql#5533, issue memql#5560).
//
// twoclients_test.go proves both fixture domains PARSE and that their
// concepts REGISTER, which is what it can do without a database. This
// mounts both over the unedited pack and BOOTS AN ENGINE, which this
// repository does strictly: one skipped construct refuses the boot and
// names it.
//
// WHAT THAT CATCHES, MEASURED RATHER THAN ASSUMED. Each of these was
// introduced into the northwind fixture and the result recorded:
//
//	`use wholesale.builtins.{ noSuchBuiltin }`   -> CAUGHT, boot refused
//	@trigger(concept="v1:wholesale:noSuchConcept") -> not caught
//	`builtin wholesaleProvisionEntitlementTypo(...)` in a body -> not caught
//
// So this asserts the one that matters most for section 7 and does not
// pretend to the other two: A CLIENT MAY DEPEND ONLY ON WHAT THE PACK
// EXPORTS. An import of a name the pack does not declare refuses the boot,
// which is exactly "could this client express its process over the pack's
// public surface" turned into a failure.
//
// The two it does not catch are covered where they can be:
// TestBothClientsReachOnlyTheDeclaredSurface checks every imported
// wholesale.* name against the pack's declarations, and
// TestEveryConstructAnAdapterCallsIsDeclared does the same for the names
// the pack's own Go calls. A trigger naming a concept nobody declares is a
// gap in the ENGINE's automation loading rather than in this pack, and is
// worth its own issue rather than a claim here that is not true.

func TestLiveE2E_BothFixtureClientsBootOverTheUneditedPack(t *testing.T) {
	for _, client := range []string{"northwind", "contoso"} {
		dir := filepath.Join("testdata", "clients", client)
		memqldsl.RegisterTree(client, os.DirFS(dir))
		t.Cleanup(func() { memqldsl.UnregisterTree(client) })
	}

	// A STRICT BOOT WITH BOTH CLIENTS MOUNTED. If either fixture's
	// automation is invalid -- a trigger on a concept the pack does not
	// declare, a call to a builtin it does not ship, an args field the
	// payload cannot bind -- this refuses and names it.
	eng := liveWholesaleEngine(t)

	// AND THE PACK'S OWN CONSTRUCTS STILL RESOLVE with two clients laid
	// over it, which is the whole claim: neither client displaced anything.
	merchant, storeLive, _ := wholesaleScope()
	ctx := asSiteOwner(merchant)
	openApplications(t, eng, ctx, storeLive, wholesalepack.AdapterCustomerTag)
	_ = submit(t, eng, ctx, storeLive, "Both clients mounted", "buyer@both.example")
	if rows := wsRows(t, eng, ctx,
		`query applicationsForStore(storeId: `+wsQuote(storeLive)+`)`); len(rows) != 1 {
		t.Fatalf("the pack's own write answered %d rows with two clients mounted", len(rows))
	}

	// THE CLIENTS' OWN CONCEPTS ARE REACHABLE TOO, which is what an
	// @relationship into the pack's namespace having bound looks like from
	// the outside.
	for _, id := range []string{"v1:northwind:northwindTaxDetail", "v1:contoso:contosoReview"} {
		if _, err := memoryNodes.DefaultRegistry().Get(id); err != nil {
			t.Fatalf("%q must be registered after a boot with both clients: %v", id, err)
		}
	}
}

// ---------------------------------------------------------------------------
// THE CLIENT'S GRANT AUTOMATION ON A DEVELOPMENT STORE (U4, step 6).
//
// TestAClientGrantAutomationProvisionsAnApprovedApplication proves the
// automation reaches the pack's builtin, through a test adapter that reaches no
// store. This runs the same automation over the customer-tag adapter, with the
// Shopify integration registered against a real development store: the
// merchant approves, contoso's automation fires, the pack's engineCaller calls
// shopifyTagWholesaleCustomer with its mark, and the buyer's customer on the
// store carries the tag.
//
// It skips without shopifylive's two variables, naming the one it needs, and
// refuses a store that is not a partner development store before anything is
// written to it (shopifylive.RequireDevelopmentStore, the refusal every live
// Shopify test makes). The buyer is the run's own invented address
// (wholesale-live-<random>@example.com); the customer the grant created is
// deleted by the id the entitlement recorded, in a cleanup that runs when the
// test fails too. Terms are not required, so the approval grants at once.
//
//	MEMQL_DATABASE_DSN=... MEMQL_TEST_SHOPIFY_STORE_DOMAIN=... MEMQL_TEST_SHOPIFY_ADMIN_TOKEN=... \
//	  go test -count=1 -timeout 30m ./packs/wholesalepack/ -run 'TestLiveE2E_AClientGrantAutomationTagsTheBuyerOnADevelopmentStore' -v

// liveTagTest names this test in a skip line.
const liveTagTest = "the wholesale pack's live tag test"

// liveGrantWait is how long the approval has to reach a granted entitlement:
// the automation, then three Admin calls to the real store.
const liveGrantWait = 90 * time.Second

// tagRouteOnly is the Shopify integration with only the two capabilities the
// customer-tag adapter calls. The scheduler this test runs loads every
// automation in the tree, the Shopify overlay's scheduled ones included, and
// with the whole integration registered one of them (the daily webhook
// reconcile) would write subscriptions to the development store; with these
// two alone, a scheduled call finds no handler and the store is asked nothing
// but the grant.
type tagRouteOnly struct{ inner *shopify.Integration }

func (p tagRouteOnly) IntegrationName() string { return p.inner.IntegrationName() }

func (p tagRouteOnly) Capabilities() []memql.IntegrationCapability {
	var out []memql.IntegrationCapability
	for _, c := range p.inner.Capabilities() {
		if c.Name == "tagWholesaleCustomer" || c.Name == "untagWholesaleCustomer" {
			out = append(out, c)
		}
	}
	return out
}

const liveTagCustomersDoc = `query WholesaleLiveCustomers($query: String!) {
  customers(first: 5, query: $query) { nodes { id email } }
}`

const liveTagCustomerDoc = `query WholesaleLiveCustomer($id: ID!) {
  customer(id: $id) {
    id
    tags
    defaultEmailAddress { emailAddress }
  }
}`

const liveTagCustomerDeleteDoc = `mutation WholesaleLiveCustomerDelete($input: CustomerDeleteInput!) {
  customerDelete(input: $input) {
    deletedCustomerId
    userErrors { field message }
  }
}`

// liveShop is the development store as this test's own Admin calls reach it.
type liveShop struct {
	admin *shopify.AdminClient
	store shopify.Store
	token string
}

// ask runs one document against the store, bounded, and answers its data.
func (s liveShop) ask(ctx context.Context, document, operation string, vars map[string]any) (map[string]any, error) {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := s.admin.Do(callCtx, s.store, s.token, document, operation, vars)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}
	return resp.DataMap()
}

// holders is every customer id the store lists for exactly the address.
func (s liveShop) holders(ctx context.Context, email string) ([]string, error) {
	data, err := s.ask(ctx, liveTagCustomersDoc, "WholesaleLiveCustomers", map[string]any{"query": "email:" + strconv.Quote(email)})
	if err != nil {
		return nil, err
	}
	customers, _ := data["customers"].(map[string]any)
	nodes, _ := customers["nodes"].([]any)
	var out []string
	for _, n := range nodes {
		m, _ := n.(map[string]any)
		if got, _ := m["email"].(string); strings.EqualFold(got, email) {
			if gid, _ := m["id"].(string); gid != "" {
				out = append(out, gid)
			}
		}
	}
	return out, nil
}

// remove deletes the customers the run made for its address: the id the
// entitlement recorded, or, when it recorded none, whatever holds the run's
// own address, which only this run can have used.
func (s liveShop) remove(t *testing.T, email string, recorded func() string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ids := []string{}
	if id := recorded(); id != "" {
		ids = append(ids, id)
	} else {
		found, err := s.holders(ctx, email)
		if err != nil {
			t.Logf("LEFT IN THE STORE, perhaps: a customer holding %s could not be looked up, so none was deleted; if one exists it is this run's, delete it in the Shopify admin: %v", email, err)
			return
		}
		ids = found
	}
	for _, id := range ids {
		data, err := s.ask(ctx, liveTagCustomerDeleteDoc, "WholesaleLiveCustomerDelete", map[string]any{"input": map[string]any{"id": id}})
		if err == nil {
			payload, _ := data["customerDelete"].(map[string]any)
			if errs, _ := payload["userErrors"].([]any); len(errs) > 0 {
				err = fmt.Errorf("customerDelete refused: %v", errs)
			}
		}
		if err != nil {
			t.Logf("LEFT IN THE STORE: customer %s (the invented buyer %s); delete it in the Shopify admin: %v", id, email, err)
			continue
		}
		t.Logf("deleted customer %s, the invented buyer %s", id, email)
	}
}

func TestLiveE2E_AClientGrantAutomationTagsTheBuyerOnADevelopmentStore(t *testing.T) {
	domain := shopifylive.Env(t, shopifylive.EnvStoreDomain, liveTagTest)
	token := shopifylive.Env(t, shopifylive.EnvAdminToken, liveTagTest)
	shop, err := shopify.NormalizeShopDomain(domain)
	if err != nil {
		t.Fatalf("%s is not a bare <shop>.myshopify.com host; no request was sent", shopifylive.EnvStoreDomain)
	}
	admin := shopify.NewAdminClient()
	live := liveShop{admin: admin, token: token, store: shopify.Store{ID: "wholesale-live", Domain: shop, APIVersion: generated.APIVersion}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	// BEFORE ANYTHING IS WRITTEN, to the store or to the database.
	shopifylive.RequireDevelopmentStore(t, shopifylive.EnvStoreDomain, func(document, operation string) (map[string]any, error) {
		return live.ask(ctx, document, operation, nil)
	})

	eng := automatedWholesaleEngine(t)
	merchant, storeID, _ := wholesaleScope()
	owner := asSiteOwner(merchant)
	// The store set up as the pack requires: a settings row tied to an
	// organization, through a site of it bound to the store.
	openApplications(t, eng, owner, storeID, wholesalepack.AdapterCustomerTag)

	// THE CONNECTOR'S ROW FOR THE STORE, over the one the set-up seeded: the
	// development store's domain and a reference to its token, which the
	// registry below answers. The token is never written to the database.
	tokenRef := "wholesale-live-admin-token-" + storeID
	row, _ := json.Marshal(map[string]any{
		"domain": shop, "name": "Wholesale live test store", "adminTokenRef": tokenRef,
		"apiVersion": generated.APIVersion, "status": "live",
	})
	if _, err := eng.Execute(seedCtx(), `insert(`+quote("v1:shopify:store")+`, id=`+quote(storeID)+`, payload=`+string(row)+`)`); err != nil {
		t.Fatalf("seed the connector's store row: %v", err)
	}
	registry := shopify.NewStoreRegistry(eng, func(_ context.Context, name string) (string, error) {
		if name == tokenRef {
			return token, nil
		}
		return "", fmt.Errorf("no such secret %q", name)
	})
	conn := shopify.NewConnector(eng, slog.New(slog.DiscardHandler), registry, admin)
	if err := eng.RegisterIntegration(tagRouteOnly{inner: shopify.NewIntegration(conn)}); err != nil {
		t.Fatalf("register the Shopify tag route: %v", err)
	}

	// THE RUN'S OWN BUYER, removed by the id the grant records.
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("no randomness for the run's own address: %v", err)
	}
	email := "wholesale-live-" + hex.EncodeToString(suffix) + "@example.com"
	var recordedMu sync.Mutex
	recorded := ""
	t.Cleanup(func() {
		live.remove(t, email, func() string {
			recordedMu.Lock()
			defer recordedMu.Unlock()
			return recorded
		})
	})
	if held, err := live.holders(ctx, email); err != nil || len(held) != 0 {
		t.Fatalf("the invented address %s must be held by nobody before the test (held by %v, %v); nothing was created", email, held, err)
	}

	appID := submit(t, eng, owner, storeID, "Live Tag Trading", email)
	decide(t, eng, asDecider(t, eng, merchant), appID, wholesalepack.TransitionApprove, merchant, "approved on a development store")

	var ent map[string]any
	deadline := time.Now().Add(liveGrantWait)
	for time.Now().Before(deadline) {
		rows := wsRows(t, eng, owner, `query entitlementForApplication(applicationId: `+quote("v1:wholesale:application:"+appID)+`)`)
		if len(rows) > 0 {
			if id := asString(rows[0]["shopifyCustomerId"]); id != "" {
				recordedMu.Lock()
				recorded = id
				recordedMu.Unlock()
			}
			if state := rows[0]["state"]; state == wholesalepack.EntitlementGranted || state == wholesalepack.EntitlementFailed {
				ent = rows[0]
				break
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	if ent == nil {
		t.Fatalf("within %s of the approval no entitlement was granted or failed; the client's automation did not provision it", liveGrantWait)
	}
	if ent["state"] != wholesalepack.EntitlementGranted {
		t.Fatalf("the entitlement is %v with code %v: %v", ent["state"], ent["errorCode"], ent["error"])
	}
	if got := asString(ent["adapter"]); got != wholesalepack.AdapterCustomerTag {
		t.Fatalf("adapter = %q, want %s", got, wholesalepack.AdapterCustomerTag)
	}
	customerID := asString(ent["shopifyCustomerId"])
	if customerID == "" || ent["customerCreated"] != true {
		t.Fatalf("the granted entitlement records customer %q, created %v; want the customer the grant created for a buyer the store had none for", customerID, ent["customerCreated"])
	}
	if ref := asString(ent["reference"]); ref != "customer="+customerID+";tag="+wholesalepack.DefaultWholesaleTag {
		t.Fatalf("reference = %q, want customer=%s;tag=%s", ref, customerID, wholesalepack.DefaultWholesaleTag)
	}

	// AND THE STORE SAYS SO: the customer holds the address and carries the tag.
	data, err := live.ask(ctx, liveTagCustomerDoc, "WholesaleLiveCustomer", map[string]any{"id": customerID})
	if err != nil {
		t.Fatalf("read customer %s back: %v", customerID, err)
	}
	customer, _ := data["customer"].(map[string]any)
	if customer == nil {
		t.Fatalf("customer %s, which the entitlement records, is not on the store", customerID)
	}
	address, _ := customer["defaultEmailAddress"].(map[string]any)
	if got, _ := address["emailAddress"].(string); !strings.EqualFold(got, email) {
		t.Fatalf("customer %s holds %q, want the invented address %s", customerID, got, email)
	}
	tags, _ := customer["tags"].([]any)
	carried := 0
	for _, tag := range tags {
		if strings.EqualFold(fmt.Sprint(tag), wholesalepack.DefaultWholesaleTag) {
			carried++
		}
	}
	if carried != 1 {
		t.Fatalf("customer %s carries %s %d times (tags %v), want once", customerID, wholesalepack.DefaultWholesaleTag, carried, tags)
	}
}
