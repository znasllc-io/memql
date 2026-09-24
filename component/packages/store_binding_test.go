package packages

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/edge"
)

// store_binding_test.go covers the one fact epic memql#5530 moved into the
// manifest: a storefront NAMES the v1:shopify:store row it fronts, and
// everything about that store -- the myshopify.com domain, the Storefront
// token reference -- lives on the row rather than being copied beside it.
//
// The split these four cases measure is the design's, not an accident of
// where the code happened to land: the DECLARATION is a manifest fact, read
// offline by Analyze, and the RESOLUTION is a cluster read that belongs to
// publish. A test that resolved a store through Analyze would pass against an
// Analyze that had quietly started reaching a database, which is the property
// D12 exists to keep.

// storefrontOnlyReport is the one-deployable report the publish cases drive
// through: a storefront naming a store, with nothing else in the package to
// distract the assertion.
func storefrontOnlyReport(storeDomain string) *Report {
	return &Report{Deployables: []DeployableReport{{
		Name:    "storefront",
		Kind:    KindStorefront,
		Path:    "clients/web",
		Binding: &ManifestBinding{Store: storeDomain},
	}}}
}

// storeIdFor is the stub Deps.Stores: a fixed domain-to-row-id table, and ""
// for everything else, which is exactly the shape the production resolver
// answers with when a read comes back empty.
func storeIdFor(table map[string]string) StoreResolver {
	return func(_ context.Context, domain, _, _ string) (string, error) {
		return table[strings.TrimSpace(domain)], nil
	}
}

// TestAStorefrontManifestNamesItsStoreRatherThanCopyingIt is the DECLARATION
// half: the manifest carries one value, the store's myshopify.com domain, and
// it travels into the report untouched so the confirm gate can show what this
// deployable fronts before anything is built.
func TestAStorefrontManifestNamesItsStoreRatherThanCopyingIt(t *testing.T) {
	// THE NEGATIVE CONTROL IS THE CALL ITSELF, and it is the D12 property:
	// Analyze is handed no Deps, no engine, no resolver and no context. If a
	// store resolution ever moves into the analysis, there is nowhere for it
	// to read from and this line stops compiling or stops passing.
	rep, err := Analyze(validPackage(), Options{SourceVersion: "sha-abc"})
	if err != nil {
		t.Fatalf("the valid fixture must analyze clean offline: %v (problems: %+v)", err, rep.Problems)
	}

	front := rep.Deployables[0]
	if front.Name != "storefront" || front.Kind != KindStorefront {
		t.Fatalf("first deployable is not the storefront: %+v", front)
	}
	if front.Binding == nil {
		t.Fatal("the storefront's binding did not reach the report")
	}
	if got := front.Binding.Store; got != "acme.myshopify.com" {
		t.Fatalf("binding.store = %q, want the store the manifest names", got)
	}

	// AND IT NAMES, RATHER THAN DESCRIBES. The report is what the OS renders
	// and what the deployment row keeps, so a token reference travelling here
	// would be the duplication this epic removed, written down again.
	encoded, merr := json.Marshal(rep)
	if merr != nil {
		t.Fatalf("marshal report: %v", merr)
	}
	raw := strings.ToLower(string(encoded))
	if strings.Contains(raw, "storefronttokenref") || strings.Contains(raw, "storedomain") {
		t.Fatalf("the report still carries a copied store field:\n%s", raw)
	}
}

// TestAManifestNamingAnUnknownStoreDeploysAnUnattachedDraft is the RESOLUTION
// half, and it is deliberately a publish test: Analyze stays offline and
// resolves nothing, so the only place this can be measured is the stage that
// reads the cluster.
//
// A STORE THIS CALLER CANNOT ATTACH IS A DRAFT, NOT A REFUSAL (Connect Shopify,
// D5). It used to refuse the whole run, which is what stopped a developer's
// first deploy of a storefront whose store they were not allowed to see. Now
// the site is created as a draft with no binding and the run records a
// non-fatal note; the go-live rule is what keeps that draft away from
// shoppers until a store is connected.
func TestAManifestNamingAnUnknownStoreDeploysAnUnattachedDraft(t *testing.T) {
	pub := &fakePublisher{}
	engine := &recordingEngine{rows: map[string][]map[string]any{"query sitesForPackage": nil}}
	d := &Deps{
		Store:     &store{engine: engine},
		Publisher: pub,
		Logger:    discardLogger(),
		// A cluster with no such store: the resolver answers "", which is a
		// miss and never an error.
		Stores: storeIdFor(map[string]string{}),
	}
	req := DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Placements: map[string]Placement{"storefront": {Hostname: "shop.example.com"}},
	}
	bundles := map[string]edge.Bundle{"storefront": {}}

	outcomes, err := d.publish(callerCtx("v1:identity:user:someone"), req,
		map[string]any{}, storefrontOnlyReport("acme.myshopify.com"), bundles)
	if err != nil {
		t.Fatalf("a storefront whose store cannot be attached refused the run: %v", err)
	}
	if len(pub.createdStores) != 1 || pub.createdStores[0] != "" {
		t.Fatalf("the site must be created with no binding, got stores %q", pub.createdStores)
	}
	if len(pub.published) != 1 {
		t.Fatalf("the draft's files must still be put in place: %v", pub.published)
	}
	if len(pub.bound) != 0 {
		t.Fatalf("nothing resolved, so nothing may be bound: %v", pub.bound)
	}
	assertUnattachedNote(t, outcomes, "acme.myshopify.com")

	// THE REACHABLE POSITIVE: the same run, the same manifest, against a
	// cluster that HAS the store, binds it at create. Without it this test
	// would pass against a publish stage that had stopped resolving stores.
	ok := &fakePublisher{}
	d.Publisher = ok
	d.Stores = storeIdFor(map[string]string{"acme.myshopify.com": "v1:shopify:store:acme"})
	outcomes, perr := d.publish(callerCtx("v1:identity:user:someone"), req,
		map[string]any{}, storefrontOnlyReport("acme.myshopify.com"), bundles)
	if perr != nil {
		t.Fatalf("control failed: the same run against a cluster holding the store must publish: %v", perr)
	}
	if len(ok.createdStores) != 1 || ok.createdStores[0] != "v1:shopify:store:acme" {
		t.Fatalf("control failed: the site must be created bound to the store, got %q", ok.createdStores)
	}
	if len(outcomes) != 1 || outcomes[0].Refusal != nil {
		t.Fatalf("control failed: a bound storefront carries no note: %+v", outcomes)
	}
}

// assertUnattachedNote is the note an unattached storefront's run carries:
// non-fatal, deployable_store_unknown, naming the store and saying the
// storefront cannot go live until one is connected.
func assertUnattachedNote(t *testing.T, outcomes []DeployableOutcome, named string) {
	t.Helper()
	if len(outcomes) != 1 || outcomes[0].Refusal == nil {
		t.Fatalf("the outcome carries no note about the unattached store: %+v", outcomes)
	}
	note := outcomes[0].Refusal
	if note.Fatal {
		t.Errorf("the note is fatal, which would refuse the run it is attached to: %+v", note)
	}
	if note.Code != CodeDeployableStoreUnknown {
		t.Errorf("note code = %q, want %q", note.Code, CodeDeployableStoreUnknown)
	}
	for _, want := range []string{named, "not attached to a store", "cannot go live until a store is connected on its Store panel"} {
		if !strings.Contains(note.Message, want) {
			t.Errorf("the note does not say %q: %s", want, note.Message)
		}
	}
}

// TestARedeployKeepsItsBinding: the second deploy of an unchanged manifest
// finds its site and writes nothing to the binding.
//
// The site is created by the FIRST publish, not planted -- so the count below
// is the real sequence rather than a fixture's claim about one.
func TestARedeployKeepsItsBinding(t *testing.T) {
	pub := &fakePublisher{}
	engine := &recordingEngine{rows: map[string][]map[string]any{"query sitesForPackage": nil}}
	d := &Deps{
		Store:     &store{engine: engine},
		Publisher: pub,
		Logger:    discardLogger(),
		Stores:    storeIdFor(map[string]string{"acme.myshopify.com": "v1:shopify:store:acme"}),
	}
	req := DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Placements: map[string]Placement{"storefront": {Hostname: "shop.example.com"}},
	}
	bundles := map[string]edge.Bundle{"storefront": {}}
	ctx := callerCtx("v1:identity:user:someone")

	if _, err := d.publish(ctx, req, map[string]any{}, storefrontOnlyReport("acme.myshopify.com"), bundles); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	// What the first deploy left behind, as the next run reads it back.
	engine.rows["query sitesForPackage"] = []map[string]any{{
		"id":                    "v1:platform:site:storefront",
		"hostname":              "shop.example.com",
		"packageDeployableName": "storefront",
		"binding":               map[string]any{"storeId": "v1:shopify:store:acme"},
	}}
	if _, err := d.publish(ctx, req, map[string]any{}, storefrontOnlyReport("acme.myshopify.com"), bundles); err != nil {
		t.Fatalf("redeploy: %v", err)
	}

	if len(pub.created) != 1 {
		t.Fatalf("a redeploy must find its site rather than create a second one: %v", pub.created)
	}
	if len(pub.bound) != 0 {
		t.Fatalf("an unchanged manifest must not rewrite the binding: %v", pub.bound)
	}
	if len(pub.published) != 2 {
		t.Fatalf("both runs must publish: %v", pub.published)
	}
}

// TestARedeployRePointsABindingTheManifestChanged is the other half, and it
// is why the comparison is not "set on create only".
//
// The manifest is the DECLARATION of what this storefront fronts. A manifest
// that says one store while the site serves another is a storefront quietly
// talking to the wrong merchant -- the source says acme, the shopper's
// browser is handed beta's catalog, and nothing in either record disagrees
// with itself. So a redeploy re-points, and the site follows the source.
func TestARedeployRePointsABindingTheManifestChanged(t *testing.T) {
	pub := &fakePublisher{}
	engine := &recordingEngine{rows: map[string][]map[string]any{"query sitesForPackage": nil}}
	d := &Deps{
		Store:     &store{engine: engine},
		Publisher: pub,
		Logger:    discardLogger(),
		Stores: storeIdFor(map[string]string{
			"acme.myshopify.com": "v1:shopify:store:acme",
			"beta.myshopify.com": "v1:shopify:store:beta",
		}),
	}
	req := DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Placements: map[string]Placement{"storefront": {Hostname: "shop.example.com"}},
	}
	bundles := map[string]edge.Bundle{"storefront": {}}
	ctx := callerCtx("v1:identity:user:someone")

	if _, err := d.publish(ctx, req, map[string]any{}, storefrontOnlyReport("acme.myshopify.com"), bundles); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	engine.rows["query sitesForPackage"] = []map[string]any{{
		"id":                    "v1:platform:site:storefront",
		"hostname":              "shop.example.com",
		"packageDeployableName": "storefront",
		"binding":               map[string]any{"storeId": "v1:shopify:store:acme"},
	}}
	// THE MANIFEST MOVED: the same deployable, a different store.
	if _, err := d.publish(ctx, req, map[string]any{}, storefrontOnlyReport("beta.myshopify.com"), bundles); err != nil {
		t.Fatalf("redeploy: %v", err)
	}

	if len(pub.bound) != 1 {
		t.Fatalf("want exactly one re-point, got %v", pub.bound)
	}
	if got := pub.bound[0]; got != "v1:platform:site:storefront -> v1:shopify:store:beta" {
		t.Fatalf("the re-point named the wrong pair: %q", got)
	}
	if len(pub.created) != 1 {
		t.Fatalf("re-pointing is a write to the existing site, never a second one: %v", pub.created)
	}
}

// AN UNRESOLVABLE STORE IS FATAL ONLY WHEN NOTHING IS BOUND YET.
//
// This is the case the auto-deploy feed is in on EVERY automatic run. It
// borrows the package owner as a rankless writer (Deps.Roles), which is
// neither a cluster owner nor at v1:shopify:store's developer read floor, so
// storeByDomain answers zero rows. Fatal here would mean an armed source
// can never republish a storefront again -- something it could do before this
// epic, when a redeploy never read a store at all.
//
// It is safe because the manifest's store CANNOT have changed on that path:
// `bindingWord` puts it in the plan fingerprint, and a changed one parks the
// run at the confirm gate for a person. And it is not silent: the outcome
// carries a non-fatal problem saying the store is unchanged and why.
func TestARedeployWhoseStoreCannotBeResolvedKeepsItsBindingAndPublishes(t *testing.T) {
	pub := &fakePublisher{}
	engine := &recordingEngine{rows: map[string][]map[string]any{
		"query sitesForPackage": {{
			"id":                    "v1:platform:site:storefront",
			"hostname":              "shop.example.com",
			"packageDeployableName": "storefront",
			"binding":               map[string]any{"storeId": "v1:shopify:store:acme"},
		}},
	}}
	d := &Deps{
		Store:     &store{engine: engine},
		Publisher: pub,
		Logger:    discardLogger(),
		// Reads nothing: the caller is not a cluster owner.
		Stores: storeIdFor(map[string]string{}),
	}
	req := DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Placements: map[string]Placement{"storefront": {Hostname: "shop.example.com"}},
	}
	outcomes, err := d.publish(
		context.Background(), req, map[string]any{},
		storefrontOnlyReport("acme.myshopify.com"), map[string]edge.Bundle{"storefront": {}},
	)
	if err != nil {
		t.Fatalf("a redeploy of a bound storefront was refused for a store it does not change: %v", err)
	}
	if len(pub.published) != 1 {
		t.Fatalf("the bundle must still publish: %v", pub.published)
	}
	if len(pub.bound) != 0 {
		t.Fatalf("nothing resolved, so nothing may be re-pointed: %v", pub.bound)
	}
	// NOT SILENT. The run records why the store is unchanged.
	if len(outcomes) != 1 || outcomes[0].Refusal == nil {
		t.Fatalf("the outcome carries no note about the unresolved store: %+v", outcomes)
	}
	note := outcomes[0].Refusal
	if note.Fatal {
		t.Errorf("the note is fatal, which would refuse the run it is attached to: %+v", note)
	}
	if note.Code != CodeDeployableStoreUnknown {
		t.Errorf("note code = %q, want %q", note.Code, CodeDeployableStoreUnknown)
	}
	for _, want := range []string{"acme.myshopify.com", "unchanged"} {
		if !strings.Contains(note.Message, want) {
			t.Errorf("the note does not say %q: %s", want, note.Message)
		}
	}
}

// THE OTHER HALF of the case above: with NOTHING bound, the same unresolvable
// store is not "unchanged" -- there is nothing to keep -- so the storefront
// stays an unattached draft and the note says so. It holds on a REDEPLOY of an
// unattached site as well as on a first deploy (Connect Shopify, D5): the
// site is found, published, and still bound to nothing.
func TestAnUnboundStorefrontWhoseStoreCannotBeResolvedStaysAnUnattachedDraft(t *testing.T) {
	pub := &fakePublisher{}
	engine := &recordingEngine{rows: map[string][]map[string]any{
		"query sitesForPackage": {{
			"id":                    "v1:platform:site:storefront",
			"hostname":              "shop.example.com",
			"packageDeployableName": "storefront",
		}},
	}}
	d := &Deps{
		Store:     &store{engine: engine},
		Publisher: pub,
		Logger:    discardLogger(),
		Stores:    storeIdFor(map[string]string{}),
	}
	req := DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Placements: map[string]Placement{"storefront": {Hostname: "shop.example.com"}},
	}
	outcomes, err := d.publish(
		context.Background(), req, map[string]any{},
		storefrontOnlyReport("acme.myshopify.com"), map[string]edge.Bundle{"storefront": {}},
	)
	if err != nil {
		t.Fatalf("a redeploy of an unattached storefront was refused: %v", err)
	}
	if len(pub.created) != 0 || len(pub.bound) != 0 {
		t.Fatalf("the existing site must be found and left unbound: created %v, bound %v", pub.created, pub.bound)
	}
	if len(pub.published) != 1 {
		t.Errorf("the redeploy must publish: %v", pub.published)
	}
	assertUnattachedNote(t, outcomes, "acme.myshopify.com")
	if strings.Contains(outcomes[0].Refusal.Message, "unchanged") {
		t.Errorf("an unattached storefront's note claims a store was kept: %s", outcomes[0].Refusal.Message)
	}
}

// AN UNATTACHED DRAFT IS ATTACHED BY THE FIRST REDEPLOY THAT CAN ATTACH IT.
// The draft a first deploy left (the case above) is not stuck: once the
// manifest's store resolves for the caller -- the store was mirrored, or the
// caller now holds the store part -- the redeploy re-points the existing site
// to it, exactly once, and the note is gone because nothing is unattached.
func TestARedeployAttachesAnUnattachedDraftOnceItsStoreResolves(t *testing.T) {
	pub := &fakePublisher{}
	engine := &recordingEngine{rows: map[string][]map[string]any{
		"query sitesForPackage": {{
			"id":                    "v1:platform:site:storefront",
			"hostname":              "shop.example.com",
			"packageDeployableName": "storefront",
		}},
	}}
	d := &Deps{
		Store:     &store{engine: engine},
		Publisher: pub,
		Logger:    discardLogger(),
		Stores:    storeIdFor(map[string]string{"acme.myshopify.com": "v1:shopify:store:acme"}),
	}
	req := DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Placements: map[string]Placement{"storefront": {Hostname: "shop.example.com"}},
	}
	outcomes, err := d.publish(
		context.Background(), req, map[string]any{},
		storefrontOnlyReport("acme.myshopify.com"), map[string]edge.Bundle{"storefront": {}},
	)
	if err != nil {
		t.Fatalf("a redeploy of an unattached storefront whose store now resolves was refused: %v", err)
	}
	if len(pub.created) != 0 {
		t.Fatalf("the existing draft must be found, not a second site created: %v", pub.created)
	}
	if len(pub.bound) != 1 || pub.bound[0] != "v1:platform:site:storefront -> v1:shopify:store:acme" {
		t.Fatalf("want exactly one binding of the draft to acme, got %v", pub.bound)
	}
	if len(outcomes) != 1 || outcomes[0].Refusal != nil {
		t.Fatalf("an attached storefront carries a note: %+v", outcomes)
	}
}

// TestTheStoreIsAskedForAtTheSitesOrganization: the resolver is told which
// organization the site belongs to, because attaching a store takes the store
// part THERE as well as of the caller (memql#5598). A first deploy names the
// placement's organization; a redeploy names the one its site row carries.
func TestTheStoreIsAskedForAtTheSitesOrganization(t *testing.T) {
	var asked []string
	engine := &recordingEngine{rows: map[string][]map[string]any{"query sitesForPackage": nil}}
	d := &Deps{
		Store:     &store{engine: engine},
		Publisher: &fakePublisher{},
		Logger:    discardLogger(),
		Stores: func(_ context.Context, _, _, account string) (string, error) {
			asked = append(asked, account)
			return "v1:shopify:store:acme", nil
		},
	}
	req := DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Placements: map[string]Placement{"storefront": {Hostname: "shop.example.com", AccountId: "acme"}},
	}
	bundles := map[string]edge.Bundle{"storefront": {}}
	ctx := callerCtx("v1:identity:user:someone")

	if _, err := d.publish(ctx, req, map[string]any{}, storefrontOnlyReport("acme.myshopify.com"), bundles); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	engine.rows["query sitesForPackage"] = []map[string]any{{
		"id":                    "v1:platform:site:storefront",
		"hostname":              "shop.example.com",
		"packageDeployableName": "storefront",
		"accountId":             "beta",
	}}
	if _, err := d.publish(ctx, req, map[string]any{}, storefrontOnlyReport("acme.myshopify.com"), bundles); err != nil {
		t.Fatalf("redeploy: %v", err)
	}
	if strings.Join(asked, ",") != "acme,beta" {
		t.Fatalf("the resolver was asked at %q, want the placement's organization and then the site's own", asked)
	}
}
