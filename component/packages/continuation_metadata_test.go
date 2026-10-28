package packages

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/edge"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

func metadataContinuationFixture(t *testing.T) (*continuationFixture, string, string, string) {
	t.Helper()
	source := validPackage()
	source[ManifestName] = file(strings.Replace(validManifest, "    kind: static", "    kind: shopify_storefront\n    binding:\n      store: selected.myshopify.com\n    testing:\n      binding:\n        store: testing.myshopify.com", 1))
	c := newContinuationFixtureSource(t, source)
	f := c.base
	suffix := memql.BareShortId(f.pkg)
	oldStore, nextStore, testStore := suffix+"-old", suffix+"-selected", suffix+"-testing"
	engine := f.deps[0].Store.engine.(*memql.MemQLEngine)
	for index, id := range []string{oldStore, nextStore, testStore} {
		mustExecute(t, engine, tieSeederCtx(), fmt.Sprintf(`mutation createStore(storeId: %s, domain: %s, storefrontTokenRef: %s)`, langparser.QuoteString(id), langparser.QuoteString(fmt.Sprintf("metadata-%d-%s.myshopify.com", index, suffix)), langparser.QuoteString(memql.StorefrontTokenSecretName(id))))
	}
	db, err := f.deps[0].Store.continuationDB()
	if err != nil {
		t.Fatal(err)
	}
	siteID := "v1:platform:site:" + suffix
	if _, err := db.ExecContext(f.ctx, `UPDATE "MemoryNodes" SET payload=payload || jsonb_build_object('kind','shopify_storefront','status','live','bundleRef','blob://prior/','binding',jsonb_build_object('storeId',$2::text),'previewBinding',jsonb_build_object('storeId',$2::text)) WHERE id=$1`, siteID, oldStore); err != nil {
		t.Fatal(err)
	}
	for _, d := range f.deps {
		d.Stores = func(_ context.Context, named, _, _ string) (string, error) {
			if named == "selected.myshopify.com" {
				return nextStore, nil
			}
			if named == "testing.myshopify.com" {
				return testStore, nil
			}
			return "", nil
		}
	}
	f.ctx = memql.ContextWithFreshRead(f.ctx)
	return c, oldStore, nextStore, testStore
}

func metadataSite(t *testing.T, c *continuationFixture) map[string]any {
	t.Helper()
	f := c.base
	row, err := f.deps[1].Store.siteById(memql.ContextWithFreshRead(f.ctx), "v1:platform:site:"+memql.BareShortId(f.pkg))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

type preparationHookPublisher struct {
	*continuationPublisher
	after func() error
}

func (p *preparationHookPublisher) PrepareBundle(ctx context.Context, id string, b edge.Bundle, target edge.Target) (PreparedSiteBundle, error) {
	out, err := p.continuationPublisher.PrepareBundle(ctx, id, b, target)
	if err != nil {
		return out, err
	}
	if p.after != nil {
		err = p.after()
	}
	return out, err
}

func TestPackageContinuationDefersStoreMetadata(t *testing.T) {
	for _, failure := range []string{"upload", "checkpoint", "rollout"} {
		t.Run(failure, func(t *testing.T) {
			c, _, _, _ := metadataContinuationFixture(t)
			f := c.base
			before := metadataSite(t, c)
			switch failure {
			case "upload":
				f.deps[0].Publisher = &preparationHookPublisher{continuationPublisher: c.publishers[0], after: func() error { return errors.New("retained upload failed") }}
			case "checkpoint":
				f.deps[0].Publisher = &preparationHookPublisher{continuationPublisher: c.publishers[0], after: func() error {
					db, _ := f.deps[0].Store.continuationDB()
					_, err := db.ExecContext(f.ctx, `UPDATE "MemoryNodes" SET payload=jsonb_set(payload,'{sourceVersion}','"changed-before-checkpoint"'::jsonb) WHERE id=$1`, f.run)
					return err
				}}
			case "rollout":
				c.roller.lost = true
			}
			out, err := Deploy(f.ctx, f.deps[0], DeployRequest{PackageId: f.pkg, DeploymentId: f.run, Confirmed: true, Actor: actorFromContext(f.ctx), Placements: map[string]Placement{"storefront": {Skip: true}}})
			if failure == "rollout" {
				if err != nil || out.Status != StatusRolling {
					t.Fatalf("rollout: %+v %v", out, err)
				}
				c.resume(t)
			} else if err == nil {
				t.Fatal("injected preparation/checkpoint failure was accepted")
			}
			if after := metadataSite(t, c); !reflect.DeepEqual(before, after) {
				t.Fatalf("%s changed live site before successful rollout", failure)
			}
			if c.publishers[0].published.Load() != 0 || c.publishers[1].published.Load() != 0 {
				t.Fatal("publication crossed failure")
			}
		})
	}
}

func TestPackageContinuationPublishesBindingsWithBundle(t *testing.T) {
	for _, fault := range []string{"none", "lost-reply", "receipt-rollback", "site-drift", "revoked-store"} {
		t.Run(fault, func(t *testing.T) {
			c, oldStore, nextStore, testStore := metadataContinuationFixture(t)
			f := c.base
			c.start(t)
			if row := metadataSite(t, c); boundStoreId(row) != oldStore || rowString(row, "bundleRef") != "blob://prior/" {
				t.Fatal("preparation changed serving metadata")
			}
			before := metadataSite(t, c)
			db, _ := f.deps[0].Store.continuationDB()
			if fault == "lost-reply" {
				c.publishers[1].lost.Store(true)
			}
			if fault == "receipt-rollback" {
				name := fmt.Sprintf("metadata_receipt_fail_%d", time.Now().UnixNano())
				if _, err := db.ExecContext(f.ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.deployment_id='%s' AND NEW.publications<>OLD.publications THEN RAISE EXCEPTION 'metadata receipt failure'; END IF; RETURN NEW; END $$`, name, memql.BareShortId(f.run))); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(f.ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON package_deployment_continuations FOR EACH ROW EXECUTE FUNCTION %s()`, name, name)); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_, _ = db.ExecContext(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON package_deployment_continuations`, name))
					_, _ = db.ExecContext(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name))
				})
			}
			if fault == "site-drift" {
				if _, err := f.deps[1].Store.engine.Execute(f.ctx, fmt.Sprintf(`mutation updateSiteSettings(siteId: %s, settings: { changed: "after-selection" })`, langparser.QuoteString(rowString(before, "id")))); err != nil {
					t.Fatal(err)
				}
				before = metadataSite(t, c)
			}
			if fault == "revoked-store" {
				engine := f.deps[0].Store.engine.(*memql.MemQLEngine)
				engine.InstallGrantResolution()
				t.Cleanup(func() { auth.SetGrantSource(nil); auth.SetMembershipSource(nil) })
				grant := auth.GrantRowID("user", memql.BareShortId(f.owner), auth.VerbExecute, "app:deployables/store")
				mustExecute(t, engine, tieSeederCtx(), fmt.Sprintf(`mutation writeGrant(grantId: %s, subjectKind: "user", subjectId: %s, verb: "execute", resourceType: "app:deployables/store", effect: "deny", grantedBy: "metadata-test")`, langparser.QuoteString(grant), langparser.QuoteString(memql.BareShortId(f.owner))))
				t.Cleanup(func() {
					_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE id=$1`, "v1:rbac:grant:"+grant)
				})
			}
			c.roller.ready = true
			c.resume(t)
			if fault == "lost-reply" {
				c.resume(t)
			}
			after := metadataSite(t, c)
			record, err := f.deps[1].Store.readContinuation(f.ctx, f.run)
			if err != nil {
				t.Fatal(err)
			}
			if fault == "none" || fault == "lost-reply" {
				if !sameShortId(boundStoreId(after), nextStore) || !sameShortId(boundStoreId(map[string]any{"binding": after["previewBinding"]}), testStore) || rowString(after, "bundleRef") == "blob://prior/" || len(record.Receipts) != 1 || rowString(c.row(t), "status") != StatusSucceeded {
					t.Fatal("bundle and selected bindings did not commit together")
				}
				if c.publishers[1].published.Load() != 1 || c.roller.calls != 1 {
					t.Fatal("lost reply repeated a committed publication or rollout")
				}
			} else if !reflect.DeepEqual(before, after) || len(record.Receipts) != 0 || rowString(c.row(t), "status") == StatusSucceeded {
				t.Fatalf("%s allowed a partial/unauthorized publication", fault)
			}
		})
	}
}

func TestPackagePublicationRejectsUnselectedBindingDelta(t *testing.T) {
	for _, change := range []string{"missing-binding", "extra-preview", "extra-provenance"} {
		t.Run(change, func(t *testing.T) {
			c, _, _, _ := metadataContinuationFixture(t)
			c.start(t)
			f := c.base
			r, err := f.deps[1].Store.claimContinuation(f.ctx, f.run)
			if err != nil || r == nil {
				t.Fatalf("claim: %v", err)
			}
			defer f.deps[1].Store.releaseContinuation(f.ctx, r)
			if err := f.deps[1].Store.saveContinuation(f.ctx, r, "publishing"); err != nil {
				t.Fatal(err)
			}
			p := r.Selection.Publications[0]
			b := r.Selection.Bundles[0]
			ctx := memql.ContextWithPackagePublicationFence(memql.ContextWithRowVersionFence(f.ctx, "v1:platform:site", p.SiteID, p.SiteVersion), r.Selection.DeploymentID, r.SHA, r.Lease, p.Key)
			before := metadataSite(t, c)
			if change == "extra-provenance" {
				_, err = f.deps[1].Store.engine.Execute(ctx, fmt.Sprintf(`mutation updateSiteBundle(siteId: %s, bundleRef: %s, binding: {storeId: %s}, previewBinding: {storeId: %s}, artifactId: "unselected")`, langparser.QuoteString(b.SiteID), langparser.QuoteString(b.BundleRef), langparser.QuoteString(b.Binding.StoreID), langparser.QuoteString(b.PreviewBinding.StoreID)))
			} else {
				if change == "missing-binding" {
					b.Binding = nil
				} else {
					b.PreviewBinding = b.Binding
				}
				err = c.publishers[1].writer.PointPrepared(ctx, b)
			}
			if err == nil || !reflect.DeepEqual(before, metadataSite(t, c)) {
				t.Fatal("unselected publication delta reached the site")
			}
		})
	}
}

type firstSiteListEmptyEngine struct {
	Engine
	first  bool
	writes int
}

func (e *firstSiteListEmptyEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(q, "query sitesForPackage(") && !e.first {
		e.first = true
		return &memql.ExecuteResult{}, nil
	}
	if !strings.HasPrefix(q, "query ") {
		e.writes++
	}
	return e.Engine.Execute(ctx, q)
}

func TestPackageContinuationDraftAndExistingSitePreparation(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%v", existing), func(t *testing.T) {
			c, _, _, _ := metadataContinuationFixture(t)
			f := c.base
			before := metadataSite(t, c)
			native := f.deps[0].Store.engine
			db, _ := f.deps[0].Store.continuationDB()
			d := *f.deps[0]
			s := *d.Store
			d.Store = &s
			var observed *firstSiteListEmptyEngine
			if existing {
				observed = &firstSiteListEmptyEngine{Engine: native}
				s.engine = observed
			} else {
				if _, err := db.ExecContext(f.ctx, `UPDATE "MemoryNodes" SET payload=jsonb_set(payload,'{deleted}','true'::jsonb) WHERE id=$1`, "v1:platform:site:"+memql.BareShortId(f.pkg)); err != nil {
					t.Fatal(err)
				}
			}
			pub := *c.publishers[0]
			pub.SitePublisher = &enginePublisher{engine: native, store: &s, logger: discardLogger()}
			d.Publisher = &pub
			pkg, err := s.packageById(f.ctx, f.pkg)
			if err != nil {
				t.Fatal(err)
			}
			report := reportFromRow(c.row(t))
			_, _, intents, _, err := d.preparePublications(f.ctx, DeployRequest{PackageId: f.pkg, Placements: map[string]Placement{"storefront": {Skip: true}, "docs": {Hostname: "new-" + memql.BareShortId(f.pkg) + ".example.test"}}}, pkg, report, map[string]edge.Bundle{"docs": {"index.html": []byte("selected")}})
			if err != nil || len(intents) != 1 {
				t.Fatalf("prepare: %+v %v", intents, err)
			}
			after, err := s.siteById(memql.ContextWithFreshRead(f.ctx), intents[0].SiteID)
			if err != nil {
				t.Fatal(err)
			}
			if existing {
				version, _ := continuationRowVersion(before)
				if observed.writes != 0 || !reflect.DeepEqual(before, after) || !intents[0].SiteVersion.Equal(version) {
					t.Fatal("EnsureSite's existing result was mutated or its original witness replaced")
				}
			} else {
				t.Cleanup(func() {
					_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE id=$1`, "v1:platform:site:"+intents[0].SiteID)
				})
				if rowString(after, "status") != siteStatusDraft || rowString(after, "bundleRef") != "" || boundStoreId(after) != "" || boundStoreId(map[string]any{"binding": after["previewBinding"]}) != "" {
					t.Fatal("new draft acquired serving bytes or bindings during preparation")
				}
				if intents[0].Binding == nil || intents[0].PreviewBinding == nil {
					t.Fatal("new draft lost selected deferred store bindings")
				}
			}
		})
	}
}

func TestImmediatePublicationKeepsStoreResolutionBehavior(t *testing.T) {
	for _, selection := range []string{"selected", "unresolved", "omitted"} {
		t.Run(selection, func(t *testing.T) {
			h := newHarness(t, spaOnlyPackage(), ownerPackage())
			h.engine.rows["query sitesForPackage("] = []map[string]any{{"id": "v1:platform:site:storefront", "packageDeployableName": "storefront", "hostname": "existing.example.test", "accountId": "account", "binding": map[string]any{"storeId": "old"}, "previewBinding": map[string]any{"storeId": "old-test"}}}
			dep := DeployableReport{Name: "storefront", Kind: KindStorefront}
			if selection != "omitted" {
				dep.Binding = &ManifestBinding{Store: "selected.myshopify.com"}
				dep.Testing = &ManifestTesting{Binding: &ManifestBinding{Store: "testing.myshopify.com"}}
			}
			h.deps.Stores = func(_ context.Context, named, _, _ string) (string, error) {
				if selection == "unresolved" {
					return "", nil
				}
				if named == "selected.myshopify.com" {
					return "selected", nil
				}
				return "testing", nil
			}
			out, err := h.deps.publish(context.Background(), DeployRequest{PackageId: "package"}, ownerPackage(), &Report{Deployables: []DeployableReport{dep}}, map[string]edge.Bundle{"storefront": {"index.html": []byte("ordinary")}})
			if err != nil || len(out) != 1 || len(h.publisher.published) != 1 {
				t.Fatalf("ordinary publish: %+v %v", out, err)
			}
			if selection == "selected" {
				if len(h.publisher.bound) != 2 || out[0].Refusal != nil {
					t.Fatal("ordinary publication lost selected serving/testing connections")
				}
			} else {
				if len(h.publisher.bound) != 0 {
					t.Fatal("omitted or unreadable selection changed an existing connection")
				}
				if (out[0].Refusal != nil) != (selection == "unresolved") {
					t.Fatal("store resolution changed nonfatal-note semantics")
				}
			}
		})
	}
}
