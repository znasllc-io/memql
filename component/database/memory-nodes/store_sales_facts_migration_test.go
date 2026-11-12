package memoryNodes

import (
	"context"
	"database/sql"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
)

const (
	storeSalesUpFile   = "20261112110100_store_sales_facts.up.sql"
	storeSalesDownFile = "20261112110100_store_sales_facts.down.sql"
)

var storeSalesTables = []string{
	"store_sales_check",
	"store_sales_order_fact",
	"store_sales_line_fact",
	"store_sales_refund_fact",
	"store_sales_refund_line_fact",
	"store_sales_store",
	"store_sales_watermark",
}

var storeSalesIndexes = []string{
	"store_sales_line_fact_order_idx",
	"store_sales_order_fact_buyer_idx",
	"store_sales_order_fact_ordered_idx",
	"store_sales_refund_fact_order_idx",
	"store_sales_refund_fact_refunded_idx",
	"store_sales_refund_line_fact_line_idx",
}

// storeSalesPrimaryKeys is every table's primary key columns, in key order.
var storeSalesPrimaryKeys = map[string]string{
	"store_sales_order_fact":       "store_id,order_gid",
	"store_sales_line_fact":        "store_id,line_gid",
	"store_sales_refund_fact":      "store_id,refund_gid",
	"store_sales_refund_line_fact": "store_id,refund_gid,position",
	"store_sales_store":            "store_id",
	"store_sales_watermark":        "store_id,source",
	"store_sales_check":            "store_id",
}

func readStoreSalesFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("migrations/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// storeSalesStatements splits a migration file the way the runner does, on the
// bun split marker.
func storeSalesStatements(file string) []string {
	var out []string
	for _, s := range strings.Split(file, "--bun:split") {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// storeSalesTx opens a transaction the test rolls back, and returns it with an
// exec helper and a runner for a migration file over TEMP copies of the tables.
type storeSalesTx struct {
	t   *testing.T
	ctx context.Context
	tx  *sql.Tx
}

func newStoreSalesTx(t *testing.T) *storeSalesTx {
	t.Helper()
	dsn := os.Getenv("MEMQL_DATABASE_DSN")
	if dsn == "" {
		if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
			t.Fatal("MEMQL_DATABASE_DSN is required")
		}
		t.Skip("requires Postgres")
	}
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	return &storeSalesTx{t: t, ctx: ctx, tx: tx}
}

func (s *storeSalesTx) exec(q string, args ...any) {
	s.t.Helper()
	if _, err := s.tx.ExecContext(s.ctx, q, args...); err != nil {
		s.t.Fatalf("%v\n%s", err, q)
	}
}

// run applies every statement of a migration file, with each CREATE TABLE
// rewritten to CREATE TEMP TABLE so nothing outside the transaction is touched.
func (s *storeSalesTx) run(file string) {
	s.t.Helper()
	for _, st := range storeSalesStatements(file) {
		s.exec(strings.ReplaceAll(st, "CREATE TABLE", "CREATE TEMP TABLE"))
	}
}

func (s *storeSalesTx) strings(q string) []string {
	s.t.Helper()
	rows, err := s.tx.QueryContext(s.ctx, q)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			s.t.Fatal(err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		s.t.Fatal(err)
	}
	return out
}

// tables names the store_sales_* tables in this session's temporary schema.
func (s *storeSalesTx) tables() []string {
	return s.strings(`SELECT c.relname FROM pg_class c WHERE c.relnamespace = pg_my_temp_schema() AND c.relkind = 'r' AND c.relname LIKE 'store\_sales\_%' ORDER BY 1`)
}

// indexes names the non-primary-key indexes of the store_sales_* tables.
func (s *storeSalesTx) indexes() []string {
	return s.strings(`SELECT i.relname FROM pg_index x JOIN pg_class i ON i.oid = x.indexrelid JOIN pg_class t ON t.oid = x.indrelid
		WHERE t.relnamespace = pg_my_temp_schema() AND t.relname LIKE 'store\_sales\_%' AND NOT x.indisprimary ORDER BY 1`)
}

// primaryKey reads a table's primary key columns, in key order, from the catalog.
func (s *storeSalesTx) primaryKey(table string) string {
	s.t.Helper()
	got := s.strings(`SELECT string_agg(a.attname, ',' ORDER BY k.ord)
		FROM pg_index x
		JOIN pg_class t ON t.oid = x.indrelid
		CROSS JOIN LATERAL unnest(x.indkey) WITH ORDINALITY AS k(attnum, ord)
		JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = k.attnum
		WHERE x.indisprimary AND t.relnamespace = pg_my_temp_schema() AND t.relname = '` + table + `'`)
	if len(got) != 1 {
		return ""
	}
	return got[0]
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func (s *storeSalesTx) wantShape(step string) {
	s.t.Helper()
	if got := s.tables(); !reflect.DeepEqual(got, sorted(storeSalesTables)) {
		s.t.Fatalf("%s: tables\n got %v\nwant %v", step, got, sorted(storeSalesTables))
	}
	if got := s.indexes(); !reflect.DeepEqual(got, sorted(storeSalesIndexes)) {
		s.t.Fatalf("%s: indexes\n got %v\nwant %v", step, got, sorted(storeSalesIndexes))
	}
	for table, want := range storeSalesPrimaryKeys {
		if got := s.primaryKey(table); got != want {
			s.t.Fatalf("%s: primary key of %s is (%s), want (%s)", step, table, got, want)
		}
	}
}

func (s *storeSalesTx) wantNothing(step string) {
	s.t.Helper()
	if got := s.tables(); len(got) != 0 {
		s.t.Fatalf("%s: tables left behind: %v", step, got)
	}
	if got := s.indexes(); len(got) != 0 {
		s.t.Fatalf("%s: indexes left behind: %v", step, got)
	}
}

// TestStoreSalesFactsMigrationRoundTrips runs up, down, up over TEMP copies in
// one transaction it rolls back: after the first and the third step the seven
// tables, their primary keys and the six named indexes exist, and after the
// second none does.
func TestStoreSalesFactsMigrationRoundTrips(t *testing.T) {
	up, down := readStoreSalesFile(t, storeSalesUpFile), readStoreSalesFile(t, storeSalesDownFile)
	s := newStoreSalesTx(t)
	s.wantNothing("before up")
	s.run(up)
	s.wantShape("after up")
	s.run(down)
	s.wantNothing("after down")
	s.run(up)
	s.wantShape("after the second up")
}

// TestStoreSalesMoneyColumnsAddExactly pins that money is NUMERIC: two
// fractions that a float cannot add exactly add exactly, and a wide figure
// reads back digit for digit.
func TestStoreSalesMoneyColumnsAddExactly(t *testing.T) {
	s := newStoreSalesTx(t)
	s.run(readStoreSalesFile(t, storeSalesUpFile))
	insert := func(gid, shipping string) {
		s.exec(`INSERT INTO store_sales_order_fact (store_id, order_gid, order_version, recorded_at, ordered_at, currency,
				cancelled, test, taxes_included, shipping, tax, duties, expected_quantity)
			VALUES ('store', $1, now(), now(), now(), 'USD', false, false, false, $2::numeric, 0, 0, 0)`, gid, shipping)
	}
	insert("order-a", "0.1")
	insert("order-b", "0.2")
	if got := s.strings(`SELECT SUM(shipping)::text FROM store_sales_order_fact`); len(got) != 1 || got[0] != "0.3" {
		t.Fatalf("SUM(shipping) = %v, want 0.3", got)
	}
	insert("order-c", "1234567890123.456789")
	if got := s.strings(`SELECT shipping::text FROM store_sales_order_fact WHERE order_gid = 'order-c'`); len(got) != 1 || got[0] != "1234567890123.456789" {
		t.Fatalf("shipping read back %v, want 1234567890123.456789", got)
	}
}

// TestStoreSalesFactsMigrationUsesNothingOfTimescale holds "on Postgres and
// TimescaleDB" by the text: the lane's database is TimescaleDB, and neither
// file uses anything of it.
func TestStoreSalesFactsMigrationUsesNothingOfTimescale(t *testing.T) {
	for _, name := range []string{storeSalesUpFile, storeSalesDownFile} {
		text := strings.ToLower(readStoreSalesFile(t, name))
		for _, banned := range []string{"create_hypertable", "timescaledb", "continuous aggregate", "continuous_aggregate"} {
			if strings.Contains(text, banned) {
				t.Errorf("%s names %q", name, banned)
			}
		}
	}
}

// TestEveryStoreSalesIndexNamesItsReader pins that every CREATE INDEX line of
// the up file sits directly under a line that begins "-- read by:".
func TestEveryStoreSalesIndexNamesItsReader(t *testing.T) {
	lines := strings.Split(readStoreSalesFile(t, storeSalesUpFile), "\n")
	found := 0
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "CREATE INDEX") {
			continue
		}
		found++
		if i == 0 || !strings.HasPrefix(lines[i-1], "-- read by:") {
			t.Errorf("line %d: %q does not follow a line that begins \"-- read by:\"", i+1, line)
		}
	}
	if found != len(storeSalesIndexes) {
		t.Errorf("the up file creates %d indexes, want %d", found, len(storeSalesIndexes))
	}
}

// TestALineFactIsFoundByItsStoreAndItsLineAlone pins that a line fact is keyed
// by its store and its line, not by its order: a tombstone may carry no parent
// and must still find the fact it retires.
func TestALineFactIsFoundByItsStoreAndItsLineAlone(t *testing.T) {
	s := newStoreSalesTx(t)
	s.run(readStoreSalesFile(t, storeSalesUpFile))
	if got := s.primaryKey("store_sales_line_fact"); got != "store_id,line_gid" {
		t.Fatalf("the line fact's primary key is (%s), want (store_id,line_gid)", got)
	}
}
