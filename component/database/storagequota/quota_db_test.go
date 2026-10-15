package storagequota

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
)

// This suite needs only PostgreSQL, not engine extensions. Each test owns a
// private schema, never truncates a shared table, and opens independent pools.
func fixture(t *testing.T) (*Store, func() *Store) {
	t.Helper()
	dsn := os.Getenv("MEMQL_STORAGE_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("MEMQL_DATABASE_DSN")
	}
	if dsn == "" {
		if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
			t.Fatal("storage tests require a PostgreSQL DSN")
		}
		t.Skip("PostgreSQL DSN is not configured")
	}
	admin := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { admin.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	schema := fmt.Sprintf("quota_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	open := func() *sql.DB {
		db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn), pgdriver.WithConnParams(map[string]any{"search_path": schema})))
		t.Cleanup(func() { db.Close() })
		return db
	}
	newStore := func() *Store {
		db, gate := open(), open()
		return &Store{DB: func() *sql.DB { return db }, GateDB: func() *sql.DB { return gate }}
	}
	s := newStore()
	migration, err := os.ReadFile(filepath.Join("..", "memory-nodes", "migrations", "20261015010000_storage_capacity.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().Exec(`CREATE TABLE "MemoryNodes"(id text,concept text,payload jsonb,"createdAt" timestamptz DEFAULT clock_timestamp()); INSERT INTO "MemoryNodes"(id,concept,payload) VALUES ('member','v1:identity:user','{"role":"writer"}'),('owner','v1:identity:user','{"role":"owner"}'),('dev','v1:identity:user','{"role":"developer"}')`); err != nil {
		t.Fatal(err)
	}
	if err = s.Initialize(context.Background(), "account", func(func(string, string, int64) error) error { return nil }); err != nil {
		t.Fatal(err)
	}
	return s, newStore
}
func guard(t *testing.T, s *Store, key, owner string) *Guard {
	t.Helper()
	g, e := s.Guard(context.Background(), "account", key, owner)
	if e != nil {
		t.Fatal(e)
	}
	return g
}
func must(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}
func TestStorageQuotaConcurrentReplicasAndRestart(t *testing.T) {
	s, another := fixture(t)
	ctx := context.Background()
	must(t, s.SetLimit(ctx, "owner", "member", 10000))
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			g, e := another().Guard(ctx, "account", fmt.Sprint(i), "member")
			if e == nil {
				defer g.Close()
				e = g.Plan(ctx, 6000)
			}
			results <- e
		}(i)
	}
	wg.Wait()
	close(results)
	accepted, refused := 0, 0
	for e := range results {
		if e == nil {
			accepted++
		} else if errors.Is(e, ErrLimit) {
			refused++
		} else {
			t.Fatal(e)
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatalf("accepted=%d refused=%d", accepted, refused)
	}
	u, e := another().Usage(ctx, "member")
	must(t, e)
	if u.Reserved != 7024 || u.Available != 2976 {
		t.Fatalf("reservation did not survive fresh replica: %+v", u)
	}
	// Changing roles is observed from persisted identity, not a stale token.
	_, e = s.DB().Exec(`UPDATE "MemoryNodes" SET payload='{"role":"developer"}' WHERE id='member'`)
	must(t, e)
	u, e = s.Usage(ctx, "member")
	must(t, e)
	if !u.Exempt || u.Limit != ClusterBytes {
		t.Fatalf("role: %+v", u)
	}
}
func TestStorageQuotaVirtualClusterCeilingIncludesAdmins(t *testing.T) {
	s, _ := fixture(t)
	ctx := context.Background()
	g := guard(t, s, "large", "owner")
	must(t, g.Plan(ctx, ClusterBytes-ObjectOverhead-4))
	g.Close()
	g = guard(t, s, "next", "dev")
	defer g.Close()
	if e := g.Plan(ctx, 1); !errors.Is(e, ErrLimit) {
		t.Fatalf("cluster allowed admin over limit: %v", e)
	}
	u, e := s.Usage(ctx, "dev")
	must(t, e)
	if u.Available != 4 {
		t.Fatalf("available=%d", u.Available)
	}
}
func TestStorageQuotaStagesOverlapAndRetirement(t *testing.T) {
	s, another := fixture(t)
	ctx := context.Background()
	must(t, s.SetLimit(ctx, "dev", "member", 3000))
	g := guard(t, s, "file", "member")
	must(t, g.Plan(ctx, 1000))
	must(t, g.Stage(ctx, "a", 400))
	must(t, g.BlocksCommitted(ctx, []string{"a"}, false))
	g.Close()
	g = guard(t, another(), "file", "member")
	defer g.Close()
	must(t, g.Stage(ctx, "b", 600))
	must(t, g.BlocksCommitted(ctx, []string{"a", "b"}, true))
	u, e := s.Usage(ctx, "member")
	must(t, e)
	if u.Used != 2024 || u.Reserved != 0 {
		t.Fatalf("committed: %+v", u)
	}
	// Replacing a block must reserve physical overlap, even at the same ID.
	if e = g.Stage(ctx, "a", 1000); !errors.Is(e, ErrLimit) {
		t.Fatalf("overlap accepted: %v", e)
	}
	must(t, s.SetLimit(ctx, "owner", "member", 100))
	u, e = s.Usage(ctx, "member")
	must(t, e)
	if u.Available != 0 || u.Used != 2024 {
		t.Fatalf("lowering quota lost data: %+v", u)
	}
	// Bin is a row state: accounting stays unchanged until a fenced provider
	// retirement confirms no old grant can resurrect bytes.
	must(t, g.Retired(ctx))
	if e = g.Plan(ctx, 1); !errors.Is(e, ErrRetired) {
		t.Fatalf("retired destination reused: %v", e)
	}
	u, e = s.Usage(ctx, "member")
	must(t, e)
	if u.Used != ObjectOverhead {
		t.Fatalf("tombstone accounting: %+v", u)
	}
}
func TestStorageQuotaInventoryIsAtomicAndOwnerBound(t *testing.T) {
	s, _ := fixture(t)
	ctx := context.Background()
	failure := errors.New("inventory interrupted")
	e := s.Initialize(ctx, "other", func(add func(string, string, int64) error) error {
		if e := add("old", "member", 7); e != nil {
			return e
		}
		return failure
	})
	if !errors.Is(e, failure) {
		t.Fatal(e)
	}
	var n int
	must(t, s.DB().QueryRow(`SELECT count(*) FROM storage_capacity_objects WHERE object_key='old'`).Scan(&n))
	if n != 0 {
		t.Fatal("partial inventory admitted")
	}
	g := guard(t, s, "same", "member")
	must(t, g.Plan(ctx, 10))
	g.Close()
	g = guard(t, s, "same", "dev")
	defer g.Close()
	if e = g.Plan(ctx, 10); e == nil {
		t.Fatal("cross-owner destination allowed")
	}
	if e = s.SetLimit(ctx, "member", "owner", 200); e == nil {
		t.Fatal("member changed quota")
	}
}

func TestLegacyStagedInventoryResumesWithoutDoubleCharging(t *testing.T) {
	s, _ := fixture(t)
	ctx := context.Background()
	g := guard(t, s, "legacy", "member")
	defer g.Close()
	must(t, g.Plan(ctx, 10000))
	must(t, g.Settled(ctx, 10000, false))
	if _, err := g.BlockListSize(ctx, []string{"a", "b"}); !errors.Is(err, ErrUnreservedBlock) {
		t.Fatal("expected a pre-ledger catalog")
	}
	must(t, g.ImportBlocks(ctx, map[string]int64{"a": 4000}, map[string]int64{"b": 6000}))
	u, err := s.Usage(ctx, "member")
	must(t, err)
	if u.Used != 4000+ObjectOverhead || u.Reserved != 6000 {
		t.Fatalf("wrong adopted footprint: %+v", u)
	}
	must(t, g.Stage(ctx, "c", 2000))
	u, err = s.Usage(ctx, "member")
	must(t, err)
	if u.Used+u.Reserved != 12000+ObjectOverhead {
		t.Fatalf("staging was charged twice: %+v", u)
	}
	must(t, g.BlocksCommitted(ctx, []string{"a", "b", "c"}, true))
	u, err = s.Usage(ctx, "member")
	must(t, err)
	if u.Used != 12000+ObjectOverhead || u.Reserved != 0 {
		t.Fatal("commit did not settle inventory", u)
	}
}
