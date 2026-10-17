package workbench

// build_quota_hop_test.go -- an oversized build output, stored by the
// workbench node under the REAL storage quota.
//
// The hop tests beside this one carry the output through an in-memory store,
// which has no quota, so they could not see this failure: the workbench node
// acts for the engine (a SYSTEM-class forwarded authority named for a node),
// the quota took that name for a person's id, found no user row, and refused
// the write -- "cannot resolve storage owner" -- after a perfectly good build.
// A Fylo storefront (38 MiB of public/ assets) could not deploy.
//
// Gated on a throwaway Azurite and a throwaway Postgres, and skips -- naming
// both variables -- without them:
//
//	MEMQL_AZURITE_TEST_CONNECTION_STRING  a connection string for an Azurite
//	MEMQL_STORAGE_TEST_DSN                a PostgreSQL DSN (a schema is created)

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

func TestAnOversizedBuildOutputIsStoredUnderTheRealQuota(t *testing.T) {
	conn, dsn := os.Getenv("MEMQL_AZURITE_TEST_CONNECTION_STRING"), os.Getenv("MEMQL_STORAGE_TEST_DSN")
	if conn == "" || dsn == "" {
		t.Skip("MEMQL_AZURITE_TEST_CONNECTION_STRING and MEMQL_STORAGE_TEST_DSN are both needed to run this against the real quota")
	}
	t.Setenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING", conn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// The real capacity ledger, in a schema of its own, with NO user rows.
	admin := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { admin.Close() })
	schema := fmt.Sprintf("buildquota_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	open := func() *sql.DB {
		db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn), pgdriver.WithConnParams(map[string]any{"search_path": schema})))
		t.Cleanup(func() { db.Close() })
		return db
	}
	db, gate := open(), open()
	migration, err := os.ReadFile(filepath.Join("..", "..", "component", "database", "memory-nodes", "migrations", "20261015010000_storage_capacity.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE "MemoryNodes"(id text,concept text,payload jsonb,"createdAt" timestamptz DEFAULT clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	azureblob.ConfigureCapacity(func() *sql.DB { return db }, func() *sql.DB { return gate })

	// The production uploader: every constructor of it meters its writes.
	store, err := azureblob.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const container = "buildquotatest"
	if err := store.EnsureContainer(ctx, container); err != nil {
		t.Fatal(err)
	}

	workbenchSide, _ := buildIntegration(t)
	workbenchSide.SetBuildBlobStore(store, container)
	mesh := newBuildMesh(t, workbenchSide)
	origin := NewIntegration(buildLogger())
	origin.remote = true
	origin.SetBuildForwarder(mesh)
	origin.SetBuildBlobStore(store, container)

	req := buildRequest(t, "mkdir -p dist && head -c 3000000 /dev/urandom > dist/big.bin && printf 'hello' > dist/index.html", webFixture())
	// Smaller than the 3 MB output (random bytes do not compress), so the
	// output must go through object storage, as a real storefront's does.
	req.Limits.MaxInlineBytes = 1 << 20

	res := origin.RunBuild(ctx, req, "")
	if !res.OK {
		t.Fatalf("an oversized output must be stored under the quota: %s: %s\n%s", res.ErrorCode, res.ErrorMessage, res.LogTail)
	}
	if got := unpack(t, res.Output.Inline)["dist/index.html"]; got != "hello" {
		t.Fatalf("the built bytes must come back whole, got %q", got)
	}
}
