package memoryNodes

import (
	"context"
	"database/sql"
	"os"
	"reflect"
	"regexp"
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

// storeSalesColumns is every column of every table, in column order, written
// out by hand: name|type|nullability|default, as the catalog reports them. It is
// not derived from the up file, or the test would compare the file with itself.
var storeSalesColumns = map[string][]string{
	"store_sales_order_fact": {
		"store_id|text|not null|",
		"order_gid|text|not null|",
		"order_version|timestamp with time zone|not null|",
		"deleted|boolean|not null|false",
		"recorded_at|timestamp with time zone|not null|",
		"ordered_at|timestamp with time zone|not null|",
		"currency|text|not null|",
		"cancelled|boolean|not null|",
		"test|boolean|not null|",
		"taxes_included|boolean|not null|",
		"buyer_key|text|not null|''::text",
		"wholesale|boolean|not null|false",
		"shipping|numeric|not null|",
		"tax|numeric|not null|",
		"duties|numeric|not null|",
		"expected_quantity|integer|not null|",
		"shopify_current_subtotal|numeric|null|",
		"shopify_total_discounts|numeric|null|",
		"shopify_total_refunded|numeric|null|",
		"gross|numeric|not null|0",
		"discounts|numeric|not null|0",
		"units|integer|not null|0",
		"line_count|integer|not null|0",
		"lines_version|timestamp with time zone|null|",
		"lines_complete|boolean|not null|false",
		"checked_at|timestamp with time zone|null|",
	},
	"store_sales_line_fact": {
		"store_id|text|not null|",
		"line_gid|text|not null|",
		"order_gid|text|not null|''::text",
		"line_version|timestamp with time zone|not null|",
		"deleted|boolean|not null|false",
		"currency|text|not null|''::text",
		"gift_card|boolean|not null|",
		"product_key|text|not null|",
		"title|text|not null|",
		"quantity|integer|not null|",
		"current_quantity|integer|not null|",
		"unit_price|numeric|not null|",
		"allocated_discounts|numeric|not null|",
		"refunded_quantity|integer|not null|0",
		"sold_quantity|integer|not null|0",
		"gross|numeric|not null|0",
		"discounts|numeric|not null|0",
		"checked_at|timestamp with time zone|null|",
	},
	"store_sales_refund_fact": {
		"store_id|text|not null|",
		"refund_gid|text|not null|",
		"order_gid|text|not null|''::text",
		"refund_version|timestamp with time zone|not null|",
		"deleted|boolean|not null|false",
		"recorded_at|timestamp with time zone|not null|",
		"refunded_at|timestamp with time zone|not null|",
		"money_refunded|numeric|not null|",
		"shipping_refunded|numeric|null|",
		"tax_refunded|numeric|null|",
		"duties_refunded|numeric|null|",
		"detail_fetched|boolean|not null|",
		"lines_complete|boolean|not null|",
		"gift_card_refunded|numeric|null|",
		"item_refunds|numeric|null|",
		"units_returned|integer|null|",
		"checked_at|timestamp with time zone|null|",
	},
	"store_sales_refund_line_fact": {
		"store_id|text|not null|",
		"refund_gid|text|not null|",
		"position|integer|not null|",
		"order_gid|text|not null|",
		"line_gid|text|not null|",
		"quantity|integer|not null|",
		"subtotal|numeric|not null|",
		"tax|numeric|not null|",
	},
	"store_sales_store": {
		"store_id|text|not null|",
		"generation|bigint|not null|nextval('store_sales_generation'::regclass)",
		"purging|boolean|not null|false",
		"store_domain|text|not null|''::text",
		"store_name|text|not null|''::text",
		"currency|text|not null|''::text",
		"timezone|text|not null|''::text",
		"taxes_included|boolean|null|",
		"development|boolean|null|",
		"all_orders_readable|boolean|not null|false",
		"history_floor|timestamp with time zone|null|",
		"history_since|timestamp with time zone|null|",
		"wholesale_enabled|boolean|not null|false",
		"wholesale_digest|text|not null|''::text",
		"wholesale_cursor|text|not null|''::text",
		"wholesale_window_seconds|integer|not null|86400",
		"verdict_state|text|not null|''::text",
		"verdict_reasons|jsonb|not null|'[]'::jsonb",
		"verdict_since|timestamp with time zone|null|",
		"verdict_read_at|timestamp with time zone|null|",
		"recording_blocked|boolean|not null|false",
		"last_clear_at|timestamp with time zone|null|",
		"last_change_at|timestamp with time zone|null|",
		"walked_through|timestamp with time zone|null|",
		"rebuilding_since|timestamp with time zone|null|",
		"definition|integer|not null|0",
	},
	"store_sales_watermark": {
		"store_id|text|not null|",
		"source|text|not null|",
		"through|timestamp with time zone|not null|",
		"window_seconds|integer|not null|86400",
		"walked_through|timestamp with time zone|null|",
	},
	"store_sales_check": {
		"store_id|text|not null|",
		"pass_started_at|timestamp with time zone|null|",
		"order_cursor|text|not null|''::text",
		"order_window_seconds|integer|not null|86400",
		"line_cursor|text|not null|''::text",
		"line_window_seconds|integer|not null|86400",
		"refund_cursor|text|not null|''::text",
		"refund_window_seconds|integer|not null|86400",
		"compared|integer|not null|0",
		"mismatched|integer|not null|0",
		"repaired|integer|not null|0",
		"last_started_at|timestamp with time zone|null|",
		"last_finished_at|timestamp with time zone|null|",
		"last_compared|integer|not null|0",
		"last_mismatched|integer|not null|0",
		"last_repaired|integer|not null|0",
		"last_disagreeing|integer|not null|0",
		"clean_at|timestamp with time zone|null|",
	},
}

// storeSalesIndexDefinitions is every non-primary-key index as pg_get_indexdef
// reports it, with the session's temporary schema name removed.
var storeSalesIndexDefinitions = []string{
	"CREATE INDEX store_sales_line_fact_order_idx ON store_sales_line_fact USING btree (store_id, order_gid)",
	"CREATE INDEX store_sales_order_fact_buyer_idx ON store_sales_order_fact USING btree (store_id, buyer_key, ordered_at)",
	"CREATE INDEX store_sales_order_fact_ordered_idx ON store_sales_order_fact USING btree (store_id, ordered_at)",
	"CREATE INDEX store_sales_refund_fact_order_idx ON store_sales_refund_fact USING btree (store_id, order_gid)",
	"CREATE INDEX store_sales_refund_fact_refunded_idx ON store_sales_refund_fact USING btree (store_id, refunded_at)",
	"CREATE INDEX store_sales_refund_line_fact_line_idx ON store_sales_refund_line_fact USING btree (store_id, order_gid, line_gid)",
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

// storeSalesSequences is every sequence the migration creates: the one every
// store's generation is drawn from.
var storeSalesSequences = []string{"store_sales_generation"}

var (
	storeSalesCreateTable    = regexp.MustCompile(`(?m)^CREATE TABLE`)
	storeSalesCreateSequence = regexp.MustCompile(`(?m)^CREATE SEQUENCE`)
	storeSalesCommentLine    = regexp.MustCompile(`(?m)^\s*--.*$`)
)

// run applies every statement of a migration file, with each statement that
// starts CREATE TABLE rewritten to CREATE TEMP TABLE, and CREATE SEQUENCE to
// CREATE TEMP SEQUENCE, so nothing outside the transaction is touched. Only a line that starts the statement is rewritten,
// so comment text is left alone; a CREATE TABLE anywhere else in the code of a
// statement would escape the rewrite, and fails the test instead.
func (s *storeSalesTx) run(file string) {
	s.t.Helper()
	for _, st := range storeSalesStatements(file) {
		temp := storeSalesCreateTable.ReplaceAllString(st, "CREATE TEMP TABLE")
		temp = storeSalesCreateSequence.ReplaceAllString(temp, "CREATE TEMP SEQUENCE")
		code := storeSalesCommentLine.ReplaceAllString(temp, "")
		if strings.Contains(code, "CREATE TABLE") || strings.Contains(code, "CREATE SEQUENCE") {
			s.t.Fatalf("a CREATE TABLE or CREATE SEQUENCE that does not start its line would escape the TEMP rewrite:\n%s", st)
		}
		s.exec(temp)
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

// sequences names the store_sales_* sequences in this session's temporary schema.
func (s *storeSalesTx) sequences() []string {
	return s.strings(`SELECT c.relname FROM pg_class c WHERE c.relnamespace = pg_my_temp_schema() AND c.relkind = 'S' AND c.relname LIKE 'store\_sales\_%' ORDER BY 1`)
}

var storeSalesTempSchema = regexp.MustCompile(`pg_temp(_[0-9]+)?\.`)

// indexes reads the definition of every non-primary-key index of the
// store_sales_* tables, ordered by index name.
func (s *storeSalesTx) indexes() []string {
	defs := s.strings(`SELECT pg_get_indexdef(x.indexrelid) FROM pg_index x JOIN pg_class i ON i.oid = x.indexrelid JOIN pg_class t ON t.oid = x.indrelid
		WHERE t.relnamespace = pg_my_temp_schema() AND t.relname LIKE 'store\_sales\_%' AND NOT x.indisprimary ORDER BY i.relname`)
	for k := range defs {
		defs[k] = storeSalesTempSchema.ReplaceAllString(defs[k], "")
	}
	return defs
}

// columns reads a table's columns in position order as name|type|nullability|default.
func (s *storeSalesTx) columns(table string) []string {
	return s.strings(`SELECT a.attname || '|' || format_type(a.atttypid, a.atttypmod) || '|' || CASE WHEN a.attnotnull THEN 'not null' ELSE 'null' END || '|' || coalesce(pg_get_expr(d.adbin, d.adrelid), '')
		FROM pg_class c JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
		WHERE c.relnamespace = pg_my_temp_schema() AND c.relkind = 'r' AND c.relname = '` + table + `' ORDER BY a.attnum`)
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

func storeSalesSorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func (s *storeSalesTx) wantShape(step string) {
	s.t.Helper()
	if got := s.tables(); !reflect.DeepEqual(got, storeSalesSorted(storeSalesTables)) {
		s.t.Fatalf("%s: tables\n got %v\nwant %v", step, got, storeSalesSorted(storeSalesTables))
	}
	if got := s.sequences(); !reflect.DeepEqual(got, storeSalesSorted(storeSalesSequences)) {
		s.t.Fatalf("%s: sequences\n got %v\nwant %v", step, got, storeSalesSorted(storeSalesSequences))
	}
	if got := s.indexes(); !reflect.DeepEqual(got, storeSalesIndexDefinitions) {
		s.t.Fatalf("%s: index definitions\n got %q\nwant %q", step, got, storeSalesIndexDefinitions)
	}
	for table, want := range storeSalesColumns {
		got := s.columns(table)
		for k := 0; k < len(got) || k < len(want); k++ {
			var g, w string
			if k < len(got) {
				g = got[k]
			}
			if k < len(want) {
				w = want[k]
			}
			if g != w {
				s.t.Fatalf("%s: column %d of %s is %q, want %q", step, k+1, table, g, w)
			}
		}
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
	if got := s.sequences(); len(got) != 0 {
		s.t.Fatalf("%s: sequences left behind: %v", step, got)
	}
}

// TestStoreSalesFactsMigrationRoundTrips runs up, down, up over TEMP copies in
// one transaction it rolls back: after the first and the third step the seven
// tables, their primary keys, the six named indexes and the generation
// sequence exist, and after the second none does.
func TestStoreSalesFactsMigrationRoundTrips(t *testing.T) {
	up, down := readStoreSalesFile(t, storeSalesUpFile), readStoreSalesFile(t, storeSalesDownFile)
	s := newStoreSalesTx(t)
	s.wantNothing("before up")
	s.run(up)
	s.wantShape("after up")
	s.run(up)
	s.wantShape("after up run again on top of itself")
	s.run(down)
	s.wantNothing("after down")
	s.run(up)
	s.wantShape("after the second up")
}

// TestStoreSalesMoneyColumnsAddExactly pins that every NUMERIC column of the
// seven tables is exact: two fractions a float cannot add exactly add exactly,
// and a figure with more digits than a double keeps reads back digit for digit.
// The columns are listed from storeSalesColumns, not typed again.
func TestStoreSalesMoneyColumnsAddExactly(t *testing.T) {
	s := newStoreSalesTx(t)
	s.run(readStoreSalesFile(t, storeSalesUpFile))
	tables := make([]string, 0, len(storeSalesColumns))
	for table := range storeSalesColumns {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	checked := 0
	for _, table := range tables {
		for _, column := range storeSalesNumericColumns(storeSalesColumns[table]) {
			// A row is written with every column that must be given a value
			// (the keys get the row's number, the rest a neutral value of their
			// type) and with the column under test set to $2.
			var names, values []string
			for _, c := range storeSalesColumns[table] {
				f := strings.Split(c, "|")
				name, typ, notNull, def := f[0], f[1], f[2] == "not null", f[3]
				if name == column {
					names, values = append(names, name), append(values, "$2::numeric")
					continue
				}
				if !notNull || def != "" {
					continue
				}
				names = append(names, name)
				switch typ {
				case "text":
					values = append(values, "'k' || $1::text")
				case "integer":
					values = append(values, "$1::int")
				case "boolean":
					values = append(values, "false")
				case "numeric":
					values = append(values, "0")
				case "timestamp with time zone":
					values = append(values, "now()")
				default:
					t.Fatalf("%s.%s: no neutral value for %s", table, name, typ)
				}
			}
			insert := "INSERT INTO " + table + " (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(values, ", ") + ")"
			s.exec("DELETE FROM " + table)
			s.exec(insert, 1, "0.1")
			s.exec(insert, 2, "0.2")
			if got := s.strings("SELECT SUM(" + column + ")::text FROM " + table); len(got) != 1 || got[0] != "0.3" {
				t.Fatalf("%s.%s: SUM = %v, want 0.3", table, column, got)
			}
			s.exec("DELETE FROM " + table)
			s.exec(insert, 1, "1234567890123.456789")
			if got := s.strings("SELECT " + column + "::text FROM " + table); len(got) != 1 || got[0] != "1234567890123.456789" {
				t.Fatalf("%s.%s: read back %v, want 1234567890123.456789", table, column, got)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no NUMERIC column was checked")
	}
}

// storeSalesNumericColumns names the NUMERIC columns of one table's rows.
func storeSalesNumericColumns(rows []string) []string {
	var out []string
	for _, r := range rows {
		if f := strings.Split(r, "|"); f[1] == "numeric" {
			out = append(out, f[0])
		}
	}
	return out
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
	if found != len(storeSalesIndexDefinitions) {
		t.Errorf("the up file creates %d indexes, want %d", found, len(storeSalesIndexDefinitions))
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
