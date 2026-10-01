package campaigns

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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

func TestTemplatePublicationChecksExactRevisionAcrossReplicas(t *testing.T) {
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	defer db.Close()
	ping, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		dbtest.Unreachable(t, "template versions", dbtest.DSN(), err)
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	engines := make([]*memql.MemQLEngine, 2)
	workers := make([]*Worker, 2)
	for n := range engines {
		e, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		e.Logger = quietLogger()
		if err := e.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		engines[n] = e
		workers[n] = NewWorker(deliveryDBEngine{e}, nil, nil, quietLogger(), func() *sql.DB { return db.DB })
	}
	workers[0].now = func() time.Time { return time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC) }
	workers[1].now = func() time.Time { return time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) }
	suffix := fmt.Sprintf("template-%d", time.Now().UnixNano())
	user, account, template := suffix+"-user", suffix+"-org", suffix+"-template"
	owner := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: user})
	run := func(ctx context.Context, q string) {
		t.Helper()
		if _, err := engines[0].Execute(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	system := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), auth.SystemActor("template-tests")))
	system = auth.ContextWithToken(system, &auth.TokenInfo{Subject: "template-tests"})
	run(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":"%s@example.test","displayName":"Template owner","role":"owner","active":true})`, langparser.QuoteString(user), user))
	run(owner, fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Template client")`, langparser.QuoteString(account)))
	content := func(subject string) map[string]any {
		return map[string]any{"subject": subject, "textBody": "Welcome", "htmlBody": "<p>Welcome</p>"}
	}
	args := func(revision, subject, action string) map[string]any {
		return map[string]any{"templateId": template, "accountId": account, "name": "Welcome", "content": content(subject), "expectedRevision": revision, "action": action}
	}
	save := func(w *Worker, data map[string]any) string {
		t.Helper()
		rows, err := w.handleSaveTemplate(owner, data, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("receipt: %v", rows)
		}
		var decoded map[string]any
		if err := json.Unmarshal(rows[0].Payload, &decoded); err != nil {
			t.Fatal(err)
		}
		return str(decoded, "revision")
	}
	first := save(workers[0], args("", "First", "save"))
	if first == "" {
		t.Fatal("missing revision")
	}
	if retry := save(workers[1], args("", "First", "save")); retry != first {
		t.Fatal("creation retry rewrote the draft")
	}
	if _, err := engines[0].Execute(owner, fmt.Sprintf(`mutation updateTemplate(templateId: %s, name: "Bypass", subject: "No", textBody: "No", status: "ready", versionTime: "2031-01-01T00:00:00Z")`, langparser.QuoteString(template))); err == nil {
		t.Fatal("client bypassed capability")
	}
	if _, err := engines[0].Execute(owner, fmt.Sprintf(`insert("v1:campaigns:template", id=%s, payload={"name":"Bypass","subject":"No","textBody":"No","status":"ready","accountId":%s})`, langparser.QuoteString(template), langparser.QuoteString(account))); err == nil {
		t.Fatal("raw template write bypassed revision gate")
	}
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for n, w := range workers {
		wg.Add(1)
		go func(n int, w *Worker) {
			defer wg.Done()
			_, err := w.handleSaveTemplate(owner, args(first, fmt.Sprintf("Replica %d", n), "save"), 0)
			errors <- err
		}(n, w)
	}
	wg.Wait()
	close(errors)
	successes, conflicts := 0, 0
	for err := range errors {
		if err == nil {
			successes++
		} else if strings.Contains(err.Error(), "changed") {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("got %d successes, %d conflicts", successes, conflicts)
	}
	rows, err := workers[1].store.rows(memql.ContextWithFreshRead(owner), call("query", "templateById", arg{"templateId", template}))
	if err != nil || len(rows) != 1 {
		t.Fatalf("read: %v %v", rows, err)
	}
	row := rows[0]
	current := templateTime(row["createdAt"]).Format(time.RFC3339Nano)
	subject := str(row, "subject")
	if !templateTime(row["createdAt"]).After(templateTime(first)) {
		t.Fatal("lagging replica wrote behind its read")
	}
	if _, err := workers[1].handleSaveTemplate(owner, args(first, subject, "publish"), 0); err == nil {
		t.Fatal("published an unreviewed revision")
	}
	if _, err := workers[1].handleSaveTemplate(owner, args(current, "Unsaved change", "publish"), 0); err == nil {
		t.Fatal("published unsaved content")
	}
	published := save(workers[1], args(current, subject, "publish"))
	edited := save(workers[0], args(published, "Revised", "save"))
	if edited == published {
		t.Fatal("edit did not advance revision")
	}
	wrongOrg := args(edited, "Revised", "save")
	wrongOrg["accountId"] = "missing-organization"
	if _, err := workers[0].handleSaveTemplate(owner, wrongOrg, 0); err == nil {
		t.Fatal("moved a template across organizations")
	}
	denied := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: suffix + "-outsider", Role: auth.RoleWriter})
	if _, err := workers[1].handleSaveTemplate(denied, args(edited, "Stolen", "save"), 0); err == nil {
		t.Fatal("unrelated writer edited client template")
	}
}
