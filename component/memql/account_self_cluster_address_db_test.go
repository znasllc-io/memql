package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// account_self_cluster_address_db_test.go -- memql#5792.
//
// Editing the operator organization's profile was refused with
// `domain_under_cluster_domain`, naming the CLUSTER'S own domain rather than
// anything the owner submitted. The client-domain guard judges the merged
// `memqlDomain`, and the operator row's name in cluster space -- stored by an
// older seed, or derived as `memql.<domain>` from the company domain an owner
// types -- is the operator's own address, not a request to reserve a host.
//
// WHY THE ROW IS WRITTEN BENEATH THE ENGINE. The defect is about a row that
// ALREADY EXISTS in that shape, and no write path of the current binary can
// produce it: the guard under test refuses it. So the stored version is
// inserted straight into the version table, the way a cluster seeded by an
// earlier binary holds it, and every write under test goes through a FRESH
// engine over the same database -- one with no memory of the insert, so what
// it judges is the stored row and nothing a cache kept.
//
// Postgres-gated like its neighbours. CI's db-tests lane runs this package
// with MEMQL_REQUIRE_DB=1, so a skip there is a failure rather than a green.

// selfAddressClusterDomain is the cluster these cases run on, and
// selfAddressCompanyDomain the operator's company domain: its parent, which is
// the ordinary install and makes the default name exactly the cluster's own.
const (
	selfAddressClusterDomain = "memql.operator-5792.example"
	selfAddressCompanyDomain = "operator-5792.example"
)

// storeAccountVersion writes `fields` over the newest stored version of an
// account as a NEW version, beneath the engine, and removes every version this
// test wrote when it ends -- the self row is a singleton the package shares.
func storeAccountVersion(t *testing.T, db *bun.DB, id string, fields map[string]any) {
	t.Helper()
	ctx := context.Background()
	var latest memorynodes.MemoryNode
	if err := db.NewSelect().Model(&latest).
		Where("concept = ?", conceptAccountsAccount).
		Where("id = ?", id).
		Order("createdAt DESC").
		Limit(1).
		Scan(ctx); err != nil {
		t.Fatalf("read the stored %s: %v", id, err)
	}
	payload := map[string]any{}
	if err := json.Unmarshal(latest.Payload, &payload); err != nil {
		t.Fatalf("decode the stored %s: %v", id, err)
	}
	for k, v := range fields {
		payload[k] = v
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	written := time.Now().UTC()
	row := latest
	row.CreatedAt = written
	row.Payload = raw
	if _, err := db.NewInsert().Model(&row).Exec(ctx); err != nil {
		t.Fatalf("store the legacy version of %s: %v", id, err)
	}
	t.Cleanup(func() {
		if _, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).
			Where("concept = ?", conceptAccountsAccount).
			Where("id = ?", id).
			Where(`"createdAt" >= ?`, written).
			Exec(context.Background()); err != nil {
			t.Errorf("remove this test's versions of %s: %v", id, err)
		}
	})
}

// storedAccountPayload is the newest stored version, read off the table.
func storedAccountPayload(t *testing.T, db *bun.DB, id string) map[string]any {
	t.Helper()
	var latest memorynodes.MemoryNode
	if err := db.NewSelect().Model(&latest).
		Where("concept = ?", conceptAccountsAccount).
		Where("id = ?", id).
		Order("createdAt DESC").
		Limit(1).
		Scan(context.Background()); err != nil {
		t.Fatalf("read back %s: %v", id, err)
	}
	payload := map[string]any{}
	if err := json.Unmarshal(latest.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

// ensureSelfAccount materializes the self row through the engine when this
// database has none yet, the way the boot seed does: no domain at all.
func ensureSelfAccount(t *testing.T, eng *MemQLEngine, db *bun.DB) {
	t.Helper()
	n, err := db.NewSelect().Model((*memorynodes.MemoryNode)(nil)).
		Where("concept = ?", conceptAccountsAccount).
		Where("id = ?", selfAccountId).
		Count(context.Background())
	if err != nil {
		t.Fatalf("probe the self row: %v", err)
	}
	if n > 0 {
		return
	}
	seed := auth.ContextWithInternalOrigin(systemActorContext(context.Background()))
	if _, err := eng.Execute(seed, fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "My company")`, langparser.QuoteString(selfAccountId))); err != nil {
		t.Fatalf("seed the self row: %v", err)
	}
}

// freshAccountEngine is a second engine over the same database, holding no
// memory of anything the shared engine or this test did.
func freshAccountEngine(t *testing.T, db *bun.DB) *MemQLEngine {
	t.Helper()
	receiver, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := receiver.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	return receiver
}

// accountWrite runs one named account mutation and returns its error.
func accountWrite(ctx context.Context, eng *MemQLEngine, name string, args map[string]any) error {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v, err := json.Marshal(args[k])
		if err != nil {
			return err
		}
		parts = append(parts, k+": "+string(v))
	}
	_, err := eng.Execute(ctx, "mutation "+name+"("+strings.Join(parts, ", ")+")")
	return err
}

// The operator's own legacy shape: the company domain, verified, and the
// default name derived from it -- which on this install is the cluster.
func legacySelfFields() map[string]any {
	return map[string]any{
		"domain":           selfAddressCompanyDomain,
		"domainStatus":     "verified",
		"domainVerifiedAt": "2026-10-14T17:45:10Z",
		"memqlDomain":      selfAddressClusterDomain,
		"memqlReservedAt":  "2026-10-14T17:45:10Z",
	}
}

func selfAddressOwner(t *testing.T, eng *MemQLEngine) context.Context {
	t.Helper()
	ownerID := "owner-" + uniqueSuffix("self-address-"+strings.ToLower(t.Name()))
	seedPrincipal(t, eng, ownerID, auth.RoleOwner)
	return rankActorCtx(ownerID, auth.RoleOwner)
}

// THE REPORTED DEFECT. The Accounts profile edit sends only the fields it
// changed -- never memqlDomain -- so the owner of a stored operator row whose
// name is the cluster's own could not rename the company or touch a note.
func TestTheOperatorProfileEditsOverAStoredClusterAddress(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, selfAddressClusterDomain)
	owner := selfAddressOwner(t, eng)
	ensureSelfAccount(t, eng, db)
	storeAccountVersion(t, db, selfAccountId, legacySelfFields())
	if got := storedAccountPayload(t, db, selfAccountId)["memqlDomain"]; got != selfAddressClusterDomain {
		t.Fatalf("precondition: the stored operator row carries memqlDomain %v, want the cluster's own %q", got, selfAddressClusterDomain)
	}

	receiver := freshAccountEngine(t, db)
	// The bare id, as the browser sends it.
	if err := accountWrite(owner, receiver, "updateClientAccount", map[string]any{
		"accountId": "self",
		"name":      "ZNAS LLC",
		"notes":     "edited from the Accounts profile",
	}); err != nil {
		t.Fatalf("the owner's profile edit of the operator organization was refused: %v", err)
	}

	row := storedAccountPayload(t, db, selfAccountId)
	if row["name"] != "ZNAS LLC" || row["notes"] != "edited from the Accounts profile" {
		t.Fatalf("the edit did not land: %#v", row)
	}
	// The repair: the stored row no longer claims a reservation in cluster
	// space, so the next write -- the owner's or the domain walk's -- has
	// nothing to be refused for.
	if row["memqlDomain"] != "" || row["memqlReservedAt"] != "" {
		t.Fatalf("memqlDomain %q / memqlReservedAt %q kept after the edit; the operator row still holds its cluster address as a reservation",
			row["memqlDomain"], row["memqlReservedAt"])
	}
	// And nothing else the owner proved was discarded with it.
	if row["domain"] != selfAddressCompanyDomain || row["domainStatus"] != "verified" {
		t.Fatalf("the edit changed the operator's domain walk: domain %v, domainStatus %v", row["domain"], row["domainStatus"])
	}
}

// The same refusal from the other side, and the one this cluster reproduced:
// an owner TYPES the company domain on the stored operator row, the default
// fill derives `memql.<domain>`, and that is the cluster's own domain.
func TestTheOperatorDomainIsSetOnAStoredOperatorRow(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, selfAddressClusterDomain)
	owner := selfAddressOwner(t, eng)
	ensureSelfAccount(t, eng, db)
	// The row as the current seed leaves it: no domain, no name.
	storeAccountVersion(t, db, selfAccountId, map[string]any{
		"domain": "", "domainStatus": "unverified", "memqlDomain": "", "memqlReservedAt": "",
	})

	receiver := freshAccountEngine(t, db)
	// The Edit-profile form's request: only the changed field.
	request := map[string]any{"accountId": "self", "domain": selfAddressCompanyDomain}
	if err := accountWrite(owner, receiver, "updateClientAccount", request); err != nil {
		t.Fatalf("setting the operator organization's domain was refused: %v", err)
	}
	row := storedAccountPayload(t, db, selfAccountId)
	t.Logf("updateClientAccount %v accepted; stored domain %q, domainStatus %q, memqlDomain %q",
		request, row["domain"], row["domainStatus"], row["memqlDomain"])
	if row["domain"] != selfAddressCompanyDomain {
		t.Fatalf("domain = %v, want %q", row["domain"], selfAddressCompanyDomain)
	}
	if row["memqlDomain"] != "" {
		t.Fatalf("memqlDomain = %v: the operator's cluster address was recorded as a reservation", row["memqlDomain"])
	}
}

// THE GUARD IS KEPT. Without these the two cases above are satisfied by
// deleting the collision guard.
func TestTheClusterAddressIsStillRefusedWhereItIsAReservation(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, selfAddressClusterDomain)
	owner := selfAddressOwner(t, eng)
	ensureSelfAccount(t, eng, db)
	suffix := uniqueSuffix("self-address-client")
	refused := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("admitted, want a domain_under_cluster_domain refusal")
		}
		if !strings.Contains(err.Error(), accountDomainUnderClusterCode) {
			t.Fatalf("refusal does not carry %q: %v", accountDomainUnderClusterCode, err)
		}
	}

	t.Run("a client whose domain derives the cluster's name", func(t *testing.T) {
		refused(t, accountWrite(owner, eng, "createClientAccount", map[string]any{
			"accountId": "acct-" + suffix, "name": "Client", "domain": selfAddressCompanyDomain,
		}))
	})

	t.Run("a client row that already stores a name in cluster space", func(t *testing.T) {
		id := "v1:accounts:account:acct-stored-" + suffix
		if err := accountWrite(owner, eng, "createClientAccount", map[string]any{
			"accountId": id, "name": "Stored client", "domain": "stored-" + suffix + ".example",
		}); err != nil {
			t.Fatalf("seed the client: %v", err)
		}
		storeAccountVersion(t, db, id, map[string]any{"memqlDomain": "acme." + selfAddressClusterDomain})
		refused(t, accountWrite(owner, freshAccountEngine(t, db), "updateClientAccount", map[string]any{
			"accountId": id, "notes": "edited",
		}))
	})

	t.Run("the operator asking for a cluster host by name", func(t *testing.T) {
		refused(t, accountWrite(owner, eng, "updateClientAccount", map[string]any{
			"accountId": "self", "memqlDomain": "os." + selfAddressClusterDomain,
		}))
	})
}
