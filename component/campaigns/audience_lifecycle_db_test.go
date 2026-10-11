package campaigns

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// The Email app needs one number and one act that no other audience construct answers: how many
// people in an audience can be mailed now (audienceRosterSize counts everyone on the list, so it
// cannot), and how an archived audience comes back (archiveAudience had no counterpart). This runs
// both against the real engine, one node: the constructs are plain DSL, so there is no replica
// state to cross.
func TestAudienceRestoreAndSubscriberCount(t *testing.T) {
	if reachable, err := dbtest.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	} else if !reachable {
		dbtest.Unreachable(t, "audience restore and subscriber count", dbtest.DSN(), nil)
		return
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	engine, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	engine.Logger = quietLogger()
	if err := engine.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}

	suffix := fmt.Sprintf("audience-lifecycle-%d", time.Now().UnixNano())
	user, stranger, org, audience := suffix+"-user", suffix+"-stranger", suffix+"-org", suffix+"-audience"
	actor := func(id string, role auth.Role) context.Context {
		return auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: id, Role: role}), &auth.TokenInfo{Subject: id})
	}
	owner, outsider := actor(user, auth.RoleOwner), actor(stranger, auth.RoleWriter)
	system := auth.ContextWithInternalOrigin(auth.ContextWithToken(auth.ContextWithAccess(context.Background(), auth.SystemActor("audience-lifecycle-test")), &auth.TokenInfo{Subject: "audience-lifecycle-test"}))

	// Every write goes through the engine as a client would call it; every read is answered from the
	// database, because the checks below follow a write with a read of the same row.
	run := func(ctx context.Context, q string) error {
		t.Helper()
		_, err := engine.Execute(ctx, q)
		return err
	}
	mustRun := func(ctx context.Context, q string) {
		t.Helper()
		if err := run(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	read := func(q string) []map[string]any {
		t.Helper()
		res, err := engine.Execute(memql.ContextWithFreshRead(owner), q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return memql.MaterializeRows(res)
	}
	count := func(name string) int {
		t.Helper()
		rows := read(call("query", name, arg{"audienceId", audience}))
		if len(rows) != 1 {
			t.Fatalf("%s answered %d rows, want one {count: N} aggregate: %v", name, len(rows), rows)
		}
		if _, ok := rows[0]["count"]; !ok {
			t.Fatalf("%s did not answer a count: %v", name, rows[0])
		}
		return integer(rows[0], "count")
	}
	status := func() string {
		t.Helper()
		rows := read(call("query", "audienceById", arg{"audienceId", audience}))
		if len(rows) != 1 {
			t.Fatalf("audienceById answered %d rows, want one: %v", len(rows), rows)
		}
		return str(rows[0], "status")
	}
	archive := call("mutation", "archiveAudience", arg{"audienceId", audience})
	restore := call("mutation", "restoreAudience", arg{"audienceId", audience})

	for _, person := range []struct{ id, role string }{{user, "owner"}, {stranger, "writer"}} {
		mustRun(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Audience tester","role":%s,"active":true})`,
			langparser.QuoteString(person.id), langparser.QuoteString(person.id+"@example.test"), langparser.QuoteString(person.role)))
	}
	mustRun(owner, call("mutation", "createClientAccount", arg{"accountId", org}, arg{"name", "Audience client"}))
	mustRun(owner, call("mutation", "createAudience", arg{"audienceId", audience}, arg{"accountId", org}, arg{"name", "Newsletter"}))

	// Three people on the list. One stays subscribed; one left, and one's address bounced. The two that
	// cannot be mailed are still on the list, which is what audienceRosterSize keeps counting.
	people := make([]string, 3)
	for i, address := range []string{"subscribed@example.test", "left@example.test", "bounced@example.test"} {
		people[i] = fmt.Sprintf("%s-person-%d", suffix, i)
		mustRun(owner, call("mutation", "addRecipient", arg{"recipientId", people[i]}, arg{"audienceId", audience}, arg{"accountId", org}, arg{"email", address}, arg{"source", "manual"}))
	}
	mustRun(owner, call("mutation", "setRecipientSubscription", arg{"recipientId", people[1]}, arg{"subscriptionStatus", "unsubscribed"}))
	mustRun(owner, call("mutation", "setRecipientSubscription", arg{"recipientId", people[2]}, arg{"subscriptionStatus", "bounced"}))
	if got := count("audienceRosterSize"); got != 3 {
		t.Fatalf("audienceRosterSize = %d, want 3: everyone on the list, whatever their state", got)
	}
	if got := count("audienceSubscriberCount"); got != 1 {
		t.Fatalf("audienceSubscriberCount = %d, want 1: only the person who can still be mailed", got)
	}

	// An archived audience stays readable, and restoring it puts it back as it was.
	if got := status(); got != "active" {
		t.Fatalf("new audience status = %q, want active", got)
	}
	mustRun(owner, archive)
	if got := status(); got != "archived" {
		t.Fatalf("after archiveAudience status = %q, want archived", got)
	}
	mustRun(owner, restore)
	if got := status(); got != "active" {
		t.Fatalf("after restoreAudience status = %q, want active", got)
	}

	// Someone with no access to the organization cannot bring the audience back. The engine may refuse
	// the call or write nothing; either way the audience is still archived for the people who can see it.
	mustRun(owner, archive)
	if err := run(outsider, restore); err != nil {
		t.Logf("restoreAudience refused for a user outside the organization: %v", err)
	}
	if got := status(); got != "archived" {
		t.Fatalf("after a restore by a user with no access to the organization status = %q, want archived", got)
	}
}
