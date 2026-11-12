package wholesalepack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/campaigns"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// email_test.go -- the applicant's emails and the staff notices, as decisions
// over rows behind the reader, writer and lane seams. The fake lane honours
// receipts as the real one does (one send per request id) and asks the pack's
// own resolver only when it is about to hand a message over, so a test counts
// what the real lane would: sends, resolver calls and minted links.

const (
	emailStaff          = "v1:identity:user:staff-email"
	termsTemplateTestID = "tpl-terms"
	emailHost           = "kestrel.example.com"
)

// fakeLane is the row lane: one send per request id, the resolver asked only
// when a message is handed over, and an outcome a test may script.
type fakeLane struct {
	p        *Provider
	receipts map[string]map[string]any // request id -> the reply it gave
	calls    []map[string]any          // every send's arguments
	sent     []campaigns.RowMessage    // every message handed over
	resolved int                       // resolver calls
	// outcome, when set, answers a send in place of handing it over: "defer",
	// "fail" or "withdraw" (the resolver's withdrawal is the real one).
	outcome string
	// during runs inside a send, before it answers: another node's write.
	during   func()
	statuses []map[string]any // the status read's answer
	asked    []string         // the account each status read named
}

func (l *fakeLane) SendRowMessage(ctx context.Context, args map[string]any) (map[string]any, error) {
	l.calls = append(l.calls, args)
	if l.during != nil {
		l.during()
	}
	request := asString(args["requestId"])
	if reply, ok := l.receipts[request]; ok {
		out := copyRow(reply)
		out["replayed"] = true
		return out, nil
	}
	switch l.outcome {
	case "busy":
		return nil, fmt.Errorf("wholesale: campaignSendRowMessage: %w: the wait for the lock was over", memql.ErrWriteGateBusy)
	case "hold":
		return nil, fmt.Errorf("wholesale: campaignSendRowMessage: %w", testHold{"Publish “Wholesale removal” to send this email."})
	case "uncertain":
		return nil, fmt.Errorf("wholesale: campaignSendRowMessage: %w", testUncertain{errors.New("campaigns.sendToRecipient: the connection to the provider closed")})
	case "defer":
		return map[string]any{"sent": false, "skipped": false, "deferred": true,
			"reason": "The send-rate limit is reached; this email will be retried shortly."}, nil
	case "fail":
		return nil, errors.New("wholesale: campaignSendRowMessage: campaigns.sendRowMessage: the template must belong to the row's organization")
	}
	l.resolved++
	msg, err := l.p.resolveRowMessage(ctx, campaigns.RowMessageRequest{Concept: asString(args["rowConcept"]),
		RowID: asString(args["rowId"]), AccountID: asString(args["accountId"]), Kind: asString(args["kind"]), RequestID: request})
	var withdrawn campaigns.RowWithdrawn
	if errors.As(err, &withdrawn) {
		reply := map[string]any{"sent": false, "skipped": true, "reason": withdrawn.Reason}
		return reply, nil
	}
	if err != nil {
		return nil, errors.New("campaigns.sendRowMessage: the row's resolver did not answer, so nothing was sent")
	}
	l.sent = append(l.sent, msg)
	reply := map[string]any{"sent": true, "skipped": false}
	l.receipts[request] = reply
	return copyRow(reply), nil
}

func (l *fakeLane) RowMessageStatus(_ context.Context, accountID string, requestIDs []string) ([]map[string]any, error) {
	l.asked = append(l.asked, accountID)
	var out []map[string]any
	for _, s := range l.statuses {
		for _, id := range requestIDs {
			if asString(s["requestId"]) == id {
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// testHold and testUncertain stand in for the row lane's own hold and uncertain
// hand-over, whose types are the campaigns package's: the lane's readers are
// pointed at them for the length of a test (useTestLaneReaders).
type testHold struct{ reason string }

func (h testHold) Error() string { return h.reason }

type testUncertain struct{ error }

func (u testUncertain) Unwrap() error { return u.error }

func useTestLaneReaders(t *testing.T) {
	t.Helper()
	held, uncertain := laneHeldReason, laneUncertain
	laneHeldReason = func(err error) (string, bool) {
		var h testHold
		if errors.As(err, &h) {
			return h.reason, true
		}
		return held(err)
	}
	laneUncertain = func(err error) bool {
		var u testUncertain
		return errors.As(err, &u) || uncertain(err)
	}
	t.Cleanup(func() { laneHeldReason, laneUncertain = held, uncertain })
}

// emailWorld is linkWorld plus the email rows, the outbox, the site and the
// people, and an entitlement log the pack's own provisioning writes to.
type emailWorld struct {
	*linkWorld
	sends    map[string]map[string]any // emailSend row id -> current version
	versions map[string]int            // emailSend row id -> versions written
	seen     map[string]int            // emailSend row id -> the version last read
	staged   map[string]map[string]any // outbound request id -> row
	stages   int                       // stage writes
	// afterStagedRead runs once an outbox read has answered: another node's
	// stage, landing between this node's check and its write.
	afterStagedRead func()
	// stagedReadsFresh is whether each outbox read asked for a fresh read.
	stagedReadsFresh []bool
	// stageErr, when set, refuses every stage with this error.
	stageErr error
	site     map[string]any
	names    map[string]string
	tick     int
	lane     *fakeLane
	caller   *fakeCaller
}

func newEmailWorld() *emailWorld {
	w := &emailWorld{linkWorld: newLinkWorld(), sends: map[string]map[string]any{}, versions: map[string]int{},
		seen: map[string]int{}, staged: map[string]map[string]any{}, names: map[string]string{"staff-email": "Rosa Staff"}}
	w.settings["accountId"] = "acct-links"
	w.settings["siteId"] = linkSite
	w.settings["ownerUserId"] = linkMerchant
	w.settings["termsTemplateId"] = termsTemplateTestID
	w.settings["approvedTemplateId"] = "tpl-approved"
	w.settings["rejectedTemplateId"] = "tpl-rejected"
	w.settings["revokedTemplateId"] = "tpl-revoked"
	w.settings["staffEmail"] = "desk@example.com"
	w.settings["entitlementAdapter"] = AdapterCustomerTag
	w.settings["createdAt"] = linkClock.Add(-2 * time.Hour)
	w.site = map[string]any{"id": "v1:platform:site:" + linkSite, "title": "Kestrel Cycles Trade", "hostname": emailHost,
		"ownerUserId": linkMerchant, "accountId": "acct-links", "binding": map[string]any{"storeId": linkStore}}
	return w
}

func (w *emailWorld) provider() *Provider {
	p := w.linkWorld.provider()
	p.reader, p.writer = w, w
	w.lane = &fakeLane{p: p, receipts: map[string]map[string]any{}}
	w.caller = &fakeCaller{}
	p.lane, p.caller = w.lane, w.caller
	p.osBase = func() string { return "https://os.example.test/" }
	return p
}

// next is a later instant than every row written before it.
func (w *emailWorld) next() time.Time {
	w.tick++
	return w.clock.Add(time.Duration(w.tick) * time.Second)
}

func (w *emailWorld) Rows(ctx context.Context, query string, args map[string]string) ([]map[string]any, error) {
	switch query {
	case "emailSendsForApplication":
		var out []map[string]any
		for id, row := range w.sends {
			if trimmed(row["applicationId"]) == args["applicationId"] && linkVisible(ctx, row) {
				w.seen[id] = w.versions[id]
				out = append(out, copyRow(row))
			}
		}
		return newestFirst(out), nil
	case "siteById":
		if bareID(args["siteId"]) == linkSite {
			return []map[string]any{w.site}, nil
		}
		return nil, nil
	case "outboundRequestById":
		if !auth.OriginFromContext(ctx).IsInternal() {
			return nil, errors.New("outboundRequestById is @serverOnly")
		}
		w.stagedReadsFresh = append(w.stagedReadsFresh, memql.FreshReadFromContext(ctx))
		row, ok := w.staged[args["requestId"]]
		if after := w.afterStagedRead; after != nil {
			w.afterStagedRead = nil
			after()
		}
		if ok {
			return []map[string]any{row}, nil
		}
		return nil, nil
	case "userByIdSystem":
		if name, ok := w.names[args["userId"]]; ok {
			return []map[string]any{{"displayName": name}}, nil
		}
		return nil, nil
	case "templates":
		return []map[string]any{
			{"id": "v1:campaigns:template:" + termsTemplateTestID, "name": "Your trade terms", "accountId": "acct-links", "status": "ready", "purpose": "transactional"},
			{"id": "tpl-approved", "name": "Trade prices on", "accountId": "acct-links", "status": "ready", "purpose": "transactional"},
			{"id": "tpl-news", "name": "Newsletter", "accountId": "acct-links", "status": "ready", "purpose": "marketing"},
			{"id": "tpl-other-org", "name": "Theirs", "accountId": "acct-other", "status": "ready", "purpose": "transactional"},
		}, nil
	case "applicationsForBackfill":
		return w.apps, nil
	}
	return w.linkWorld.Rows(ctx, query, args)
}

func (w *emailWorld) RowsWithList(ctx context.Context, query string, args map[string]string, name string, list []string) ([]map[string]any, error) {
	if query == "sitesBoundToStore" {
		return []map[string]any{w.site}, nil
	}
	return w.Rows(ctx, query, args)
}

func (w *emailWorld) RowsPage(ctx context.Context, query string, args map[string]string, _ string) ([]map[string]any, string, error) {
	rows, err := w.Rows(ctx, query, args)
	return rows, "", err
}

func (w *emailWorld) RowsPageWithList(ctx context.Context, query string, args map[string]string, name string, list []string, cursor string) ([]map[string]any, string, error) {
	if query == "applicationsForStore" {
		return w.apps, "", nil
	}
	if query == "emailSendsForApplicationIds" {
		var out []map[string]any
		for _, id := range list {
			out = append(out, matchingApplication(mapValues(w.sends), id)...)
		}
		return newestFirst(out), "", nil
	}
	return w.linkWorld.RowsPageWithList(ctx, query, args, name, list, cursor)
}

func mapValues(m map[string]map[string]any) []map[string]any {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]map[string]any, 0, len(m))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

func (w *emailWorld) Write(ctx context.Context, mutation string, args map[string]any) error {
	switch mutation {
	case "putEmailSendRow":
		id := asString(args["emailSendId"])
		if memql.HasRowVersionFence(ctx, emailSendConcept, id) && w.versions[id] != w.seen[id] {
			return fmt.Errorf("wholesale: putEmailSendRow: %w", memql.ErrRowVersionChanged)
		}
		w.putSend(id, args)
	case "stageServerOutboundRequest":
		if !auth.OriginFromContext(ctx).IsInternal() {
			return errors.New("stageServerOutboundRequest is @serverOnly")
		}
		if w.stageErr != nil {
			return w.stageErr
		}
		id := asString(args["requestId"])
		// THE ROW-ABSENT FENCE, as the engine holds it: a stage fenced on no row
		// loses to a row already there.
		if _, there := w.staged[id]; there && memql.HasRowVersionFence(ctx, outboundRequestConcept, id) {
			return fmt.Errorf("wholesale: stageServerOutboundRequest: %w", memql.ErrRowVersionChanged)
		}
		w.stages++
		row := copyRow(args)
		row["id"] = id
		row["status"] = "pending"
		w.staged[id] = row
	case "writeEntitlementRow", "backfillEntitlementRow":
		row := copyRow(args)
		delete(row, "entitlementId")
		row["id"] = "v1:wholesale:entitlement:" + asString(args["entitlementId"])
		row["createdAt"] = w.next()
		w.ents = append(w.ents, row)
	case "putWholesaleSettingsRow":
		row := copyRow(args)
		delete(row, "settingsId")
		row["id"] = asString(args["settingsId"])
		row["createdAt"] = w.next()
		w.settings = row
	}
	if mutation == "putEmailSendRow" || mutation == "stageServerOutboundRequest" || mutation == "writeEntitlementRow" ||
		mutation == "backfillEntitlementRow" || mutation == "putWholesaleSettingsRow" {
		w.writes = append(w.writes, writeCall{mutation: mutation, args: args, actor: actorOf(ctx), ctx: ctx})
		return nil
	}
	return w.linkWorld.Write(ctx, mutation, args)
}

func (w *emailWorld) Insert(ctx context.Context, mutation string, args map[string]any) (string, error) {
	return "generated", w.Write(ctx, mutation, args)
}

func (w *emailWorld) putSend(id string, args map[string]any) {
	row := copyRow(args)
	delete(row, "emailSendId")
	row["id"] = emailSendConcept + ":" + id
	row["createdAt"] = w.next()
	w.sends[id] = row
	w.versions[id]++
}

// automation is how the store's automation calls: internal origin, as the
// scheduler's own actor, which owns nothing.
func automation() context.Context {
	return auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(context.Background(), "automation-runner"))
}

func reply(t *testing.T, nodes []memorynodes.MemoryNode, err error) map[string]any {
	t.Helper()
	return decodeOne(t, nodes, err)
}

func (w *emailWorld) sendTerms(t *testing.T, p *Provider) map[string]any {
	t.Helper()
	nodes, err := p.sendTermsEmail(automation(), map[string]any{"applicationId": linkApp}, 0)
	return reply(t, nodes, err)
}

func (w *emailWorld) sendBuyer(t *testing.T, p *Provider, kind string) map[string]any {
	t.Helper()
	nodes, err := p.sendBuyerEmail(automation(), map[string]any{"applicationId": linkApp, "kind": kind}, 0)
	return reply(t, nodes, err)
}

// approveAndGrant leaves the application approved with its access in state.
func (w *emailWorld) approveAndGrant(state string) {
	w.decisions = append(w.decisions, map[string]any{"id": "v1:wholesale:applicationDecision:dec-approve",
		"applicationId": applicationConcept + ":" + linkApp, "transition": TransitionApprove, "decidedBy": emailStaff,
		"note": "Known to us.", "accountId": "acct-links", "createdAt": w.next()})
	w.grant(state)
}

// grant writes one more version of the entitlement in state, as a grant.
func (w *emailWorld) grant(state string) {
	w.ents = append(w.ents, map[string]any{"id": "v1:wholesale:entitlement:" + EntitlementRowID(linkApp),
		"applicationId": applicationConcept + ":" + linkApp, "storeId": linkStore, "state": state,
		"adapter": AdapterCustomerTag, "ownerUserId": linkMerchant, "accountId": "acct-links",
		"direction": DirectionGrant, "attempt": len(w.ents) + 1, "createdAt": w.next()})
}

// tokenIn is the token a link in text carries, or "".
func tokenIn(text string) string {
	i := strings.Index(text, "?t=")
	if i < 0 {
		return ""
	}
	return text[i+3:]
}

// requireNoToken fails when any write, staged row, email row or log line
// carries the token.
func (w *emailWorld) requireNoToken(t *testing.T, token string) {
	t.Helper()
	if token == "" {
		t.Fatal("no token to look for")
	}
	for _, c := range w.writes {
		for k, v := range c.args {
			if strings.Contains(fmt.Sprint(v), token) {
				t.Fatalf("%s wrote the token in %s", c.mutation, k)
			}
		}
	}
	for _, row := range w.sends {
		if strings.Contains(fmt.Sprint(row), token) {
			t.Fatalf("the emailSend row carries the token: %v", row)
		}
	}
	if strings.Contains(w.log.String(), token) {
		t.Fatalf("a log line carries the token: %s", w.log.String())
	}
}

// ---------------------------------------------------------------------------
// The sends
// ---------------------------------------------------------------------------

// A KIND WITH NO TEMPLATE CHOSEN SENDS NOTHING, records nothing, and calls no
// lane: the same call once a template is chosen sends.
func TestNoTemplateMeansNoSend(t *testing.T) {
	w := newEmailWorld()
	w.settings["termsTemplateId"] = ""
	p := w.provider()
	got := w.sendTerms(t, p)
	if got["state"] != SendSkipped || got["reason"] != "no terms email template is chosen" || got["recorded"] != false {
		t.Fatalf("the terms send answered %v", got)
	}
	w.decisions = append(w.decisions, map[string]any{"id": "dec-r", "applicationId": applicationConcept + ":" + linkApp,
		"transition": TransitionReject, "createdAt": w.next()})
	w.settings["rejectedTemplateId"] = ""
	if got := w.sendBuyer(t, p, EmailRejected); got["state"] != SendSkipped || got["reason"] != "no rejected email template is chosen" {
		t.Fatalf("the rejection send answered %v", got)
	}
	if len(w.lane.calls) != 0 || len(w.sends) != 0 || w.count("putPrivateLinkRow") != 0 {
		t.Fatalf("%d lane calls, %d rows, %d links: a kind with no template sends nothing", len(w.lane.calls), len(w.sends), w.count("putPrivateLinkRow"))
	}
	w.settings["rejectedTemplateId"] = "tpl-rejected"
	if got := w.sendBuyer(t, p, EmailRejected); got["state"] != SendSent || len(w.lane.calls) != 1 {
		t.Fatalf("once a template is chosen the send answered %v after %d lane calls", got, len(w.lane.calls))
	}
}

// THE APPROVAL EMAIL WAITS FOR GRANTED ACCESS (W3): while the entitlement is
// pending nothing is sent, recorded or asked of the lane; once a granted
// version lands the same call sends.
func TestTheApprovedEmailWaitsForGrantedAccess(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.approveAndGrant(EntitlementPending)
	got := w.sendBuyer(t, p, EmailApproved)
	if got["state"] != SendSkipped || got["reason"] != ReasonAccessNotGranted || got["recorded"] != false {
		t.Fatalf("a pending entitlement answered %v", got)
	}
	if len(w.lane.calls) != 0 || len(w.sends) != 0 {
		t.Fatalf("%d lane calls and %d rows while the access was pending", len(w.lane.calls), len(w.sends))
	}
	w.grant(EntitlementGranted)
	if got := w.sendBuyer(t, p, EmailApproved); got["state"] != SendSent || got["attempt"] != float64(1) {
		t.Fatalf("a granted entitlement answered %v", got)
	}
	if len(w.lane.calls) != 1 || w.lane.resolved != 1 || len(w.lane.sent) != 1 {
		t.Fatalf("%d lane calls, %d resolver calls, %d sent", len(w.lane.calls), w.lane.resolved, len(w.lane.sent))
	}
	call := w.lane.calls[0]
	if call["templateId"] != "tpl-approved" || call["requestId"] != LaneRequestID(EmailApproved, linkApp, 1) ||
		call["rowConcept"] != applicationConcept || call["rowId"] != linkApp || call["accountId"] != "acct-links" || call["kind"] != EmailApproved {
		t.Fatalf("the lane was called with %v", call)
	}
	if _, named := call["senderIdentityId"]; named {
		t.Fatalf("a store with no sender chosen named one: %v", call)
	}
	url := w.lane.sent[0].Fields["priceSheetUrl"]
	if !strings.HasPrefix(url, "https://"+emailHost+"/wholesale/price-sheet?t=") {
		t.Fatalf("the price-sheet link is %q", url)
	}
	w.requireNoToken(t, tokenIn(url))
}

// A SECOND GRANTED VERSION OF ONE GRANT SENDS NO SECOND APPROVAL EMAIL: the
// automation calls on each granted version, and the attempt already sent is
// answered as it stands.
func TestASecondGrantedVersionSendsNoSecondApprovalEmail(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.approveAndGrant(EntitlementGranted)
	first := w.sendBuyer(t, p, EmailApproved)
	w.grant(EntitlementGranted) // a retry of the grant, or any later version
	second := w.sendBuyer(t, p, EmailApproved)
	if first["state"] != SendSent || second["state"] != SendSent || second["recorded"] != true {
		t.Fatalf("first %v, second %v", first, second)
	}
	if len(w.lane.calls) != 1 || w.lane.resolved != 1 || len(w.sends) != 1 || w.count("putEmailSendRow") != 1 {
		t.Fatalf("%d lane calls, %d resolver calls, %d rows, %d row writes; want one of each",
			len(w.lane.calls), w.lane.resolved, len(w.sends), w.count("putEmailSendRow"))
	}
}

// A VERSION THE BACKFILL RE-WROTE SENDS NOTHING: the backfill ties an untied
// entitlement with a new granted version, which a client's automation sees as
// any other, and the approval email already sent is not sent again.
func TestAVersionTheBackfillReWroteSendsNoSecondApprovalEmail(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.approveAndGrant(EntitlementGranted)
	w.ents[len(w.ents)-1]["accountId"] = "" // written before organizations reached the pack
	w.sendBuyer(t, p, EmailApproved)
	before := len(w.ents)
	nodes, err := p.backfillAccounts(automation(), map[string]any{}, 0)
	reply(t, nodes, err)
	if len(w.ents) != before+1 || trimmed(w.ents[len(w.ents)-1]["state"]) != EntitlementGranted {
		t.Fatalf("the backfill wrote %d entitlement versions, want one granted version", len(w.ents)-before)
	}
	if got := w.sendBuyer(t, p, EmailApproved); got["state"] != SendSent || len(w.lane.calls) != 1 {
		t.Fatalf("after the backfill the send answered %v with %d lane calls", got, len(w.lane.calls))
	}
}

// A SECOND AUTOMATIC TERMS SEND MINTS NO NEW LINK: the link's token hash is the
// one the first send minted.
func TestASecondAutomaticTermsSendMintsNoNewLink(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.sendTerms(t, p)
	hash := w.termsLink()["tokenHash"]
	got := w.sendTerms(t, p)
	if got["state"] != SendSent || w.termsLink()["tokenHash"] != hash || w.count("putPrivateLinkRow") != 1 || w.lane.resolved != 1 {
		t.Fatalf("the second send answered %v; %d links minted, %d resolver calls", got, w.count("putPrivateLinkRow"), w.lane.resolved)
	}
}

// THE SEND BUILTIN MINTS NOTHING ITSELF: a lane that defers asks no resolver,
// so no link is written, and the attempt is deferred -- answered as not sent
// yet, from its row, by a second call, which calls no lane.
func TestTheSendBuiltinMintsNothingItself(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.lane.outcome = "defer"
	_, err := p.sendTermsEmail(automation(), map[string]any{"applicationId": linkApp}, 0)
	if err == nil || !strings.Contains(err.Error(), "send-rate limit") {
		t.Fatalf("a deferral answered %v", err)
	}
	if row := w.sends[EmailSendRowID(EmailTerms, linkApp, 1)]; row["state"] != SendDeferred {
		t.Fatalf("the deferred attempt's row is %v", row)
	}
	if w.count("putPrivateLinkRow") != 0 || w.lane.resolved != 0 {
		t.Fatalf("a deferred send minted %d links and asked the resolver %d times", w.count("putPrivateLinkRow"), w.lane.resolved)
	}
	w.lane.outcome = ""
	if _, err := p.sendTermsEmail(automation(), map[string]any{"applicationId": linkApp}, 0); err == nil || len(w.lane.calls) != 1 {
		t.Fatalf("the deferred attempt answered %v after %d lane calls", err, len(w.lane.calls))
	}
}

// A FAILURE IS RECORDED AND ANSWERED AS NOT SENT, and the next call tries the
// same attempt again under the same request id.
func TestALaneFailureIsRecordedAndTriedAgain(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.lane.outcome = "fail"
	_, err := p.sendTermsEmail(automation(), map[string]any{"applicationId": linkApp}, 0)
	if err == nil || err.Error() != "wholesale: the terms email was not sent; the application's emails say why" {
		t.Fatalf("a failure answered %v", err)
	}
	row := w.sends[EmailSendRowID(EmailTerms, linkApp, 1)]
	if row == nil || row["state"] != SendFailed || row["reason"] != "campaigns.sendRowMessage: the template must belong to the row's organization" {
		t.Fatalf("the failure's row is %v", row)
	}
	w.lane.outcome = ""
	again := w.sendTerms(t, p)
	if again["state"] != SendSent || again["attempt"] != float64(1) || len(w.lane.calls) != 2 ||
		w.lane.calls[1]["requestId"] != w.lane.calls[0]["requestId"] {
		t.Fatalf("the retry answered %v after %d calls", again, len(w.lane.calls))
	}
}

// A BUSY RECEIPT LOCK IS NOT A FAILURE: the node that finds the lane's lock
// held (another node is sending this email) records nothing, so it cannot win
// the fence against the node that sent, and answers that another send holds it.
func TestABusyReceiptLockRecordsNothing(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.lane.outcome = "busy"
	_, err := p.sendTermsEmail(automation(), map[string]any{"applicationId": linkApp}, 0)
	if err == nil || !strings.Contains(err.Error(), "another send of the terms email") {
		t.Fatalf("a busy lock answered %v", err)
	}
	if len(w.sends) != 0 || w.count("putEmailSendRow") != 0 {
		t.Fatalf("a busy lock wrote %d email rows: %v", w.count("putEmailSendRow"), w.sends)
	}
	w.lane.outcome = ""
	if got := w.sendTerms(t, p); got["state"] != SendSent || got["attempt"] != float64(1) {
		t.Fatalf("the same call once the lock was free answered %v", got)
	}
}

// TWO NODES AT ONCE: both read no row and reach the lane; the lane sends once,
// and the node whose write loses the fence answers the row the other wrote,
// never writes its own outcome over it.
func TestTwoNodesAtOnceWriteOneRecordAndSendOnce(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	rowID := EmailSendRowID(EmailTerms, linkApp, 1)
	other := true
	w.lane.during = func() {
		if !other {
			return
		}
		other = false
		// The other node's whole send lands while this one is in the lane.
		w.lane.during = nil
		w.sendTerms(t, p)
		w.lane.during = func() {}
	}
	got := w.sendTerms(t, p)
	if got["state"] != SendSent || len(w.lane.sent) != 1 || w.lane.resolved != 1 || w.count("putEmailSendRow") != 1 {
		t.Fatalf("answered %v; %d sent, %d resolver calls, %d row writes", got, len(w.lane.sent), w.lane.resolved, w.count("putEmailSendRow"))
	}
	if w.versions[rowID] != 1 {
		t.Fatalf("the row has %d versions, want the other node's one", w.versions[rowID])
	}
}

// decide records one more decision of transition on the application.
func (w *emailWorld) decide(transition string) {
	w.decisions = append(w.decisions, map[string]any{"id": "dec-" + transition, "applicationId": applicationConcept + ":" + linkApp,
		"transition": transition, "decidedBy": emailStaff, "accountId": "acct-links", "createdAt": w.next()})
}

// application is wholesaleGet's answer for the store's person.
func (w *emailWorld) application(t *testing.T, p *Provider) map[string]any {
	t.Helper()
	nodes, err := p.get(staffCtx(emailStaff), map[string]any{"applicationId": linkApp}, 0)
	return reply(t, nodes, err)
}

// resend is a person of the store sending kind again.
func (w *emailWorld) resend(t *testing.T, p *Provider, kind string) (map[string]any, error) {
	t.Helper()
	nodes, err := p.resendEmail(staffCtx(emailStaff), map[string]any{"applicationId": linkApp, "kind": kind}, 0)
	if err != nil {
		return nil, err
	}
	return decodeOne(t, nodes, nil), nil
}

// emailShown is the application's email of kind and attempt as its page shows it.
func emailShown(t *testing.T, application map[string]any, kind string, attempt int) map[string]any {
	t.Helper()
	emails, _ := application["emails"].([]any)
	for _, e := range emails {
		m, _ := e.(map[string]any)
		if m["kind"] == kind && m["attempt"] == float64(attempt) {
			return m
		}
	}
	t.Fatalf("the application shows no %s email attempt %d: %v", kind, attempt, application["emails"])
	return nil
}

// A HELD EMAIL IS NOT SENT AND NOT THE END, for a rejection and a removal: the
// row says held with the lane's own sentence, the automation is answered that
// it was not sent, the page offers to send it again, and once the template is
// published the person's resend sends it once -- after which a rejection or
// removal that was sent is never sent again.
func TestAHeldRejectionOrRemovalIsSentOnceByAPersonOnceTheTemplateIsPublished(t *testing.T) {
	useTestLaneReaders(t)
	for _, c := range []struct {
		kind, transition, act string
	}{
		{EmailRejected, TransitionReject, ActResendRejected},
		{EmailRevoked, TransitionRevoke, ActResendRevoked},
	} {
		t.Run(c.kind, func(t *testing.T) {
			w := newEmailWorld()
			p := w.provider()
			if c.kind == EmailRevoked {
				w.approveAndGrant(EntitlementGranted)
			}
			w.decide(c.transition)
			w.lane.outcome = "hold"
			_, err := p.sendBuyerEmail(automation(), map[string]any{"applicationId": linkApp, "kind": c.kind}, 0)
			if err == nil || !strings.Contains(err.Error(), "Publish “Wholesale removal” to send this email.") || !strings.Contains(err.Error(), "not sent") {
				t.Fatalf("a held %s email answered %v; want it answered as not sent, with the lane's sentence", c.kind, err)
			}
			row := w.sends[EmailSendRowID(c.kind, linkApp, 1)]
			if row["state"] != SendHeld || row["reason"] != "Publish “Wholesale removal” to send this email." {
				t.Fatalf("the held email's row is %v", row)
			}
			if acts := fmt.Sprint(w.application(t, p)["acts"]); !strings.Contains(acts, c.act) {
				t.Fatalf("acts = %s, want %s", acts, c.act)
			}
			// THE AUTOMATIC PATH IS UNCHANGED: the same call again is answered from the
			// row, and asks nothing of the lane.
			if _, err := p.sendBuyerEmail(automation(), map[string]any{"applicationId": linkApp, "kind": c.kind}, 0); err == nil || len(w.lane.calls) != 1 {
				t.Fatalf("a re-delivered event for the held email answered %v after %d lane calls", err, len(w.lane.calls))
			}
			w.lane.outcome = "" // the template is published
			got, err := w.resend(t, p, c.kind)
			if err != nil || got["state"] != SendSent || got["attempt"] != float64(2) || got["requestedBy"] != emailStaff || len(w.lane.sent) != 1 {
				t.Fatalf("the resend answered %v, %v after %d sent", got, err, len(w.lane.sent))
			}
			if acts := fmt.Sprint(w.application(t, p)["acts"]); strings.Contains(acts, c.act) {
				t.Fatalf("acts = %s after the email was sent", acts)
			}
			if _, err := w.resend(t, p, c.kind); err == nil || len(w.lane.calls) != 2 {
				t.Fatalf("a second resend of a sent %s email answered %v after %d lane calls", c.kind, err, len(w.lane.calls))
			}
		})
	}
}

// A REJECTION THAT WAS SENT OFFERS NO RESEND, and one asked for is refused
// without calling the lane.
func TestASentRejectionOffersNoResend(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.decide(TransitionReject)
	if got := w.sendBuyer(t, p, EmailRejected); got["state"] != SendSent {
		t.Fatalf("the rejection answered %v", got)
	}
	if acts := fmt.Sprint(w.application(t, p)["acts"]); acts != "[]" {
		t.Fatalf("acts = %s", acts)
	}
	if _, err := w.resend(t, p, EmailRejected); err == nil || len(w.lane.calls) != 1 {
		t.Fatalf("a resend of a sent rejection answered %v after %d lane calls", err, len(w.lane.calls))
	}
	// A rejection the automation never sent (no template then) is not sent by a
	// resend either: no attempt of it did not reach the provider.
	w2 := newEmailWorld()
	p2 := w2.provider()
	w2.decide(TransitionReject)
	if _, err := w2.resend(t, p2, EmailRejected); err == nil || len(w2.lane.calls) != 0 {
		t.Fatalf("a resend of a rejection never attempted answered %v", err)
	}
}

// AN UNCERTAIN APPROVAL IS RECORDED UNCERTAIN, answered as not sent, and the
// page offers to send it again; a DEFERRED one is recorded deferred, and a
// resend while it is on its way is refused with a sentence that says so.
func TestAnUncertainApprovalOffersAResendAndADeferredOneRefusesIt(t *testing.T) {
	useTestLaneReaders(t)
	w := newEmailWorld()
	p := w.provider()
	w.approveAndGrant(EntitlementGranted)
	w.lane.outcome = "uncertain"
	_, err := p.sendBuyerEmail(automation(), map[string]any{"applicationId": linkApp, "kind": EmailApproved}, 0)
	if err == nil {
		t.Fatal("an uncertain hand-over was answered as a success")
	}
	if strings.Contains(err.Error(), "connection to the provider") {
		t.Fatalf("the answer carries the transport's own words: %v", err)
	}
	if row := w.sends[EmailSendRowID(EmailApproved, linkApp, 1)]; row["state"] != SendUncertain {
		t.Fatalf("the uncertain email's row is %v", row)
	}
	if acts := fmt.Sprint(w.application(t, p)["acts"]); acts != "[remove resendApproved]" {
		t.Fatalf("acts = %s", acts)
	}
	w.lane.outcome = "defer"
	if _, err := w.resend(t, p, EmailApproved); err == nil || !strings.Contains(err.Error(), "send-rate limit") {
		t.Fatalf("a deferred resend answered %v; want it answered as not sent, with the lane's sentence", err)
	}
	if row := w.sends[EmailSendRowID(EmailApproved, linkApp, 2)]; row["state"] != SendDeferred {
		t.Fatalf("the deferred email's row is %v", row)
	}
	calls := len(w.lane.calls)
	w.lane.outcome = ""
	_, err = w.resend(t, p, EmailApproved)
	if err == nil || !strings.Contains(err.Error(), "waiting") || len(w.lane.calls) != calls {
		t.Fatalf("a resend while the last attempt is deferred answered %v after %d lane calls, want a refusal and none", err, len(w.lane.calls)-calls)
	}
}

// A DEFERRED EMAIL THE LANE LATER SENT READS SENT, with the lane's time, on the
// application's page and in the export; one the lane records as failed reads
// failed. The lane's status is the truth, read when asked.
func TestADeferredEmailTheLaneSentReadsSentOnThePageAndInTheExport(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.decide(TransitionReject)
	w.lane.outcome = "defer"
	if _, err := p.sendBuyerEmail(automation(), map[string]any{"applicationId": linkApp, "kind": EmailRejected}, 0); err == nil {
		t.Fatal("a deferral was answered as a success")
	}
	sentAt := w.clock.Add(90 * time.Second).UTC().Format(time.RFC3339Nano)
	w.lane.statuses = []map[string]any{{"requestId": LaneRequestID(EmailRejected, linkApp, 1), "status": "sent", "sentAt": sentAt}}
	e := emailShown(t, w.application(t, p), EmailRejected, 1)
	if e["state"] != SendSent || e["sentAt"] != sentAt || e["reason"] != "" {
		t.Fatalf("the replayed email shows %v, want sent at the lane's time", e)
	}
	nodes, err := p.exportStore(staffCtx(emailStaff), map[string]any{"siteId": linkSite, "storeId": linkStore}, 0)
	doc := asString(reply(t, nodes, err)["json"])
	var exported struct {
		EmailSends []map[string]any `json:"emailSends"`
	}
	if err := json.Unmarshal([]byte(doc), &exported); err != nil || len(exported.EmailSends) != 1 {
		t.Fatalf("the export's emails: %v %v", err, exported.EmailSends)
	}
	if got := exported.EmailSends[0]; got["state"] != SendSent || got["sentAt"] != sentAt {
		t.Fatalf("the export shows the replayed email as %v", got)
	}
	w.lane.statuses[0]["status"] = "failed"
	w.lane.statuses[0]["sentAt"] = ""
	if e := emailShown(t, w.application(t, p), EmailRejected, 1); e["state"] != SendFailed || strings.Contains(asString(e["reason"]), "send-rate limit") {
		t.Fatalf("an email the lane failed shows %v", e)
	}
}

// ---------------------------------------------------------------------------
// The resolver
// ---------------------------------------------------------------------------

func (w *emailWorld) resolve(t *testing.T, p *Provider, kind string) (campaigns.RowMessage, error) {
	t.Helper()
	return p.resolveRowMessage(auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), linkMerchant)),
		campaigns.RowMessageRequest{Concept: applicationConcept, RowID: linkApp, AccountID: "acct-links", Kind: kind,
			RequestID: LaneRequestID(kind, linkApp, 1)})
}

// THE RESOLVER MINTS THE TERMS LINK and answers the applicant's address, the
// application's organization and the closed set of fields, the expiry as the
// day in UTC.
func TestTheResolverMintsTheTermsLinkAndAnswersTheClosedSetOfFields(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	msg, err := w.resolve(t, p, EmailTerms)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if msg.Email != linkEmail || msg.AccountID != "acct-links" {
		t.Fatalf("the resolver answered %q in %q", msg.Email, msg.AccountID)
	}
	keys := make([]string, 0, len(msg.Fields))
	for k := range msg.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := "applicantName companyName priceSheetUrl signInEmail storeName termsExpiresOn termsUrl"; strings.Join(keys, " ") != want {
		t.Fatalf("the fields are %v, want %s", keys, want)
	}
	link, err := url.Parse(msg.Fields["termsUrl"])
	if err != nil || link.Scheme != "https" || link.Host != emailHost || link.Path != "/wholesale/terms" {
		t.Fatalf("termsUrl = %q", msg.Fields["termsUrl"])
	}
	token := link.Query().Get("t")
	if w.termsLink()["tokenHash"] != sha256Hex(token) || w.writes[len(w.writes)-1].actor != linkMerchant {
		t.Fatalf("the link written does not carry the minted token's hash, or was not written as the merchant")
	}
	for k, want := range map[string]string{"companyName": "Kestrel Cycles", "applicantName": "Ada Example", "signInEmail": linkEmail,
		"storeName": "Kestrel Cycles Trade", "termsExpiresOn": "December 1, 2026", "priceSheetUrl": ""} {
		if msg.Fields[k] != want {
			t.Errorf("%s = %q, want %q", k, msg.Fields[k], want)
		}
	}
	// A site with no title is named by its host.
	w.site["title"] = ""
	if msg, _ := w.resolve(t, p, EmailTerms); msg.Fields["storeName"] != emailHost {
		t.Fatalf("storeName = %q, want the host", msg.Fields["storeName"])
	}
	w.requireNoToken(t, token)
}

// THE RESOLVER WITHDRAWS A TERMS EMAIL FOR A REJECTED APPLICATION, mints
// nothing, and the lane's skip is what the email's row says.
func TestTheResolverWithdrawsATermsEmailForARejectedApplication(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.decisions = append(w.decisions, map[string]any{"id": "dec-r", "applicationId": applicationConcept + ":" + linkApp,
		"transition": TransitionReject, "createdAt": w.next()})
	_, err := w.resolve(t, p, EmailTerms)
	var withdrawn campaigns.RowWithdrawn
	if !errors.As(err, &withdrawn) || withdrawn.Reason != "the application is no longer waiting for terms" {
		t.Fatalf("the resolver answered %v", err)
	}
	if w.count("putPrivateLinkRow") != 0 {
		t.Fatal("a withdrawn email minted a link")
	}
	// And an approval email while the access is not granted.
	w2 := newEmailWorld()
	p2 := w2.provider()
	w2.approveAndGrant(EntitlementFailed)
	if _, err := w2.resolve(t, p2, EmailApproved); !errors.As(err, &withdrawn) || withdrawn.Reason != "access is not granted" {
		t.Fatalf("the approval resolver answered %v", err)
	}
}

// A RESEND OF TERMS IS ATTEMPT TWO: its own row and request id, the person
// recorded, a new token -- and the token before it opens nothing.
func TestAResendOfTermsIsAttemptTwoAndEndsTheOldLink(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.sendTerms(t, p)
	oldToken := tokenIn(w.lane.sent[0].Fields["termsUrl"])
	nodes, err := p.resendEmail(staffCtx(emailStaff), map[string]any{"applicationId": linkApp, "kind": EmailTerms}, 0)
	got := reply(t, nodes, err)
	if got["attempt"] != float64(2) || got["state"] != SendSent || got["requestedBy"] != emailStaff ||
		got["requestId"] != LaneRequestID(EmailTerms, linkApp, 2) {
		t.Fatalf("the resend answered %v", got)
	}
	if len(w.lane.sent) != 2 || w.sends[EmailSendRowID(EmailTerms, linkApp, 2)] == nil {
		t.Fatalf("%d sent; rows %v", len(w.lane.sent), w.sends)
	}
	newToken := tokenIn(w.lane.sent[1].Fields["termsUrl"])
	if newToken == oldToken {
		t.Fatal("the resend carried the old token")
	}
	if _, err := p.termsForLink(shopper(), termsArgs(oldToken), 0); refusalCode(err) != RefusalLinkInvalid {
		t.Fatalf("the old token answered %v, want link_invalid", err)
	}
	if _, err := p.termsForLink(shopper(), termsArgs(newToken), 0); err != nil {
		t.Fatalf("the new token answered %v", err)
	}
	w.requireNoToken(t, oldToken)
	w.requireNoToken(t, newToken)
}

// AN OPERATOR CANNOT RESEND, and neither can a call with no signed-in person;
// a resend of an email that is not due is refused by name.
func TestAnOperatorCannotResend(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	for name, ctx := range map[string]context.Context{"operator": operatorCtx("op-1"), "nobody": context.Background()} {
		if _, err := p.resendEmail(ctx, map[string]any{"applicationId": linkApp, "kind": EmailTerms}, 0); err == nil {
			t.Fatalf("the %s resent an email", name)
		}
	}
	if _, err := p.resendEmail(staffCtx(emailStaff), map[string]any{"applicationId": linkApp, "kind": EmailApproved}, 0); err == nil ||
		!strings.Contains(err.Error(), "only to a buyer whose access is granted") {
		t.Fatalf("an approval resend on a waiting application answered %v", err)
	}
	if _, err := p.resendEmail(staffCtx(emailStaff), map[string]any{"applicationId": linkApp, "kind": EmailRejected}, 0); err == nil {
		t.Fatal("a rejection was resent")
	}
	if len(w.lane.calls) != 0 {
		t.Fatalf("%d lane calls", len(w.lane.calls))
	}
}

// THE INTERNAL SENDS REFUSE A CALL WITHOUT INTERNAL ORIGIN, before they read.
func TestTheInternalSendsRefuseACallWithoutInternalOrigin(t *testing.T) {
	p := &Provider{reader: refusingReader{t}, writer: &fakeWriter{err: errors.New("no write")}}
	for name, call := range map[string]func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error){
		"wholesaleSendTermsEmail": p.sendTermsEmail, "wholesaleSendBuyerEmail": p.sendBuyerEmail, "wholesaleNotifyStaff": p.notifyStaff,
	} {
		for who, ctx := range map[string]context.Context{"staff": staffCtx(emailStaff), "operator": operatorCtx("op-1"),
			"merchant": auth.ContextWithUserActor(context.Background(), linkMerchant)} {
			_, err := call(ctx, map[string]any{"applicationId": linkApp, "kind": EmailApproved, "event": StaffEventReadyToReview}, 0)
			if err == nil || !strings.Contains(err.Error(), "called by the store's automation") {
				t.Errorf("%s by the %s answered %v", name, who, err)
			}
		}
	}
}

// THE APPLICATION SHOWS ITS EMAILS with the lane's own record of each, read
// for the application's organization, and offers the resend acts.
func TestTheApplicationShowsItsEmailsAndTheResendActs(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.sendTerms(t, p)
	w.lane.statuses = []map[string]any{{"requestId": LaneRequestID(EmailTerms, linkApp, 1), "status": "sent",
		"skipReason": "", "providerStatus": "Delivered", "sentAt": w.clock.Format(time.RFC3339Nano)}}
	nodes, err := p.get(staffCtx(emailStaff), map[string]any{"applicationId": linkApp}, 0)
	got := reply(t, nodes, err)
	emails, _ := got["emails"].([]any)
	if len(emails) != 1 {
		t.Fatalf("emails = %v", got["emails"])
	}
	e := emails[0].(map[string]any)
	d, _ := e["delivery"].(map[string]any)
	if e["kind"] != EmailTerms || e["attempt"] != float64(1) || e["state"] != SendSent || d["status"] != "sent" || d["providerStatus"] != "Delivered" {
		t.Fatalf("the email is %v", e)
	}
	if len(w.lane.asked) != 1 || w.lane.asked[0] != "acct-links" {
		t.Fatalf("the status read named %v, want the application's organization", w.lane.asked)
	}
	if acts := fmt.Sprint(got["acts"]); acts != "[reject resendTerms]" {
		t.Fatalf("acts = %s", acts)
	}
}

func TestActsOfferTheResendsOnlyWhenTheyAreDue(t *testing.T) {
	for _, c := range []struct {
		in   ActsInput
		want string
	}{
		{ActsInput{Stage: StageWaitingForTerms, TermsRequired: true, TermsTemplateChosen: true}, "[reject resendTerms]"},
		{ActsInput{Stage: StageWaitingForTerms, TermsRequired: true}, "[reject]"},
		{ActsInput{Stage: StateApproved, AccessState: EntitlementGranted, EntitlementExists: true, ApprovedEmailSent: true}, "[remove resendApproved]"},
		{ActsInput{Stage: StateApproved, AccessState: EntitlementGranted, EntitlementExists: true}, "[remove]"},
		{ActsInput{Stage: StateApproved, AccessState: EntitlementPending, EntitlementExists: true, ApprovedEmailSent: true}, "[remove retryAccess]"},
		{ActsInput{Stage: StateApproved, AccessState: EntitlementGranted, EntitlementExists: true, ApprovedEmailSent: true, Operator: true}, "[]"},
		{ActsInput{Stage: StageWaitingForTerms, TermsRequired: true, TermsTemplateChosen: true, Operator: true}, "[]"},
		{ActsInput{Stage: StateRejected, LatestRejectedEmail: SendHeld}, "[resendRejected]"},
		{ActsInput{Stage: StateRejected, LatestRejectedEmail: SendUncertain}, "[resendRejected]"},
		{ActsInput{Stage: StateRejected, LatestRejectedEmail: SendFailed}, "[resendRejected]"},
		{ActsInput{Stage: StateRejected, LatestRejectedEmail: SendSent}, "[]"},
		{ActsInput{Stage: StateRejected, LatestRejectedEmail: SendDeferred}, "[]"},
		{ActsInput{Stage: StateRejected, LatestRejectedEmail: SendSkipped}, "[]"},
		{ActsInput{Stage: StateRejected}, "[]"},
		{ActsInput{Stage: StateRejected, LatestRejectedEmail: SendHeld, Operator: true}, "[]"},
		{ActsInput{Stage: StateRevoked, AccessState: EntitlementRevoked, EntitlementExists: true, LatestRevokedEmail: SendHeld}, "[resendRevoked]"},
		{ActsInput{Stage: StateRevoked, AccessState: EntitlementPending, EntitlementExists: true, LatestRevokedEmail: SendFailed}, "[retryAccess resendRevoked]"},
		{ActsInput{Stage: StateRevoked, AccessState: EntitlementRevoked, EntitlementExists: true, LatestRevokedEmail: SendSent}, "[]"},
		{ActsInput{Stage: StateApproved, AccessState: EntitlementGranted, EntitlementExists: true, LatestRejectedEmail: SendHeld}, "[remove]"},
	} {
		if got := fmt.Sprint(ActsFor(c.in)); got != c.want {
			t.Errorf("ActsFor(%+v) = %s, want %s", c.in, got, c.want)
		}
	}
}

// THE EXPORT CARRIES THE EMAIL ROWS, and no token.
func TestTheExportCarriesTheEmailSends(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.sendTerms(t, p)
	nodes, err := p.exportStore(staffCtx(emailStaff), map[string]any{"siteId": linkSite, "storeId": linkStore}, 0)
	got := reply(t, nodes, err)
	doc := asString(got["json"])
	if !strings.Contains(doc, `"emailSends"`) || !strings.Contains(doc, LaneRequestID(EmailTerms, linkApp, 1)) {
		t.Fatalf("the export carries no email rows: %s", doc)
	}
	if strings.Contains(doc, tokenIn(w.lane.sent[0].Fields["termsUrl"])) {
		t.Fatal("the export carries the token")
	}
}

// ---------------------------------------------------------------------------
// The settings
// ---------------------------------------------------------------------------

// ONLY AN OPERATOR CHOOSES THE SENDER; the store's people choose the templates,
// from the ones they are offered.
func TestOnlyAnOperatorChoosesTheSender(t *testing.T) {
	t.Setenv("MEMQL_OUTBOUND_EMAIL_ALLOWLIST", "")
	w := newEmailWorld()
	p := w.provider()
	update := func(ctx context.Context, change map[string]any) error {
		args := map[string]any{"siteId": linkSite, "storeId": linkStore}
		for k, v := range change {
			args[k] = v
		}
		_, err := p.updateSettings(ctx, args, 0)
		return err
	}
	if err := update(staffCtx(emailStaff), map[string]any{"senderIdentityId": "sender-1"}); err == nil || err.Error() != OnlyOperatorsChooseTheSenderSentence {
		t.Fatalf("a staff member choosing the sender answered %v", err)
	}
	if w.count("putWholesaleSettingsRow") != 0 {
		t.Fatal("a refused sender was written")
	}
	// The sender the row already holds may be named back by anybody.
	if err := update(staffCtx(emailStaff), map[string]any{"senderIdentityId": "", "approvedTemplateId": "tpl-approved"}); err != nil {
		t.Fatalf("a staff member naming the held sender: %v", err)
	}
	if err := update(operatorCtx("op-1"), map[string]any{"senderIdentityId": "sender-1"}); err != nil {
		t.Fatalf("an operator choosing the sender: %v", err)
	}
	if w.settings["senderIdentityId"] != "sender-1" || w.settings["termsTemplateId"] != termsTemplateTestID || w.settings["approvedTemplateId"] != "tpl-approved" {
		t.Fatalf("the row is %v", w.settings)
	}
	for _, bad := range []string{"tpl-news", "tpl-other-org", "tpl-unknown"} {
		if err := update(staffCtx(emailStaff), map[string]any{"rejectedTemplateId": bad}); err == nil {
			t.Fatalf("%s was accepted as a template", bad)
		}
	}
	if err := update(staffCtx(emailStaff), map[string]any{"rejectedTemplateId": ""}); err != nil || w.settings["rejectedTemplateId"] != "" {
		t.Fatalf("choosing none: %v, %v", err, w.settings["rejectedTemplateId"])
	}
	nodes, err := p.storeSettings(staffCtx(emailStaff), map[string]any{"siteId": linkSite, "storeId": linkStore}, 0)
	got := reply(t, nodes, err)
	if fmt.Sprint(got["templates"]) != "[map[id:tpl-terms name:Your trade terms] map[id:tpl-approved name:Trade prices on]]" ||
		got["senderIdentityId"] != "sender-1" || got["termsTemplateId"] != termsTemplateTestID || got["staffEmailAdmitted"] != false {
		t.Fatalf("the settings answered %v", got)
	}
	if may, _ := got["may"].(map[string]any); may["setSender"] != false {
		t.Fatalf("a staff member may set the sender: %v", may)
	}
}

// ---------------------------------------------------------------------------
// The staff notices
// ---------------------------------------------------------------------------

func (w *emailWorld) notify(t *testing.T, p *Provider, args map[string]any) map[string]any {
	t.Helper()
	args["applicationId"] = linkApp
	nodes, err := p.notifyStaff(automation(), args, 0)
	return reply(t, nodes, err)
}

func TestAStaffNoticeWithNoAddressIsSkipped(t *testing.T) {
	w := newEmailWorld()
	w.settings["staffEmail"] = ""
	p := w.provider()
	got := w.notify(t, p, map[string]any{"event": StaffEventReadyToReview})
	if got["status"] != "skipped" || got["reason"] != "the store names no address" || w.stages != 0 {
		t.Fatalf("answered %v with %d stages", got, w.stages)
	}
}

// THE PLACEHOLDERS ARE THE CLOSED SET, and a client's own field named in a
// template stays as written: nothing of the extension is reachable.
func TestStaffPlaceholdersAreFilledAndTheExtensionIsUnreachable(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.approveAndGrant(EntitlementGranted)
	got := w.notify(t, p, map[string]any{"event": StaffEventApproved, "decisionId": "dec-approve",
		"subject": "{{companyName}} is {{state}} ({{taxNumber}})",
		"body":    "{{applicantName}} <{{applicantEmail}}> by {{decidedBy}}: {{note}} {{adminUrl}} {{taxNumber}} {{ companyName }}"})
	want := StaffRequestID(StaffEventApproved, linkApp, "dec-approve")
	if got["status"] != "staged" || got["requestId"] != want || want != "wholesale-staff-approved-"+linkApp+"-dec-approve" {
		t.Fatalf("answered %v", got)
	}
	row := w.staged[want]
	if row["subject"] != "Kestrel Cycles is Approved ({{taxNumber}})" ||
		row["body"] != "Ada Example <"+linkEmail+"> by Rosa Staff: Known to us. https://os.example.test/ {{taxNumber}} {{ companyName }}" {
		t.Fatalf("staged %q / %q", row["subject"], row["body"])
	}
	if row["target"] != "desk@example.com" || row["medium"] != "email" || row["dedupeKey"] != want || row["requestedBy"] != "wholesale:staffNotice" {
		t.Fatalf("staged %v", row)
	}
	if got := fillPlaceholders("{{note}}", map[string]string{"note": "{{adminUrl}}", "adminUrl": "x"}); got != "{{adminUrl}}" {
		t.Fatalf("a value was filled again: %q", got)
	}
	// A revoked application is Removed to the store's people, and each event has
	// its own defaults.
	w.decisions = append(w.decisions, map[string]any{"id": "dec-revoke", "applicationId": applicationConcept + ":" + linkApp,
		"transition": TransitionRevoke, "decidedBy": emailStaff, "createdAt": w.next()})
	w.notify(t, p, map[string]any{"event": StaffEventRevoked, "decisionId": "dec-revoke", "subject": "{{state}}"})
	if row := w.staged[StaffRequestID(StaffEventRevoked, linkApp, "dec-revoke")]; row["subject"] != "Removed" ||
		!strings.Contains(asString(row["body"]), "Rosa Staff removed Kestrel Cycles") {
		t.Fatalf("the removal notice is %v", row)
	}
}

// A DECISION EVENT NAMES A DECISION OF THAT KIND ON THAT APPLICATION, and a
// readyToReview notice needs the application ready: a wrong event sends
// nothing.
func TestAWronglyWiredStaffEventSendsNothing(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.approveAndGrant(EntitlementGranted)
	for name, args := range map[string]map[string]any{
		"the wrong transition": {"event": StaffEventRejected, "decisionId": "dec-approve"},
		"no decision":          {"event": StaffEventApproved},
		"another decision":     {"event": StaffEventApproved, "decisionId": "dec-elsewhere"},
		"not ready":            {"event": StaffEventReadyToReview},
		"granted access":       {"event": StaffEventAccessNotGranted},
		"the other direction":  {"event": StaffEventAccessNotRemoved},
		"an unknown event":     {"event": "approvedd"},
	} {
		args["applicationId"] = linkApp
		if _, err := p.notifyStaff(automation(), args, 0); err == nil {
			t.Errorf("%s was staged", name)
		}
	}
	if w.stages != 0 {
		t.Fatalf("%d notices staged", w.stages)
	}
}

// TWO NODES TELL ONE EVENT AT ONCE: the other node's notice is staged, and
// sent, between this node's check and its write. The check is a fresh read,
// the stage is fenced on no row at the id, and the loser answers alreadyStaged
// and writes nothing -- so the sent notice stays sent and is not staged again.
func TestTwoNodesStagingOneNoticeStageItOnce(t *testing.T) {
	w := newEmailWorld()
	w.settings["termsRequired"] = false
	p := w.provider()
	id := StaffRequestID(StaffEventReadyToReview, linkApp, "")
	w.afterStagedRead = func() {
		w.stages++
		w.staged[id] = map[string]any{"id": id, "requestId": id, "status": "sent", "target": "desk@example.com"}
	}
	got := w.notify(t, p, map[string]any{"event": StaffEventReadyToReview})
	if got["alreadyStaged"] != true || got["status"] != "staged" || got["requestId"] != id {
		t.Fatalf("the node that lost answered %v", got)
	}
	if w.stages != 1 || w.staged[id]["status"] != "sent" {
		t.Fatalf("%d stages; the notice is %v, want the other node's, still sent", w.stages, w.staged[id])
	}
	for i, fresh := range w.stagedReadsFresh {
		if !fresh {
			t.Fatalf("outbox read %d was not a fresh read", i)
		}
	}
}

// A STAGE THAT FAILS SAYS SO WITHOUT THE NOTICE: the engine's error may quote
// the call it refused, and the call carries the store's address and a subject
// and body filled with the applicant's address, name and company. The error
// names the event and the application in a fixed sentence, and neither it nor
// a log line carries any of those values.
func TestAFailedStageCarriesNothingOfTheNotice(t *testing.T) {
	w := newEmailWorld()
	w.settings["termsRequired"] = false
	p := w.provider()
	subject := "Ready: {{companyName}} / {{applicantEmail}}"
	body := "{{applicantName}} <{{applicantEmail}}> of {{companyName}}"
	filled := fillPlaceholders(body, map[string]string{"companyName": "Kestrel Cycles", "applicantName": "Ada Example", "applicantEmail": linkEmail})
	w.stageErr = fmt.Errorf("memql: stageServerOutboundRequest(requestId: \"x\", target: \"desk@example.com\", subject: \"Ready: Kestrel Cycles / %s\", body: %q) was refused", linkEmail, filled)
	_, err := p.notifyStaff(automation(), map[string]any{"applicationId": linkApp, "event": StaffEventReadyToReview,
		"subject": subject, "body": body}, 0)
	if err == nil {
		t.Fatal("a refused stage answered no error")
	}
	for _, leaked := range []string{linkEmail, "Ada Example", "Kestrel Cycles", "desk@example.com", "Ready:", "stageServerOutboundRequest"} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("the error carries %q: %v", leaked, err)
		}
		if strings.Contains(w.log.String(), leaked) {
			t.Errorf("a log line carries %q: %s", leaked, w.log.String())
		}
	}
	if !strings.Contains(err.Error(), StaffEventReadyToReview) || !strings.Contains(err.Error(), linkApp) {
		t.Fatalf("the error does not name the event and the application: %v", err)
	}
}

// A COMPANY NAME WITH A LINE BREAK MAKES A ONE-LINE SUBJECT: the email
// integration refuses a subject with a line break, so a value filled into the
// subject is made one line; the body keeps its lines.
func TestAValueFilledIntoASubjectIsOneLine(t *testing.T) {
	w := newEmailWorld()
	w.settings["termsRequired"] = false
	w.apps[0]["companyName"] = "Kestrel\r\nCycles\tTrade\x00Co"
	p := w.provider()
	w.notify(t, p, map[string]any{"event": StaffEventReadyToReview, "body": "{{companyName}}\n\nend"})
	row := w.staged[StaffRequestID(StaffEventReadyToReview, linkApp, "")]
	if got := asString(row["subject"]); got != "A wholesale application is ready to review: Kestrel Cycles Trade Co" {
		t.Fatalf("the subject is %q", got)
	}
	if got := asString(row["body"]); got != "Kestrel\r\nCycles\tTrade\x00Co\n\nend" {
		t.Fatalf("the body is %q, want its lines kept", got)
	}
}

func TestAReDeliveredStaffEventStagesNothingNew(t *testing.T) {
	w := newEmailWorld()
	w.settings["termsRequired"] = false
	p := w.provider()
	first := w.notify(t, p, map[string]any{"event": StaffEventReadyToReview})
	second := w.notify(t, p, map[string]any{"event": StaffEventReadyToReview})
	if first["requestId"] != second["requestId"] || first["requestId"] != "wholesale-staff-readyToReview-"+linkApp ||
		second["alreadyStaged"] != true || w.stages != 1 {
		t.Fatalf("first %v, second %v, %d stages", first, second, w.stages)
	}
}

// ACCESS THAT DID NOT LAND, EITHER WAY: a failed grant tells the store's
// people once; the retry that fails again tells them again; the backfill
// re-writing that row tells nobody. The same for a removal.
func TestAccessThatDidNotLandIsToldOncePerAttempt(t *testing.T) {
	registerShippedAdapters()
	w := newEmailWorld()
	w.apps[0]["accountId"] = "" // so the backfill has the entitlement to tie
	p := w.provider()
	w.caller.err = errors.New("shopify: the connection dropped")
	w.decisions = append(w.decisions, map[string]any{"id": "dec-approve", "applicationId": applicationConcept + ":" + linkApp,
		"transition": TransitionApprove, "decidedBy": emailStaff, "accountId": "acct-links", "createdAt": w.next()})
	if _, err := p.provisionEntitlement(automation(), map[string]any{"applicationId": linkApp}, 0); err != nil {
		t.Fatalf("provision: %v", err)
	}
	last := func() map[string]any { return w.ents[len(w.ents)-1] }
	if last()["direction"] != DirectionGrant || last()["attempt"] != 1 || last()["state"] != EntitlementPending {
		t.Fatalf("the failed grant wrote %v", last())
	}
	notGranted := map[string]any{"event": StaffEventAccessNotGranted}
	if got := w.notify(t, p, notGranted); got["requestId"] != AccessStaffRequestID(StaffEventAccessNotGranted, linkApp, 1) ||
		got["requestId"] != "wholesale-staff-accessNotGranted-"+linkApp+"-attempt-1" || w.stages != 1 {
		t.Fatalf("the first failure answered %v with %d stages", got, w.stages)
	}
	w.notify(t, p, notGranted) // re-delivered
	if _, err := p.retryAccess(staffCtx(emailStaff), map[string]any{"applicationId": linkApp}, 0); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if last()["attempt"] != 2 || last()["direction"] != DirectionGrant {
		t.Fatalf("the retry wrote %v", last())
	}
	if got := w.notify(t, p, notGranted); got["alreadyStaged"] != false || w.stages != 2 {
		t.Fatalf("the second failure answered %v with %d stages", got, w.stages)
	}
	nodes, err := p.backfillAccounts(automation(), map[string]any{}, 0)
	reply(t, nodes, err)
	if last()["attempt"] != 2 || last()["direction"] != DirectionGrant || trimmed(last()["accountId"]) == "" {
		t.Fatalf("the backfill wrote %v, want attempt and direction carried and the row tied", last())
	}
	if got := w.notify(t, p, notGranted); got["alreadyStaged"] != true || w.stages != 2 {
		t.Fatalf("after the backfill the notice answered %v with %d stages", got, w.stages)
	}

	// THE REMOVAL. A granted row first, then a revoke decision whose removal fails.
	w.caller.err = nil
	if _, err := p.retryAccess(staffCtx(emailStaff), map[string]any{"applicationId": linkApp}, 0); err != nil {
		t.Fatalf("the grant: %v", err)
	}
	w.decisions = append(w.decisions, map[string]any{"id": "dec-revoke", "applicationId": applicationConcept + ":" + linkApp,
		"transition": TransitionRevoke, "decidedBy": emailStaff, "accountId": "acct-links", "createdAt": w.next()})
	w.caller.err = errors.New("shopify: the connection dropped")
	if _, err := p.revokeEntitlement(automation(), map[string]any{"applicationId": linkApp}, 0); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if last()["direction"] != DirectionRevoke || last()["state"] != EntitlementPending || last()["attempt"] != 4 {
		t.Fatalf("the failed removal wrote %v", last())
	}
	if _, err := p.notifyStaff(automation(), map[string]any{"applicationId": linkApp, "event": StaffEventAccessNotGranted}, 0); err == nil {
		t.Fatal("a failed removal was told as a grant that did not land")
	}
	notRemoved := map[string]any{"event": StaffEventAccessNotRemoved}
	if got := w.notify(t, p, notRemoved); got["requestId"] != AccessStaffRequestID(StaffEventAccessNotRemoved, linkApp, 4) || w.stages != 3 {
		t.Fatalf("the failed removal answered %v with %d stages", got, w.stages)
	}
	if _, err := p.retryAccess(staffCtx(emailStaff), map[string]any{"applicationId": linkApp}, 0); err != nil {
		t.Fatalf("retry the removal: %v", err)
	}
	if got := w.notify(t, p, notRemoved); got["alreadyStaged"] != false || w.stages != 4 {
		t.Fatalf("the second failed removal answered %v with %d stages", got, w.stages)
	}
	w.ents[len(w.ents)-1]["accountId"] = ""
	nodes, err = p.backfillAccounts(automation(), map[string]any{}, 0)
	reply(t, nodes, err)
	if got := w.notify(t, p, notRemoved); got["alreadyStaged"] != true || w.stages != 4 {
		t.Fatalf("after the backfill the removal notice answered %v with %d stages", got, w.stages)
	}
}

// A ROW WRITTEN BEFORE ROWS SAID WHICH WAY THEY WENT is read by its state where
// it can tell, and refused where it cannot.
func TestAnAccessRowWithNoDirectionIsReadByItsState(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.approveAndGrant(EntitlementPending)
	delete(w.ents[len(w.ents)-1], "direction")
	delete(w.ents[len(w.ents)-1], "attempt")
	if got := w.notify(t, p, map[string]any{"event": StaffEventAccessNotGranted}); got["requestId"] != AccessStaffRequestID(StaffEventAccessNotGranted, linkApp, 0) {
		t.Fatalf("an approved application's undirected pending row answered %v", got)
	}
	w.decisions = append(w.decisions, map[string]any{"id": "dec-revoke", "applicationId": applicationConcept + ":" + linkApp,
		"transition": TransitionRevoke, "createdAt": w.next()})
	for _, event := range []string{StaffEventAccessNotGranted, StaffEventAccessNotRemoved} {
		_, err := p.notifyStaff(automation(), map[string]any{"applicationId": linkApp, "event": event}, 0)
		if err == nil || !strings.Contains(err.Error(), "cannot tell which did not land") {
			t.Fatalf("%s on a revoked application's undirected pending row answered %v", event, err)
		}
	}
}

// THE STATUS READ STAMPS INTERNAL ORIGIN INLINE ON ITS ONE CALL, renders its
// request ids as a list, and carries no pack mark: that is the Shopify
// builtins' alone (ruling 57).
func TestTheStatusReadStampsInternalOriginOnItsOneCallAndNoMark(t *testing.T) {
	eng := &replyingEngine{}
	lane := &engineLane{caller: &engineCaller{engine: eng}}
	ctx := auth.ContextWithClientOrigin(staffCtx(emailStaff))
	if _, err := lane.RowMessageStatus(ctx, "acct-links", []string{"wholesale-terms-app-1-1", "wholesale-terms-app-1-2"}); err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(eng.origins) != 1 || !eng.origins[0].IsInternal() || eng.packs[0] != "" {
		t.Fatalf("the read ran at %v marked %v", eng.origins, eng.packs)
	}
	if auth.OriginFromContext(ctx).IsInternal() {
		t.Fatal("the stamp leaked into the caller's own context")
	}
	want := `builtin campaignRowMessageStatus(accountId: "acct-links", requestIds: ["wholesale-terms-app-1-1", "wholesale-terms-app-1-2"])`
	if eng.calls[0] != want {
		t.Fatalf("rendered %s, want %s", eng.calls[0], want)
	}
}
