package memql

import (
	"context"
	"testing"
)

// The boot repair for a stored operator row nobody edits (memql#5792).
//
// Run on a FRESH engine over the shared database, so what it repairs is the
// stored version and nothing the writing engine remembers -- the shape of a
// replica booting into a database an older binary seeded. Postgres-gated like
// account_self_cluster_address_db_test.go, whose fixtures it shares.
func TestTheBootRepairClearsAStoredClusterAddressOnce(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, selfAddressClusterDomain)
	ensureSelfAccount(t, eng, db)
	storeAccountVersion(t, db, selfAccountId, legacySelfFields())

	m := NewSeedMaterializer(freshAccountEngine(t, db), nil)
	repaired, err := m.repairSelfAccountReservation(context.Background())
	if err != nil {
		t.Fatalf("the repair failed: %v", err)
	}
	if !repaired {
		t.Fatal("the repair reported nothing to do over a stored cluster-space name")
	}
	row := storedAccountPayload(t, db, selfAccountId)
	if row["memqlDomain"] != "" || row["memqlReservedAt"] != "" {
		t.Fatalf("after the repair memqlDomain %q / memqlReservedAt %q", row["memqlDomain"], row["memqlReservedAt"])
	}
	if row["domain"] != selfAddressCompanyDomain || row["domainStatus"] != "verified" {
		t.Fatalf("the repair touched the operator's domain walk: domain %v, domainStatus %v", row["domain"], row["domainStatus"])
	}

	// Every later boot, on any replica, is a read and no write.
	again, err := NewSeedMaterializer(freshAccountEngine(t, db), nil).repairSelfAccountReservation(context.Background())
	if err != nil || again {
		t.Fatalf("second boot: repaired=%v err=%v, want a no-op", again, err)
	}
}

// THE CONTROL: a row whose name is the operator's own real reservation is not
// the repair's business.
func TestTheBootRepairLeavesARealOperatorReservation(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, selfAddressClusterDomain)
	ensureSelfAccount(t, eng, db)
	storeAccountVersion(t, db, selfAccountId, map[string]any{"memqlDomain": "workspace." + selfAddressCompanyDomain + ".test"})

	repaired, err := NewSeedMaterializer(freshAccountEngine(t, db), nil).repairSelfAccountReservation(context.Background())
	if err != nil || repaired {
		t.Fatalf("repaired=%v err=%v over a name outside cluster space, want a no-op", repaired, err)
	}
}
