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

// A sender's page sends what is in its form, so a person who clears the reply-to is saying "replies
// come to the sender itself", and the page has to be able to say it. Every update here read-merges,
// which is a contract with two halves and the OS depends on both:
//
//   - a field LEFT OUT keeps its stored value (an edit form may send only what changed), and
//   - a field sent EMPTY blanks it (an explicit "" is a value).
//
// Without the second half a cleared reply-to would be told it saved while the old address went on
// receiving replies. The OS sends the empty string; this is the engine keeping its end.
func TestUpdateSenderIdentityClearsAFieldSentEmpty(t *testing.T) {
	if reachable, err := dbtest.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	} else if !reachable {
		dbtest.Unreachable(t, "sender identity update", dbtest.DSN(), nil)
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

	suffix := fmt.Sprintf("sender-update-%d", time.Now().UnixNano())
	user, org, identity := suffix+"-user", suffix+"-org", suffix+"-sender"
	owner := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: user})
	system := auth.ContextWithInternalOrigin(auth.ContextWithToken(auth.ContextWithAccess(context.Background(), auth.SystemActor("sender-update-test")), &auth.TokenInfo{Subject: "sender-update-test"}))

	mustRun := func(ctx context.Context, q string) {
		t.Helper()
		if _, err := engine.Execute(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Read back from the database: every check follows a write with a read of the same row.
	sender := func() map[string]any {
		t.Helper()
		res, err := engine.Execute(memql.ContextWithFreshRead(owner), call("query", "senderIdentityById", arg{"senderIdentityId", identity}))
		if err != nil {
			t.Fatal(err)
		}
		rows := memql.MaterializeRows(res)
		if len(rows) != 1 {
			t.Fatalf("senderIdentityById answered %d rows, want one: %v", len(rows), rows)
		}
		return rows[0]
	}
	// What the sender's page sends: the address and organization it was opened on, and the form's fields.
	update := func(fromName string, extra ...arg) {
		t.Helper()
		args := append([]arg{{"senderIdentityId", identity}, {"address", "news@client.test"}, {"fromName", fromName}, {"accountId", org}}, extra...)
		mustRun(owner, call("mutation", "updateSenderIdentity", args...))
	}

	mustRun(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Sender tester","role":"owner","active":true})`,
		langparser.QuoteString(user), langparser.QuoteString(user+"@example.test")))
	mustRun(owner, call("mutation", "createClientAccount", arg{"accountId", org}, arg{"name", "Sender client"}))
	mustRun(owner, call("mutation", "createSenderIdentity", arg{"senderIdentityId", identity}, arg{"accountId", org}, arg{"address", "news@client.test"},
		arg{"fromName", "Client News"}, arg{"replyTo", "help@client.test"}, arg{"notes", "Warmed up in June"}))
	if got := sender(); str(got, "replyTo") != "help@client.test" || str(got, "notes") != "Warmed up in June" {
		t.Fatalf("new sender = %v, want its reply-to and notes", got)
	}

	// Left out: kept. An edit that changes only the display name does not touch the rest.
	update("Client Team")
	got := sender()
	if str(got, "fromName") != "Client Team" || str(got, "replyTo") != "help@client.test" || str(got, "notes") != "Warmed up in June" {
		t.Fatalf("an update that left reply-to and notes out changed them: %v", got)
	}

	// Sent empty: cleared. The display name, which was sent with a value, stays what it was sent as.
	update("Client Team", arg{"replyTo", ""}, arg{"notes", ""})
	got = sender()
	if str(got, "replyTo") != "" || str(got, "notes") != "" {
		t.Fatalf("an update that sent reply-to and notes empty left them set: replyTo=%q notes=%q", str(got, "replyTo"), str(got, "notes"))
	}
	if str(got, "fromName") != "Client Team" || str(got, "address") != "news@client.test" || str(got, "status") != "active" {
		t.Fatalf("clearing the optional fields changed the rest of the sender: %v", got)
	}
}
