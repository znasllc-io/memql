package database

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
)

func TestServingStatementTimeoutCancelsBeforeSocketDeadline(t *testing.T) {
	dsn := os.Getenv("MEMQL_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires Postgres")
	}
	if time.Duration(defaultStatementTimeoutMs)*time.Millisecond >= pgdriver.NewConnector().Config().ReadTimeout {
		t.Fatal("server must cancel before the driver abandons the query")
	}
	t.Setenv(envDBStatementTimeoutMs, "150")
	db, err := pgSQLOpener(sessionConnParams)("pg", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	var before, active int
	if err := db.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, "SELECT pg_sleep(2)")
	var pgerr pgdriver.Error
	if !errors.As(err, &pgerr) || pgerr.Field('C') != "57014" {
		t.Fatalf("want server cancellation, got %v", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM pg_stat_activity WHERE pid = $1 AND state = 'active' AND query = 'SELECT pg_sleep(2)'", before).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatal("cancelled statement is still running on its former backend")
	}
}
