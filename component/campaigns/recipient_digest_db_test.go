package campaigns

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// recipient_digest_db_test.go -- the recipient digest against a REAL engine and
// a REAL Postgres (memql#6022, F9): that the stamp lands on the row, that the
// backfill reaches a row written without one, and that a signup finds an
// existing membership by it without reading the roster.
//
// The database is shared with other tracks' runs and never truncated, so every
// case names its rows with an id nothing else can mint, counts only its own
// audience, and removes what it wrote when it ends.

// digestDBEngine boots one real engine against the shared test database.
func digestDBEngine(t *testing.T) (*memql.MemQLEngine, *bun.DB) {
	t.Helper()
	if reachable, err := dbtest.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	} else if !reachable {
		dbtest.Unreachable(t, "recipient email digest", dbtest.DSN(), nil)
		return nil, nil
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	e, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	e.Logger = quietLogger()
	if err := e.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	return e, db
}

// forgetWhenDone removes every row the test wrote: anything created since the
// test began whose id or payload names the test's unique suffix. Registered
// after the engine boots, so it runs before the pool closes.
func forgetWhenDone(t *testing.T, db *bun.DB, since time.Time, suffix string) {
	t.Helper()
	t.Cleanup(func() {
		pattern := "%" + suffix + "%"
		if _, err := db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE "createdAt" >= ? AND (id LIKE ? OR payload::text LIKE ?)`,
			since.Add(-time.Minute).UTC(), pattern, pattern); err != nil {
			t.Logf("removing the rows %q wrote: %v", suffix, err)
		}
	})
}

func digestOwner(user string) context.Context {
	return auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: user})
}

// digestTenant is one owner, the organization their recipients belong to (a
// recipient written without one is refused: "select an existing organization")
// and an audience in it.
type digestTenant struct {
	engine   *memql.MemQLEngine
	db       *bun.DB
	store    *Store
	owner    context.Context
	system   context.Context
	user     string
	org      string
	audience string
	suffix   string
}

func newDigestTenant(t *testing.T, name string) *digestTenant {
	t.Helper()
	eng, db := digestDBEngine(t)
	return newDigestTenantOn(t, eng, db, name)
}

// newDigestTenantOn is a tenant on an engine the test already booted, for the
// cases that put several tenants on one pod's engine and pool.
func newDigestTenantOn(t *testing.T, eng *memql.MemQLEngine, db *bun.DB, name string) *digestTenant {
	t.Helper()
	started := time.Now()
	suffix := fmt.Sprintf("%s-%d", name, started.UnixNano())
	forgetWhenDone(t, db, started, suffix)
	d := &digestTenant{
		engine: eng, db: db, store: NewStore(deliveryDBEngine{eng}), suffix: suffix,
		user: suffix + "-user", org: suffix + "-org", audience: suffix + "-aud",
		system: auth.ContextWithInternalOrigin(auth.ContextWithToken(auth.ContextWithAccess(context.Background(), auth.SystemActor("digest-test")), &auth.TokenInfo{Subject: "digest-test"})),
	}
	d.owner = digestOwner(d.user)
	d.execute(t, d.system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Sender","role":"owner","active":true})`, langparser.QuoteString(d.user), langparser.QuoteString(d.user+"@example.test")))
	d.execute(t, d.owner, call("mutation", "createClientAccount", arg{"accountId", d.org}, arg{"name", "Digest client"}))
	d.execute(t, d.owner, call("mutation", "createAudience", arg{"audienceId", d.audience}, arg{"accountId", d.org}, arg{"name", "Newsletter"}))
	return d
}

func (d *digestTenant) execute(t *testing.T, ctx context.Context, q string) {
	t.Helper()
	if _, err := d.engine.Execute(ctx, q); err != nil {
		t.Fatal(err)
	}
}

// storedRecipient reads one recipient through recipientById, fresh.
func storedRecipient(t *testing.T, store *Store, ctx context.Context, recipientID string) map[string]any {
	t.Helper()
	rows, err := store.rows(memql.ContextWithFreshRead(ctx), call("query", "recipientById", arg{"recipientId", recipientID}))
	if err != nil || len(rows) != 1 {
		t.Fatalf("recipientById %s: %d rows, %v", recipientID, len(rows), err)
	}
	return rows[0]
}

// versionsOf counts a recipient's stored versions and reads the newest one's
// provenance.
func versionsOf(t *testing.T, db *bun.DB, recipientID string) (versions int, newestProvenance string) {
	t.Helper()
	id := canonicalIDFor(recipientConcept, recipientID)
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*), COALESCE((SELECT provenance::text FROM "MemoryNodes" WHERE concept = ? AND id = ? ORDER BY "createdAt" DESC LIMIT 1), '')
		   FROM "MemoryNodes" WHERE concept = ? AND id = ?`, recipientConcept, id, recipientConcept, id).Scan(&versions, &newestProvenance); err != nil {
		t.Fatalf("versions of %s: %v", id, err)
	}
	return versions, newestProvenance
}

// TestAddRecipientStampsTheMailboxDigest: the digest is the SHA-256 of the
// NORMALIZED address, whatever spelling was typed, and it is still there after
// a later write to the row -- the unsubscribe is an update, and a digest an
// update dropped would make the member unfindable exactly when it matters.
func TestAddRecipientStampsTheMailboxDigest(t *testing.T) {
	d := newDigestTenant(t, "digest-stamp")
	recipient := d.suffix + "-rec"

	if err := d.store.AddRecipient(d.owner, recipient, d.audience, " Foo@Example.TEST ", "", "manual", d.org, nil); err != nil {
		t.Fatal(err)
	}
	want := EmailDigest("foo@example.test")
	row := storedRecipient(t, d.store, d.owner, recipient)
	if got := str(row, "emailDigest"); want == "" || got != want {
		t.Errorf("emailDigest = %q, want %q", got, want)
	}
	if got := str(row, "email"); got != " Foo@Example.TEST " {
		t.Errorf("the stored address is %q: the digest is of the normalized form, the row keeps what was typed", got)
	}

	if err := d.store.SetRecipientSubscription(d.owner, recipient, "unsubscribed", time.Now()); err != nil {
		t.Fatal(err)
	}
	row = storedRecipient(t, d.store, d.owner, recipient)
	if str(row, "subscriptionStatus") != "unsubscribed" || str(row, "emailDigest") != want {
		t.Errorf("after an unsubscribe the row reads status %q, digest %q; want unsubscribed and %q",
			str(row, "subscriptionStatus"), str(row, "emailDigest"), want)
	}
}

// sweepThese runs the digest sweep over a COPY of the named recipients in a
// scratch schema, then appends what the sweep stamped to the shared table, and
// says how many versions that was.
//
// The sweep is cluster-wide: it stamps every recipient without a digest that it
// finds. Run on the shared table it would append versions to rows that belong to
// other tests in the middle of what they are asserting -- this package's own
// consent cases plant digest-less rows on purpose. In a scratch schema it reaches
// the rows given and no others, and the engine still reads the result from the
// shared table, which is the half of the contract this package owns: that what the
// sweep writes is what a lookup finds and an update accepts.
func sweepThese(t *testing.T, db *bun.DB, recipientIDs ...string) int {
	t.Helper()
	ctx := context.Background()
	schema := fmt.Sprintf("digest_sweep_%d", time.Now().UnixNano())
	exec := func(on *bun.DB, query string, args ...any) {
		t.Helper()
		if _, err := on.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(db, `CREATE SCHEMA `+schema)
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Logf("dropping the scratch schema %s: %v", schema, err)
		}
	})
	// The shared table's columns, defaults, primary key and indexes -- not its
	// triggers -- so the sweep's ON CONFLICT and its latest-row index are the real ones.
	exec(db, `CREATE TABLE `+schema+`."MemoryNodes" (LIKE "MemoryNodes" INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING INDEXES)`)
	canonical := make([]string, 0, len(recipientIDs))
	for _, id := range recipientIDs {
		canonical = append(canonical, canonicalIDFor(recipientConcept, id))
	}
	exec(db, `INSERT INTO `+schema+`."MemoryNodes" SELECT * FROM "MemoryNodes" WHERE concept = ? AND id IN (?)`, recipientConcept, bun.In(canonical))

	versions := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+schema+`."MemoryNodes"`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	copied := versions()

	scratch := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()),
		pgdriver.WithConnParams(map[string]any{"search_path": schema}))), pgdialect.New())
	defer scratch.Close()
	if _, err := database.BackfillRecipientEmailDigests(ctx, scratch, quietLogger()); err != nil {
		t.Fatal(err)
	}
	// Only the versions the sweep appended go back: the copies of what was already
	// there are in the shared table, and are left alone.
	exec(db, `INSERT INTO "MemoryNodes" SELECT * FROM `+schema+`."MemoryNodes" WHERE provenance->>'kind' = 'system' AND provenance->>'name' = ?
		ON CONFLICT (id, "createdAt") DO NOTHING`, "reconcile:campaigns-recipient-digest")
	return versions() - copied
}

// TestRecipientDigestBackfill: a recipient written before the column existed --
// planted here with a raw insert as its owner, which stamps nothing -- gains the
// digest as one new version, the engine then finds it by that digest and can
// update it, and a second pass leaves it alone.
//
// The sweep runs over a copy of this one row in a scratch schema (sweepThese), not
// over the shared table.
func TestRecipientDigestBackfill(t *testing.T) {
	d := newDigestTenant(t, "digest-backfill")
	eng, db, owner, store := d.engine, d.db, d.owner, d.store
	recipient := d.suffix + "-rec"

	insert := fmt.Sprintf(`insert("v1:campaigns:recipient", id=%s, payload={"ownerUserId":%s,"accountId":%s,"audienceId":%s,"email":" Backfill@Example.TEST ","subscriptionStatus":"subscribed","source":"manual"})`,
		langparser.QuoteString(recipient), langparser.QuoteString(d.user), langparser.QuoteString(d.org), langparser.QuoteString(d.audience))
	if _, err := eng.Execute(owner, insert); err != nil {
		t.Fatal(err)
	}
	if got := str(storedRecipient(t, store, owner, recipient), "emailDigest"); got != "" {
		t.Fatalf("the planted row already carries digest %q; the backfill has nothing to prove", got)
	}
	if n, _ := versionsOf(t, db, recipient); n != 1 {
		t.Fatalf("the planted row has %d versions, want 1", n)
	}
	// Until the sweep has been through, the row is found by the other half of the lookup.
	fresh := memql.ContextWithFreshRead(owner)
	if byDigest, err := store.RecipientsInAudienceByDigest(fresh, d.audience, EmailDigest("backfill@example.test")); err != nil || len(byDigest) != 0 {
		t.Fatalf("a row with no digest was found by digest: %v %v", byDigest, err)
	}
	if members, err := store.MailboxMembers(fresh, d.audience, "backfill@example.test"); err != nil || len(members) != 1 || members[0].ID != recipient {
		t.Fatalf("a row with no digest was not found by comparing addresses: %v %v", members, err)
	}

	if stamped := sweepThese(t, db, recipient); stamped != 1 {
		t.Errorf("the sweep stamped %d versions of the one recipient it was given", stamped)
	}
	row := storedRecipient(t, store, owner, recipient)
	if got, want := str(row, "emailDigest"), EmailDigest("backfill@example.test"); want == "" || got != want {
		t.Errorf("emailDigest = %q, want %q", got, want)
	}
	if str(row, "email") != " Backfill@Example.TEST " || str(row, "subscriptionStatus") != "subscribed" {
		t.Errorf("the sweep changed more than the digest: %v", row)
	}
	versions, provenance := versionsOf(t, db, recipient)
	if versions != 2 || !strings.Contains(provenance, `"system"`) || !strings.Contains(provenance, "reconcile:campaigns-recipient-digest") {
		t.Errorf("after the sweep: %d versions, newest provenance %s; want the original plus one version from the sweep", versions, provenance)
	}

	// The engine reads what the sweep wrote as it reads what addRecipient wrote:
	// the row is found by its digest, and it takes an update (the concept is
	// closed, and a payload it does not recognise would be refused).
	byDigest, err := store.RecipientsInAudienceByDigest(fresh, d.audience, EmailDigest("backfill@example.test"))
	if err != nil || len(byDigest) != 1 || byDigest[0].ID != recipient {
		t.Fatalf("the swept row is not found by its digest: %v %v", byDigest, err)
	}
	if err := store.SetRecipientSubscription(owner, recipient, "unsubscribed", time.Now()); err != nil {
		t.Fatalf("the swept row refused an update: %v", err)
	}
	if got := str(storedRecipient(t, store, owner, recipient), "emailDigest"); got != EmailDigest("backfill@example.test") {
		t.Errorf("an update dropped the digest the sweep put on the row: %q", got)
	}

	if stamped := sweepThese(t, db, recipient); stamped != 0 {
		t.Errorf("a second pass appended %d versions: it stamped a row that already carried the digest", stamped)
	}
	if n, _ := versionsOf(t, db, recipient); n != 3 {
		t.Errorf("the row has %d versions, want 3: the original, the sweep's, and the update", n)
	}
}

// rosterReadCounter counts the roster walks a signup makes, and records each
// read of the rows a mailbox holds -- by digest, and of the rows with no digest --
// with whether it was a fresh read.
type rosterReadCounter struct {
	Engine
	mu         sync.Mutex
	reads      int
	lookups    []bool
	digestLess []bool
}

func (e *rosterReadCounter) Execute(ctx context.Context, statement string) (any, error) {
	e.mu.Lock()
	switch {
	case strings.HasPrefix(statement, "query audienceRosterForSend"):
		e.reads++
	case strings.HasPrefix(statement, "query recipientInAudienceByDigest"):
		e.lookups = append(e.lookups, memql.FreshReadFromContext(ctx))
	case strings.HasPrefix(statement, "query recipientsWithoutDigestInAudience"):
		e.digestLess = append(e.digestLess, memql.FreshReadFromContext(ctx))
	}
	e.mu.Unlock()
	return e.Engine.Execute(ctx, statement)
}

func (e *rosterReadCounter) rosterReads() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reads
}

// digestLookups returns, for each lookup by digest, whether it was a fresh read.
func (e *rosterReadCounter) digestLookups() []bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]bool(nil), e.lookups...)
}

// digestLessLookups returns, for each read of the audience's rows with no digest,
// whether it was a fresh read.
func (e *rosterReadCounter) digestLessLookups() []bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]bool(nil), e.digestLess...)
}

// digestNewsletter is one organization with a newsletter bound to one audience
// and a worker that takes its signups.
type digestNewsletter struct {
	*digestTenant
	worker *Worker
	sender *recordingSender
	// member is the recipient newDigestNewsletter put in the audience, added by
	// addRecipient under a spelling that is not the one the signup form will
	// send. Empty for a newsletter whose audience starts empty.
	member string
	args   map[string]any
}

// newEmptyNewsletter is a newsletter whose audience holds nobody: the form posts
// subscriber@example.test, and a case plants whatever rows it needs.
func newEmptyNewsletter(t *testing.T, name string) *digestNewsletter {
	t.Helper()
	d := newDigestTenant(t, name)
	sender := &recordingSender{}
	worker := newGatedWorker(t, d.engine, d.db, sender)
	return setUpStorefront(t, d, worker, sender)
}

// newGatedWorker is a worker on an engine, with the gates the application wires:
// they hold their locks on a pool of their own (the Database component's gate
// pool), not on the engine's.
func newGatedWorker(t *testing.T, eng *memql.MemQLEngine, db *bun.DB, sender *recordingSender) *Worker {
	t.Helper()
	worker := newTestWorker(t, deliveryDBEngine{eng}, sender)
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gates.Close() })
	wired := NewWorker(deliveryDBEngine{eng}, nil, nil, quietLogger(), func() *sql.DB { return gates })
	worker.newsletterGate, worker.templateGate, worker.singleSendGate = wired.newsletterGate, wired.templateGate, wired.singleSendGate
	return worker
}

// setUpStorefront gives the tenant a sender, a published welcome template, a site
// and a newsletter bound to the tenant's audience, all through the worker.
func setUpStorefront(t *testing.T, d *digestTenant, worker *Worker, sender *recordingSender) *digestNewsletter {
	t.Helper()
	template, identity, site := d.suffix+"-tpl", d.suffix+"-sender", d.suffix+"-site"

	d.execute(t, d.owner, call("mutation", "createSenderIdentity", arg{"senderIdentityId", identity}, arg{"accountId", d.org}, arg{"address", "hello@client.test"}, arg{"fromName", "Client"}))
	templateArgs := map[string]any{"templateId": template, "accountId": d.org, "name": "Welcome", "content": map[string]any{"subject": "Welcome", "textBody": "Thanks for subscribing", "htmlBody": "<p>Welcome</p>"}}
	nodes, err := worker.handleSaveTemplate(d.owner, templateArgs, 0)
	if err != nil {
		t.Fatal(err)
	}
	templateArgs["expectedRevision"], templateArgs["action"] = decodeResult(t, nodes)["revision"], "publish"
	if _, err := worker.handleSaveTemplate(d.owner, templateArgs, 0); err != nil {
		t.Fatal(err)
	}
	d.execute(t, d.owner, call("mutation", "createSite", arg{"siteId", site}, arg{"accountId", d.org}, arg{"hostname", d.suffix + ".example.test"}, arg{"bundleRef", "sites/newsletter/test"}, arg{"kind", "static"}))
	nodes, err = worker.handleConfigureNewsletter(d.owner, map[string]any{"siteId": site, "audienceId": d.audience, "templateId": template, "senderIdentityId": identity, "consentText": "I agree to receive Client's newsletter by email.", "enabled": true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return &digestNewsletter{
		digestTenant: d, worker: worker, sender: sender,
		args: map[string]any{"siteId": site, "storeId": "", "submissionId": "submission-" + d.suffix, "email": "subscriber@example.test", "displayName": "Subscriber", "consent": "yes", "consentRevision": decodeResult(t, nodes)["revision"]},
	}
}

// newDigestNewsletter is a newsletter whose audience already holds one member.
func newDigestNewsletter(t *testing.T, name string) *digestNewsletter {
	t.Helper()
	n := newEmptyNewsletter(t, name)
	n.member = n.suffix + "-rec"
	n.execute(t, n.owner, call("mutation", "addRecipient", arg{"recipientId", n.member}, arg{"audienceId", n.audience}, arg{"accountId", n.org}, arg{"email", " Subscriber@Example.TEST "}))
	return n
}

// replica boots another node of the mesh on the same database: its own engine,
// with its own node-local result cache, and its own worker.
func (n *digestNewsletter) replica(t *testing.T) *Worker {
	t.Helper()
	eng, db := digestDBEngine(t)
	return newGatedWorker(t, eng, db, &recordingSender{})
}

// roster reads the audience through the tenant's own store, not the worker's:
// the cases below put a counting engine under the worker, and the test's own
// look at the roster is not a read the signup made.
func (n *digestNewsletter) roster(t *testing.T) []Recipient {
	t.Helper()
	roster, err := n.store.Roster(memql.ContextWithFreshRead(n.owner), n.audience)
	if err != nil {
		t.Fatal(err)
	}
	return roster
}

// TestSignupFindsTheExistingMembershipByDigest: the audience already holds the
// address under another spelling, and the signup finds it by its digest -- it
// reads no roster, adds no second membership, and the signup names the member
// that was there.
func TestSignupFindsTheExistingMembershipByDigest(t *testing.T) {
	n := newDigestNewsletter(t, "digest-signup")
	actual := n.worker.store.engine
	counting := &rosterReadCounter{Engine: actual}
	n.worker.store.engine = counting
	_, err := n.worker.handleSubscribe(n.owner, n.args, 0)
	n.worker.store.engine = actual
	if err != nil {
		t.Fatal(err)
	}

	if reads := counting.rosterReads(); reads != 0 {
		t.Errorf("the signup read the roster %d times; a membership is found by digest, and the rows with no digest are one bounded page", reads)
	}
	if lookups := counting.digestLookups(); len(lookups) != 1 || !lookups[0] {
		t.Errorf("the signup looked the address up %v (true = a fresh read); want exactly one, fresh: it decides whether to write, and a cached answer from before another replica's enrolment would add a second membership", lookups)
	}
	if lookups := counting.digestLessLookups(); len(lookups) != 1 || !lookups[0] {
		t.Errorf("the signup looked at the rows with no digest %v (true = a fresh read); want exactly one, fresh: an opt-out on one of them is what the form must not miss, and a cached page from before it would", lookups)
	}
	if roster := n.roster(t); len(roster) != 1 || roster[0].ID != n.member {
		t.Errorf("the roster after the signup is %+v, want the one member that was there (%s)", roster, n.member)
	}
	address := "subscriber@example.test"
	signup, err := n.worker.newsletterRow(n.owner, "newsletterSignupById", "signupId", newsletterSignupID(n.audience, address))
	if err != nil || signup == nil {
		t.Fatalf("signup: %v %v", signup, err)
	}
	if got := bare(str(signup, "recipientId")); got != n.member {
		t.Errorf("the signup names recipient %q, want the existing member %q", got, n.member)
	}
}

// TestSignupAddsANewAddressAndCountsTheAudienceForItsCap: an address nobody has
// is added without a roster read, carries its own digest from the first write,
// and is refused at the audience ceiling by a COUNT -- while the member already
// in a full audience is still accepted.
func TestSignupAddsANewAddressAndCountsTheAudienceForItsCap(t *testing.T) {
	n := newDigestNewsletter(t, "digest-cap")
	actual := n.worker.store.engine
	counting := &rosterReadCounter{Engine: actual}
	n.worker.store.engine = counting
	defer func() { n.worker.store.engine = actual }()

	n.args["email"] = "newcomer@example.test"
	if _, err := n.worker.handleSubscribe(n.owner, n.args, 0); err != nil {
		t.Fatal(err)
	}
	if reads := counting.rosterReads(); reads != 0 {
		t.Errorf("signing up a new address read the roster %d times", reads)
	}
	roster := n.roster(t)
	if len(roster) != 2 {
		t.Fatalf("the roster holds %d members after a new address signed up, want 2", len(roster))
	}
	for _, r := range roster {
		if r.ID == n.member {
			continue
		}
		if got, want := str(storedRecipient(t, n.store, n.owner, r.ID), "emailDigest"), EmailDigest("newcomer@example.test"); got != want {
			t.Errorf("the new member carries digest %q, want %q: a second signup would not find it", got, want)
		}
	}

	// At the ceiling: a new address is turned away by a count; the member who is
	// already in -- the one addRecipient put there, who has not signed up yet --
	// is not.
	n.worker.cfg.MaxAudience = 2
	n.args["email"] = "toolate@example.test"
	if _, err := n.worker.handleSubscribe(n.owner, n.args, 0); err == nil || !strings.Contains(err.Error(), "cannot accept more subscribers") {
		t.Errorf("a new address at the ceiling: %v", err)
	}
	n.args["email"] = "subscriber@example.test"
	if _, err := n.worker.handleSubscribe(n.owner, n.args, 0); err != nil {
		t.Errorf("a member already in a full audience was refused: %v", err)
	}
	if reads := counting.rosterReads(); reads != 0 {
		t.Errorf("the ceiling check read the roster %d times", reads)
	}
	if got := len(n.roster(t)); got != 2 {
		t.Errorf("the audience holds %d members after the ceiling was met, want 2", got)
	}
}

// TestASignupOnAnotherReplicaSeesAMemberWhoUnsubscribedAfterItLastLooked: the
// mesh has more than one node, and each keeps its own result cache that hears of
// another node's writes only through an asynchronous broadcast. Replica B looked
// the address up and saw a subscribed member; replica A then recorded that the
// member unsubscribed; a signup that reaches B must see that, and accept the form
// without reopening the address.
//
// What keeps B from answering out of a stale cache is that the lookup is a fresh
// read. That is asserted directly, on the context the lookup arrives with
// (TestSignupFindsTheExistingMembershipByDigest): two engines in one test process
// share an invalidation channel, so no test here can make one of them lag. This
// pins the behaviour itself across two engines on one database.
func TestASignupOnAnotherReplicaSeesAMemberWhoUnsubscribedAfterItLastLooked(t *testing.T) {
	n := newDigestNewsletter(t, "digest-replica")
	other := n.replica(t)
	digest := EmailDigest("subscriber@example.test")

	if members, err := other.store.RecipientsInAudienceByDigest(n.owner, n.audience, digest); err != nil || len(members) != 1 || members[0].SubscriptionStatus != "subscribed" {
		t.Fatalf("before the unsubscribe: %+v err=%v", members, err)
	}

	// Replica A records the unsubscribe -- an operator honouring it by hand.
	if err := n.store.SetRecipientSubscription(n.owner, n.member, "unsubscribed", time.Now()); err != nil {
		t.Fatal(err)
	}

	if _, err := other.handleSubscribe(n.owner, n.args, 0); err != nil {
		t.Fatal(err)
	}
	signup, err := other.newsletterRow(n.owner, "newsletterSignupById", "signupId", newsletterSignupID(n.audience, "subscriber@example.test"))
	if err != nil {
		t.Fatal(err)
	}
	if signup != nil {
		t.Errorf("a signup was recorded for a member who had unsubscribed: replica B answered from what it had last seen")
	}
	if got := str(storedRecipient(t, n.store, n.owner, n.member), "subscriptionStatus"); got != "unsubscribed" {
		t.Errorf("the member reads %q after the form; a public form must never reopen an address that left", got)
	}
}
