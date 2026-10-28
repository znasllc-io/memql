package memql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

type packageDomainFixture struct {
	engine          *MemQLEngine
	db              *bun.DB
	actor           context.Context
	intent          PackageDomainIntent
	run, sha, lease string
	siteVersion     time.Time
}

func newPackageDomainFixture(t *testing.T) packageDomainFixture {
	t.Helper()
	engine, db, _ := sharedReadMergeEngine(t)
	suffix := cdSuffix(t)
	user, account, pkg, site := "user-"+suffix, "account-"+suffix, "package-"+suffix, "site-"+suffix
	seedPrincipal(t, engine, user, auth.RoleOwner)
	actor := rankActorCtx(user, auth.RoleOwner)
	if err := organizationInsert(t, engine, actor, conceptAccountsAccount, account, map[string]any{"name": account, "status": "active", "domainStatus": "unverified"}); err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"createPackage", map[string]any{"packageId": pkg, "name": "Domain fixture", "sourceKind": "artifact", "artifactId": "uploaded", "accountId": account}},
		{"createSite", map[string]any{"siteId": site, "hostname": "site-" + suffix + ".example.net", "kind": "static", "bundleRef": "blob://sites/" + site + "/v1/", "status": "live", "accountId": account}},
		{"recordSitePackageOrigin", map[string]any{"siteId": site, "packageId": pkg, "packageDeployableName": "docs"}},
	} {
		if _, err := runSiteMutation(t, auth.ContextWithInternalOrigin(actor), engine, call.name, call.args); err != nil {
			t.Fatalf("%s: %v", call.name, err)
		}
	}
	var siteRow memorynodes.MemoryNode
	if err := db.NewSelect().Model(&siteRow).ModelTableExpr(memorynodes.ObservedRowsSQL+" AS mn").Where("concept = ?", conceptPlatformSite).Where("id = ?", conceptPlatformSite+":"+site).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(actor); err != nil {
		t.Fatal(err)
	}
	f := packageDomainFixture{engine: engine, db: db, actor: actor, run: "run-" + suffix, lease: "lease-" + suffix, siteVersion: siteRow.CreatedAt}
	f.intent = PackageDomainIntent{Key: "domain/" + suffix, PublicationKey: site + "/serving", DomainID: "domain-" + suffix, SiteID: site, AccountID: account, Hostname: "client-" + suffix + ".example.org"}
	publication := PackagePublicationIntent{Key: f.intent.PublicationKey, SiteID: site, AccountID: account, Target: "serving", BundleRef: "blob://sites/" + site + "/v1/", SiteVersion: siteRow.CreatedAt}
	body, err := json.Marshal(map[string]any{"domains": []PackageDomainIntent{f.intent}, "publications": []PackagePublicationIntent{publication}})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	f.sha = hex.EncodeToString(digest[:])
	publications, err := json.Marshal(map[string]PackagePublicationReceipt{publication.Key: {SiteID: site, Target: publication.Target, BundleRef: publication.BundleRef, SiteVersion: siteRow.CreatedAt}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(actor, `INSERT INTO package_deployment_continuations (deployment_id,owner_user_id,requested_by,package_id,selection_sha256,body,phase,publications,deadline,lease_id,lease_until) VALUES (?,?,?,?,?,?::jsonb,'publishing',?::jsonb,clock_timestamp()+interval '5 minutes',?,clock_timestamp()+interval '90 seconds')`, f.run, user, user, pkg, f.sha, string(body), string(publications), f.lease); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM package_deployment_continuations WHERE deployment_id=?`, f.run)
		_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id LIKE ?", "%"+suffix+"%").Exec(context.Background())
	})
	return f
}

func (f packageDomainFixture) context() context.Context {
	ctx := ContextWithRowVersionFence(f.actor, conceptPlatformSite, f.intent.SiteID, f.siteVersion)
	ctx = ContextWithPackageDomainFence(ctx, f.run, f.sha, f.lease, f.intent)
	// The same stamp as customdomain.Store: the native fence must recheck the
	// original requester rather than trust this synthetic owner identity.
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "system:customDomain", Role: auth.RoleOwner})
	ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "system:customDomain"})
	return auth.ContextWithInternalOrigin(ctx)
}

func (f packageDomainFixture) create(t *testing.T, ctx context.Context) error {
	t.Helper()
	_, err := runSiteMutation(t, ctx, f.engine, "createCustomDomain", map[string]any{"domainId": f.intent.DomainID, "siteId": f.intent.SiteID, "hostname": f.intent.Hostname, "accountId": f.intent.AccountID, "token": "fresh-verification-token"})
	return err
}

func (f packageDomainFixture) receipts(t *testing.T) map[string]PackageDomainReceipt {
	t.Helper()
	var raw []byte
	if err := f.db.QueryRowContext(f.actor, `SELECT domain_receipts FROM package_deployment_continuations WHERE deployment_id=?`, f.run).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var receipts map[string]PackageDomainReceipt
	if err := json.Unmarshal(raw, &receipts); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "verification-token") || strings.Contains(string(raw), `"token"`) {
		t.Fatal("verification token escaped into continuation receipts")
	}
	return receipts
}

func TestPackageDomainCreationAndReceiptCommitTogether(t *testing.T) {
	f := newPackageDomainFixture(t)
	second, err := New(f.db)
	if err != nil {
		t.Fatal(err)
	}
	second.Logger = f.engine.Logger
	if err := second.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	replacement := f
	replacement.engine = second
	// A replacement replica has no original engine cache or callback state.
	// It must still require the selected requester and current native lease.
	for _, fault := range []string{"requester", "lease"} {
		invalid := replacement
		if fault == "requester" {
			invalid.actor = rankActorCtx("foreign-requester", auth.RoleOwner)
		} else {
			invalid.lease = "replaced-lease"
		}
		if err := invalid.create(t, invalid.context()); err == nil || len(f.receipts(t)) != 0 {
			t.Fatalf("second replica accepted invalid %s: %v", fault, err)
		}
	}
	if err := f.create(t, f.context()); err != nil {
		t.Fatal(err)
	}
	// Simulate a lost success reply: only the durable receipt, read by B,
	// establishes that A's selected operation committed.
	receipt := replacement.receipts(t)[f.intent.Key]
	if !PackageDomainReceiptMatches(f.intent, receipt) || receipt.Refusal != "" || receipt.ExistingDomainID != "" {
		t.Fatalf("missing exact creation receipt: %+v", receipt)
	}
	_, err = runSiteMutation(t, replacement.context(), second, "createCustomDomain", map[string]any{"domainId": f.intent.DomainID, "siteId": f.intent.SiteID, "hostname": f.intent.Hostname, "accountId": f.intent.AccountID, "token": "replacement-verification-token"})
	if err == nil {
		t.Fatal("replayed domain creation was accepted")
	}
	count, err := f.db.NewSelect().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", conceptPlatformCustomDomain).Where("id = ?", conceptPlatformCustomDomain+":"+f.intent.DomainID).Count(f.actor)
	if err != nil || count != 1 {
		t.Fatalf("domain was recreated after its committed receipt: %d %v", count, err)
	}
	var token string
	if err := f.db.QueryRowContext(f.actor, `SELECT payload->>'token' FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, conceptPlatformCustomDomain+":"+f.intent.DomainID).Scan(&token); err != nil || token != "fresh-verification-token" {
		t.Fatal("second replica rotated the committed verification token")
	}
}

func TestPackageDomainFenceRefusesChangedAuthorityAndSelection(t *testing.T) {
	for _, fault := range []string{"requester", "role", "lease", "selection", "publication", "site-witness", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			f := newPackageDomainFixture(t)
			switch fault {
			case "requester":
				f.actor = rankActorCtx("foreign", auth.RoleOwner)
			case "role":
				access, _ := auth.AccessFromContext(f.actor)
				seedPrincipal(t, f.engine, access.UserId, auth.RoleReader)
			case "lease":
				f.lease = "foreign"
			case "selection":
				f.sha = strings.Repeat("0", 64)
			case "publication":
				if _, err := f.db.ExecContext(f.actor, `UPDATE package_deployment_continuations SET publications='{}' WHERE deployment_id=?`, f.run); err != nil {
					t.Fatal(err)
				}
			case "site-witness":
				f.siteVersion = f.siteVersion.Add(-time.Microsecond)
			case "cancel":
				if _, err := f.db.ExecContext(f.actor, `UPDATE package_deployment_continuations SET cancel_requested=true WHERE deployment_id=?`, f.run); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.create(t, f.context()); err == nil || len(f.receipts(t)) != 0 {
				t.Fatalf("invalid domain selection wrote a result: %v", err)
			}
		})
	}
}

func TestPackageDomainReusesMatchingBindingAndRecordsCollision(t *testing.T) {
	for _, sameSite := range []bool{true, false} {
		name := "matching"
		if !sameSite {
			name = "collision"
		}
		t.Run(name, func(t *testing.T) {
			f := newPackageDomainFixture(t)
			existingSite := f.intent.SiteID
			if !sameSite {
				existingSite = seedCustomDomainSite(t, f.engine, cdSuffix(t)+"-other")
			}
			existingID := f.intent.DomainID + "-existing"
			if _, err := runSiteMutation(t, ownerCustomDomainCtx(), f.engine, "createCustomDomain", map[string]any{"domainId": existingID, "siteId": existingSite, "hostname": f.intent.Hostname, "accountId": f.intent.AccountID, "token": "retained-verification-token"}); err != nil {
				t.Fatal(err)
			}
			if err := f.create(t, f.context()); !errors.Is(err, ErrPackageDomainRecorded) {
				t.Fatalf("existing binding did not produce a committed observation: %v", err)
			}
			receipt := f.receipts(t)[f.intent.Key]
			if !PackageDomainReceiptMatches(f.intent, receipt) || sameSite && receipt.ExistingDomainID != existingID || !sameSite && receipt.Refusal == "" {
				t.Fatalf("wrong existing-domain result: %+v", receipt)
			}
			count, err := f.db.NewSelect().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", conceptPlatformCustomDomain).Where("id = ?", conceptPlatformCustomDomain+":"+f.intent.DomainID).Count(f.actor)
			if err != nil || count != 0 {
				t.Fatal("observation created another binding")
			}
			var token string
			if err := f.db.QueryRowContext(f.actor, `SELECT payload->>'token' FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, conceptPlatformCustomDomain+":"+existingID).Scan(&token); err != nil || token != "retained-verification-token" {
				t.Fatal("existing domain token was rotated")
			}
		})
	}
}

func TestPackageDomainReceiptFailureRollsBackCreation(t *testing.T) {
	f := newPackageDomainFixture(t)
	name := fmt.Sprintf("domain_receipt_fail_%d", time.Now().UnixNano())
	if _, err := f.db.ExecContext(f.actor, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.deployment_id = '%s' AND NEW.domain_receipts <> OLD.domain_receipts THEN RAISE EXCEPTION 'injected domain receipt failure'; END IF; RETURN NEW; END $$`, name, f.run)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(f.actor, fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON package_deployment_continuations FOR EACH ROW EXECUTE FUNCTION %s()`, name, name)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.db.ExecContext(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON package_deployment_continuations`, name))
		_, _ = f.db.ExecContext(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name))
	})
	if err := f.create(t, f.context()); err == nil || len(f.receipts(t)) != 0 {
		t.Fatalf("receipt failure did not refuse: %v", err)
	}
	count, err := f.db.NewSelect().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", conceptPlatformCustomDomain).Where("id = ?", conceptPlatformCustomDomain+":"+f.intent.DomainID).Count(f.actor)
	if err != nil || count != 0 {
		t.Fatal("binding escaped the rolled-back receipt transaction")
	}
}
