package email

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"testing"
	"time"
)

// Setup and receipt tests have a real primary owner even when maintenance
// creates an account. Each fixture removes only its own identity afterward.
func seedEmailOwner(t *testing.T, engine ConfigWriter, user string) {
	t.Helper()
	dsn := dbtest.DSN()
	t.Cleanup(func() {
		db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
		defer db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept='v1:identity:user' AND id=$1`, "v1:identity:user:"+user); err != nil {
			t.Errorf("remove fixture owner: %v", err)
		}
	})
	q := fmt.Sprintf(`insert("v1:identity:user", id=%q, payload={"displayName":"Email owner","primaryEmail":%q,"role":"owner","active":true})`, user, user+"@example.test")
	if _, err := engine.Execute(emailStateContext(context.Background()), q); err != nil {
		t.Fatal(err)
	}
}
