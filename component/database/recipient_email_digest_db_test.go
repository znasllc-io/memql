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
)

// THE RECIPIENT DIGEST SWEEP, AGAINST A REAL DATABASE (memql#6022, F9).
//
// Each case gets a scratch schema (migrationLockDB) holding a "MemoryNodes" with
// the real table's columns, its primary key and its latest-row index -- a
// hypertable in one-day chunks when TimescaleDB is installed -- and pools whose
// search_path is that schema. The sweep's unqualified names land there and
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
	// base is when the newest seeded version could have been written.
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

func sweepProvenance() map[string]any {
	return map[string]any{"kind": "system", "name": recipientEmailDigestProvenanceName}
}

// sweptVersions counts the versions the sweep appended to the ids matching the
// pattern.
func (f *recipientDigestFixture) sweptVersions(idPattern string) int {
	f.t.Helper()
	return f.count(`SELECT count(*) FROM "MemoryNodes" WHERE id LIKE ? AND provenance @> ?::jsonb`,
		idPattern, `{"kind":"system","name":"`+recipientEmailDigestProvenanceName+`"}`)
}

// TestRecipientDigestBackfillStampsTheNewestVersionOfEachRecipient: one new
// version per recipient whose NEWEST version has no digest, the newest version
// copied whole with the digest added and nothing else changed, over several
// batches; every other row exactly as it was; a second pass finds nothing to do.
// A recipient that holds text that is not an address (a scrubbed one, a malformed
// import) is stamped like any other, with the digest of its text, which is what
// takes it out of the rows a signup has to compare.
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
	// An address full of what a statement is built from: quotes, a backslash, a
	// placeholder character and braces are all legal in a local part.
	const awkward = `Q"uote\back?slash{brace}'@Example.TEST`
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
	// The text each stamped recipient holds; the stamp is of the text, as
	// addRecipient stamps it.
	stamped := map[string]string{
		rcp(1): " Alpha@Example.TEST ", rcp(2): "beta@example.test", rcp(4): "no-at-sign", rcp(5): "",
		rcp(7): "Epsilon+tag@Example.TEST", rcp(8): "ÄBC@x.test", rcp(9): "zeta@example.test", rcp(10): awkward,
	}
	if report.Stamped != len(stamped) || report.NotAddresses != 2 || report.Changed != 0 {
		t.Errorf("report %+v, want %d stamped, 2 of them not addresses (rcp-04, rcp-05) and none changed", report, len(stamped))
	}
	// Ten ids in batches of two: five full batches, and the walk ends on the
	// first batch that is not full -- here an empty one.
	if want := ids/batch + 1; report.Batches != want {
		t.Errorf("%d batches, want %d: the walk is bounded by the batch size, not by one pass over the table", report.Batches, want)
	}

	for id, text := range stamped {
		old := before[id]
		now := f.versions(concept, id)
		if len(now) != len(old)+1 {
			t.Errorf("%s has %d versions, want %d: one new version, appended", id, len(now), len(old)+1)
			continue
		}
		for i := range old { // everything that was there is still there, untouched
			if !reflect.DeepEqual(now[i+1], old[i]) {
				t.Errorf("%s: version %d changed: the sweep appends, it does not rewrite", id, i)
			}
		}
		newest := now[0]
		wantPayload := map[string]any{}
		for k, v := range old[0].Payload {
			wantPayload[k] = v
		}
		wantPayload["emailDigest"] = RecipientEmailStamp(text)
		if !reflect.DeepEqual(newest.Payload, wantPayload) {
			t.Errorf("%s: newest payload\n  %v\nwant the previous newest plus the digest\n  %v", id, newest.Payload, wantPayload)
		}
		if want := old[0].At.Add(time.Microsecond); !newest.At.Equal(want) {
			t.Errorf("%s: the new version is at %s, want %s: one microsecond after the version it copies, so it is the newest and a write that lands later stays newer",
				id, newest.At.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
		}
		if newest.CreatedBy != old[0].CreatedBy || newest.Type != old[0].Type ||
			!reflect.DeepEqual(newest.Schema, old[0].Schema) || !reflect.DeepEqual(newest.Metadata, old[0].Metadata) {
			t.Errorf("%s: the new version does not copy createdBy, type, schema and metadata from the one it follows: %+v vs %+v", id, newest, old[0])
		}
		if !reflect.DeepEqual(newest.Provenance, sweepProvenance()) {
			t.Errorf("%s: provenance %v, want %v", id, newest.Provenance, sweepProvenance())
		}
	}
	for _, id := range []string{rcp(3), rcp(6)} {
		if now := f.versions(concept, id); !reflect.DeepEqual(now, before[id]) {
			t.Errorf("%s was written to although it already carried a digest: %d versions, was %d", id, len(now), len(before[id]))
		}
	}
	for _, b := range bystanders {
		if now := f.versions(b[0], b[1]); !reflect.DeepEqual(now, bystanderBefore[b[0]+"/"+b[1]]) {
			t.Errorf("%s %s is not a recipient and was written to", b[0], b[1])
		}
	}
	if left := f.count(`SELECT count(*) FROM (SELECT DISTINCT ON (id) id, payload FROM "MemoryNodes" WHERE concept = ? ORDER BY id, "createdAt" DESC) newest
	 WHERE COALESCE(payload->>'emailDigest', '') = ''`, concept); left != 0 {
		t.Errorf("%d recipients still have no digest after the sweep: each is a row every signup has to compare", left)
	}

	again, err := backfillRecipientEmailDigests(context.Background(), f.db, nil, batch)
	if err != nil {
		t.Fatal(err)
	}
	if again.Stamped != 0 || again.NotAddresses != 0 || again.Changed != 0 {
		t.Errorf("second pass %+v, want nothing stamped: the first left every recipient with a digest", again)
	}
	// Twelve versions were seeded (rcp-02 and rcp-06 have two), and one was appended per stamped recipient.
	if total := f.count(`SELECT count(*) FROM "MemoryNodes" WHERE concept = ?`, concept); total != 12+len(stamped) {
		t.Errorf("the table holds %d recipient versions after two passes, want %d", total, 12+len(stamped))
	}
}

// TestAWriteThatLandsDuringAPassStaysTheNewestVersion: the swept version sorts
// one microsecond after the version it copied, not at the clock's now. A hand
// unsubscribe (or a bounce) that commits while the statement runs was read-merged
// from the version BEFORE the stamp, so it carries no digest and a createdAt of its
// own; had the sweep stamped its version at the clock it would sort AFTER that
// write and shadow it, and the opt-out would stop being the row every read
// returns. As it is the write stays the newest, and the next pass stamps it.
func TestAWriteThatLandsDuringAPassStaysTheNewestVersion(t *testing.T) {
	f := newRecipientDigestFixture(t)
	const concept = recipientEmailDigestConcept
	id := recipientID("landed")
	person := func(status string) map[string]any {
		return map[string]any{"ownerUserId": "u1", "audienceId": "a1", "email": "landed@example.test", "subscriptionStatus": status}
	}
	source := f.seed(concept, id, time.Hour, person("subscribed"))

	if _, err := backfillRecipientEmailDigests(context.Background(), f.db, nil, 10); err != nil {
		t.Fatal(err)
	}
	// What the engine writes for the opt-out that raced the pass: the payload it
	// read before the stamp, a status change, and a createdAt later than the
	// source by far more than the pass added.
	raw, _ := json.Marshal(person("unsubscribed"))
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", schema, payload, metadata, type, concept, provenance)
		 VALUES (?, ?, 'digest-test', '{"v":1}'::jsonb, ?::jsonb, '{"m":1}'::jsonb, 'node', ?, '{"kind":"mutation","name":"setRecipientSubscription"}'::jsonb)`,
		id, source.Add(time.Millisecond), string(raw), concept); err != nil {
		t.Fatal(err)
	}
	versions := f.versions(concept, id)
	if len(versions) != 3 || versions[0].Payload["subscriptionStatus"] != "unsubscribed" || versions[0].Provenance["name"] != "setRecipientSubscription" {
		t.Fatalf("the write that landed after the sweep is not the newest version: %+v", versions)
	}

	// The next pass finds the opt-out without a digest and stamps it, keeping it.
	next, err := backfillRecipientEmailDigests(context.Background(), f.db, nil, 10)
	if err != nil || next.Stamped != 1 {
		t.Fatalf("the next pass: %+v, %v; want the opt-out stamped", next, err)
	}
	versions = f.versions(concept, id)
	if len(versions) != 4 || versions[0].Payload["subscriptionStatus"] != "unsubscribed" || versions[0].Payload["emailDigest"] != RecipientEmailDigest("landed@example.test") {
		t.Errorf("after the next pass the newest version is %+v; want the opt-out, now carrying the digest", versions[0])
	}
}

// TestRecipientDigestBackfillRunsOnceWhenTwoReplicasRunIt: two replicas, each
// with its own connections, walk the same rows at the same time. A batch is taken
// by whichever tries its lock first; the other skips it (and tries it once more at
// the end of its pass), so a pass can leave a batch behind, and the next pass takes
// it. Whatever the interleaving, every recipient ends with exactly one new version:
// the replicas never stamp one twice, and between them and the pass after they
// stamp each once.
func TestRecipientDigestBackfillRunsOnceWhenTwoReplicasRunIt(t *testing.T) {
	f := newRecipientDigestFixture(t)
	const (
		rows   = 400
		batch  = 8
		rounds = 3
	)
	other := f.replica()
	skipped := 0
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
		// What a skipped batch left, the next pass takes.
		rest, err := backfillRecipientEmailDigests(context.Background(), f.db, nil, batch)
		if err != nil {
			t.Fatal(err)
		}
		skipped += reports[0].Skipped + reports[1].Skipped

		if got := reports[0].Stamped + reports[1].Stamped + rest.Stamped; got != rows {
			t.Errorf("round %d: the two replicas and the pass after them stamped %d versions (%d, %d and %d), want %d: a recipient was stamped twice, or not at all",
				round, got, reports[0].Stamped, reports[1].Stamped, rest.Stamped, rows)
		}
		if twice := f.count(`SELECT count(*) FROM (SELECT id FROM "MemoryNodes" WHERE id LIKE ? GROUP BY id HAVING count(*) <> 2) x`, recipientID(prefix)+"-%"); twice != 0 {
			t.Errorf("round %d: %d recipients do not have exactly the original version and one stamped one", round, twice)
		}
		if wrong := f.count(`SELECT count(*) FROM "MemoryNodes" m WHERE id LIKE ? AND provenance->>'kind' = 'system'
		   AND payload->>'emailDigest' IS DISTINCT FROM encode(sha256(convert_to(lower(btrim(payload->>'email')), 'UTF8')), 'hex')`, recipientID(prefix)+"-%"); wrong != 0 {
			t.Errorf("round %d: %d stamped versions carry a digest that is not the SHA-256 of their trimmed, lower-cased address", round, wrong)
		}
		t.Logf("round %d: replica 0 stamped %d (skipped %d), replica 1 stamped %d (skipped %d), the pass after stamped %d",
			round, reports[0].Stamped, reports[0].Skipped, reports[1].Stamped, reports[1].Skipped, rest.Stamped)
	}
	t.Logf("%d batches were left to a later pass over %d rounds", skipped, rounds)
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

// TestRecipientDigestBackfillResumesAfterBeingInterrupted: a pass that is
// cancelled between two batches ends with the context's error -- a node shutting
// down, which the worker does not report as a failure -- the committed batches
// stand, nothing is half done, and the next pass stamps exactly the rest.
func TestRecipientDigestBackfillResumesAfterBeingInterrupted(t *testing.T) {
	f := newRecipientDigestFixture(t)
	const rows, batch = 25, 5
	f.seedMany("resume", rows)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := backfillRecipientEmailDigests(ctx, f.db.WithQueryHook(&batchWatcher{cancelAfter: 2, cancel: cancel}), nil, batch)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a pass cancelled between batches must end with the context's error, got %v", err)
	}
	if first.Batches != 2 || first.Stamped != 2*batch {
		t.Fatalf("first pass %+v, want exactly the two batches that committed, %d recipients", first, 2*batch)
	}
	if got := f.sweptVersions("v1:campaigns:recipient:resume-%"); got != 2*batch {
		t.Fatalf("%d recipients were stamped by the interrupted pass, want %d", got, 2*batch)
	}

	second, err := backfillRecipientEmailDigests(context.Background(), f.db, nil, batch)
	if err != nil {
		t.Fatal(err)
	}
	if second.Stamped != rows-2*batch {
		t.Errorf("second pass stamped %d, want the %d the first left", second.Stamped, rows-2*batch)
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

// TestAPassNeverWaitsForABatchAnotherReplicaHolds: another replica is stamping a
// batch -- it holds the sweep's lock -- and this pass tries the lock and moves on
// instead of waiting for it. A wait is a pool connection held idle in a
// transaction for as long as the other replica takes, on a pool of four, and it
// ends in a statement timeout that is not a failure of anything. The batches it
// could not take are left for the next pass (or the other replica, which is
// stamping them), and nothing is warned about: it is the walk working as designed.
func TestAPassNeverWaitsForABatchAnotherReplicaHolds(t *testing.T) {
	f := newRecipientDigestFixture(t)
	const rows, batch = 12, 4
	f.seedMany("held", rows)

	other := f.replica()
	holder, err := other.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.ExecContext(context.Background(), `SELECT pg_advisory_xact_lock(?)`, recipientEmailDigestLockKey); err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()

	started := time.Now()
	held, err := backfillRecipientEmailDigests(context.Background(), f.db, nil, batch)
	if err != nil {
		t.Fatalf("a pass over batches another replica holds failed: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("the pass took %s: it waited for the lock", elapsed)
	}
	if held.Stamped != 0 || held.Skipped != 3 {
		t.Errorf("report %+v, want nothing stamped and the three batches skipped", held)
	}
	if got := f.sweptVersions("v1:campaigns:recipient:held-%"); got != 0 {
		t.Errorf("%d recipients were stamped under a lock another replica held", got)
	}
	if waiting := f.count(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted
		AND objsubid = 1 AND classid = (?::bigint >> 32)::oid AND objid = (?::bigint & 4294967295)::oid`, recipientEmailDigestLockKey, recipientEmailDigestLockKey); waiting != 0 {
		t.Errorf("%d requests for the sweep's lock are left on the server", waiting)
	}
	if inUse := f.db.Stats().InUse; inUse != 0 {
		t.Errorf("%d connections are still held by the pass", inUse)
	}

	// The other replica lets go; the next pass takes what was left.
	_ = holder.Rollback()
	next, err := backfillRecipientEmailDigests(context.Background(), f.db, nil, batch)
	if err != nil || next.Stamped != rows || next.Skipped != 0 {
		t.Errorf("the next pass: %+v, %v; want the %d recipients stamped and nothing skipped", next, err, rows)
	}
}

// TestASkippedBatchGetsOneMoreTryBeforeThePassEnds: the replica that held a batch
// is usually done within a moment. A pass that skipped it tries once more at its
// end, so the batch is not left for the next pass an hour away when it can be
// taken now.
func TestASkippedBatchGetsOneMoreTryBeforeThePassEnds(t *testing.T) {
	f := newRecipientDigestFixture(t)
	const rows, batch = 12, 4
	f.seedMany("again", rows)

	other := f.replica()
	holder, err := other.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.ExecContext(context.Background(), `SELECT pg_advisory_xact_lock(?)`, recipientEmailDigestLockKey); err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	// The other replica lets go while this pass is still walking.
	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = holder.Rollback()
	}()

	pause := recipientEmailDigestRetryPause
	recipientEmailDigestRetryPause = 300 * time.Millisecond
	defer func() { recipientEmailDigestRetryPause = pause }()

	report, err := backfillRecipientEmailDigests(context.Background(), f.db, nil, batch)
	if err != nil {
		t.Fatal(err)
	}
	if report.Stamped != rows || report.Skipped != 0 {
		t.Errorf("report %+v, want all %d stamped by the retry and nothing left skipped", report, rows)
	}
}
