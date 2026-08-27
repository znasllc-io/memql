package memql

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/frontdoor"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// Self-serve deployables (memql#4344): v1:platform:site's composite tier, the
// owner stamp, and the hostname policy.
//
// These run against the REAL concept, not a fixture. That is the whole point --
// rowauthz_composite_test.go had to invent one because no concept declared the
// composite tier; site is the first that does, and a test measuring a fixture
// would not notice the declaration being reverted.

// siteDecl loads the real tree and returns v1:platform:site's declaration,
// failing loudly if it is undeclared -- which is the state in which every
// assertion below would pass while measuring nothing.
func siteDecl(t *testing.T) *langparser.RowAuthzDecl {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	decl := rowAuthzDeclFor(conceptPlatformSite)
	if decl == nil {
		t.Fatalf("%s declares NO row-authz tier. An undeclared concept admits every row to every "+
			"caller, so every assertion in this file would pass for the wrong reason",
			conceptPlatformSite)
	}
	return decl
}

// ---------------------------------------------------------------------------
// The declaration
// ---------------------------------------------------------------------------

// The composite tier, and specifically NOT the plain clusterOwner tier it
// replaced nor a plain owner= tier.
//
// Both wrong answers are silent in production and opposite in effect: staying
// clusterOwner leaves a user unable to see the site they just created, and a
// plain owner= tier hides every other user's site from the operator console and
// makes the hostname-uniqueness probe unable to see the row it must collide
// with.
func TestSiteDeclaresTheCompositeTier(t *testing.T) {
	decl := siteDecl(t)
	if decl.Tier != langparser.RowAuthzOwned {
		t.Fatalf("%s declares tier %q, want the owned tier carrying the cluster-owner bypass "+
			"(@rowAuthz(owner=\"ownerUserId\", clusterOwner)). A clusterOwner tier makes a user's "+
			"own deployable invisible to them", conceptPlatformSite, decl.Tier)
	}
	if decl.Owner != "ownerUserId" {
		t.Fatalf("%s names owner field %q, want \"ownerUserId\"", conceptPlatformSite, decl.Owner)
	}
	if !decl.ClusterOwnerBypass {
		t.Fatalf("%s declares a PLAIN owned tier with no cluster-owner bypass. \"List every site in "+
			"this cluster\" -- the portal's primary screen -- is then not expressible, which is the "+
			"exact reason the concept carried clusterOwner before memql#4344", conceptPlatformSite)
	}
}

// The kind enum is EXACTLY these three (design decision D5).
//
// Android / iOS / macOS are artifact distribution -- stores, TestFlight,
// notarisation -- not hostname-resolved web surfaces, so they get no schema
// until they are designed and the portal shows them as disabled "coming soon"
// kinds. A value here that the edge cannot resolve would be the wrong kind of
// additive: siteByHostname would hand the edge a row whose resolution tail does
// not exist.
//
// Read off the LOADED concept rather than the file text, so the assertion is
// about what the engine believes rather than what a regex found.
func TestSiteKindEnumIsExactlyThreeValues(t *testing.T) {
	siteDecl(t)
	c, ok := memorynodes.All()[conceptPlatformSite]
	if !ok || c == nil {
		t.Fatalf("%s is not in the concept registry", conceptPlatformSite)
	}
	got := siteKindEnumValues(t, c)
	want := []string{"shopify_storefront", "spa", "static"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("v1:platform:site.kind = %v, want exactly %v.\n"+
			"Adding a value here is a DESIGN change: each one costs a resolution tail in "+
			"component/edge and a picker entry in the portal. Android / iOS / macOS deliberately "+
			"have none (design D5) -- they are distribution, not a hostname the edge resolves.",
			got, want)
	}
}

// siteKindEnumValues pulls the declared enum values for `kind` off the loaded
// concept's schema, sorted.
func siteKindEnumValues(t *testing.T, c *memorynodes.Concept) []string {
	t.Helper()
	raw, err := c.DefinitionSchema()
	if err != nil {
		t.Fatalf("v1:platform:site definition schema: %v", err)
	}
	var doc struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal v1:platform:site schema: %v", err)
	}
	prop, ok := doc.Properties["kind"]
	if !ok {
		t.Fatalf("v1:platform:site declares no `kind` field")
	}
	out := append([]string(nil), prop.Enum...)
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Reads: a user sees their own, a cluster owner sees all
// ---------------------------------------------------------------------------

func TestSiteRowGateUserSeesOwnClusterOwnerSeesAll(t *testing.T) {
	siteDecl(t)
	mine := rowOf(t, conceptPlatformSite, conceptPlatformSite+":mine",
		map[string]any{"ownerUserId": "user-a", "hostname": "mine.example.com"})
	theirs := rowOf(t, conceptPlatformSite, conceptPlatformSite+":theirs",
		map[string]any{"ownerUserId": "user-b", "hostname": "theirs.example.com"})

	if !admitRowAuthzNode(callerCtx("user-a"), mine) {
		t.Error("a user was denied the deployable they own")
	}
	if admitRowAuthzNode(callerCtx("user-a"), theirs) {
		t.Error("user-a was shown a site owned by user-b -- self-serve deployables are per-user, " +
			"and this is the read a raw query string reaches around the declared binding")
	}
	for _, row := range []memorynodes.MemoryNode{mine, theirs} {
		if !admitRowAuthzNode(ownerRoleCtx("root"), row) {
			t.Errorf("a cluster owner was denied site %q. \"List every site in this cluster\" is "+
				"the portal's primary screen and the reason this concept carries the bypass at all",
				row.ID)
		}
	}
	if admitRowAuthzNode(context.Background(), mine) {
		t.Error("a caller with no access context was admitted; no identity, no rows (memql#2801)")
	}
}

// The seeded portal row carries an EMPTY ownerUserId, which means CLUSTER-OWNED
// rather than "owned by nobody in particular". An empty owner must never match a
// caller -- sameRowAuthzOwner refuses it outright -- so the row is reachable
// through the admin branch alone.
func TestSiteClusterOwnedRowIsReachableOnlyThroughTheAdminBranch(t *testing.T) {
	siteDecl(t)
	portal := rowOf(t, conceptPlatformSite, conceptPlatformSite+":portal",
		map[string]any{"ownerUserId": "", "hostname": "portal.memql.localhost", "systemOwned": true})

	if admitRowAuthzNode(callerCtx("user-a"), portal) {
		t.Error("an ordinary user was admitted to the cluster-owned portal row. An EMPTY " +
			"ownerUserId is a legal predicate value that would match every caller whose id is " +
			"also empty -- which is why sameRowAuthzOwner refuses it rather than comparing it")
	}
	if !admitRowAuthzNode(ownerRoleCtx("root"), portal) {
		t.Error("a cluster owner was denied the portal row; the admin branch does not read the " +
			"owner field at all")
	}
}

// ---------------------------------------------------------------------------
// Writes: owner-only, with the cluster-owner path explicit
// ---------------------------------------------------------------------------

// A cross-user write is refused BEFORE the read-merge, against the row's stored
// owner rather than the merged payload -- which matters because a merged payload
// on an owned-tier write carries the attacker's own stamp.
func TestSiteCrossUserUpdateIsRefused(t *testing.T) {
	siteDecl(t)
	prior := map[string]any{"ownerUserId": "user-a", "hostname": "shop.example.com"}
	rowId := conceptPlatformSite + ":shop"

	if err := guardRowAuthzWrite(callerCtx("user-b"), conceptPlatformSite, rowId, prior, true, true); err == nil {
		t.Fatal("user-b was allowed to write user-a's site. updateSiteBundle IS the deploy and the " +
			"rollback, so this is one user republishing another's storefront")
	}
	if err := guardRowAuthzWrite(callerCtx("user-a"), conceptPlatformSite, rowId, prior, true, true); err != nil {
		t.Fatalf("the site's own owner was refused their own write: %v", err)
	}
	// The cluster-owner path is the standing, EXPLICIT escape (memql#3174),
	// not something the composite tier adds -- the tier's second argument is
	// read-side only.
	if err := guardRowAuthzWrite(ownerRoleCtx("root"), conceptPlatformSite, rowId, prior, true, true); err != nil {
		t.Fatalf("a cluster owner was refused a write onto a user's site: %v", err)
	}
	// And a cluster-OWNED row (empty owner) is nobody's but the operator's.
	cluster := map[string]any{"ownerUserId": "", "hostname": "portal.memql.localhost"}
	if err := guardRowAuthzWrite(callerCtx("user-a"), conceptPlatformSite,
		conceptPlatformSite+":portal", cluster, true, true); err == nil {
		t.Fatal("a user was allowed to write the cluster-owned portal row")
	}
}

// ---------------------------------------------------------------------------
// The owner stamp
// ---------------------------------------------------------------------------

// createSite stamps the owner in its own stamp{} block, which is the property
// TestDeclaredOwnerFieldsAreServerStamped requires and the reason ownerUserId is
// not an arg at all. This test pins the DECLARATION, so removing the stamp line
// fails here as well as there.
func TestCreateSiteStampsTheOwnerFromTheActor(t *testing.T) {
	siteDecl(t)
	registry := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(nil, registry, memorynodes.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	res := OwnerFieldProvenance(registry, map[string]string{conceptPlatformSite: "ownerUserId"})
	if len(res) != 1 {
		t.Fatalf("got %d verdicts, want 1", len(res))
	}
	if !res[0].ServerStamped {
		t.Fatalf("v1:platform:site.ownerUserId is not server-stamped: %s\n"+
			"  stamped by:  %v\n  writable by: %v\n"+
			"A declared owner tier over a caller-supplied field records a guarantee nothing "+
			"provides, and reads as safe -- so an auditor seeing the tier stops looking.",
			res[0].Reason, res[0].StampedBy, res[0].WritableBy)
	}
	if len(res[0].StampedBy) == 0 {
		t.Fatal("nothing stamps v1:platform:site.ownerUserId")
	}
}

// The Go step UNDOES that stamp when the writer is the deployment rather than a
// person, leaving the row cluster-owned. That is how the seeded portal -- site
// #1, re-materialized on EVERY boot -- stays the platform's row.
func TestSiteOwnerStampIsUndoneForADeploymentWriter(t *testing.T) {
	cases := []struct {
		name    string
		ctx     context.Context
		actor   string
		payload map[string]any
	}{
		{
			name:    "the seed materializer",
			ctx:     ownerRoleCtx("system:seedMaterializer"),
			actor:   "system:seedMaterializer",
			payload: map[string]any{"hostname": "portal.memql.localhost", "ownerUserId": "system:seedMaterializer"},
		},
		{
			name:    "a cluster owner creating a site",
			ctx:     ownerRoleCtx("root"),
			actor:   "root",
			payload: map[string]any{"hostname": "shop.memql.localhost", "ownerUserId": "root"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := applySiteOwnerStamp(tc.ctx, tc.payload, false, tc.actor); err != nil {
				t.Fatalf("applySiteOwnerStamp: %v", err)
			}
			if got, present := tc.payload["ownerUserId"]; present {
				t.Fatalf("ownerUserId = %v, want the field REMOVED. An empty/absent ownerUserId is "+
					"the cluster-owned state; leaving the stamp would make site #1 the seed "+
					"materializer's row and hand its id whatever ownership means", got)
			}
		})
	}
}

// An ordinary caller's stamp stands. This is the half that must not be
// weakened: it is what makes the declared tier true.
func TestSiteOwnerStampStandsForAnOrdinaryCaller(t *testing.T) {
	payload := map[string]any{"hostname": "shop.memql.localhost", "ownerUserId": "user-a"}
	if err := applySiteOwnerStamp(callerCtx("user-a"), payload, false, "user-a"); err != nil {
		t.Fatalf("applySiteOwnerStamp: %v", err)
	}
	if got := payload["ownerUserId"]; got != "user-a" {
		t.Fatalf("ownerUserId = %v, want the actor's id", got)
	}
}

// THE PROPERTY THAT KEEPS AN OPERATOR FROM TAKING A USER'S SITE. A cluster owner
// running updateSiteBundle against somebody else's deployable arrives here with
// the merged payload carrying THAT USER's ownerUserId. It is not the caller's
// own id, so nothing is undone and the row stays the user's.
//
// Without the self-match this would silently transfer every site an operator
// ever published to, which is the opposite of what the undo exists for.
func TestSiteOwnerStampOnlyUndoesTheCallersOwnStamp(t *testing.T) {
	payload := map[string]any{"hostname": "shop.memql.localhost", "ownerUserId": "user-a"}
	if err := applySiteOwnerStamp(ownerRoleCtx("root"), payload, true, "root"); err != nil {
		t.Fatalf("applySiteOwnerStamp: %v", err)
	}
	if got := payload["ownerUserId"]; got != "user-a" {
		t.Fatalf("ownerUserId = %v after a cluster owner's write onto user-a's site, want it "+
			"unchanged. Only the value createSite just stamped -- the caller's OWN id -- may be "+
			"undone; anything else is an operator quietly taking a user's deployable", got)
	}
}

// It never names a third party. There is no arg and no code path that writes an
// ownerUserId the caller did not already hold, which is the property the owner
// gate is actually about -- so the Go step can be a narrowing without reopening
// what that gate protects.
func TestSiteOwnerStampNeverNamesAThirdParty(t *testing.T) {
	payload := map[string]any{"hostname": "shop.memql.localhost", "ownerUserId": "user-b"}
	if err := applySiteOwnerStamp(callerCtx("user-a"), payload, false, "user-a"); err != nil {
		t.Fatalf("applySiteOwnerStamp: %v", err)
	}
	// The value is left as the template rendered it (createSite renders
	// actor.userId, so "user-b" cannot arise through the mutation) -- what
	// matters is that nothing HERE substituted a different owner.
	if got := payload["ownerUserId"]; got != "user-b" {
		t.Fatalf("ownerUserId = %v; this step must not rewrite the owner at all, only delete "+
			"the caller's own stamp", got)
	}
}

// An update by an ordinary caller is left alone entirely: guardRowAuthzWrite has
// already refused anybody who is not the stored owner, and the deltas for
// updateSiteBundle / updateSiteStatus / deleteSite name no owner at all.
func TestSiteOwnerStampDoesNotRunOnAnOrdinaryUpdate(t *testing.T) {
	payload := map[string]any{"hostname": "portal.memql.localhost", "ownerUserId": ""}
	if err := applySiteOwnerStamp(callerCtx("user-a"), payload, true, "user-a"); err != nil {
		t.Fatalf("applySiteOwnerStamp: %v", err)
	}
	if got := payload["ownerUserId"]; got != "" {
		t.Fatalf("ownerUserId = %v after an ordinary UPDATE; a write onto a cluster-owned row "+
			"must not rewrite it as the writer's", got)
	}
}

// No identity, no row. createSite's stamp renders EMPTY when actor.userId does
// not resolve, and empty is the cluster-owned state -- so letting it through
// would mint an operator's row on an unauthenticated call.
func TestSiteOwnerStampRefusesAnUnauthenticatedCreate(t *testing.T) {
	payload := map[string]any{"hostname": "shop.memql.localhost", "ownerUserId": ""}
	err := applySiteOwnerStamp(context.Background(), payload, false, "")
	if err == nil {
		t.Fatal("an unauthenticated create was admitted; an empty ownerUserId is the cluster-owned " +
			"state, so this would mint an operator's row")
	}
}

// ---------------------------------------------------------------------------
// The hostname policy (pure)
// ---------------------------------------------------------------------------

func TestUserSiteHostnamePolicy(t *testing.T) {
	const domain = "example.com"
	cases := []struct {
		hostname string
		ok       bool
		why      string
	}{
		{"shop.example.com", true, "the ordinary case"},
		{"abc.example.com", true, "exactly the 3-character minimum"},
		{strings.Repeat("a", 40) + ".example.com", true, "exactly the 40-character maximum"},
		{"a-b-c.example.com", true, "hyphens are allowed inside the label"},
		{"shop123.example.com", true, "digits are allowed"},
		{"SHOP.example.com", true, "case-folded before the check, so an uppercase request resolves"},

		{"ab.example.com", false, "two characters is under the minimum"},
		{strings.Repeat("a", 41) + ".example.com", false, "41 characters is over the maximum"},
		{"sh_op.example.com", false, "underscore is not a hostname label character"},
		{"shop!.example.com", false, "punctuation is not a hostname label character"},
		{"example.com", false, "the apex is the deployment's own front door"},
		{"shop.eu.example.com", false, "two labels -- an Ingress wildcard matches exactly one"},
		{"shop.elsewhere.com", false, "another domain entirely; nothing routes it"},
		{"", false, "empty"},

		{"api.example.com", false, "reserved: the engine's API edge"},
		{"identity.example.com", false, "reserved: sign-in"},
		{"mcp.example.com", false, "reserved: the MCP protocol head"},
		{"portal.example.com", false, "reserved: the platform's own console, site #1"},
		{"os.example.com", false, "reserved: the platform's OS shell"},
		{"www.example.com", false, "reserved: reads as the organisation's"},
		{"admin.example.com", false, "reserved: reads as the organisation's"},
		{"mail.example.com", false, "reserved: where a mail host would land"},
	}
	for _, tc := range cases {
		err := validateUserSiteHostname(tc.hostname, domain)
		if tc.ok && err != nil {
			t.Errorf("validateUserSiteHostname(%q) refused a hostname that should pass (%s): %v",
				tc.hostname, tc.why, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("validateUserSiteHostname(%q) ADMITTED a hostname it must refuse (%s)",
				tc.hostname, tc.why)
		}
	}
}

// The reserved list is DERIVED from the front door's own role set plus the
// portal, not re-listed. A second copy would mean adding a front-door role
// silently opens its hostname to the first user who asks for it.
func TestReservedSiteLabelsCoverEveryFrontDoorRole(t *testing.T) {
	reserved := reservedSiteLabels()
	for _, r := range frontdoor.Roles() {
		if !reserved[string(r)] {
			t.Errorf("front-door role %q is not a reserved site label. A user could claim %q and "+
				"be handed traffic the front door means for that role", r,
				frontdoor.RoleHost(r, "example.com"))
		}
	}
	if !reserved[frontdoor.PortalSite] {
		t.Error("the portal's own label is not reserved")
	}
	if !reserved[frontdoor.OsSite] {
		t.Error("the OS shell's own label is not reserved")
	}
	for _, l := range squatReservedSiteLabels {
		if !reserved[l] {
			t.Errorf("%q is missing from the reserved set", l)
		}
	}
	// And nothing beyond those: an over-broad reserved list refuses names for
	// no stated reason, which is how a list acquires entries nobody can defend.
	if want := len(frontdoor.Roles()) + 2 + len(squatReservedSiteLabels); len(reserved) != want {
		t.Errorf("the reserved set holds %d labels, want %d (%d roles + portal + os + %d squat "+
			"labels). Every entry needs a reason recorded beside it",
			len(reserved), want, len(frontdoor.Roles()), len(squatReservedSiteLabels))
	}
}

// The domain the policy derives against is MEMQL_DOMAIN, the same single key
// every other domain-shaped value in the cluster comes from -- so the hostnames
// users may claim and the hostnames the front door routes cannot disagree.
func TestSiteHostnamePolicyDomainComesFromMemqlDomain(t *testing.T) {
	t.Setenv(memqlDomainEnv, "lab.example.com")
	if got := siteHostnamePolicyDomain(); got != "lab.example.com" {
		t.Fatalf("siteHostnamePolicyDomain() = %q with MEMQL_DOMAIN set, want the env value", got)
	}
	if err := validateUserSiteHostname("shop.lab.example.com", siteHostnamePolicyDomain()); err != nil {
		t.Fatalf("a site under the configured domain was refused: %v", err)
	}
	t.Setenv(memqlDomainEnv, "")
	if got := siteHostnamePolicyDomain(); got != defaultSiteDomain {
		t.Fatalf("siteHostnamePolicyDomain() = %q with MEMQL_DOMAIN empty, want the committed "+
			"default %q. Returning \"\" would make the suffix test admit nothing at all",
			got, defaultSiteDomain)
	}
}

// The policy's default domain and the portal seed's default hostname are ONE
// derivation. If they drift, a fresh local cluster serves its portal at one
// domain and admits user sites on another -- and both files look right.
func TestSiteHostnamePolicyDefaultDomainMatchesThePortalSeed(t *testing.T) {
	if got := frontdoor.PortalHost(defaultSiteDomain); got != defaultPortalHostname {
		t.Fatalf("frontdoor.PortalHost(%q) = %q, but the portal seed defaults to %q",
			defaultSiteDomain, got, defaultPortalHostname)
	}
}

// ---------------------------------------------------------------------------
// The hostname policy (wiring)
// ---------------------------------------------------------------------------

// A cluster owner is exempt from the SHAPE rule: a custom apex or a second
// domain is a legitimate operator deployment that arrives with its own DNS and
// its own hand-issued Certificate (memql#4224). The exemption is checked here
// rather than assumed, because it is the half of the policy that a user must not
// have.
//
// Uniqueness is checked separately and binds everyone, so these cases run with
// no database and assert only that the SHAPE half did not refuse them: a
// privileged caller reaches the probe (and fails on the missing database),
// while an ordinary caller is refused by the policy before the probe.
func TestSiteHostnamePolicyShapeRuleIsWaivedForPrivilegedCallers(t *testing.T) {
	e := &MemQLEngine{}
	const custom = "www.some-other-domain.test"

	err := e.validateSiteHostnamePolicy(callerCtx("user-a"),
		map[string]any{"hostname": custom}, "s1", "user-a", false, "")
	if err == nil {
		t.Fatal("an ordinary user was allowed a hostname outside this cluster's domain")
	}
	if !strings.Contains(err.Error(), "not under this cluster's domain") {
		t.Fatalf("the user's refusal was not the policy's: %v", err)
	}

	err = e.validateSiteHostnamePolicy(ownerRoleCtx("root"),
		map[string]any{"hostname": custom}, "s1", "root", false, "")
	if err == nil {
		t.Fatal("expected the uniqueness probe to fail on this engine's absent database -- if it " +
			"passed, the probe is being skipped for a cluster owner and two live rows can share a " +
			"hostname")
	}
	if strings.Contains(err.Error(), "not under this cluster's domain") {
		t.Fatalf("a cluster owner was refused by the SHAPE rule: %v\n"+
			"A custom apex or a second domain is an operator deployment, and refusing it here "+
			"would make the cluster unable to host anything but <slug>.<domain>", err)
	}
}

// A system actor skips the probe entirely, so the SeedMaterializer's re-write of
// the portal row on every boot cannot be refused by a rule written for people --
// and cannot be broken by the database being unreachable at that moment either.
func TestSiteHostnamePolicySkipsTheSystemActorEntirely(t *testing.T) {
	e := &MemQLEngine{}
	err := e.validateSiteHostnamePolicy(context.Background(),
		map[string]any{"hostname": "portal.memql.localhost"}, "portal", "system:seedMaterializer", false, "")
	if err != nil {
		t.Fatalf("the seed materializer's portal write was refused: %v\n"+
			"It re-materializes site #1 on EVERY boot; a refusal here is a cluster that comes up "+
			"with no way to manage sites", err)
	}
}

// Self-exclusion is by the CANONICAL id. A mutation carries a bare `portal`
// while storage holds `v1:platform:site:portal`; compared raw, a row is a
// duplicate of itself and every re-publish of an existing site is refused.
func TestCanonicalSiteStorageIdCollapsesBothSpellings(t *testing.T) {
	bare := canonicalSiteStorageId("portal")
	qualified := canonicalSiteStorageId(conceptPlatformSite + ":portal")
	if bare != qualified {
		t.Fatalf("canonicalSiteStorageId: bare=%q qualified=%q -- self-exclusion never matches, so "+
			"every update of an existing site is refused as a duplicate of itself", bare, qualified)
	}
	if bare != conceptPlatformSite+":portal" {
		t.Fatalf("canonicalSiteStorageId(\"portal\") = %q, want the stored spelling", bare)
	}
	if canonicalSiteStorageId("  ") != "" {
		t.Fatal("a blank id must not canonicalize into a real-looking one")
	}
}

// ---------------------------------------------------------------------------
// The land gate's matcher, taught the composite predicate (memql#4344)
// ---------------------------------------------------------------------------

// The three site queries spell the composite tier's predicate out, which is what
// TestRowAuthzEnforcementLandGate asks an author to do -- and until this change
// AnalyzeShadow could not see it: the injected term is a DISJUNCTION and the
// matcher only recognised a single comparison leaf. So the author who wrote
// exactly what InjectedPredicate renders was told their read would change.
//
// This pins the matcher against the renderer directly, rather than through the
// land gate, so a regression names the cause instead of the symptom.
func TestShadowAnalyzerRecognisesTheCompositePredicate(t *testing.T) {
	decl := &langparser.RowAuthzDecl{
		Tier:               langparser.RowAuthzOwned,
		Owner:              "ownerUserId",
		ClusterOwnerBypass: true,
	}
	filter := &LogicalExpression{
		Op:   LogicalAnd,
		Left: &SpecReferenceExpression{Name: "isNotDeleted"},
		Right: &LogicalExpression{
			Op:    LogicalOr,
			Left:  ownerLeafExpr("ownerUserId"),
			Right: clusterOwnerLeafExpr(),
		},
	}
	if v, reason := AnalyzeShadow(filter, decl); v != ShadowAlreadyImplied {
		t.Fatalf("a filter carrying the composite tier's OWN predicate analysed as %q (%s), want "+
			"already-implied.\nInjectedPredicate renders %q; a matcher that cannot read what the "+
			"renderer writes tells an author their correct query would change its result set",
			v, reason, InjectedPredicate(decl))
	}
	// Order-independent, matching the declaration's own two arguments. Wrapped
	// in an AND like every authored filter, because a BARE top-level
	// disjunction takes a different branch (allDisjunctsImply) that this change
	// deliberately leaves alone -- undecidable is fail-closed there, and no
	// query in the tree has that shape.
	flipped := &LogicalExpression{
		Op:   LogicalAnd,
		Left: &SpecReferenceExpression{Name: "isNotDeleted"},
		Right: &LogicalExpression{
			Op:    LogicalOr,
			Left:  clusterOwnerLeafExpr(),
			Right: ownerLeafExpr("ownerUserId"),
		},
	}
	if v, _ := AnalyzeShadow(flipped, decl); v != ShadowAlreadyImplied {
		t.Fatalf("the composite predicate written admin-arm-first analysed as %q", v)
	}
}

// NEGATIVE CONTROL, and the one that matters. On a PLAIN owned tier the same
// shape does NOT imply the tier -- the admin arm returns rows the owner term
// excludes -- so admitting it there would be the fail-open widening this gate
// exists to prevent (memql#2839's shape, one tier over).
func TestShadowAnalyzerStillRefusesTheOrShapeOnAPlainOwnedTier(t *testing.T) {
	plain := &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"}
	// AND-wrapped so this runs through the CONJUNCT arm the composite case
	// added, not through the top-level-disjunction branch that predates it --
	// a negative control that exercises a different code path proves nothing
	// about the one that changed.
	filter := &LogicalExpression{
		Op:   LogicalAnd,
		Left: &SpecReferenceExpression{Name: "isNotDeleted"},
		Right: &LogicalExpression{
			Op:    LogicalOr,
			Left:  ownerLeafExpr("ownerUserId"),
			Right: clusterOwnerLeafExpr(),
		},
	}
	if v, _ := AnalyzeShadow(filter, plain); v == ShadowAlreadyImplied {
		t.Fatal("`owner==actor.userId || actor.isClusterOwner` was accepted as implying a PLAIN " +
			"owned tier. It does not: the admin arm returns rows the owner term excludes, so the " +
			"read is WIDER than the tier and every caller with the owner role reads everybody's " +
			"rows. The composite arm must stay gated on ClusterOwnerBypass")
	}
}

// And a selection term ORed with the admin gate is still not the composite
// predicate, even on a composite declaration -- `fromE164==args.e164 ||
// actor.isClusterOwner==true` is fail-open because a false gate zeroes nothing.
func TestShadowAnalyzerRefusesASelectionTermOredWithTheAdminGate(t *testing.T) {
	decl := &langparser.RowAuthzDecl{
		Tier:               langparser.RowAuthzOwned,
		Owner:              "ownerUserId",
		ClusterOwnerBypass: true,
	}
	filter := &LogicalExpression{
		Op:   LogicalAnd,
		Left: &SpecReferenceExpression{Name: "isNotDeleted"},
		Right: &LogicalExpression{
			Op: LogicalOr,
			Left: &ComparisonExpression{
				Field:    FieldReference{Raw: "hostname", Parts: []string{"hostname"}},
				Operator: OpEq,
				Value:    "shop.example.com",
			},
			Right: clusterOwnerLeafExpr(),
		},
	}
	if v, _ := AnalyzeShadow(filter, decl); v == ShadowAlreadyImplied {
		t.Fatal("a SELECTION term ORed with the admin gate was accepted as the composite tier's " +
			"predicate. A false admin gate zeroes nothing there -- any caller supplying the " +
			"selection reads rows -- which is memql#2839 exactly")
	}
}

// ownerLeafExpr builds `<field> == actor.userId`, the owner arm as the loader
// produces it (the value side is an unresolved *ActorReference because the
// analyzer runs before resolution).
func ownerLeafExpr(field string) ExpressionNode {
	return &ComparisonExpression{
		Field:    FieldReference{Raw: field, Parts: []string{field}},
		Operator: OpEq,
		Value:    &ActorReference{Path: "userId"},
	}
}

// clusterOwnerLeafExpr (`actor.isClusterOwner == true`) lives in
// rowauthz_composite_test.go, which landed the same helper for the same tier.
// One definition per package, and the composite tier's own file is its home.

// THE SHAPE RULE RUNS ONLY ON A CLAIM. Every write that is not choosing a
// hostname inherits the stored one through the read-merge, and judging that
// inherited value would strand a user who owns a site at a hostname a cluster
// owner created for them: publish, disable and delete each arrive here carrying
// a hostname the user never supplied and cannot change.
func TestSiteHostnamePolicyDoesNotJudgeAnInheritedHostname(t *testing.T) {
	e := &MemQLEngine{}
	const custom = "www.a-custom-domain.test"

	// Unchanged from the stored value: the shape rule must not fire, so the
	// only thing left to fail on is this engine's absent database (the
	// uniqueness probe, which binds every caller).
	err := e.validateSiteHostnamePolicy(callerCtx("user-a"),
		map[string]any{"hostname": custom}, "s1", "user-a", true, custom)
	if err != nil && strings.Contains(err.Error(), "not under this cluster's domain") {
		t.Fatalf("an inherited hostname was judged against the user policy: %v\n"+
			"That is every publish, disable and delete on a site whose hostname an operator chose", err)
	}

	// CHANGING it is a claim, and the rule fires.
	err = e.validateSiteHostnamePolicy(callerCtx("user-a"),
		map[string]any{"hostname": custom}, "s1", "user-a", true, "shop.memql.localhost")
	if err == nil || !strings.Contains(err.Error(), "not under this cluster's domain") {
		t.Fatalf("changing a site's hostname to one outside the cluster's domain was not refused: %v\n"+
			"The rule is about who may CLAIM what, and a rename is a claim", err)
	}
}
