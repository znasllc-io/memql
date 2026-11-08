package memoryNodes

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
)

// TestRoleTestCustomRolesMigrationRoundTrips runs the custom-roles migration
// both ways over temporary copies of the two role-test tables, built from the
// original DDL itself. Up admits any non-empty role slug; down puts back the
// exact four-role CHECK and does not fail on the rows only up allowed: it
// deletes those tests (sessions first, then their personas) and keeps the rest.
func TestRoleTestCustomRolesMigrationRoundTrips(t *testing.T) {
	dsn := os.Getenv("MEMQL_DATABASE_DSN")
	if dsn == "" {
		if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
			t.Fatal("MEMQL_DATABASE_DSN is required")
		}
		t.Skip("requires Postgres")
	}
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	original := strings.ReplaceAll(read("20261015220000_role_test_sessions.up.sql"), "CREATE TABLE", "CREATE TEMP TABLE")
	up, down := read("20261107010000_role_test_custom_roles.up.sql"), read("20261107010000_role_test_custom_roles.down.sql")

	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	exec := func(q string, args ...any) error {
		_, err := tx.ExecContext(ctx, q, args...)
		return err
	}
	// attempt runs one statement that may be refused, keeping it when it lands
	// and the transaction the case lives in when it does not.
	attempt := func(q string, args ...any) error {
		t.Helper()
		if err := exec(`SAVEPOINT attempt`); err != nil {
			t.Fatal(err)
		}
		err := exec(q, args...)
		settle := `RELEASE SAVEPOINT attempt`
		if err != nil {
			settle = `ROLLBACK TO SAVEPOINT attempt`
		}
		if serr := exec(settle); serr != nil {
			t.Fatal(serr)
		}
		return err
	}
	checks := func() string {
		t.Helper()
		rows, err := tx.QueryContext(ctx, `SELECT conrelid::regclass::text || ' ' || pg_get_constraintdef(oid) FROM pg_constraint
			WHERE contype = 'c' AND conrelid IN ('pg_temp.identity_role_test_personas'::regclass, 'pg_temp.identity_role_test_sessions'::regclass)
			ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return strings.Join(out, "\n")
	}
	persona := func(operator, role, user string) error {
		return attempt(`INSERT INTO identity_role_test_personas(operator_id, role, user_id) VALUES ($1, $2, $3)`, operator, role, user)
	}
	session := func(id, role, user string) error {
		return attempt(`INSERT INTO identity_role_test_sessions(id, token_hash, operator_id, operator_epoch, user_id, role, expires_at)
			VALUES ($1, $1, 'operator', 0, $3, $2, now() + interval '30 minutes')`, id, role, user)
	}

	if err := exec(original); err != nil {
		t.Fatal(err)
	}
	before := checks()
	if !strings.Contains(before, "'reader'") || persona("operator", "site-moderator", "u-custom") == nil {
		t.Fatalf("the original tables must refuse a custom role:\n%s", before)
	}

	if err := exec(up); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"", "   "} {
		if persona("operator", role, "u-empty") == nil {
			t.Fatalf("up must still refuse an empty role %q", role)
		}
	}
	for _, f := range []struct{ role, user string }{{"reader", "u-reader"}, {"site-moderator", "u-custom"}} {
		if err := persona("operator", f.role, f.user); err != nil {
			t.Fatalf("up must admit %s: %v", f.role, err)
		}
		if err := session("s-"+f.user, f.role, f.user); err != nil {
			t.Fatalf("up must admit a %s session: %v", f.role, err)
		}
	}

	if err := exec(down); err != nil {
		t.Fatalf("down must not fail on rows up allowed: %v", err)
	}
	if after := checks(); after != before {
		t.Fatalf("down must restore the original CHECK constraints\nbefore:\n%s\nafter:\n%s", before, after)
	}
	var personas, sessions string
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(string_agg(role, ',' ORDER BY role), '') FROM identity_role_test_personas`).Scan(&personas); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(string_agg(role, ',' ORDER BY role), '') FROM identity_role_test_sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if personas != "reader" || sessions != "reader" {
		t.Fatalf("down keeps the predefined tests and deletes the custom ones: personas=%q sessions=%q", personas, sessions)
	}
	if persona("operator", "site-moderator", "u-again") == nil {
		t.Fatal("after down a custom role is refused again")
	}
}
