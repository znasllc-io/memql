package emailrules

// fired_count_db_test.go -- an email rule's fired count equals the number of
// firings, whichever replica each one ran on (memql#5431).
//
// The count was written read-then-plus-one: every firing read the rule, sent,
// and wrote back what it had read plus one. Two firings that overlapped both
// read N and both wrote N+1. Multi-node is the default topology, so the test
// is two REPLICAS -- two engines, two connection pools, two result caches,
// sharing one database and nothing else -- firing concurrently behind a
// barrier. A mutex in either one's memory is exactly what this cannot see.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// dbEngine adapts the concrete engine to this package's Engine seam.
type dbEngine struct{ *memql.MemQLEngine }

func (e dbEngine) Execute(ctx context.Context, q string) (any, error) {
	return e.MemQLEngine.Execute(ctx, q)
}

// replica is one node's view of the cluster: its own pool, its own engine and
// result cache, and the Firer the production wiring would give it.
type replica struct {
	db    *bun.DB
	eng   *memql.MemQLEngine
	firer *Firer
}

func openReplica(t *testing.T) replica {
	t.Helper()
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ping, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		_ = db.Close()
		dbtest.Unreachable(t, "email rule fired count", dsn, err)
		return replica{}
	}
	t.Cleanup(func() { _ = db.Close() })
	eng, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	eng.Logger = quietLogger()
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	return replica{db: db, eng: eng, firer: replicaFirer(eng, db)}
}

// replicaFirer is the Firer one node's integration hands the fire builtin: the
// production wiring, with THIS replica's own gate over THIS replica's own pool.
// Two replicas therefore share no process-local state at all -- whatever keeps
// their counts straight has to be in the database.
func replicaFirer(eng *memql.MemQLEngine, db *bun.DB) *Firer {
	integration := &Integration{
		engine: dbEngine{eng},
		logger: quietLogger(),
		gate:   newFiringGate(func() *bun.DB { return db }, quietLogger()),
	}
	return integration.firer()
}

// seedFiringRule writes an active "updated" rule on the marketing lane whose
// audience has nobody in it: every firing records itself and sends nothing, so
// the count is the only thing a firing changes. It returns the author's
// envelope -- the one the authored scheduler runs the rule under -- and the
// rule's bare id.
func seedFiringRule(t *testing.T, eng *memql.MemQLEngine) (context.Context, string) {
	t.Helper()
	ctx := context.Background()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	user, account, audience, template, rule := "fc-user-"+suffix, "fc-org-"+suffix, "fc-aud-"+suffix, "fc-tpl-"+suffix, "fc-rule-"+suffix

	system := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, auth.SystemActor("fired-count-test")))
	system = auth.ContextWithToken(system, &auth.TokenInfo{Subject: "fired-count-test"})
	execute := func(ctx context.Context, q string) {
		t.Helper()
		if _, err := eng.Execute(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	execute(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"displayName":"Rule author","primaryEmail":"%s@example.test","role":"owner","active":true})`,
		langparser.QuoteString(user), user))

	// The owner authors the rule inside an organization, exactly as the
	// campaigns db test does: every campaign input is organization-owned.
	owner := auth.ContextWithToken(auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: user, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: user})
	q := langparser.QuoteString
	execute(owner, fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Fired count organization")`, q(account)))
	execute(owner, fmt.Sprintf(`mutation createAudience(audienceId: %s, name: "Fired count audience", accountId: %s)`, q(audience), q(account)))
	execute(owner, fmt.Sprintf(`mutation createTemplate(templateId: %s, name: "Fired count template", subject: "Hello", textBody: "Hello", accountId: %s)`, q(template), q(account)))
	execute(owner, fmt.Sprintf(`mutation createEmailRule(emailRuleId: %s, name: "Fired count rule", triggerConcept: "v1:todos:todo", eventKind: "updated", templateId: %s, recipientMode: "audience", audienceId: %s, accountId: %s)`,
		q(rule), q(template), q(audience), q(account)))
	execute(auth.ContextWithInternalOrigin(owner), fmt.Sprintf(`mutation recordEmailRuleGeneration(emailRuleId: %s, status: "active")`, q(rule)))
	// The author is a MEMBER of the rule's organization, by row. A run is
	// fired under a writer-role envelope (automations.AuthorContext), which
	// carries none of the standing reach a staff role has, and the
	// organization boundary (memql#5598) admits the rule -- even to its own
	// author -- only to a member. Without these two rows every firing reads no
	// rule and fails "rule is gone".
	group := "acct-" + account
	execute(system, fmt.Sprintf(`mutation writeGroup(groupId: %s, name: "Fired count organization", kind: "account", accountId: %s, status: "active")`, q(group), q(account)))
	execute(system, fmt.Sprintf(`mutation writeGroupMembership(membershipId: %s, groupId: %s, userId: %s, origin: "added", status: "active", accountId: %s)`, q("m-"+suffix), q(group), q(user), q(account)))

	// Fired under the envelope the authored scheduler builds for every run.
	return automations.AuthorContext(ctx, user), rule
}

// storedFiring reads the rule's LATEST version straight from the table -- no
// engine, no cache -- which is what every later read of the rule is served.
type storedFiring struct {
	count       int64
	lastError   string
	lastFiredAt string
	createdAt   time.Time
	versions    int64
}

func readStoredFiring(t *testing.T, db *bun.DB, rule string) storedFiring {
	t.Helper()
	var s storedFiring
	err := db.QueryRowContext(context.Background(), `
		SELECT COALESCE((payload->>'firedCount')::bigint, -1),
		       COALESCE(payload->>'lastError', ''),
		       COALESCE(payload->>'lastFiredAt', ''),
		       "createdAt",
		       (SELECT count(*) FROM "MemoryNodes" WHERE id = ?)
		  FROM "MemoryNodes"
		 WHERE id = ?
		 ORDER BY "createdAt" DESC
		 LIMIT 1`, "v1:campaigns:emailRule:"+rule, "v1:campaigns:emailRule:"+rule).
		Scan(&s.count, &s.lastError, &s.lastFiredAt, &s.createdAt, &s.versions)
	if err != nil {
		t.Fatalf("read the rule's latest version: %v", err)
	}
	return s
}

// TestFiredCountCountsEveryFiringAcrossReplicas fires one rule from two
// replicas at once, round after round, and requires the stored count to equal
// the number of firings exactly.
func TestFiredCountCountsEveryFiringAcrossReplicas(t *testing.T) {
	a := openReplica(t)
	b := openReplica(t)
	author, rule := seedFiringRule(t, a.eng)

	const rounds, perRound = 8, 8
	for round := 0; round < rounds; round++ {
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < perRound; i++ {
			r := a
			if i%2 == 1 {
				r = b
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ctx, cancel := context.WithTimeout(author, 30*time.Second)
				defer cancel()
				out, err := r.firer.Fire(ctx, "v1:campaigns:emailRule:"+rule, "v1:todos:todo:fc", map[string]any{})
				if err != nil {
					t.Errorf("fire: %v", err)
					return
				}
				if out.Duplicate || len(out.Refusals) > 0 || out.Recipients != 0 {
					t.Errorf("a firing of an active rule with an empty audience = %+v, want one clean firing that sends nothing", out)
				}
			}()
		}
		close(start)
		wg.Wait()
		if t.Failed() {
			t.FailNow()
		}
	}

	got := readStoredFiring(t, a.db, rule)
	if want := int64(rounds * perRound); got.count != want {
		t.Fatalf("firedCount = %d after %d firings across two replicas (%d versions written): increments were lost", got.count, want, got.versions)
	}
	if got.lastError != "" {
		t.Errorf("lastError = %q after clean firings, want empty", got.lastError)
	}
	if got.lastFiredAt == "" {
		t.Error("lastFiredAt is empty after firings")
	}
}

// TestAFiringLandsAfterAVersionAFasterClockWrote: a replica whose clock runs
// ahead wrote the rule's newest version, stamped in this replica's future. A
// firing here reads that version and must file its increment AFTER it -- a
// version stamped at this replica's own clock would sort behind the one it was
// derived from, and every later read would be served the older count.
func TestAFiringLandsAfterAVersionAFasterClockWrote(t *testing.T) {
	r := openReplica(t)
	author, rule := seedFiringRule(t, r.eng)
	fire := func() {
		t.Helper()
		if _, err := r.firer.Fire(author, "v1:campaigns:emailRule:"+rule, "v1:todos:todo:fc", map[string]any{}); err != nil {
			t.Fatalf("fire: %v", err)
		}
	}
	fire()
	if got := readStoredFiring(t, r.db, rule); got.count != 1 {
		t.Fatalf("firedCount = %d after one firing, want 1", got.count)
	}

	// The other replica's firing: count 5, stamped 30 seconds into this
	// replica's future.
	id := "v1:campaigns:emailRule:" + rule
	if _, err := r.db.ExecContext(context.Background(), `
		INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", schema, payload, metadata, "type", concept, provenance)
		SELECT id, "createdAt" + interval '30 seconds', "createdBy", schema,
		       jsonb_set(payload, '{firedCount}', '5'::jsonb), metadata, "type", concept, provenance
		  FROM "MemoryNodes" WHERE id = ? ORDER BY "createdAt" DESC LIMIT 1`, id); err != nil {
		t.Fatalf("write the faster clock's version: %v", err)
	}
	fast := readStoredFiring(t, r.db, rule)
	if fast.count != 5 || !fast.createdAt.After(time.Now()) {
		t.Fatalf("the fixture did not land as the newest version stamped in the future: %+v", fast)
	}

	fire()
	got := readStoredFiring(t, r.db, rule)
	if got.versions != fast.versions+1 {
		t.Fatalf("the firing wrote %d versions, want 1", got.versions-fast.versions)
	}
	if got.count != 6 || !got.createdAt.After(fast.createdAt) {
		t.Fatalf("after the faster clock's version (count 5 at %s) a firing here left the newest version at count %d, stamped %s: "+
			"the increment was filed behind the version it was derived from",
			fast.createdAt.Format(time.RFC3339Nano), got.count, got.createdAt.Format(time.RFC3339Nano))
	}
	if lastFiredAt, err := time.Parse(time.RFC3339Nano, got.lastFiredAt); err != nil || !lastFiredAt.Equal(got.createdAt) {
		t.Errorf("lastFiredAt = %q, want the firing version's own createdAt %s", got.lastFiredAt, got.createdAt.Format(time.RFC3339Nano))
	}
}
