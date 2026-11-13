package reviewspack

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/campaigns"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
	memqldsl "github.com/znasllc-io/memql/dsl"
	"github.com/znasllc-io/memql/integrations/email"
)

type retryCampaignEngine struct{ *memql.MemQLEngine }

func (e retryCampaignEngine) Execute(ctx context.Context, q string) (any, error) {
	return e.MemQLEngine.Execute(ctx, q)
}

type retryTransport struct {
	mu       sync.Mutex
	messages []email.Message
	response error
	entered  chan struct{}
	release  chan struct{}
	cancel   context.CancelFunc
	echo     bool
}

func (s *retryTransport) Send(ctx context.Context, msg email.Message, _ email.SendAs) error {
	s.mu.Lock()
	s.messages = append(s.messages, msg)
	response, entered, release, cancel, echo := s.response, s.entered, s.release, s.cancel, s.echo
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if entered != nil {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if echo {
		return &email.SendError{NotAccepted: true, Detail: "provider echoed " + msg.TextBody + " " + msg.HTMLBody}
	}
	return response
}
func (s *retryTransport) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.messages) }

type retryFailWriter struct {
	rowWriter
	failState string
}

func (w retryFailWriter) Write(ctx context.Context, mutation string, row map[string]any) error {
	if row["sendState"] == w.failState {
		return errors.New("injected request write failure")
	}
	return w.rowWriter.Write(ctx, mutation, row)
}

// Real request mutations/fences, independent engine caches, PostgreSQL request
// and Campaigns gates, and the real Campaigns order-mail receipt state machine.
// Only the external email transport is fake; these tests never send mail.
func TestReviewRetryDB(t *testing.T) {
	if reachable, err := dbtest.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	} else if !reachable {
		dbtest.Unreachable(t, "review retry", dbtest.DSN(), nil)
		return
	}
	t.Setenv(secret.EnvMasterKey, strings.Repeat("31", 32))
	t.Setenv("MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET", strings.Repeat("test", 16))
	t.Setenv("MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL", "https://mail.example.test")
	t.Setenv("MEMQL_CAMPAIGNS_WARMUP_ENABLED", "false")
	memqldsl.RegisterTree(Domain, Tree())
	t.Cleanup(func() { memqldsl.UnregisterTree(Domain) })
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engines := make([]*memql.MemQLEngine, 2)
	providers := make([]*Provider, 2)
	var dbs []*bun.DB
	transport := &retryTransport{}
	for i := range engines {
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
		dbs = append(dbs, db)
		t.Cleanup(func() { _ = db.Close() })
		gate, err := database.NewGatePool(dbtest.DSN(), 1)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = gate.Close() })
		eng, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		eng.Logger = logger
		if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		worker := campaigns.NewWorker(retryCampaignEngine{eng}, nil, func() email.Sender { return transport }, logger, func() *sql.DB { return gate })
		if err := eng.RegisterIntegration(worker); err != nil {
			t.Fatal(err)
		}
		provider, err := NewProvider(memql.PluginContext{Engine: eng, GateDB: func() *sql.DB { return gate }, BunDB: func() *bun.DB { return db }})
		if err != nil {
			t.Fatal(err)
		}
		engines[i], providers[i] = eng, provider.(*Provider)
	}
	sys := auth.ContextWithSystemActor(auth.ContextWithInternalOrigin(context.Background()), "reviews-retry-test")
	execute := func(ctx context.Context, q string) []map[string]any {
		t.Helper()
		r, err := engines[0].Execute(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		return memql.MaterializeRows(r)
	}
	insert := func(concept, id string, row map[string]any) {
		t.Helper()
		payload, _ := json.Marshal(row)
		ctx := sys
		if concept == "v1:shopify:order" {
			ctx = auth.ContextWithInternalOrigin(auth.ContextWithConnectorActor(context.Background(), "shopify"))
		}
		execute(ctx, fmt.Sprintf("insert(%s, id=%s, payload=%s)", langparser.QuoteString(concept), langparser.QuoteString(id), payload))
	}
	suffix := fmt.Sprintf("retry-%d", time.Now().UnixNano())
	owner := "v1:identity:user:" + suffix
	store, account, siteID, template, sender := suffix+"-store", suffix+"-org", suffix+"-site", suffix+"-template", suffix+"-sender"
	insert("v1:identity:user", suffix, map[string]any{"primaryEmail": suffix + "@example.test", "displayName": "Retry fixture", "role": "owner", "active": true})
	actor := auth.ContextWithInternalOrigin(auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: owner, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: owner}))
	insert("v1:accounts:account", account, map[string]any{"name": "Retry test", "status": "active", "domainStatus": "unverified"})
	insert("v1:shopify:store", store, map[string]any{"domain": store + ".myshopify.com"})
	site := map[string]any{"ownerUserId": owner, "title": siteID, "hostname": "shop.example.test", "status": "live", "kind": "shopify_storefront", "bundleRef": "blob://sites/" + siteID + "/v1/", "binding": map[string]any{"storeId": store}, "accountId": account}
	insert("v1:platform:site", siteID, site)
	site["id"] = siteID
	execute(actor, "mutation "+renderMutationCall("createSenderIdentity", map[string]any{"senderIdentityId": sender, "accountId": account, "address": "hello@example.test", "fromName": "Test"}))
	templateArgs := map[string]any{"templateId": template, "accountId": account, "name": "Review request", "content": map[string]any{"subject": "Your review", "textBody": "Review {{fields.reviewUrl}} token {{fields.reviewToken}} until {{fields.expiresOn}}", "htmlBody": "<p>Review {{fields.reviewToken}}</p>"}}
	draft := execute(actor, "builtin "+renderMutationCall("campaignSaveTemplate", templateArgs))
	templateArgs["expectedRevision"], templateArgs["action"] = draft[0]["revision"], "publish"
	execute(actor, "builtin "+renderMutationCall("campaignSaveTemplate", templateArgs))
	eff := Settings{LinkLifetimeDays: 14, RequestTemplateID: template, RequestSenderIdentityID: sender}
	now := time.Now().UTC()
	for _, p := range providers {
		p.now = func() time.Time { return now }
	}
	newOrder := func(label string) (map[string]any, string) {
		t.Helper()
		gid := "gid://shopify/Order/" + suffix + label
		row := map[string]any{"storeId": store, "gid": gid, "email": "buyer@example.test", "name": "#test", "updatedAt": now.Format(time.RFC3339Nano), "syncedAt": now.Format(time.RFC3339Nano)}
		insert("v1:shopify:order", suffix+label, row)
		return row, gid
	}
	read := func(id string) map[string]any {
		t.Helper()
		row, _, err := providers[0].currentRequest(sys, id)
		if err != nil || row == nil {
			t.Fatalf("request read: %v", err)
		}
		return row
	}
	ask := func(p *Provider, order map[string]any, gid string) string {
		return p.askLine(sys, site, eff, order, store, account, gid, "floss", "Floss", now)
	}
	t.Run("AcceptedThenLostRequestWrite", func(t *testing.T) {
		order, gid := newOrder("accepted")
		id := requestIDFor(store, gid, "floss")
		before := transport.count()
		original := providers[0].writer
		providers[0].writer = retryFailWriter{original, "sent"}
		if status := ask(providers[0], order, gid); status != "failed" {
			t.Fatalf("first result %s", status)
		}
		providers[0].writer = original
		pending := requestWrite(read(id), id)
		if transport.count() != before+1 {
			t.Fatal("first send did not reach fake transport exactly once")
		}
		if status := ask(providers[1], order, gid); status != "sent" {
			t.Fatalf("replay result %s", status)
		}
		settled := read(id)
		for _, key := range []string{"tokenHash", "issuedAt", "expiresAt"} {
			if asString(pending[key]) != asString(settled[key]) {
				t.Fatalf("replay changed %s", key)
			}
		}
		if transport.count() != before+1 {
			t.Fatal("Campaigns replay resent mail")
		}
		envelope, err := openRetry(pending, owner)
		if err != nil {
			t.Fatal(err)
		}
		if tokenHash(envelope.Token) != settled["tokenHash"] {
			t.Fatal("only delivered bearer no longer validates")
		}
		if _, err := providers[1].openLink(actor, store, envelope.Token, "floss"); err != nil {
			t.Fatal("original delivered link failed after replay")
		}
		providers[1].now = func() time.Time { return asTime(settled["expiresAt"]) }
		if _, err := providers[1].openLink(actor, store, envelope.Token, "floss"); err == nil {
			t.Fatal("retry extended the original expiry")
		}
		providers[1].now = func() time.Time { return now }
		var leaks int
		if err := dbs[0].QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE payload::text LIKE ? OR metadata::text LIKE ?`, "%"+envelope.Token+"%", "%"+envelope.Token+"%").Scan(&leaks); err != nil {
			t.Fatal(err)
		}
		if leaks != 0 {
			t.Fatal("plaintext bearer persisted in row payload or metadata")
		}
	})
	t.Run("UncertainProviderResponse", func(t *testing.T) {
		order, gid := newOrder("uncertain")
		id := requestIDFor(store, gid, "floss")
		before := transport.count()
		transport.response = errors.New("provider response lost")
		if status := ask(providers[0], order, gid); status != "failed" {
			t.Fatalf("first result %s", status)
		}
		transport.response = nil
		original := requestWrite(read(id), id)
		if status := ask(providers[1], order, gid); status != "pending" {
			t.Fatalf("uncertain replay result %s", status)
		}
		for _, key := range []string{"tokenHash", "issuedAt", "expiresAt", "tokenCiphertext"} {
			if asString(original[key]) != asString(read(id)[key]) {
				t.Fatalf("uncertainty changed %s", key)
			}
		}
		if transport.count() != before+1 {
			t.Fatal("uncertain provider handover was resubmitted")
		}
	})
	t.Run("ProviderEchoIsAbsentFromDurableLedgerHistory", func(t *testing.T) {
		order, gid := newOrder("provider-echo")
		id := requestIDFor(store, gid, "floss")
		transport.echo = true
		status := ask(providers[0], order, gid)
		transport.echo = false
		if status != "failed" {
			t.Fatalf("echo failure result %s", status)
		}
		original := requestWrite(read(id), id)
		envelope, err := openRetry(original, owner)
		if err != nil {
			t.Fatal(err)
		}
		if status := ask(providers[1], order, gid); status != "sent" {
			t.Fatalf("retry after typed refusal result %s", status)
		}
		var leaks, ledgerVersions int
		if err := dbs[0].QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE payload::text LIKE ? OR metadata::text LIKE ?`, "%"+envelope.Token+"%", "%"+envelope.Token+"%").Scan(&leaks); err != nil {
			t.Fatal(err)
		}
		if leaks != 0 {
			t.Fatal("provider echoed bearer persisted in row payload or metadata")
		}
		if err := dbs[0].QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE concept='v1:campaigns:delivery' AND payload->>'rowId'=?`, gid).Scan(&ledgerVersions); err != nil {
			t.Fatal(err)
		}
		if ledgerVersions < 2 {
			t.Fatal("privacy assertion did not cover failed and sent delivery versions")
		}
	})

	t.Run("DefiniteRefusalRetriesOriginalBearer", func(t *testing.T) {
		order, gid := newOrder("refused")
		id := requestIDFor(store, gid, "floss")
		before := transport.count()
		transport.response = &email.SendError{NotAccepted: true, Detail: "test provider refusal"}
		status := ask(providers[0], order, gid)
		transport.response = nil
		if status != "failed" {
			t.Fatalf("refused result %s", status)
		}
		original := requestWrite(read(id), id)
		if status := ask(providers[1], order, gid); status != "sent" {
			t.Fatalf("retry result %s", status)
		}
		if transport.count() != before+2 {
			t.Fatal("definitely unaccepted message did not retry once")
		}
		for _, key := range []string{"tokenHash", "issuedAt", "expiresAt"} {
			if asString(original[key]) != asString(read(id)[key]) {
				t.Fatalf("refused retry changed %s", key)
			}
		}
	})
	t.Run("AuthoritativeBuyerChangedAfterDueScan", func(t *testing.T) {
		order, gid := newOrder("buyer-changed")
		id := requestIDFor(store, gid, "floss")
		before := transport.count()
		transport.response = &email.SendError{NotAccepted: true, Detail: "test provider refusal"}
		status := ask(providers[0], order, gid)
		transport.response = nil
		if status != "failed" {
			t.Fatalf("refused result %s", status)
		}
		original := requestWrite(read(id), id)
		changed := map[string]any{}
		for k, v := range order {
			changed[k] = v
		}
		changed["email"] = "changed-buyer@example.test"
		insert("v1:shopify:order", suffix+"buyer-changed", changed)
		// The due scan still carries its old snapshot; Campaigns alone sees
		// the changed authoritative recipient at its own address resolution.
		if status := ask(providers[1], order, gid); status != "failed" {
			t.Fatalf("changed buyer result %s", status)
		}
		if transport.count() != before+1 {
			t.Fatal("old review bearer was handed to a changed buyer")
		}
		for _, key := range []string{"tokenHash", "issuedAt", "expiresAt"} {
			if asString(original[key]) != asString(read(id)[key]) {
				t.Fatalf("recipient mismatch changed %s", key)
			}
		}
	})

	t.Run("CancellationAfterProviderAcceptanceKeepsBearer", func(t *testing.T) {
		order, gid := newOrder("cancelled")
		id := requestIDFor(store, gid, "floss")
		before := transport.count()
		cancelled, cancel := context.WithCancel(sys)
		transport.cancel = cancel
		status := providers[0].askLine(cancelled, site, eff, order, store, account, gid, "floss", "Floss", now)
		transport.cancel = nil
		cancel()
		if status != "failed" {
			t.Fatalf("cancelled request result %s", status)
		}
		original := requestWrite(read(id), id)
		status = ask(providers[1], order, gid)
		if status != "sent" && status != "pending" {
			t.Fatalf("cancelled acceptance replay result %s", status)
		}
		if transport.count() != before+1 {
			t.Fatal("cancelled provider acceptance was resubmitted")
		}
		for _, key := range []string{"tokenHash", "issuedAt", "expiresAt"} {
			if asString(original[key]) != asString(read(id)[key]) {
				t.Fatalf("cancelled replay changed %s", key)
			}
		}
	})

	t.Run("TwoReplicasOneLinkOneMail", func(t *testing.T) {
		order, gid := newOrder("concurrent")
		before := transport.count()
		results := make(chan string, 2)
		for _, p := range providers {
			go func(p *Provider) { results <- ask(p, order, gid) }(p)
		}
		a, b := <-results, <-results
		if !((a == "sent" && b == "") || (b == "sent" && a == "")) {
			t.Fatalf("concurrent outcomes %q %q", a, b)
		}
		if transport.count() != before+1 {
			t.Fatal("concurrent requests did not converge on one email")
		}
	})
	t.Run("PendingClaimMustCommitBeforeMail", func(t *testing.T) {
		order, gid := newOrder("claim-failure")
		before := transport.count()
		original := providers[0].writer
		providers[0].writer = retryFailWriter{original, "pending"}
		status := ask(providers[0], order, gid)
		providers[0].writer = original
		row, _, err := providers[0].currentRequest(sys, requestIDFor(store, gid, "floss"))
		if status != "failed" || err != nil || row != nil || transport.count() != before {
			t.Fatal("uncommitted claim wrote or sent")
		}
	})
	manualOrders := func(order map[string]any) orderReader {
		return func(context.Context, string, string) (map[string]any, []map[string]any, func(string) string, error) {
			paid := map[string]any{}
			for k, v := range order {
				paid[k] = v
			}
			paid["displayFinancialStatus"], paid["totalPriceSet"] = "PAID", money("12.00")
			return paid, []map[string]any{line("gid://shopify/Product/1", "12.00")}, func(string) string { return "floss" }, nil
		}
	}
	pendingRequest := func(t *testing.T, label string) (map[string]any, string, string, map[string]any, time.Time) {
		t.Helper()
		order, gid := newOrder(label)
		id := requestIDFor(store, gid, "floss")
		original := providers[0].writer
		providers[0].writer = retryFailWriter{original, "sent"}
		status := ask(providers[0], order, gid)
		providers[0].writer = original
		if status != "failed" {
			t.Fatalf("pending setup result %s", status)
		}
		row, at, err := providers[0].currentRequest(sys, id)
		if err != nil || row == nil {
			t.Fatal("pending setup failed")
		}
		return order, gid, id, requestWrite(row, id), at
	}
	t.Run("ManualIssueFencesStaleFinalWrite", func(t *testing.T) {
		order, gid, id, stale, at := pendingRequest(t, "manual")
		p := providers[1]
		p.orders = manualOrders(order)
		defer func() { p.orders = nil }()
		if _, err := p.issueRequest(actor, map[string]any{"storeId": store, "orderGid": gid, "productHandle": "floss", "siteId": siteID}, 0); err != nil {
			t.Fatal(err)
		}
		current := read(id)
		if current["sendState"] != "skipped" || current["tokenCiphertext"] != "" || current["tokenHash"] == stale["tokenHash"] {
			t.Fatal("manual issue did not replace the request deliberately")
		}
		stale["sendState"] = "sent"
		err := providers[0].writer.Write(memql.ContextWithRowVersionFence(actor, requestConcept, id, at), "putReviewRequest", stale)
		if err == nil || !strings.Contains(err.Error(), memql.ErrRowVersionChanged.Error()) {
			t.Fatalf("stale final write was not fenced: %v", err)
		}
		if read(id)["tokenHash"] != current["tokenHash"] {
			t.Fatal("stale sender replaced manually issued link")
		}
	})
	t.Run("ManualIssueWaitsForAutomaticHandover", func(t *testing.T) {
		order, gid := newOrder("overlap")
		id := requestIDFor(store, gid, "floss")
		before := transport.count()
		transport.entered, transport.release = make(chan struct{}), make(chan struct{})
		autoDone := make(chan string, 1)
		go func() { autoDone <- ask(providers[0], order, gid) }()
		select {
		case <-transport.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("automatic send did not reach transport")
		}
		p := providers[1]
		p.orders = manualOrders(order)
		defer func() { p.orders = nil }()
		acquiring := make(chan struct{})
		coordinate := p.coordinateRequest
		p.coordinateRequest = func(ctx context.Context, id string) (requestLease, error) {
			close(acquiring)
			return coordinate(ctx, id)
		}
		manualDone := make(chan error, 1)
		go func() {
			_, err := p.issueRequest(actor, map[string]any{"storeId": store, "orderGid": gid, "productHandle": "floss", "siteId": siteID}, 0)
			manualDone <- err
		}()
		<-acquiring
		select {
		case err := <-manualDone:
			t.Fatalf("manual issue crossed an active handover: %v", err)
		case <-time.After(75 * time.Millisecond):
		}
		close(transport.release)
		if status := <-autoDone; status != "sent" {
			t.Fatalf("automatic result %s", status)
		}
		if err := <-manualDone; err != nil {
			t.Fatal(err)
		}
		transport.entered, transport.release = nil, nil
		p.coordinateRequest = coordinate
		if row := read(id); row["sendState"] != "skipped" || row["sendNote"] != "issued by hand" || transport.count() != before+1 {
			t.Fatal("manual/automatic overlap did not serialize")
		}
	})

	t.Run("ErasureScrubsRetryHistoryAndFencesStaleFinal", func(t *testing.T) {
		order, gid, id, stale, at := pendingRequest(t, "erase")
		p := providers[1]
		actual := p.reader
		reviewID := reviewIDForRequest(id)
		// Existing moderation authorization is covered by the live moderation
		// suite. Here only that input is controlled; request reads, mutations,
		// gate, terminal version and historical redaction all use the real DB.
		p.reader = retryEraseReader{rowReader: actual, reviewID: reviewID, requestID: id, store: store, owner: owner}
		held, oldHold, release, err := providers[0].lockRequest(sys, id)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		var pid int
		if err := dbs[0].QueryRowContext(context.Background(), `SELECT pid FROM pg_locks WHERE locktype='advisory' AND classid=1296387409 AND objid=hashtext(?)::oid AND granted`, "reviews/request/"+id).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		var killed bool
		if err := dbs[0].QueryRowContext(context.Background(), `SELECT pg_terminate_backend(?)`, pid).Scan(&killed); err != nil || !killed {
			t.Fatalf("could not terminate this test's own gate: %v", err)
		}
		if _, err := p.eraseReviewer(moderatorCtx("eraser-test"), map[string]any{"reviewId": reviewID}, 0); err != nil {
			t.Fatal(err)
		}
		p.reader = actual
		current := read(id)
		if current["email"] != "" || current["tokenCiphertext"] != "" || asString(current["erasedAt"]) == "" {
			t.Fatal("erasure left an active retry")
		}
		var remaining int
		if err := dbs[0].QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE concept=? AND id=? AND (coalesce(payload->>'email','')<>'' OR coalesce(payload->>'tokenCiphertext','')<>'')`, requestConcept, requestConcept+":"+id).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining != 0 {
			t.Fatal("erasure left personal retry data in history")
		}
		stale["sendState"] = "sent"
		if err := providers[0].writeRequest(held, oldHold, id, at, stale); err == nil {
			t.Fatal("killed gate holder wrote after erasure")
		}
		err = providers[0].writer.Write(memql.ContextWithRowVersionFence(actor, requestConcept, id, at), "putReviewRequest", stale)
		if err == nil || !strings.Contains(err.Error(), memql.ErrRowVersionChanged.Error()) {
			t.Fatalf("stale sender restored erased data: %v", err)
		}
		before := transport.count()
		if status := ask(p, order, gid); status != "" || transport.count() != before {
			t.Fatal("erased request was retried")
		}
		p.orders = manualOrders(order)
		defer func() { p.orders = nil }()
		if _, err := p.issueRequest(actor, map[string]any{"storeId": store, "orderGid": gid, "productHandle": "floss", "siteId": siteID}, 0); err == nil {
			t.Fatal("erased request was manually restored")
		}
	})

}

type retryEraseReader struct {
	rowReader
	reviewID, requestID, store, owner string
}

func (r retryEraseReader) Rows(ctx context.Context, q string, args map[string]string) ([]map[string]any, error) {
	switch q {
	case "reviewById":
		return []map[string]any{{"id": r.reviewID, "requestId": r.requestID, "storeId": r.store, "ownerUserId": r.owner, "revision": 1}}, nil
	case "reviewDecisionsForReview":
		return []map[string]any{{"reviewId": r.reviewID, "action": ActionErase, "revision": 1, "createdAt": time.Now().UTC()}}, nil
	case "reviewSettingsForStore":
		return []map[string]any{{"storeId": r.store, "removalApproverUserId": "eraser-test"}}, nil
	}
	return r.rowReader.Rows(ctx, q, args)
}
