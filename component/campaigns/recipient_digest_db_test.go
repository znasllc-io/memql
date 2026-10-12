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

// TestRecipientDigestBackfill: a recipient written before the column existed --
// planted here with a raw insert as its owner, which stamps nothing -- gains the
// digest as one new version, and a second run leaves it alone.
//
// The backfill is cluster-wide, so what it reports is every row it stamped in
// the shared database, not just this one; the assertions are about this row.
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

	stamped, err := database.BackfillRecipientEmailDigests(context.Background(), db, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if stamped < 1 {
		t.Errorf("the backfill reports %d rows stamped; this test planted one", stamped)
	}
	row := storedRecipient(t, store, owner, recipient)
	if got, want := str(row, "emailDigest"), EmailDigest("backfill@example.test"); want == "" || got != want {
		t.Errorf("emailDigest = %q, want %q", got, want)
	}
	if str(row, "email") != " Backfill@Example.TEST " || str(row, "subscriptionStatus") != "subscribed" {
		t.Errorf("the backfill changed more than the digest: %v", row)
	}
	versions, provenance := versionsOf(t, db, recipient)
	if versions != 2 || !strings.Contains(provenance, `"migration"`) || !strings.Contains(provenance, "20261011000000_recipient_email_digest") {
		t.Errorf("after the backfill: %d versions, newest provenance %s; want the original plus one migration version", versions, provenance)
	}

	if _, err := database.BackfillRecipientEmailDigests(context.Background(), db, quietLogger()); err != nil {
		t.Fatal(err)
	}
	if n, _ := versionsOf(t, db, recipient); n != 2 {
		t.Errorf("a second backfill left %d versions of the row, want 2: it appended to a row that was already stamped", n)
	}
}

// rosterReadCounter counts the roster walks a signup makes, and records each
// lookup by digest with whether it was a fresh read.
type rosterReadCounter struct {
	Engine
	mu      sync.Mutex
	reads   int
	lookups []bool
}

func (e *rosterReadCounter) Execute(ctx context.Context, statement string) (any, error) {
	e.mu.Lock()
	switch {
	case strings.HasPrefix(statement, "query audienceRosterForSend"):
		e.reads++
	case strings.HasPrefix(statement, "query recipientInAudienceByDigest"):
		e.lookups = append(e.lookups, memql.FreshReadFromContext(ctx))
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

// digestNewsletter is one organization with a newsletter bound to one audience
// that already holds one member, added by addRecipient under a spelling that is
// not the one the signup form will send.
type digestNewsletter struct {
	*digestTenant
	worker *Worker
	member string
	args   map[string]any
}

func newDigestNewsletter(t *testing.T, name string) *digestNewsletter {
	t.Helper()
	d := newDigestTenant(t, name)
	template, identity, site := d.suffix+"-tpl", d.suffix+"-sender", d.suffix+"-site"

	worker := newTestWorker(t, deliveryDBEngine{d.engine}, &recordingSender{})
	wired := NewWorker(deliveryDBEngine{d.engine}, nil, nil, quietLogger(), func() *sql.DB { return d.db.DB })
	worker.newsletterGate, worker.templateGate, worker.singleSendGate = wired.newsletterGate, wired.templateGate, wired.singleSendGate

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

	member := d.suffix + "-rec"
	d.execute(t, d.owner, call("mutation", "addRecipient", arg{"recipientId", member}, arg{"audienceId", d.audience}, arg{"accountId", d.org}, arg{"email", " Subscriber@Example.TEST "}))

	return &digestNewsletter{
		digestTenant: d, worker: worker, member: member,
		args: map[string]any{"siteId": site, "storeId": "", "submissionId": "submission-" + d.suffix, "email": "subscriber@example.test", "displayName": "Subscriber", "consent": "yes", "consentRevision": decodeResult(t, nodes)["revision"]},
	}
}

// roster reads the audience through the tenant's own store, not the worker's:
// the cases below put a counting engine under the worker, and the test's own
// look at the roster is not a read the signup made.
// replica boots another node of the mesh on the same database: its own engine,
// with its own node-local result cache, and its own worker.
func (n *digestNewsletter) replica(t *testing.T) *Worker {
	t.Helper()
	eng, db := digestDBEngine(t)
	worker := newTestWorker(t, deliveryDBEngine{eng}, &recordingSender{})
	wired := NewWorker(deliveryDBEngine{eng}, nil, nil, quietLogger(), func() *sql.DB { return db.DB })
	worker.newsletterGate, worker.templateGate, worker.singleSendGate = wired.newsletterGate, wired.templateGate, wired.singleSendGate
	return worker
}

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
		t.Errorf("the signup read the roster %d times; a membership is one keyed read by digest", reads)
	}
	if lookups := counting.digestLookups(); len(lookups) != 1 || !lookups[0] {
		t.Errorf("the signup looked the address up %v (true = a fresh read); want exactly one, fresh: it decides whether to write, and a cached answer from before another replica's enrolment would add a second membership", lookups)
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

	if member, found, err := other.store.RecipientInAudienceByDigest(n.owner, n.audience, digest); err != nil || !found || member.SubscriptionStatus != "subscribed" {
		t.Fatalf("before the unsubscribe: %+v found=%v err=%v", member, found, err)
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
