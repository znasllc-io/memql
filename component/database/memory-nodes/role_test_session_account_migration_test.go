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

// TestRoleTestSessionAccountMigrationRoundTrips runs the session-account
// migration both ways over temporary copies of the two role-test tables, built
// from the original DDL and the custom-roles migration that follows it. Up adds
// the organization a test ran in as a nullable text column with no default: a
// session written before it stays NULL, and what Start writes -- an empty
// string for a global role, an organization's id for an organization role -- is
// admitted. Down removes the column and keeps every session.
func TestRoleTestSessionAccountMigrationRoundTrips(t *testing.T) {
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
	customRoles := read("20261107010000_role_test_custom_roles.up.sql")
	up, down := read("20261108120000_role_test_session_account.up.sql"), read("20261108120000_role_test_session_account.down.sql")

	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	// column describes account_id on the temporary sessions table: its type,
	// whether it may be NULL and whether it has a default, or "" when absent.
	column := func() string {
		t.Helper()
		var out string
		err := tx.QueryRowContext(ctx, `SELECT format_type(atttypid, atttypmod) || CASE WHEN attnotnull THEN ' not null' ELSE ' null' END || CASE WHEN atthasdef THEN ' default' ELSE '' END
			FROM pg_attribute WHERE attrelid = 'pg_temp.identity_role_test_sessions'::regclass AND attname = 'account_id' AND NOT attisdropped`).Scan(&out)
		if err == sql.ErrNoRows {
			return ""
		}
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	session := func(id, role, user string) {
		t.Helper()
		exec(`INSERT INTO identity_role_test_sessions(id, token_hash, operator_id, operator_epoch, user_id, role, expires_at)
			VALUES ($1, $1, 'operator', 0, $3, $2, now() + interval '30 minutes')`, id, role, user)
	}

	exec(original)
	exec(customRoles)
	exec(`INSERT INTO identity_role_test_personas(operator_id, role, user_id) VALUES ('operator', 'reader', 'u-reader'), ('operator', 'site-moderator', 'u-moderator')`)
	session("s-before", "site-moderator", "u-moderator")
	if got := column(); got != "" {
		t.Fatalf("the tables before up must carry no account_id, got %q", got)
	}

	exec(up)
	if got := column(); got != "text null" {
		t.Fatalf("up must add account_id as nullable text with no default, got %q", got)
	}
	// A writer that names no account_id, as every Start before the column did,
	// leaves it NULL; Start names it, '' for a global role.
	session("s-unwritten", "reader", "u-reader")
	exec(`INSERT INTO identity_role_test_sessions(id, token_hash, operator_id, operator_epoch, user_id, role, expires_at, account_id) VALUES
		('s-global', 's-global', 'operator', 0, 'u-reader', 'reader', now() + interval '30 minutes', ''),
		('s-organization', 's-organization', 'operator', 0, 'u-moderator', 'site-moderator', now() + interval '30 minutes', 'org-acme')`)
	var accounts string
	if err := tx.QueryRowContext(ctx, `SELECT string_agg(id || '=' || coalesce(quote_literal(account_id), 'NULL'), ',' ORDER BY id) FROM identity_role_test_sessions`).Scan(&accounts); err != nil {
		t.Fatal(err)
	}
	if want := "s-before=NULL,s-global='',s-organization='org-acme',s-unwritten=NULL"; accounts != want {
		t.Fatalf("a session no Start recorded an organization for stays NULL, and Start's values are kept as written:\n got %s\nwant %s", accounts, want)
	}

	exec(down)
	if got := column(); got != "" {
		t.Fatalf("down must remove account_id, got %q", got)
	}
	var sessions string
	if err := tx.QueryRowContext(ctx, `SELECT string_agg(id, ',' ORDER BY id) FROM identity_role_test_sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != "s-before,s-global,s-organization,s-unwritten" {
		t.Fatalf("down keeps every session, got %q", sessions)
	}
}
