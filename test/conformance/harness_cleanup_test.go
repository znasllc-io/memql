package conformance

import (
	"context"
	"os"
	"testing"

	"github.com/uptrace/bun"
)

func TestHarnessReleasesDatabaseAfterEachTest(t *testing.T) {
	var db *bun.DB
	t.Run("owns a database lifecycle", func(t *testing.T) {
		var reachable bool
		db, reachable = tryDB(t)
		if !reachable {
			if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
				t.Fatal("conformance cleanup requires a reachable database")
			}
			t.Skip("conformance cleanup requires Postgres")
		}
		if err := db.PingContext(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	if db == nil {
		t.Skip("conformance cleanup requires Postgres")
	}
	if err := db.PingContext(context.Background()); err == nil {
		t.Fatal("the completed test still owns an open database pool")
	}
	if stats := db.Stats(); stats.OpenConnections != 0 {
		t.Fatalf("the completed test retained %d connections", stats.OpenConnections)
	}
}
