package storagequota

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOwnerIdentityRepairPreservesQuotaAcrossReplicas(t *testing.T) {
	s, another := fixture(t)
	ctx := context.Background()
	_, err := s.DB().Exec(`
INSERT INTO storage_capacity_objects(object_key,account,owner_id,used_bytes,capacity_bytes,planned_bytes) VALUES
 ('canonical','account','v1:identity:user:member',2000,3000,1976),
 ('bare','account','member',4000,5000,3976);
INSERT INTO storage_capacity_totals(scope,used_bytes,reserved_bytes) VALUES
 ('cluster',6000,2000),('user:v1:identity:user:member',2000,1000),('user:member',4000,1000);
INSERT INTO storage_capacity_users(owner_id,limit_bytes,changed_by,updated_at) VALUES
 ('v1:identity:user:member',12000,'owner','2026-10-15T10:00:00Z'),
 ('member',11000,'owner','2026-10-15T11:00:00Z');`)
	must(t, err)
	migration, err := os.ReadFile(filepath.Join("..", "memory-nodes", "migrations", "20261015190000_storage_owner_identity.up.sql"))
	must(t, err)
	for range 2 { // rerunning must not double-charge or discard reservations
		tx, err := s.DB().BeginTx(ctx, nil)
		must(t, err)
		if _, err = tx.ExecContext(ctx, string(migration)); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		must(t, tx.Commit())
	}
	for _, id := range []string{"member", "v1:identity:user:member"} {
		u, err := another().Usage(ctx, id)
		must(t, err)
		if u.Used != 6000 || u.Reserved != 2000 || u.ClusterUsed != 6000 || u.ClusterReserved != 2000 || u.Limit != 11000 {
			t.Fatalf("%s usage after repair: %+v", id, u)
		}
	}
	// A different replica can continue an existing canonical-key object using
	// a bare request, with precisely the existing capacity still charged.
	g := guard(t, another(), "canonical", "member")
	must(t, g.Plan(ctx, 1976))
	g.Close()
	// New writes and allowance changes also converge without a migration.
	must(t, another().SetLimit(ctx, "v1:identity:user:owner", "v1:identity:user:member", 15000))
	g = guard(t, another(), "new", "v1:identity:user:member")
	must(t, g.Plan(ctx, 1000))
	g.Close()
	u, err := s.Usage(ctx, "member")
	must(t, err)
	if u.Limit != 15000 || u.Used != 6000 || u.Reserved != 2000+1000+ObjectOverhead {
		t.Fatalf("replica writes split owner accounting: %+v", u)
	}
}

func TestOwnerIdentityInventoryUsesOneQuota(t *testing.T) {
	s, _ := fixture(t)
	ctx := context.Background()
	must(t, s.Initialize(ctx, "second-account", func(add func(string, string, int64) error) error {
		if err := add("one", "v1:identity:user:member", 100); err != nil {
			return err
		}
		return add("two", "member", 200)
	}))
	u, err := s.Usage(ctx, "member")
	must(t, err)
	if u.Used != 300+2*ObjectOverhead {
		t.Fatalf("inventory lost canonical owner usage: %+v", u)
	}
}
