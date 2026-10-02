package campaigns

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/znasllc-io/memql/integrations/email"
)

func TestSingleSendReceiptAcrossReplicas(t *testing.T) {
	if reachable, err := dbtest.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	} else if !reachable {
		dbtest.Unreachable(t, "single send receipt", dbtest.DSN(), nil)
		return
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{}
	engines := make([]*memql.MemQLEngine, 2)
	workers := make([]*Worker, 2)
	for n := range engines {
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
		engines[n] = e
		workers[n] = newTestWorker(t, deliveryDBEngine{e}, sender)
		wired := NewWorker(deliveryDBEngine{e}, nil, nil, quietLogger(), func() *sql.DB { return db.DB })
		workers[n].singleSendGate, workers[n].templateGate = wired.singleSendGate, wired.templateGate
	}
	suffix := fmt.Sprintf("single-%d", time.Now().UnixNano())
	user, org, audience, recipient, template, identity := suffix+"-user", suffix+"-org", suffix+"-aud", suffix+"-rec", suffix+"-tpl", suffix+"-sender"
	actor := func(user string, role auth.Role) context.Context {
		return auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: role}), &auth.TokenInfo{Subject: user})
	}
	owner := actor(user, auth.RoleOwner)
	system := auth.ContextWithInternalOrigin(auth.ContextWithToken(auth.ContextWithAccess(context.Background(), auth.SystemActor("single-send-test")), &auth.TokenInfo{Subject: "single-send-test"}))
	execute := func(ctx context.Context, q string) {
		t.Helper()
		if _, err := engines[0].Execute(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	execute(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Sender","role":"owner","active":true})`, langparser.QuoteString(user), langparser.QuoteString(user+"@example.test")))
	execute(owner, call("mutation", "createClientAccount", arg{"accountId", org}, arg{"name", "Receipt client"}))
	execute(owner, call("mutation", "createAudience", arg{"audienceId", audience}, arg{"accountId", org}, arg{"name", "Newsletter"}))
	execute(owner, call("mutation", "addRecipient", arg{"recipientId", recipient}, arg{"audienceId", audience}, arg{"accountId", org}, arg{"email", "subscriber@example.test"}))
	execute(owner, call("mutation", "createSenderIdentity", arg{"senderIdentityId", identity}, arg{"accountId", org}, arg{"address", "hello@client.test"}, arg{"fromName", "Client"}))
	templateArgs := map[string]any{"templateId": template, "accountId": org, "name": "Welcome", "content": map[string]any{"subject": "Welcome", "textBody": "Thanks for subscribing", "htmlBody": "<p>Welcome</p>"}}
	nodes, err := workers[0].handleSaveTemplate(owner, templateArgs, 0)
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"templateId": template, "recipientId": recipient, "senderIdentityId": identity, "requestId": "welcome-signup-once"}
	if _, err := workers[0].handleSendToRecipient(owner, args, 0); err == nil {
		t.Fatal("unpublished template sent")
	}
	templateArgs["expectedRevision"], templateArgs["action"] = decodeResult(t, nodes)["revision"], "publish"
	if _, err := workers[0].handleSaveTemplate(owner, templateArgs, 0); err != nil {
		t.Fatal(err)
	}
	// Prime the receipt miss on a second engine; its eventual read must bypass it.
	key := sha256Hex("campaign-single-send\x00" + user + "\x00welcome-signup-once")
	if _, err := workers[1].store.rows(auth.ContextWithInternalOrigin(owner), call("query", "campaignSingleSendById", arg{"receiptId", key})); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for _, worker := range workers {
		wg.Add(1)
		go func(w *Worker) { defer wg.Done(); _, err := w.handleSendToRecipient(owner, args, 0); failures <- err }(worker)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if sender.count() != 1 {
		t.Fatalf("submitted %d messages", sender.count())
	}
	nodes, err = workers[1].handleSendToRecipient(owner, args, 0)
	if err != nil || decodeResult(t, nodes)["sent"] != true || decodeResult(t, nodes)["replayed"] != true {
		t.Fatalf("replay: %v %v", nodes, err)
	}
	if sender.identities()[0].Address != "hello@client.test" {
		t.Fatal("sent under the wrong organization identity")
	}
	conflicting := map[string]any{}
	for k, v := range args {
		conflicting[k] = v
	}
	conflicting["recipientId"] = "different-recipient"
	if _, err := workers[1].handleSendToRecipient(owner, conflicting, 0); err == nil {
		t.Fatal("request identifier reused for another message")
	}
	for _, denied := range []context.Context{actor(user, auth.RoleReader), actor(suffix+"-other", auth.RoleWriter)} {
		if _, err := workers[1].handleSendToRecipient(denied, args, 0); err == nil {
			t.Fatal("unauthorized receipt access")
		}
	}
	// A timeout may follow provider acceptance. The next replica returns uncertainty
	// without calling the transport, regardless of how often the automation retries.
	var attempts atomic.Int32
	workers[0].sendHook = func(context.Context, email.Sender, email.Message, email.SendAs) error {
		attempts.Add(1)
		return errors.New("provider response lost")
	}
	args["requestId"] = "welcome-uncertain-once"
	if _, err := workers[0].handleSendToRecipient(owner, args, 0); err == nil {
		t.Fatal("lost provider response reported success")
	}
	nodes, err = workers[1].handleSendToRecipient(owner, args, 0)
	if err != nil || decodeResult(t, nodes)["uncertain"] != true || attempts.Load() != 1 || sender.count() != 1 {
		t.Fatalf("uncertain replay resent: %v attempts=%d sends=%d", err, attempts.Load(), sender.count())
	}
	workers[0].sendHook = nil
	actual := workers[0].store.engine
	args["requestId"] = "receipt-store-unavailable"
	workers[0].store.engine = singleReceiptFailure{Engine: actual, status: "attempting"}
	if _, err := workers[0].handleSendToRecipient(owner, args, 0); err == nil || sender.count() != 1 {
		t.Fatalf("sent without a durable attempt receipt: %v", err)
	}
	// Acceptance followed by a receipt-store failure still leaves the durable
	// attempting version. A different replica must never resubmit it.
	args["requestId"] = "receipt-response-unavailable"
	workers[0].store.engine = singleReceiptFailure{Engine: actual, status: "complete"}
	nodes, err = workers[0].handleSendToRecipient(owner, args, 0)
	if err != nil || decodeResult(t, nodes)["sent"] != true || sender.count() != 2 {
		t.Fatalf("accepted send reported incorrectly: %v", err)
	}
	nodes, err = workers[1].handleSendToRecipient(owner, args, 0)
	if err != nil || decodeResult(t, nodes)["uncertain"] != true || sender.count() != 2 {
		t.Fatalf("receipt-store failure led to duplicate mail: %v", err)
	}
	workers[0].store.engine = actual
	// Raw and named client writes cannot forge an attestation to suppress a send.
	if _, err := engines[0].Execute(owner, `insert("v1:campaigns:singleSendReceipt", id="forged", payload={"status":"complete"})`); err == nil || !strings.Contains(err.Error(), "internal origin") {
		t.Fatalf("raw receipt forgery: %v", err)
	}
	if _, err := engines[0].Execute(owner, fmt.Sprintf(`insert("v1:campaigns:singleSendReceipt", id=%s, payload={"status":"attempting"})`, langparser.QuoteString(key))); err == nil || !strings.Contains(err.Error(), "internal origin") {
		t.Fatalf("raw receipt rewrite: %v", err)
	}
	statement, err := langparser.RenderCall("recordCampaignSingleSend", map[string]any{"receiptId": "forged", "accountId": org, "fingerprint": "forged", "status": "complete", "result": map[string]any{"sent": true}, "versionTime": time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engines[0].Execute(owner, "mutation "+statement); err == nil {
		t.Fatal("named receipt forgery accepted")
	}
	if _, err := engines[0].Execute(owner, `query campaignSingleSendById(receiptId: "forged")`); err == nil {
		t.Fatal("external private receipt read")
	}
}

type singleReceiptFailure struct {
	Engine
	status string
}

func (e singleReceiptFailure) Execute(ctx context.Context, statement string) (any, error) {
	if strings.HasPrefix(statement, "mutation recordCampaignSingleSend(") && strings.Contains(statement, `status: "`+e.status+`"`) {
		return nil, errors.New("receipt store unavailable")
	}
	return e.Engine.Execute(ctx, statement)
}

func TestSingleSendRefusesUnpublishedCopy(t *testing.T) {
	for _, status := range []string{"draft", "archived", ""} {
		e := recipientSendEngine()
		e.template["status"] = status
		sender := &recordingSender{}
		w := newTestWorker(t, e, sender)
		if _, err := w.handleSendToRecipient(importCtx(), sendToRecipientArgs(), 0); err == nil || sender.count() != 0 {
			t.Fatalf("sent %q template: %v", status, err)
		}
	}
}
