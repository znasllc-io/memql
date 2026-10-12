package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/migrate"
)

// THE RECIPIENT DIGEST BACKFILL, AGAINST A REAL DATABASE (memql#6022, F9).
//
// Each case gets a scratch schema (migrationLockDB) holding a "MemoryNodes" with
// the real table's columns, its primary key and its latest-row index -- a
// hypertable in one-day chunks when TimescaleDB is installed -- and pools whose
// search_path is that schema. The backfill's unqualified names land there and
// never on the shared table, where other packages' tests write recipients: it
// stamps every recipient it finds, so run on the shared table it would append
// versions to rows that are not this test's.

// recipientDigestFixture is one scratch schema.
type recipientDigestFixture struct {
	t      *testing.T
	db     *bun.DB
	admin  *bun.DB
	schema string
	dsn    string
	// base is when the newest seeded version could have been written; the
	// backfill's versions are stamped after it.
	base time.Time
}

func newRecipientDigestFixture(t *testing.T) *recipientDigestFixture {
	t.Helper()
	db, admin, schema := migrationLockDB(t, 10*time.Second)
	f := &recipientDigestFixture{t: t, db: db, admin: admin, schema: schema, dsn: os.Getenv("MEMQL_DATABASE_DSN"), base: time.Now().UTC().Truncate(time.Microsecond)}
	ctx := context.Background()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`CREATE TABLE "MemoryNodes" (
  id          text NOT NULL,
  "createdAt" timestamptz NOT NULL,
  "createdBy" text NOT NULL,
  schema      jsonb NOT NULL,
  payload     jsonb NOT NULL,
  metadata    jsonb NOT NULL DEFAULT '{}'::jsonb,
  type        text NOT NULL DEFAULT 'object',
  concept     text NOT NULL,
  provenance  jsonb NOT NULL DEFAULT '{"kind": "system", "name": "migration-bootstrap"}'::jsonb,
  PRIMARY KEY (id, "createdAt")
)`)
	exec(`CREATE INDEX memory_nodes_concept_id_created_at_desc_idx ON "MemoryNodes" (concept, id, "createdAt" DESC)`)
	var extension sql.NullString
	if err := admin.QueryRowContext(ctx, `SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
 WHERE e.extname = 'timescaledb'`).Scan(&extension); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if extension.Valid {
		exec(`SELECT ` + quoteIdentifier(extension.String) + `.create_hypertable('"MemoryNodes"', 'createdAt', chunk_time_interval => interval '1 day')`)
	}
	return f
}

// replica is another process's pool onto the same scratch schema.
func (f *recipientDigestFixture) replica() *bun.DB {
	f.t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(f.dsn),
		pgdriver.WithConnParams(map[string]any{"search_path": f.schema}),
		pgdriver.WithApplicationName(f.schema), pgdriver.WithReadTimeout(10*time.Second))), pgdialect.New())
	f.t.Cleanup(func() { db.Close() })
	return db
}

// seed writes one version, age before the fixture's base.
func (f *recipientDigestFixture) seed(concept, id string, age time.Duration, payload map[string]any) time.Time {
	f.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	at := f.base.Add(-age)
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", schema, payload, metadata, type, concept, provenance)
		 VALUES (?, ?, 'digest-test', '{"v":1}'::jsonb, ?::jsonb, '{"m":1}'::jsonb, 'node', ?, '{"kind":"direct","name":"digest-test"}'::jsonb)`,
		id, at, string(raw), concept); err != nil {
		f.t.Fatalf("seeding %s: %v", id, err)
	}
	return at
}

// storedVersion is one stored version of one id.
type storedVersion struct {
	At         time.Time
	CreatedBy  string
	Type       string
	Schema     map[string]any
	Metadata   map[string]any
	Payload    map[string]any
	Provenance map[string]any
}

// versions reads every stored version of (concept, id), newest first.
func (f *recipientDigestFixture) versions(concept, id string) []storedVersion {
	f.t.Helper()
	rows, err := f.db.QueryContext(context.Background(),
		`SELECT "createdAt", "createdBy", type, schema, metadata, payload, provenance FROM "MemoryNodes"
		  WHERE concept = ? AND id = ? ORDER BY "createdAt" DESC`, concept, id)
	if err != nil {
		f.t.Fatalf("versions of %s: %v", id, err)
	}
	defer rows.Close()
	var out []storedVersion
	for rows.Next() {
		var v storedVersion
		var schemaRaw, metadataRaw, payloadRaw, provenanceRaw []byte
		if err := rows.Scan(&v.At, &v.CreatedBy, &v.Type, &schemaRaw, &metadataRaw, &payloadRaw, &provenanceRaw); err != nil {
			f.t.Fatal(err)
		}
		for raw, into := range map[*[]byte]*map[string]any{&schemaRaw: &v.Schema, &metadataRaw: &v.Metadata, &payloadRaw: &v.Payload, &provenanceRaw: &v.Provenance} {
			if err := json.Unmarshal(*raw, into); err != nil {
				f.t.Fatal(err)
			}
		}
		out = append(out, v)
	}
	return out
}

func (f *recipientDigestFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// recipientID is the stored id of the recipient with this short id.
func recipientID(short string) string { return recipientEmailDigestConcept + ":" + short }

// seedMany writes n recipients, one version each, with a mailbox apiece and no
// digest: ids "<prefix>-00001" under the recipient concept.
func (f *recipientDigestFixture) seedMany(prefix string, n int) {
	f.t.Helper()
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", schema, payload, metadata, type, concept, provenance)
		 SELECT ? || '-' || lpad(g::text, 5, '0'), ?::timestamptz - interval '1 hour', 'digest-test', '{}'::jsonb,
		        jsonb_build_object('ownerUserId', 'u1', 'audienceId', 'a1', 'email', ' Person' || g || '@Example.TEST ', 'subscriptionStatus', 'subscribed'),
		        '{}'::jsonb, 'node', ?, '{"kind":"direct","name":"digest-test"}'::jsonb
		   FROM generate_series(1, ?) AS g`, recipientID(prefix), f.base, recipientEmailDigestConcept, n); err != nil {
		f.t.Fatal(err)
	}
}

func migrationProvenance() map[string]any {
	return map[string]any{"kind": "migration", "name": recipientEmailDigestMigrationName}
}

// TestRecipientDigestBackfillStampsTheNewestVersionOfEachRecipient: one new
// version per recipient whose NEWEST version has a usable address and no digest,
// the newest version copied whole with the digest added and nothing else
// changed, over several batches; every other row exactly as it was; a second
// run finds nothing to do.
func TestRecipientDigestBackfillStampsTheNewestVersionOfEachRecipient(t *testing.T) {
	f := newRecipientDigestFixture(t)
	const concept = recipientEmailDigestConcept
	person := func(email string) map[string]any {
		return map[string]any{"ownerUserId": "u1", "audienceId": "a1", "email": email, "displayName": "Name",
			"subscriptionStatus": "subscribed", "fields": map[string]any{"Company Name": "Acme"}}
	}
	with := func(p map[string]any, extra map[string]any) map[string]any {
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	const day = 24 * time.Hour
	// Ten recipients, ids in the order the walk takes them.
	rcp := func(n int) string { return recipientID(fmt.Sprintf("rcp-%02d", n)) }
	f.seed(concept, rcp(1), time.Hour, person(" Alpha@Example.TEST "))
	f.seed(concept, rcp(2), 3*day, with(person("old@example.test"), map[string]any{"emailDigest": RecipientEmailDigest("old@example.test")}))
	f.seed(concept, rcp(2), time.Hour, person("beta@example.test")) // the address changed; the newest has no digest
	f.seed(concept, rcp(3), time.Hour, with(person("gamma@example.test"), map[string]any{"emailDigest": RecipientEmailDigest("gamma@example.test")}))
	f.seed(concept, rcp(4), time.Hour, person("no-at-sign"))
	f.seed(concept, rcp(5), time.Hour, map[string]any{"ownerUserId": "u1", "audienceId": "a1", "subscriptionStatus": "subscribed"}) // scrubbed: no address
	f.seed(concept, rcp(6), 2*time.Hour, person("delta@example.test"))                                                              // an older version lacks the digest...
	f.seed(concept, rcp(6), time.Hour, with(person("delta@example.test"), map[string]any{"emailDigest": RecipientEmailDigest("delta@example.test")}))
	f.seed(concept, rcp(7), time.Hour, person("Epsilon+tag@Example.TEST"))
	f.seed(concept, rcp(8), time.Hour, person("ÄBC@x.test"))
	f.seed(concept, rcp(9), time.Hour, with(person("zeta@example.test"), map[string]any{"emailDigest": ""})) // an empty digest is none
	// An address full of what a statement is built from: quotes, a backslash, a
	// placeholder character and braces are all legal in a local part.
	const awkward = `Q"uote\back?slash{brace}'@Example.TEST`
	f.seed(concept, rcp(10), time.Hour, person(awkward))
	// Rows that are not recipients: a row of another concept under the id of the
	// first recipient, NEWER than it (so by id alone it is that id's newest
	// version), and a contact.
	f.seed("v1:product:recipient", rcp(1), time.Minute, person("alpha@example.test"))
	f.seed("v1:product:contact", "v1:product:contact:rcp-11", time.Hour, person("other@example.test"))

	const ids = 10
	before := map[string][]storedVersion{}
	for n := 1; n <= ids; n++ {
		before[rcp(n)] = f.versions(concept, rcp(n))
	}
	bystanders := [][2]string{{"v1:product:recipient", rcp(1)}, {"v1:product:contact", "v1:product:contact:rcp-11"}}
	bystanderBefore := map[string][]storedVersion{}
	for _, b := range bystanders {
		bystanderBefore[b[0]+"/"+b[1]] = f.versions(b[0], b[1])
	}

	const batch = 2
	report, err := backfillRecipientEmailDigests(context.Background(), f.db, nil, batch)
	if err != nil {
		t.Fatal(err)
	}
	stamped := map[string]string{rcp(1): "alpha@example.test", rcp(2): "beta@example.test", rcp(7): "epsilon+tag@example.test", rcp(8): "äbc@x.test", rcp(9): "zeta@example.test", rcp(10): `q"uote\back?slash{brace}'@example.test`}
	if report.Stamped != len(stamped) || report.Unusable != 2 || report.Changed != 0 {
		t.Errorf("report %+v, want %d stamped, 2 unusable (rcp-04, rcp-05) and none changed", report, len(stamped))
	}
	// Ten ids in batches of two: five full batches, and the walk ends on the
	// first batch that is not full -- here an empty one.
	if want := ids/batch + 1; report.Batches != want {
		t.Errorf("%d batches, want %d: the walk is bounded by the batch size, not by one pass over the table", report.Batches, want)
	}

	for id, mailbox := range stamped {
		old := before[id]
		now := f.versions(concept, id)
		if len(now) != len(old)+1 {
			t.Errorf("%s has %d versions, want %d: one new version, appended", id, len(now), len(old)+1)
			continue
		}
		for i := range old { // everything that was there is still there, untouched
			if !reflect.DeepEqual(now[i+1], old[i]) {
				t.Errorf("%s: version %d changed: the backfill appends, it does not rewrite", id, i)
			}
		}
		newest := now[0]
		wantPayload := map[string]any{}
		for k, v := range old[0].Payload {
			wantPayload[k] = v
		}
		wantPayload["emailDigest"] = RecipientEmailDigest(mailbox)
		if !reflect.DeepEqual(newest.Payload, wantPayload) {
			t.Errorf("%s: newest payload\n  %v\nwant the previous newest plus the digest\n  %v", id, newest.Payload, wantPayload)
		}
		if !newest.At.After(old[0].At) {
			t.Errorf("%s: the new version (%s) is not after the version it copies (%s), so a read would not see it", id, newest.At, old[0].At)
		}
		if newest.CreatedBy != old[0].CreatedBy || newest.Type != old[0].Type ||
			!reflect.DeepEqual(newest.Schema, old[0].Schema) || !reflect.DeepEqual(newest.Metadata, old[0].Metadata) {
			t.Errorf("%s: the new version does not copy createdBy, type, schema and metadata from the one it follows: %+v vs %+v", id, newest, old[0])
		}
		if !reflect.DeepEqual(newest.Provenance, migrationProvenance()) {
			t.Errorf("%s: provenance %v, want %v: the down direction finds its versions by it", id, newest.Provenance, migrationProvenance())
		}
	}
	for _, id := range []string{rcp(3), rcp(4), rcp(5), rcp(6)} {
		if now := f.versions(concept, id); !reflect.DeepEqual(now, before[id]) {
			t.Errorf("%s was written to although it needed nothing: %d versions, was %d", id, len(now), len(before[id]))
		}
	}
	for _, b := range bystanders {
		if now := f.versions(b[0], b[1]); !reflect.DeepEqual(now, bystanderBefore[b[0]+"/"+b[1]]) {
			t.Errorf("%s %s is not a recipient and was written to", b[0], b[1])
		}
	}

	again, err := backfillRecipientEmailDigests(context.Background(), f.db, nil, batch)
	if err != nil {
		t.Fatal(err)
	}
	if again.Stamped != 0 || again.Unusable != 2 {
		t.Errorf("second run %+v, want nothing stamped: the first run left every usable recipient with a digest", again)
	}
	// Twelve versions were seeded (rcp-02 and rcp-06 have two), and one was appended per stamped recipient.
	if total := f.count(`SELECT count(*) FROM "MemoryNodes" WHERE concept = ?`, concept); total != 12+len(stamped) {
		t.Errorf("the table holds %d recipient versions after two runs, want %d", total, 12+len(stamped))
	}
}

// TestRecipientDigestBackfillRunsOnceWhenTwoReplicasRunIt: two replicas, each
// with its own connections, walk the same rows at the same time. The batches
// take turns -- each is taken under one advisory lock, and its candidates are
// read after the lock -- so every recipient is stamped exactly once between
// them: one new version each, not one apiece.
func TestRecipientDigestBackfillRunsOnceWhenTwoReplicasRunIt(t *testing.T) {
	f := newRecipientDigestFixture(t)
	const (
		rows   = 400
		batch  = 8
		rounds = 3
	)
	other := f.replica()
	for round := 0; round < rounds; round++ {
		prefix := fmt.Sprintf("round%d", round)
		f.seedMany(prefix, rows)

		start := make(chan struct{})
		var wg sync.WaitGroup
		reports := make([]recipientEmailDigestReport, 2)
		errs := make([]error, 2)
		for i, db := range []*bun.DB{f.db, other} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				reports[i], errs[i] = backfillRecipientEmailDigests(context.Background(), db, nil, batch)
			}()
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("replica %d: %v", i, err)
			}
		}

		if got := reports[0].Stamped + reports[1].Stamped; got != rows {
			t.Errorf("round %d: the two replicas stamped %d versions between them (%d and %d), want %d: a recipient was stamped twice, or not at all",
				round, got, reports[0].Stamped, reports[1].Stamped, rows)
		}
		if twice := f.count(`SELECT count(*) FROM (SELECT id FROM "MemoryNodes" WHERE id LIKE ? GROUP BY id HAVING count(*) <> 2) x`, recipientID(prefix)+"-%"); twice != 0 {
			t.Errorf("round %d: %d recipients do not have exactly the original version and one stamped one", round, twice)
		}
		if wrong := f.count(`SELECT count(*) FROM "MemoryNodes" m WHERE id LIKE ? AND provenance->>'kind' = 'migration'
		   AND payload->>'emailDigest' IS DISTINCT FROM encode(sha256(convert_to(lower(btrim(payload->>'email')), 'UTF8')), 'hex')`, recipientID(prefix)+"-%"); wrong != 0 {
			t.Errorf("round %d: %d stamped versions carry a digest that is not the SHA-256 of their trimmed, lower-cased address", round, wrong)
		}
		t.Logf("round %d: replica 0 stamped %d, replica 1 stamped %d", round, reports[0].Stamped, reports[1].Stamped)
	}
}

// batchWatcher cancels a run's context after a chosen number of its
// transactions have committed, which is a SIGTERM at a node's restart landing
// BETWEEN two steps of the walk.
type batchWatcher struct {
	mu          sync.Mutex
	commits     int
	cancelAfter int
	cancel      context.CancelFunc
}

func (w *batchWatcher) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (w *batchWatcher) AfterQuery(_ context.Context, e *bun.QueryEvent) {
	if e.Query != "COMMIT" || e.Err != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.commits++
	if w.commits == w.cancelAfter {
		w.cancel()
	}
}

// TestRecipientDigestBackfillResumesAfterBeingInterrupted: an attempt that is
// cancelled between two batches ends with the deferral -- the committed batches
// stand, nothing is half done, the runner is not told a statement may still be
// running -- and the next attempt stamps exactly the rest.
func TestRecipientDigestBackfillResumesAfterBeingInterrupted(t *testing.T) {
	f := newRecipientDigestFixture(t)
	const rows, batch = 25, 5
	f.seedMany("resume", rows)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := backfillRecipientEmailDigests(ctx, f.db.WithQueryHook(&batchWatcher{cancelAfter: 2, cancel: cancel}), nil, batch)
	if !errors.Is(err, ErrMigrationDeferred) {
		t.Fatalf("an attempt cancelled between batches must defer, got %v", err)
	}
	if migrationMayStillBeRunning(err) {
		t.Fatalf("the runner would keep its migration lock over a run with nothing in flight: %v", err)
	}
	if first.Batches != 2 || first.Stamped != 2*batch {
		t.Fatalf("first attempt %+v, want exactly the two batches that committed, %d recipients", first, 2*batch)
	}
	if got := f.count(`SELECT count(*) FROM "MemoryNodes" WHERE id LIKE 'v1:campaigns:recipient:resume-%' AND provenance->>'kind' = 'migration'`); got != 2*batch {
		t.Fatalf("%d recipients were stamped by the interrupted attempt, want %d", got, 2*batch)
	}

	second, err := backfillRecipientEmailDigests(context.Background(), f.db, nil, batch)
	if err != nil {
		t.Fatal(err)
	}
	if second.Stamped != rows-2*batch {
		t.Errorf("second attempt stamped %d, want the %d the first left", second.Stamped, rows-2*batch)
	}
	if twice := f.count(`SELECT count(*) FROM (SELECT id FROM "MemoryNodes" WHERE id LIKE 'v1:campaigns:recipient:resume-%' GROUP BY id HAVING count(*) <> 2) x`); twice != 0 {
		t.Errorf("%d recipients are not the original version plus exactly one stamped one", twice)
	}
}

// raceWriter writes to chosen recipients through another connection at the
// moment the backfill sends its insert -- after the batch's candidates were
// read, before the new versions are appended: a replica of the previous
// release, or a current one, writing the same rows.
type raceWriter struct {
	mu   sync.Mutex
	done bool
	race func()
}

func (w *raceWriter) BeforeQuery(ctx context.Context, e *bun.QueryEvent) context.Context {
	if !strings.Contains(e.Query, `INSERT INTO "MemoryNodes"`) {
		return ctx
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.done {
		w.done = true
		w.race()
	}
	return ctx
}

func (w *raceWriter) AfterQuery(context.Context, *bun.QueryEvent) {}

// TestRecipientDigestBackfillLeavesARowThatChangedUnderIt: between the moment a
// batch reads its candidates and the moment it appends, another writer stamps
// one recipient and changes another's address. The append is guarded in its own
// statement, so neither gets a version built from the stale one: the digest of an
// old address must never land on a row that has a new one, and a row that is
// already stamped is not stamped twice.
func TestRecipientDigestBackfillLeavesARowThatChangedUnderIt(t *testing.T) {
	f := newRecipientDigestFixture(t)
	const concept = recipientEmailDigestConcept
	person := func(email string, extra map[string]any) map[string]any {
		p := map[string]any{"ownerUserId": "u1", "audienceId": "a1", "email": email, "subscriptionStatus": "subscribed"}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	rcp := func(n int) string { return recipientID(fmt.Sprintf("race-%d", n)) }
	for n := 1; n <= 4; n++ {
		f.seed(concept, rcp(n), time.Hour, person(fmt.Sprintf("race%d@example.test", n), nil))
	}

	other := f.replica()
	racing := &raceWriter{race: func() {
		write := func(id string, payload map[string]any) {
			raw, _ := json.Marshal(payload)
			if _, err := other.ExecContext(context.Background(),
				`INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", schema, payload, metadata, type, concept, provenance)
				 VALUES (?, ?, 'digest-test', '{"v":1}'::jsonb, ?::jsonb, '{"m":1}'::jsonb, 'node', ?, '{"kind":"direct","name":"racing-writer"}'::jsonb)`,
				id, f.base.Add(-time.Minute), string(raw), concept); err != nil {
				t.Errorf("the racing write to %s: %v", id, err)
			}
		}
		write(rcp(2), person("race2@example.test", map[string]any{"emailDigest": RecipientEmailDigest("race2@example.test")})) // stamped by someone else
		write(rcp(3), person("moved3@example.test", nil))                                                                      // the address changed
	}}

	report, err := backfillRecipientEmailDigests(context.Background(), f.db.WithQueryHook(racing), nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if report.Stamped != 2 || report.Changed != 2 {
		t.Errorf("report %+v, want the two untouched rows stamped and the two that changed under it left alone", report)
	}
	for _, n := range []int{1, 4} {
		if v := f.versions(concept, rcp(n)); len(v) != 2 || v[0].Payload["emailDigest"] != RecipientEmailDigest(fmt.Sprintf("race%d@example.test", n)) {
			t.Errorf("race-%d: %d versions, newest %v; want it stamped", n, len(v), v[0].Payload)
		}
	}
	if v := f.versions(concept, rcp(2)); len(v) != 2 || v[0].Provenance["name"] != "racing-writer" {
		t.Errorf("race-2 was stamped although another writer had stamped it first: %d versions, newest provenance %v", len(v), v[0].Provenance)
	}
	if v := f.versions(concept, rcp(3)); len(v) != 2 || v[0].Provenance["name"] != "racing-writer" || v[0].Payload["email"] != "moved3@example.test" {
		t.Errorf("race-3 was stamped from its old address: %d versions, newest %v", len(v), v[0].Payload)
	}
}

// TestRecipientDigestBackfillRevertDeletesOnlyWhatItAppended: the down direction
// removes the versions this migration wrote, found by provenance, and leaves
// every other version of every row -- another migration's included -- as it was.
func TestRecipientDigestBackfillRevertDeletesOnlyWhatItAppended(t *testing.T) {
	f := newRecipientDigestFixture(t)
	const concept = recipientEmailDigestConcept
	f.seedMany("revert", 12)
	// A version another migration wrote to a recipient, and the row it follows.
	f.seed(concept, recipientID("other-migration"), 2*time.Hour, map[string]any{"email": "someone@example.test"})
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", schema, payload, metadata, type, concept, provenance)
		 VALUES (?, ?, 'digest-test', '{}'::jsonb, '{"email":"someone@example.test","touched":true}'::jsonb, '{}'::jsonb, 'node', ?,
		         '{"kind":"migration","name":"20260912000000_site_owner_restamp"}'::jsonb)`, recipientID("other-migration"), f.base.Add(-time.Hour), concept); err != nil {
		t.Fatal(err)
	}
	before := f.count(`SELECT count(*) FROM "MemoryNodes"`)

	if _, err := backfillRecipientEmailDigests(context.Background(), f.db, nil, 5); err != nil {
		t.Fatal(err)
	}
	stamped := f.count(`SELECT count(*) FROM "MemoryNodes" WHERE provenance @> ?::jsonb`, `{"kind":"migration","name":"`+recipientEmailDigestMigrationName+`"}`)
	if stamped != 13 { // 12 + the other-migration row's newest version, which has an address and no digest
		t.Fatalf("the backfill stamped %d versions, want 13", stamped)
	}

	if err := RevertRecipientEmailDigests(context.Background(), f.db, nil); err != nil {
		t.Fatal(err)
	}
	if after := f.count(`SELECT count(*) FROM "MemoryNodes"`); after != before {
		t.Errorf("after the down the table holds %d versions, want the %d it held before the up", after, before)
	}
	if left := f.count(`SELECT count(*) FROM "MemoryNodes" WHERE provenance @> ?::jsonb`, `{"kind":"migration","name":"`+recipientEmailDigestMigrationName+`"}`); left != 0 {
		t.Errorf("the down left %d versions this migration appended", left)
	}
	if kept := f.count(`SELECT count(*) FROM "MemoryNodes" WHERE provenance->>'name' = '20260912000000_site_owner_restamp'`); kept != 1 {
		t.Errorf("the down removed another migration's version: %d of 1 remain", kept)
	}

	// Up again after a down stamps everything again: nothing about the first run
	// is remembered anywhere but in the rows.
	again, err := backfillRecipientEmailDigests(context.Background(), f.db, nil, 5)
	if err != nil || again.Stamped != 13 {
		t.Errorf("up after down: %+v, %v; want 13 stamped", again, err)
	}
}

// TestTheRecipientDigestMigrationRunsOnceWhenTwoReplicasBoot: the registered
// migration, as the runner runs it, on two replicas that boot together. bun's
// migration lock lets one run it and sends the other away to try again on its
// next tick, where it finds the migration applied; the rows end as one run left
// them.
func TestTheRecipientDigestMigrationRunsOnceWhenTwoReplicasBoot(t *testing.T) {
	f := newRecipientDigestFixture(t)
	const rows = 150
	f.seedMany("boot", rows)

	all := migrate.NewMigrations()
	registerTimescaleMigrations(all, nil)
	only := migrate.NewMigrations()
	for _, mig := range all.Sorted() {
		if mig.Name == "20261011000000" {
			only.Add(mig)
		}
	}
	if len(only.Sorted()) != 1 {
		t.Fatalf("the registered set carries %d migrations named 20261011000000, want 1", len(only.Sorted()))
	}

	type replica struct {
		runner *Database
		db     *bun.DB
	}
	replicas := []replica{{lockedMigrationRunner(t, only), f.db}, {lockedMigrationRunner(t, only), f.replica()}}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, r := range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r.runner.runMigrations(context.Background(), r.db)
		}()
	}
	close(start)
	wg.Wait()
	for i, r := range replicas {
		if r.runner.migrationsPending() { // refused by the other's lock: the next tick
			r.runner.runMigrations(context.Background(), r.db)
		}
		if err := r.runner.MigrationError(); err != nil {
			t.Fatalf("replica %d after its retry: %v", i, err)
		}
	}

	if n := f.count(`SELECT count(*) FROM bun_migrations WHERE name = '20261011000000'`); n != 1 {
		t.Errorf("the migration is recorded %d times, want once", n)
	}
	if twice := f.count(`SELECT count(*) FROM (SELECT id FROM "MemoryNodes" WHERE id LIKE 'v1:campaigns:recipient:boot-%' GROUP BY id HAVING count(*) <> 2) x`); twice != 0 {
		t.Errorf("%d recipients are not the original version plus exactly one stamped one", twice)
	}
	if stamped := f.count(`SELECT count(*) FROM "MemoryNodes" WHERE id LIKE 'v1:campaigns:recipient:boot-%' AND provenance @> ?::jsonb`, `{"kind":"migration","name":"`+recipientEmailDigestMigrationName+`"}`); stamped != rows {
		t.Errorf("%d recipients were stamped, want %d", stamped, rows)
	}
}
