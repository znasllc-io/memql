package campaigns

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

// Each fixture owns its identity. Leaving a cluster owner behind would make
// the next fixture an invalid second owner in the same test database.
func cleanupCampaignUser(t *testing.T, user string) {
	t.Helper()
	if user == "" {
		t.Fatal("fixture user must have an id")
	}
	dsn := dbtest.DSN()
	t.Cleanup(func() {
		db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
		defer db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept='v1:identity:user' AND id IN ($1,$2)`, user, "v1:identity:user:"+user); err != nil {
			t.Errorf("remove fixture user %s: %v", user, err)
		}
	})
}

// Client fixtures start after the primary owner has assigned their apps.
func campaignAccountApps(t *testing.T, db *sql.DB, account string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `UPDATE "MemoryNodes" SET payload=jsonb_set(payload,'{allowedApps}','["app:email","app:deployables"]'::jsonb) WHERE concept='v1:accounts:account' AND id=$1`, "v1:accounts:account:"+account); err != nil {
		t.Fatal(err)
	}
}
