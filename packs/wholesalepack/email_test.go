package wholesalepack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

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
	// senders answers the sender read by the sender id asked for; senderErr,
	// when set, fails it. sendersAsked is each read's account and sender.
	senders      map[string]map[string]any
	senderErr    error
	sendersAsked []string
	senderActors []string
	// statusErr, when set, fails every status read.
	statusErr error
	// unauthorized is the people without the send's authority in the
	// organization: the sender read answers one as the real lane answers an
	// organization it shows nothing of, and a send made as one is refused.
	unauthorized map[string]bool
	// sendActors is who each send was made as.
	sendActors []string
	// duringSender runs inside a sender read, before it answers: another node's
	// call.
	duringSender func()
	// reply, when set, answers a send in place of handing it over.
	reply map[string]any
}

// The row lane's own sentences, as the campaigns package answers them.
const (
	laneNoneChosen  = "No sender is chosen for this organization."
	laneSenderGone  = "The sender of this email no longer exists. Choose another sender to send it."
	laneNoAuthority = "No sender can be read for this organization."
	laneReadFailed  = "The sender could not be read just now."
)

func (l *fakeLane) SendRowMessage(ctx context.Context, args map[string]any) (map[string]any, error) {
	l.calls = append(l.calls, args)
	l.sendActors = append(l.sendActors, actorOf(ctx))
	if l.during != nil {
		l.during()
	}
	if l.unauthorized[actorOf(ctx)] {
		return nil, errors.New("wholesale: campaignSendRowMessage: campaign send requires write permission in this organization")
	}
	request := asString(args["requestId"])
	if reply, ok := l.receipts[request]; ok {
		out := copyRow(reply)
		out["replayed"] = true
		return out, nil
	}
	if l.reply != nil {
		return copyRow(l.reply), nil
	}
	switch l.outcome {
	case "suppressed":
		// THE ADDRESS IS ONE THE EMAIL SERVICE NO LONGER SENDS TO: the reason is the
		// lane's code, and beside it the sentence a person reads.
		return map[string]any{"sent": false, "skipped": true, "reason": "hard_bounce",
			"skipSentence": campaigns.SkipSentence("hard_bounce")}, nil
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
		// A WITHDRAWAL'S SENTENCE IS THE ROW'S OWN, as the lane answers it.
		reply := map[string]any{"sent": false, "skipped": true, "reason": withdrawn.Reason, "skipSentence": withdrawn.Reason}
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
	if l.statusErr != nil {
		return nil, l.statusErr
	}
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

func (l *fakeLane) RowSender(ctx context.Context, accountID, senderID string) (map[string]any, error) {
	l.sendersAsked = append(l.sendersAsked, accountID+"/"+senderID)
	l.senderActors = append(l.senderActors, actorOf(ctx))
	if l.duringSender != nil {
		l.duringSender()
	}
	if l.senderErr != nil {
		return nil, l.senderErr
	}
	refused := func(reason string, noneChosen bool) (map[string]any, error) {
		return map[string]any{"ready": false, "fromName": "", "address": "", "reason": reason,
			"readFailed": false, "noneChosen": noneChosen}, nil
	}
	if l.unauthorized[actorOf(ctx)] {
		// WITHOUT THE SEND'S AUTHORITY: as for an organization that does not exist.
		return refused(laneNoAuthority, false)
	}
	if l.senders == nil {
		// A WORLD THAT SCRIPTS NO SENDER sends as the store's one.
		return map[string]any{"ready": true, "fromName": "Kestrel Trade", "address": "trade@example.com", "reason": "",
			"readFailed": false, "noneChosen": false}, nil
	}
	if answer, ok := l.senders[senderID]; ok {
		return answer, nil
	}
	// AS THE REAL LANE ANSWERS: none chosen says so in its field; a sender named
	// that it does not have is one that no longer exists.
	if senderID == "" {
		return refused(laneNoneChosen, true)
	}
	return refused(laneSenderGone, false)
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
	// onSendsRead runs after each read of the application's email rows has been
	// answered, with how many such reads there have been: a test lands another
	// node's attempt between one of this node's reads and its write.
	onSendsRead func(n int)
	sendsReads  int
	site        map[string]any
	names       map[string]string
	tick        int
	lane        *fakeLane
	caller      *fakeCaller
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
		w.sendsReads++
		if w.onSendsRead != nil {
			w.onSendsRead(w.sendsReads)
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
	case "senderIdentityById":
		for _, sender := range []map[string]any{
			{"id": "v1:campaigns:senderIdentity:sender-1", "accountId": "acct-links", "status": "active"},
			{"id": "v1:campaigns:senderIdentity:sender-other", "accountId": "v1:accounts:account:acct-other", "status": "active"},
		} {
			if bareID(trimmed(sender["id"])) == bareID(args["senderIdentityId"]) {
				return []map[string]any{sender}, nil
			}
		}
		return nil, nil
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
	// ONE ROW, written twice: its record before the hand-over and its outcome.
	if len(w.lane.calls) != 1 || w.lane.resolved != 1 || len(w.sends) != 1 || w.count("putEmailSendRow") != 2 {
		t.Fatalf("%d lane calls, %d resolver calls, %d rows, %d row writes; want one, one, one and two",
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

// NOTHING IS MAILED FOR WHAT HAPPENED BEFORE THIS RELEASE. An application
// approved and granted before the pack sent emails has no email row, and its
// entitlement no attempt and no direction. The real backfill ties it with new
// versions; an automation wired on entitlement versions then asks for the
// approval email and the access notice, and nothing is sent, staged or
// recorded. A retry of the buyer's access after the release is attempt 1, and
// its approval email is sent once.
func TestNothingIsMailedForAnAccessGrantedBeforeEmailsAndARetryIsMailedOnce(t *testing.T) {
	registerShippedAdapters()
	w := newEmailWorld()
	w.apps[0]["accountId"] = ""
	p := w.provider()
	w.decisions = append(w.decisions, map[string]any{"id": "dec-approve", "applicationId": applicationConcept + ":" + linkApp,
		"transition": TransitionApprove, "decidedBy": emailStaff, "createdAt": w.next()})
	w.ents = append(w.ents, map[string]any{"id": "v1:wholesale:entitlement:" + EntitlementRowID(linkApp),
		"applicationId": applicationConcept + ":" + linkApp, "storeId": linkStore, "state": EntitlementGranted,
		"adapter": AdapterCustomerTag, "ownerUserId": linkMerchant, "createdAt": w.next()})
	before := len(w.ents)
	nodes, err := p.backfillAccounts(automation(), map[string]any{}, 0)
	reply(t, nodes, err)
	if len(w.ents) != before+1 {
		t.Fatalf("the backfill wrote %d entitlement versions, want one", len(w.ents)-before)
	}
	if _, has := w.ents[len(w.ents)-1]["attempt"]; has {
		t.Fatalf("the backfill gave an old row an attempt: %v", w.ents[len(w.ents)-1])
	}
	// THE AUTOMATION ON THE RE-WRITTEN VERSION.
	got := w.sendBuyer(t, p, EmailApproved)
	if got["state"] != SendSkipped || got["recorded"] != false || got["reason"] != ReasonAccessPredatesEmail {
		t.Fatalf("the approval email for an access granted before emails answered %v", got)
	}
	notice := w.notify(t, p, map[string]any{"event": StaffEventAccessNotGranted})
	if notice["status"] != "skipped" || notice["reason"] != ReasonAccessPredatesEmail {
		t.Fatalf("the access notice for an access record from before emails answered %v", notice)
	}
	if len(w.lane.calls) != 0 || w.stages != 0 || len(w.sends) != 0 {
		t.Fatalf("%d lane calls, %d notices staged, %d email rows for an old access", len(w.lane.calls), w.stages, len(w.sends))
	}
	// AN ACCESS RECORD FROM BEFORE EMAILS THAT DID NOT LAND is told of by no notice either.
	w.ents = append(w.ents, map[string]any{"id": "v1:wholesale:entitlement:" + EntitlementRowID(linkApp),
		"applicationId": applicationConcept + ":" + linkApp, "storeId": linkStore, "state": EntitlementFailed,
		"adapter": AdapterCustomerTag, "ownerUserId": linkMerchant, "accountId": "acct-links", "createdAt": w.next()})
	if notice := w.notify(t, p, map[string]any{"event": StaffEventAccessNotGranted}); notice["status"] != "skipped" || w.stages != 0 {
		t.Fatalf("a failed access record from before emails answered %v", notice)
	}
	// A RETRY AFTER THE RELEASE is attempt 1, and is mailed once. (This world
	// keeps the application's rows as seeded; the tie the backfill wrote is the
	// database test's to read.)
	w.apps[0]["accountId"] = "acct-links"
	if _, err := p.retryAccess(staffCtx(emailStaff), map[string]any{"applicationId": linkApp}, 0); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if last := w.ents[len(w.ents)-1]; last["attempt"] != 1 || last["state"] != EntitlementGranted {
		t.Fatalf("the retry wrote %v, want attempt 1 granted", last)
	}
	if got := w.sendBuyer(t, p, EmailApproved); got["state"] != SendSent || got["attempt"] != float64(1) || len(w.lane.sent) != 1 {
		t.Fatalf("the approval email after the retry answered %v after %d sent", got, len(w.lane.sent))
	}
	if again := w.sendBuyer(t, p, EmailApproved); again["state"] != SendSent || len(w.lane.calls) != 1 {
		t.Fatalf("a second call answered %v after %d lane calls", again, len(w.lane.calls))
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
// yet, and answered from its row by a second call, which calls no lane.
func TestTheSendBuiltinMintsNothingItself(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.lane.outcome = "defer"
	got := w.sendTerms(t, p)
	if got["state"] != SendDeferred || got["sent"] != false || !strings.Contains(asString(got["reason"]), "send-rate limit") {
		t.Fatalf("a deferral answered %v", got)
	}
	if row := w.sends[EmailSendRowID(EmailTerms, linkApp, 1)]; row["state"] != SendDeferred {
		t.Fatalf("the deferred attempt's row is %v", row)
	}
	if w.count("putPrivateLinkRow") != 0 || w.lane.resolved != 0 {
		t.Fatalf("a deferred send minted %d links and asked the resolver %d times", w.count("putPrivateLinkRow"), w.lane.resolved)
	}
	w.lane.outcome = ""
	if again := w.sendTerms(t, p); again["state"] != SendDeferred || len(w.lane.calls) != 1 {
		t.Fatalf("the deferred attempt answered %v after %d lane calls", again, len(w.lane.calls))
	}
}

// A FAILURE IS RECORDED AND ANSWERED AS NOT SENT -- a reply, not an error the
// engine would run the automation again for -- with the pack's own sentence
// (the lane's words stay on the row); and the attempt is made once: the next
// automatic call answers the row and calls no lane.
func TestALaneFailureIsRecordedAndNotTriedAgainAutomatically(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.lane.outcome = "fail"
	got := w.sendTerms(t, p)
	if got["state"] != SendFailed || got["sent"] != false || got["reason"] != reasonFailedForAutomation {
		t.Fatalf("a failure answered %v", got)
	}
	row := w.sends[EmailSendRowID(EmailTerms, linkApp, 1)]
	// THE LANE'S WORDS, AS A WHOLE SENTENCE: a capital first and a full stop last,
	// as every reason is answered.
	if row == nil || row["state"] != SendFailed || row["reason"] != "Campaigns.sendRowMessage: the template must belong to the row's organization." {
		t.Fatalf("the failure's row is %v", row)
	}
	w.lane.outcome = ""
	if again := w.sendTerms(t, p); again["state"] != SendFailed || again["attempt"] != float64(1) || len(w.lane.calls) != 1 {
		t.Fatalf("the next automatic call answered %v after %d lane calls", again, len(w.lane.calls))
	}
}

// A BUSY RECEIPT LOCK IS NOT A FAILURE: the attempt is recorded before the lane
// is asked, and a lane that finds its receipt lock for the request held says
// nothing of what became of the email, so the attempt is uncertain -- the
// lane's status read says what the other holder did -- and is not handed over
// again.
func TestABusyReceiptLockIsRecordedUncertain(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.lane.outcome = "busy"
	got := w.sendTerms(t, p)
	if got["state"] != SendUncertain || got["reason"] != ReasonUncertain || got["sent"] != false {
		t.Fatalf("a busy lock answered %v", got)
	}
	if row := w.sends[EmailSendRowID(EmailTerms, linkApp, 1)]; row["state"] != SendUncertain {
		t.Fatalf("a busy lock's row is %v", row)
	}
	w.lane.outcome = ""
	if again := w.sendTerms(t, p); again["state"] != SendUncertain || len(w.lane.calls) != 1 {
		t.Fatalf("the next automatic call answered %v after %d lane calls", again, len(w.lane.calls))
	}
}

// TWO NODES AT ONCE: the other node's call arrives while this one is in the
// lane, finds the attempt recorded, and hands over nothing; this one settles the
// row it recorded.
func TestTwoNodesAtOnceWriteOneRecordAndSendOnce(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	rowID := EmailSendRowID(EmailTerms, linkApp, 1)
	var other map[string]any
	w.lane.during = func() {
		w.lane.during = nil
		other = w.sendTerms(t, p)
	}
	got := w.sendTerms(t, p)
	if got["state"] != SendSent || len(w.lane.sent) != 1 || w.lane.resolved != 1 || len(w.lane.calls) != 1 || w.count("putEmailSendRow") != 2 {
		t.Fatalf("answered %v; %d sent, %d resolver calls, %d row writes", got, len(w.lane.sent), w.lane.resolved, w.count("putEmailSendRow"))
	}
	if other["state"] != SendDeferred || other["reason"] != reasonSending || w.versions[rowID] != 2 {
		t.Fatalf("the other node answered %v; the row has %d versions, want its record and its outcome", other, w.versions[rowID])
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
			held := w.sendBuyer(t, p, c.kind)
			if held["state"] != SendHeld || held["sent"] != false || held["reason"] != ReasonHeldForAutomation {
				t.Fatalf("a held %s email answered %v; want it answered as not sent, with the pack's sentence", c.kind, held)
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
			if again := w.sendBuyer(t, p, c.kind); again["state"] != SendHeld || len(w.lane.calls) != 1 {
				t.Fatalf("a re-delivered event for the held email answered %v after %d lane calls", again, len(w.lane.calls))
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
	got := w.sendBuyer(t, p, EmailApproved)
	if got["state"] != SendUncertain || got["sent"] != false {
		t.Fatalf("an uncertain hand-over answered %v", got)
	}
	if strings.Contains(fmt.Sprint(got), "connection to the provider") {
		t.Fatalf("the answer carries the transport's own words: %v", got)
	}
	if row := w.sends[EmailSendRowID(EmailApproved, linkApp, 1)]; row["state"] != SendUncertain {
		t.Fatalf("the uncertain email's row is %v", row)
	}
	if acts := fmt.Sprint(w.application(t, p)["acts"]); acts != "[remove resendApproved]" {
		t.Fatalf("acts = %s", acts)
	}
	w.lane.outcome = "defer"
	if deferred, err := w.resend(t, p, EmailApproved); err != nil || deferred["state"] != SendDeferred || deferred["sent"] != false ||
		!strings.Contains(asString(deferred["reason"]), "send-rate limit") || deferred["note"] != NoteMaybeSecondCopy {
		t.Fatalf("a deferred resend over an uncertain attempt answered %v, %v", deferred, err)
	}
	if row := w.sends[EmailSendRowID(EmailApproved, linkApp, 2)]; row["state"] != SendDeferred {
		t.Fatalf("the deferred email's row is %v", row)
	}
	calls := len(w.lane.calls)
	w.lane.outcome = ""
	_, err := w.resend(t, p, EmailApproved)
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
	if got := w.sendBuyer(t, p, EmailRejected); got["state"] != SendDeferred || got["sent"] != false {
		t.Fatalf("a deferral answered %v", got)
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

// worldIn is an email world whose application is where kind's email is due.
func worldIn(kind string) (*emailWorld, *Provider) {
	w := newEmailWorld()
	p := w.provider()
	switch kind {
	case EmailApproved:
		w.approveAndGrant(EntitlementGranted)
	case EmailRejected:
		w.decide(TransitionReject)
	case EmailRevoked:
		w.approveAndGrant(EntitlementGranted)
		w.decide(TransitionRevoke)
	}
	return w, p
}

// automatic is the store's automation sending kind, answered as a reply.
func (w *emailWorld) automatic(t *testing.T, p *Provider, kind string) map[string]any {
	t.Helper()
	if kind == EmailTerms {
		return w.sendTerms(t, p)
	}
	return w.sendBuyer(t, p, kind)
}

// N1. AN AUTOMATIC SEND MAKES ITS ATTEMPT AT MOST ONCE, and never after a
// person's resend: attempt 1 fails and is answered as not sent (not as an
// error the engine would run again); a person's resend sends attempt 2; the
// automatic call made again answers attempt 1's row and hands over nothing.
// For the terms and approval emails the resend's link still opens.
func TestAFailedAutomaticAttemptIsNeverSentAgainAfterAPersonsResend(t *testing.T) {
	for _, kind := range EmailKinds {
		t.Run(kind, func(t *testing.T) {
			w, p := worldIn(kind)
			w.lane.outcome = "fail"
			first := w.automatic(t, p, kind)
			if first["state"] != SendFailed || first["sent"] != false || first["attempt"] != float64(1) {
				t.Fatalf("the failed automatic attempt answered %v", first)
			}
			w.lane.outcome = ""
			resent, err := w.resend(t, p, kind)
			if err != nil || resent["state"] != SendSent || resent["attempt"] != float64(2) {
				t.Fatalf("the resend answered %v, %v", resent, err)
			}
			again := w.automatic(t, p, kind)
			if again["attempt"] != float64(1) || again["state"] != SendFailed {
				t.Fatalf("the automatic call after the resend answered %v, want attempt 1's row", again)
			}
			if len(w.lane.sent) != 1 {
				t.Fatalf("%d emails handed over, want the resend's one", len(w.lane.sent))
			}
			switch kind {
			case EmailTerms:
				if _, err := p.termsForLink(shopper(), termsArgs(tokenIn(w.lane.sent[0].Fields["termsUrl"])), 0); err != nil {
					t.Fatalf("the resend's terms link answered %v", err)
				}
			case EmailApproved:
				if _, err := p.priceSheetForLink(shopper(), termsArgs(tokenIn(w.lane.sent[0].Fields["priceSheetUrl"])), 0); err != nil {
					t.Fatalf("the resend's price-sheet link answered %v", err)
				}
			}
		})
	}
}

// AN AUTOMATIC CALL REPEATED TEN TIMES AFTER A FAILURE HANDS OVER NOTHING more.
func TestAnAutomaticCallRepeatedAfterAFailureHandsOverNothing(t *testing.T) {
	w, p := worldIn(EmailRejected)
	w.lane.outcome = "fail"
	w.automatic(t, p, EmailRejected)
	w.lane.outcome = ""
	for i := 0; i < 10; i++ {
		if got := w.automatic(t, p, EmailRejected); got["state"] != SendFailed || got["attempt"] != float64(1) {
			t.Fatalf("call %d answered %v", i, got)
		}
	}
	if len(w.lane.calls) != 1 || len(w.lane.sent) != 0 {
		t.Fatalf("%d lane calls and %d emails after one failure and ten more calls", len(w.lane.calls), len(w.lane.sent))
	}
}

// TWO AUTOMATIC CALLS AT ONCE FOR A FRESH ATTEMPT HAND OVER ONCE: the fenced
// record of the attempt is what admits the hand-over, so the second caller,
// arriving while the first is in the lane, finds the record and hands over
// nothing.
func TestTwoAutomaticCallsAtOnceForAFreshAttemptHandOverOnce(t *testing.T) {
	w, p := worldIn(EmailRejected)
	var nested map[string]any
	w.lane.during = func() {
		w.lane.during = nil
		nested = w.automatic(t, p, EmailRejected)
	}
	got := w.automatic(t, p, EmailRejected)
	if got["state"] != SendSent || len(w.lane.calls) != 1 || len(w.lane.sent) != 1 {
		t.Fatalf("answered %v after %d lane calls, %d sent", got, len(w.lane.calls), len(w.lane.sent))
	}
	if nested["attempt"] != float64(1) || nested["sent"] == true {
		t.Fatalf("the second caller answered %v, want attempt 1 not yet settled", nested)
	}
}

// N2. AN UNCERTAIN HAND-OVER STAYS UNCERTAIN, with its plain sentence, on the
// page, in the export and in the automatic answer -- the lane's early failed is
// not final for it -- until the lane reports something definite: delivered
// turns it sent, a bounce failed. A resend of it says it may be a second copy.
func TestAnUncertainHandOverStaysUncertainUntilTheLaneSaysSomethingDefinite(t *testing.T) {
	useTestLaneReaders(t)
	w, p := worldIn(EmailRejected)
	w.lane.outcome = "uncertain"
	got := w.automatic(t, p, EmailRejected)
	if got["state"] != SendUncertain || got["reason"] != ReasonUncertain || got["sent"] != false {
		t.Fatalf("the uncertain hand-over answered %v", got)
	}
	id := LaneRequestID(EmailRejected, linkApp, 1)
	w.lane.statuses = []map[string]any{{"requestId": id, "status": "failed"}}
	exported := func() map[string]any {
		t.Helper()
		nodes, err := p.exportStore(staffCtx(emailStaff), map[string]any{"siteId": linkSite, "storeId": linkStore}, 0)
		var doc struct {
			EmailSends []map[string]any `json:"emailSends"`
		}
		if err := json.Unmarshal([]byte(asString(reply(t, nodes, err)["json"])), &doc); err != nil || len(doc.EmailSends) == 0 {
			t.Fatalf("the export: %v", err)
		}
		return doc.EmailSends[0]
	}
	for where, e := range map[string]map[string]any{"the page": emailShown(t, w.application(t, p), EmailRejected, 1), "the export": exported()} {
		if e["state"] != SendUncertain || e["reason"] != ReasonUncertain {
			t.Fatalf("%s shows the uncertain email as %v", where, e)
		}
	}
	if again := w.automatic(t, p, EmailRejected); again["state"] != SendUncertain || again["reason"] != ReasonUncertain {
		t.Fatalf("the automatic answer after the lane's early failed is %v", again)
	}
	w.lane.statuses[0]["providerStatus"] = "delivered"
	if e := emailShown(t, w.application(t, p), EmailRejected, 1); e["state"] != SendSent {
		t.Fatalf("a delivery report shows %v", e)
	}
	w.lane.statuses[0]["providerStatus"] = "bounced"
	if e := emailShown(t, w.application(t, p), EmailRejected, 1); e["state"] != SendFailed || e["reason"] != reasonBounced {
		t.Fatalf("a bounce shows %v", e)
	}
	w.lane.statuses[0]["providerStatus"] = ""
	w.lane.outcome = ""
	resent, err := w.resend(t, p, EmailRejected)
	if err != nil || resent["state"] != SendSent || resent["note"] != NoteMaybeSecondCopy {
		t.Fatalf("the resend of an uncertain rejection answered %v, %v", resent, err)
	}
}

// A HOLD'S SENTENCE STAYS ON THE ROW for the page; the automation's answer and
// the log carry the pack's own fixed sentence, which names no template.
func TestAHeldAutomaticAnswerAndTheLogNameNoTemplate(t *testing.T) {
	useTestLaneReaders(t)
	w, p := worldIn(EmailRevoked)
	w.lane.outcome = "hold"
	got := w.automatic(t, p, EmailRevoked)
	if got["state"] != SendHeld || got["reason"] != ReasonHeldForAutomation || strings.Contains(fmt.Sprint(got), "Wholesale removal") {
		t.Fatalf("the held automatic send answered %v", got)
	}
	if strings.Contains(w.log.String(), "Wholesale removal") {
		t.Fatalf("a log line names the template: %s", w.log.String())
	}
	if row := w.sends[EmailSendRowID(EmailRevoked, linkApp, 1)]; !strings.Contains(asString(row["reason"]), "Wholesale removal") {
		t.Fatalf("the row lost the lane's sentence: %v", row)
	}
	if e := emailShown(t, w.application(t, p), EmailRevoked, 1); !strings.Contains(asString(e["reason"]), "Wholesale removal") {
		t.Fatalf("the page shows %v, want the lane's sentence", e)
	}
}

// N4. THE EXPORT IS PRODUCED WHEN THE LANE'S STATUS CANNOT BE READ, each
// attempt saying its status could not be read.
func TestTheExportIsProducedWhenTheLaneCannotBeRead(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.sendTerms(t, p)
	w.lane.statusErr = errors.New("campaigns.rowMessageStatus: the read timed out")
	nodes, err := p.exportStore(staffCtx(emailStaff), map[string]any{"siteId": linkSite, "storeId": linkStore}, 0)
	if err != nil {
		t.Fatalf("the export was refused: %v", err)
	}
	var doc struct {
		EmailSends []map[string]any `json:"emailSends"`
	}
	if err := json.Unmarshal([]byte(asString(decodeOne(t, nodes, nil)["json"])), &doc); err != nil || len(doc.EmailSends) != 1 {
		t.Fatalf("the export: %v %v", err, doc.EmailSends)
	}
	if doc.EmailSends[0]["deliveryUnreadable"] != true || doc.EmailSends[0]["state"] != SendSent {
		t.Fatalf("the email in the export is %v", doc.EmailSends[0])
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
	if !errors.As(err, &withdrawn) || withdrawn.Reason != "The application is no longer waiting for terms." {
		t.Fatalf("the resolver answered %v", err)
	}
	if w.count("putPrivateLinkRow") != 0 {
		t.Fatal("a withdrawn email minted a link")
	}
	// And an approval email while the access is not granted.
	w2 := newEmailWorld()
	p2 := w2.provider()
	w2.approveAndGrant(EntitlementFailed)
	if _, err := w2.resolve(t, p2, EmailApproved); !errors.As(err, &withdrawn) || withdrawn.Reason != "Access is not granted." {
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

// THE RESENDS ARE OFFERED ONLY WHEN THEY ARE DUE. A kind the input says
// nothing of is not offered; the terms email while the application waits for
// its terms, whatever its last attempt came to; the approval email to a buyer
// whose access is granted once the automation's attempt is recorded; the
// rejection and removal emails while the application is still rejected or
// removed and the last attempt did not reach the applicant -- skipped, held,
// uncertain or failed, or sent and since reported not delivered. None while
// an attempt is on its way, none while the store would not send it now, and
// none to an operator.
func TestActsOfferTheResendsOnlyWhenTheyAreDue(t *testing.T) {
	one := func(kind string, s EmailStanding) map[string]EmailStanding { return map[string]EmailStanding{kind: s} }
	granted := ActsInput{Stage: StateApproved, AccessState: EntitlementGranted, EntitlementExists: true}
	with := func(in ActsInput, emails map[string]EmailStanding) ActsInput {
		in.Emails = emails
		return in
	}
	for _, c := range []struct {
		in   ActsInput
		want string
	}{
		// THE TERMS EMAIL.
		{ActsInput{Stage: StageWaitingForTerms, Emails: one(EmailTerms, EmailStanding{})}, "[reject resendTerms]"},
		{ActsInput{Stage: StageWaitingForTerms, Emails: one(EmailTerms, EmailStanding{FirstRecorded: true, LastState: SendSent})}, "[reject resendTerms]"},
		{ActsInput{Stage: StageWaitingForTerms, Emails: one(EmailTerms, EmailStanding{FirstRecorded: true, LastState: SendSkipped})}, "[reject resendTerms]"},
		{ActsInput{Stage: StageWaitingForTerms}, "[reject]"},
		{ActsInput{Stage: StageWaitingForTerms, Emails: one(EmailTerms, EmailStanding{Blocker: noTemplateReason(EmailTerms)})}, "[reject]"},
		{ActsInput{Stage: StageWaitingForTerms, Emails: one(EmailTerms, EmailStanding{Blocker: NoSenderChosenReason})}, "[reject]"},
		{ActsInput{Stage: StageWaitingForTerms, Emails: one(EmailTerms, EmailStanding{FirstRecorded: true, LastState: SendDeferred, OnItsWay: true})}, "[reject]"},
		{ActsInput{Stage: StageWaitingForTerms, Operator: true, Emails: one(EmailTerms, EmailStanding{})}, "[]"},
		{ActsInput{Stage: StageReadyToReview, Emails: one(EmailTerms, EmailStanding{})}, "[reject approve]"},
		// THE APPROVAL EMAIL.
		{with(granted, one(EmailApproved, EmailStanding{FirstRecorded: true, LastState: SendSent})), "[remove resendApproved]"},
		{with(granted, one(EmailApproved, EmailStanding{FirstRecorded: true, LastState: SendSkipped})), "[remove resendApproved]"},
		{with(granted, one(EmailApproved, EmailStanding{})), "[remove]"},
		{granted, "[remove]"},
		{with(granted, one(EmailApproved, EmailStanding{FirstRecorded: true, LastState: SendSent, Blocker: NoSenderChosenReason})), "[remove]"},
		{with(granted, one(EmailApproved, EmailStanding{FirstRecorded: true, LastState: SendDeferred, OnItsWay: true})), "[remove]"},
		{ActsInput{Stage: StateApproved, AccessState: EntitlementPending, EntitlementExists: true,
			Emails: one(EmailApproved, EmailStanding{FirstRecorded: true, LastState: SendSent})}, "[remove retryAccess]"},
		{ActsInput{Stage: StateApproved, AccessState: EntitlementGranted, EntitlementExists: true, Operator: true,
			Emails: one(EmailApproved, EmailStanding{FirstRecorded: true, LastState: SendSent})}, "[]"},
		{with(granted, one(EmailRejected, EmailStanding{FirstRecorded: true, LastState: SendHeld})), "[remove]"},
		// THE REJECTION EMAIL.
		{ActsInput{Stage: StateRejected, Emails: one(EmailRejected, EmailStanding{FirstRecorded: true, LastState: SendHeld})}, "[resendRejected]"},
		{ActsInput{Stage: StateRejected, Emails: one(EmailRejected, EmailStanding{FirstRecorded: true, LastState: SendUncertain})}, "[resendRejected]"},
		{ActsInput{Stage: StateRejected, Emails: one(EmailRejected, EmailStanding{FirstRecorded: true, LastState: SendFailed})}, "[resendRejected]"},
		{ActsInput{Stage: StateRejected, Emails: one(EmailRejected, EmailStanding{FirstRecorded: true, LastState: SendSkipped})}, "[resendRejected]"},
		{ActsInput{Stage: StateRejected, Emails: one(EmailRejected, EmailStanding{FirstRecorded: true, LastState: SendSent, LastBounced: true})}, "[resendRejected]"},
		{ActsInput{Stage: StateRejected, Emails: one(EmailRejected, EmailStanding{FirstRecorded: true, LastState: SendSent})}, "[]"},
		{ActsInput{Stage: StateRejected, Emails: one(EmailRejected, EmailStanding{FirstRecorded: true, LastState: SendDeferred, OnItsWay: true})}, "[]"},
		{ActsInput{Stage: StateRejected, Emails: one(EmailRejected, EmailStanding{})}, "[]"},
		{ActsInput{Stage: StateRejected}, "[]"},
		{ActsInput{Stage: StateRejected, Emails: one(EmailRejected, EmailStanding{FirstRecorded: true, LastState: SendHeld, Blocker: noTemplateReason(EmailRejected)})}, "[]"},
		{ActsInput{Stage: StateRejected, Emails: one(EmailRejected, EmailStanding{FirstRecorded: true, LastState: SendHeld, Unreadable: true})}, "[]"},
		{ActsInput{Stage: StateRejected, Operator: true, Emails: one(EmailRejected, EmailStanding{FirstRecorded: true, LastState: SendHeld})}, "[]"},
		// THE REMOVAL EMAIL.
		{ActsInput{Stage: StateRevoked, AccessState: EntitlementRevoked, EntitlementExists: true,
			Emails: one(EmailRevoked, EmailStanding{FirstRecorded: true, LastState: SendHeld})}, "[resendRevoked]"},
		{ActsInput{Stage: StateRevoked, AccessState: EntitlementPending, EntitlementExists: true,
			Emails: one(EmailRevoked, EmailStanding{FirstRecorded: true, LastState: SendFailed})}, "[retryAccess resendRevoked]"},
		{ActsInput{Stage: StateRevoked, AccessState: EntitlementRevoked, EntitlementExists: true,
			Emails: one(EmailRevoked, EmailStanding{FirstRecorded: true, LastState: SendSent})}, "[]"},
		{ActsInput{Stage: StateRevoked, AccessState: EntitlementRevoked, EntitlementExists: true,
			Emails: one(EmailRevoked, EmailStanding{FirstRecorded: true, LastState: SendSent, LastBounced: true})}, "[resendRevoked]"},
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
// A SENDER IS ONE OF THE STORE'S ORGANIZATION'S: a cluster operator who names
// another organization's sender, or one that does not exist, is refused with a
// sentence, and nothing is written.
func TestTheSenderIsASenderOfTheStoresOrganization(t *testing.T) {
	t.Setenv("MEMQL_OUTBOUND_EMAIL_ALLOWLIST", "")
	w := newEmailWorld()
	p := w.provider()
	for _, bad := range []string{"sender-other", "sender-none"} {
		_, err := p.updateSettings(operatorCtx("op-1"), map[string]any{"siteId": linkSite, "storeId": linkStore, "senderIdentityId": bad}, 0)
		if err == nil || !strings.Contains(err.Error(), "is not a sender of this store's organization") {
			t.Fatalf("an operator naming %s answered %v", bad, err)
		}
	}
	if w.count("putWholesaleSettingsRow") != 0 {
		t.Fatal("a refused sender was written")
	}
	if _, err := p.updateSettings(operatorCtx("op-1"), map[string]any{"siteId": linkSite, "storeId": linkStore, "senderIdentityId": "sender-1"}, 0); err != nil {
		t.Fatalf("an operator naming the organization's sender: %v", err)
	}
}

// THE SENDER AS A PERSON READS IT: the settings answer, beside the id, the name
// and address the emails go out as, resolved by the lane as a send resolves it;
// with none chosen, what the lane resolves for the organization; and when the
// lane cannot say, a sentence instead of a blank.
func TestTheSettingsAnswerTheSenderAsTheEmailsGoOutAs(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	sender := func() map[string]any {
		t.Helper()
		nodes, err := p.storeSettings(staffCtx(emailStaff), map[string]any{"siteId": linkSite, "storeId": linkStore}, 0)
		got, _ := reply(t, nodes, err)["sender"].(map[string]any)
		return got
	}
	w.settings["senderIdentityId"] = "sender-1"
	w.lane.senders = map[string]map[string]any{"sender-1": {"ready": true, "fromName": "Kestrel Trade", "address": "trade@example.com", "reason": ""}}
	if got := sender(); got["ready"] != true || got["fromName"] != "Kestrel Trade" || got["address"] != "trade@example.com" || got["reason"] != "" {
		t.Fatalf("the chosen sender reads %v", got)
	}
	if w.lane.sendersAsked[0] != "acct-links/sender-1" {
		t.Fatalf("the lane was asked %v, want the store's organization and its sender", w.lane.sendersAsked)
	}
	w.settings["senderIdentityId"] = ""
	if got := sender(); got["ready"] != false || got["address"] != "" || got["reason"] != laneNoneChosen {
		t.Fatalf("no sender chosen reads %v", got)
	}
	for _, actor := range w.lane.senderActors {
		if actor != linkMerchant {
			t.Fatalf("the sender was read as %v, want the store's merchant", w.lane.senderActors)
		}
	}
	w.lane.senderErr = errors.New("campaigns.rowSender: the read timed out")
	if got := sender(); got["ready"] != false || got["reason"] != "The sender could not be read just now." || strings.Contains(fmt.Sprint(got), "timed out") {
		t.Fatalf("a sender the lane cannot resolve reads %v", got)
	}
	// THE INSTALLATION'S OWN ORGANIZATION, none chosen: its default sender, said so.
	w.lane.senderErr = nil
	w.lane.senders[""] = map[string]any{"ready": true, "fromName": "", "address": "", "reason": ""}
	if got := sender(); got["ready"] != true || got["reason"] != "The installation's default sender." {
		t.Fatalf("the installation's default sender reads %v", got)
	}
}

// MAY PUBLISH AND MAY EXPORT ARE THE BUILTINS' OWN CHECKS: each agrees with
// whether the builtin that does it then refuses.
func TestMayPublishAndMayExportAgreeWithTheBuiltins(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	store := map[string]any{"siteId": linkSite, "storeId": linkStore}
	for who, ctx := range map[string]context.Context{"a member": staffCtx(emailStaff), "an operator": operatorCtx("op-1")} {
		nodes, err := p.storeSettings(ctx, store, 0)
		may, _ := reply(t, nodes, err)["may"].(map[string]any)
		_, publishErr := p.publishDocument(ctx, map[string]any{"siteId": linkSite, "storeId": linkStore, "kind": DocumentTerms,
			"label": "Terms 2027", "body": "Net 45."}, 0)
		_, exportErr := p.exportStore(ctx, store, 0)
		if may["publish"] != (publishErr == nil) || may["export"] != (exportErr == nil) {
			t.Fatalf("%s: may %v, publish %v, export %v", who, may, publishErr, exportErr)
		}
	}
}

// THE SETTINGS SAY WHETHER EACH EMAIL WOULD BE SENT NOW, for an application in
// the right state, and when not the one sentence why -- the same checks a send
// makes. The terms email in every combination of its three conditions, and one
// case of each other kind.
func TestTheSettingsSayWhetherEachEmailWouldBeSent(t *testing.T) {
	readiness := func(t *testing.T, w *emailWorld, p *Provider) map[string]any {
		t.Helper()
		nodes, err := p.storeSettings(staffCtx(emailStaff), map[string]any{"siteId": linkSite, "storeId": linkStore}, 0)
		got, _ := reply(t, nodes, err)["emailsWouldSend"].(map[string]any)
		return got
	}
	for _, required := range []bool{true, false} {
		for _, template := range []bool{true, false} {
			for _, sender := range []bool{true, false} {
				w := newEmailWorld()
				p := w.provider()
				w.settings["termsRequired"] = required
				if !template {
					w.settings["termsTemplateId"] = ""
				}
				if !sender {
					w.lane.senders = map[string]map[string]any{}
				}
				want := ""
				switch {
				case !required:
					want = ReasonTermsNotRequired
				case !template:
					want = noTemplateReason(EmailTerms)
				case !sender:
					want = NoSenderChosenReason
				}
				terms, _ := readiness(t, w, p)[EmailTerms].(map[string]any)
				if terms["wouldSend"] != (want == "") || terms["reason"] != want {
					t.Errorf("terms required %v, template %v, sender %v: %v, want reason %q", required, template, sender, terms, want)
				}
				// AND IT IS WHAT THE SEND ITSELF DECLINES WITH.
				if want != "" {
					if got := w.sendTerms(t, p); got["reason"] != want || len(w.lane.calls) != 0 {
						t.Errorf("the send declined with %v after %d lane calls, the settings said %q", got["reason"], len(w.lane.calls), want)
					}
				}
			}
		}
	}
	w := newEmailWorld()
	p := w.provider()
	w.settings["rejectedTemplateId"] = ""
	got := readiness(t, w, p)
	for kind, want := range map[string]string{EmailApproved: "", EmailRejected: noTemplateReason(EmailRejected), EmailRevoked: ""} {
		k, _ := got[kind].(map[string]any)
		if k["wouldSend"] != (want == "") || k["reason"] != want {
			t.Errorf("%s: %v, want reason %q", kind, k, want)
		}
	}
}

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
	if got["status"] != "skipped" || got["reason"] != "The store names no address." || w.stages != 0 {
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

// EVERY DEFAULT TEXT IS TRUE IN EVERY CASE IT COVERS: an application the
// store's staff added is not said to have applied; a grant that did not land
// does not say the buyer is approved (a removal may have been decided since);
// a decision with no note leaves no empty paragraph; and a sentence that tells
// a person to open MemQL OS appears only when its address is known.
func TestTheDefaultNoticesSayOnlyWhatIsTrue(t *testing.T) {
	w := newEmailWorld()
	w.settings["termsRequired"] = false
	w.apps[0]["addedBy"] = emailStaff
	p := w.provider()
	w.notify(t, p, map[string]any{"event": StaffEventReadyToReview})
	body := asString(w.staged[StaffRequestID(StaffEventReadyToReview, linkApp, "")]["body"])
	if strings.Contains(body, "applied") || !strings.Contains(body, "is ready to review") ||
		!strings.HasSuffix(body, "Open MemQL OS: https://os.example.test/") {
		t.Fatalf("the readyToReview notice of an application the staff added reads %q", body)
	}

	// NO ADDRESS FOR MEMQL OS: no sentence sends anybody there.
	w2 := newEmailWorld()
	p2 := w2.provider()
	p2.osBase = func() string { return "" }
	w2.approveAndGrant(EntitlementPending)
	w2.decide(TransitionRevoke)
	w2.notify(t, p2, map[string]any{"event": StaffEventRevoked, "decisionId": "dec-revoke"})
	removed := asString(w2.staged[StaffRequestID(StaffEventRevoked, linkApp, "dec-revoke")]["body"])
	if strings.Contains(removed, "Open") || strings.Contains(removed, "\n\n\n") || strings.HasSuffix(removed, "\n") {
		t.Fatalf("the removal notice with no note and no OS address reads %q", removed)
	}
	// THE GRANT THAT DID NOT LAND, on an application removed since: not "approved".
	w2.notify(t, p2, map[string]any{"event": StaffEventAccessNotGranted})
	notOn := asString(w2.staged[AccessStaffRequestID(StaffEventAccessNotGranted, linkApp, 1)]["body"])
	if strings.Contains(notOn, "approved") || strings.Contains(notOn, "Open") || !strings.Contains(notOn, "did not turn on") {
		t.Fatalf("the access notice reads %q", notOn)
	}
	// THE APPROVAL EMAIL OF AN APPLICATION THAT IS NOT APPROVED is not "not granted yet".
	if got := w2.sendBuyer(t, p2, EmailApproved); got["reason"] != "The application is not approved." {
		t.Fatalf("the approval email of a removed application declined with %v", got["reason"])
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
	if got := w.notify(t, p, map[string]any{"event": StaffEventAccessNotGranted}); got["requestId"] != AccessStaffRequestID(StaffEventAccessNotGranted, linkApp, 1) {
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

// ---------------------------------------------------------------------------
// An email that was due and was not sent is recorded (ruling 79)
// ---------------------------------------------------------------------------

// resendActOf is the act that sends each kind again.
var resendActOf = map[string]string{EmailTerms: ActResendTerms, EmailApproved: ActResendApproved,
	EmailRejected: ActResendRejected, EmailRevoked: ActResendRevoked}

// templateSetting is the setting that holds each kind's template.
var templateSetting = map[string]string{EmailTerms: "termsTemplateId", EmailApproved: "approvedTemplateId",
	EmailRejected: "rejectedTemplateId", EmailRevoked: "revokedTemplateId"}

// offers reports whether the application's page offers act to the store's person.
func (w *emailWorld) offers(t *testing.T, p *Provider, act string) bool {
	t.Helper()
	acts, _ := w.application(t, p)["acts"].([]any)
	for _, a := range acts {
		if a == act {
			return true
		}
	}
	return false
}

// noSender leaves the store with no sender chosen, as the lane then resolves none.
func (w *emailWorld) noSender() { w.lane.senders = map[string]map[string]any{} }

// AN EMAIL THAT WAS DUE AND THAT THE STORE'S OWN CHECKS DECLINE IS RECORDED.
// For each of the four emails -- the terms email of a store that requires its
// terms, the approval, the rejection and the removal -- and for each check that
// declines it (no sender chosen, no template chosen): when its moment arrives
// one attempt is recorded, skipped, with the check's own sentence, and the lane
// is not asked; the application's page lists it; no resend is offered, and one
// asked for is refused with that sentence, while the store would still decline
// it; once the store is put right the act is offered, a person's resend is
// attempt 2 and sends, and the automatic call made again sends nothing.
func TestADueEmailTheStoreDeclinesIsRecordedAndAPersonSendsItOnceTheStoreIsReady(t *testing.T) {
	for _, d := range []struct {
		name           string
		reason         func(kind string) string
		unready, ready func(w *emailWorld, kind string)
	}{
		{"no sender chosen", func(string) string { return NoSenderChosenReason },
			func(w *emailWorld, _ string) { w.noSender() },
			func(w *emailWorld, _ string) { w.lane.senders = nil }},
		{"no template chosen", noTemplateReason,
			func(w *emailWorld, kind string) { w.settings[templateSetting[kind]] = "" },
			func(w *emailWorld, kind string) { w.settings[templateSetting[kind]] = "tpl-" + kind }},
	} {
		for _, kind := range EmailKinds {
			t.Run(d.name+"/"+kind, func(t *testing.T) {
				w, p := worldIn(kind)
				d.unready(w, kind)
				want := d.reason(kind)
				got := w.automatic(t, p, kind)
				if got["state"] != SendSkipped || got["sent"] != false || got["recorded"] != true || got["attempt"] != float64(1) || got["reason"] != want {
					t.Fatalf("the declined %s email answered %v, want attempt 1 recorded as skipped with %q", kind, got, want)
				}
				row := w.sends[EmailSendRowID(kind, linkApp, 1)]
				if len(w.sends) != 1 || row == nil || row["state"] != SendSkipped || row["reason"] != want || len(w.lane.calls) != 0 {
					t.Fatalf("%d rows, the attempt's is %v, and %d lane calls; want one skipped row and the lane not asked", len(w.sends), row, len(w.lane.calls))
				}
				if e := emailShown(t, w.application(t, p), kind, 1); e["state"] != SendSkipped || e["reason"] != want || e["delivery"] != nil {
					t.Fatalf("the page shows the declined email as %v", e)
				}
				if w.offers(t, p, resendActOf[kind]) {
					t.Fatalf("%s is offered while the store declines the email", resendActOf[kind])
				}
				if _, err := w.resend(t, p, kind); err == nil || err.Error() != "wholesale: "+want || len(w.sends) != 1 || len(w.lane.calls) != 0 {
					t.Fatalf("a resend while the store declines answered %v with %d rows and %d lane calls; want the check's sentence and nothing done",
						err, len(w.sends), len(w.lane.calls))
				}
				d.ready(w, kind)
				if !w.offers(t, p, resendActOf[kind]) {
					t.Fatalf("%s is not offered once the store would send the email: %v", resendActOf[kind], w.application(t, p)["acts"])
				}
				resent, err := w.resend(t, p, kind)
				if err != nil || resent["state"] != SendSent || resent["attempt"] != float64(2) || len(w.lane.sent) != 1 {
					t.Fatalf("the resend answered %v, %v after %d emails handed over", resent, err, len(w.lane.sent))
				}
				again := w.automatic(t, p, kind)
				if again["attempt"] != float64(1) || again["state"] != SendSkipped || again["sent"] != false || len(w.lane.calls) != 1 || len(w.lane.sent) != 1 {
					t.Fatalf("the automatic call after the resend answered %v after %d lane calls and %d emails", again, len(w.lane.calls), len(w.lane.sent))
				}
			})
		}
	}
}

// AN EMAIL THAT IS NOT DUE RECORDS NOTHING, however the store stands: the
// terms email of a store that requires no terms, and an email whose
// application is not in the state for it. No act is offered for either.
func TestAnEmailThatIsNotDueRecordsNothing(t *testing.T) {
	w := newEmailWorld()
	w.settings["termsRequired"] = false
	p := w.provider()
	for _, unready := range []func(){func() {}, func() { w.settings["termsTemplateId"] = "" }, w.noSender} {
		unready()
		got := w.sendTerms(t, p)
		if got["state"] != SendSkipped || got["recorded"] != false || got["reason"] != ReasonTermsNotRequired || len(w.sends) != 0 || len(w.lane.calls) != 0 {
			t.Fatalf("the terms email of a store that requires no terms answered %v with %d rows", got, len(w.sends))
		}
	}
	if w.offers(t, p, ActResendTerms) {
		t.Fatal("resendTerms is offered on a store that requires no terms")
	}
	if _, err := w.resend(t, p, EmailTerms); err == nil || len(w.sends) != 0 {
		t.Fatalf("a terms resend on a store that requires no terms answered %v with %d rows", err, len(w.sends))
	}

	// AN APPLICATION THAT IS NOT IN THE STATE FOR THE EMAIL, on a store that
	// would decline it: not due, so nothing is recorded.
	for kind, want := range map[string]string{EmailTerms: ReasonNotWaiting, EmailApproved: notInStateReason(StateApproved),
		EmailRevoked: notInStateReason(StateRevoked)} {
		w, p := worldIn(EmailRejected) // rejected: waiting for nothing, approved by nobody
		w.settings[templateSetting[kind]] = ""
		w.noSender()
		got := w.automatic(t, p, kind)
		if got["recorded"] != false || got["reason"] != want || len(w.sends) != 0 || len(w.lane.calls) != 0 {
			t.Fatalf("the %s email of a rejected application answered %v with %d rows, want %q and nothing recorded", kind, got, len(w.sends), want)
		}
	}
}

// AN ACCESS FROM BEFORE THE PACK SENT EMAIL RECORDS NO ATTEMPT, whatever the
// store's readiness: the account backfill still mails nobody and leaves no
// record, on a store that would decline the approval email too.
func TestAnAccessFromBeforeEmailsRecordsNoAttemptOnAStoreThatWouldDecline(t *testing.T) {
	w := newEmailWorld()
	p := w.provider()
	w.decide(TransitionApprove)
	w.ents = append(w.ents, map[string]any{"id": "v1:wholesale:entitlement:" + EntitlementRowID(linkApp),
		"applicationId": applicationConcept + ":" + linkApp, "storeId": linkStore, "state": EntitlementGranted,
		"adapter": AdapterCustomerTag, "ownerUserId": linkMerchant, "accountId": "acct-links", "createdAt": w.next()})
	for name, unready := range map[string]func(){"no sender": w.noSender, "no template": func() { w.settings["approvedTemplateId"] = "" }} {
		unready()
		got := w.sendBuyer(t, p, EmailApproved)
		if got["recorded"] != false || got["reason"] != ReasonAccessPredatesEmail || len(w.sends) != 0 || len(w.lane.calls) != 0 {
			t.Fatalf("with %s the approval email of an access from before emails answered %v with %d rows", name, got, len(w.sends))
		}
	}
}

// TWO AUTOMATIC CALLS AT ONCE FOR A DECLINED EMAIL RECORD ONE ROW: the record
// goes through the fenced write every attempt goes through. The other node's
// call lands after this node's first read of the rows, and again after its
// last read before the write; either way one version of one row is written and
// both calls answer it.
func TestTwoAutomaticCallsAtOnceForADeclinedEmailRecordOneRow(t *testing.T) {
	for name, unready := range map[string]func(w *emailWorld){
		"no template chosen": func(w *emailWorld) { w.settings["rejectedTemplateId"] = "" },
		"no sender chosen":   func(w *emailWorld) { w.noSender() },
	} {
		for _, at := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/the other lands after read %d", name, at), func(t *testing.T) {
				w, p := worldIn(EmailRejected)
				unready(w)
				rowID := EmailSendRowID(EmailRejected, linkApp, 1)
				var other map[string]any
				w.onSendsRead = func(n int) {
					if n == at {
						other = w.automatic(t, p, EmailRejected)
					}
				}
				got := w.automatic(t, p, EmailRejected)
				if w.versions[rowID] != 1 || len(w.sends) != 1 || len(w.lane.calls) != 0 {
					t.Fatalf("%d versions of the attempt in %d rows after %d lane calls, want one version of one row", w.versions[rowID], len(w.sends), len(w.lane.calls))
				}
				for who, answer := range map[string]map[string]any{"this node": got, "the other node": other} {
					if answer["attempt"] != float64(1) || answer["state"] != SendSkipped || answer["recorded"] != true || answer["sent"] != false {
						t.Fatalf("%s answered %v, want attempt 1's skipped row", who, answer)
					}
				}
			})
		}
	}
}

// A REPEAT CALL READS THE ATTEMPTS FIRST: an automatic call for a kind that
// already has an attempt answers that attempt whatever the store and the
// application are now -- the sender or the template cleared, the terms
// accepted or no longer required, the buyer removed -- and asks the lane
// nothing, its sender included.
func TestARepeatedAutomaticCallAnswersItsAttemptWhateverTheStoreAndTheApplicationAreNow(t *testing.T) {
	for _, c := range []struct {
		kind, name string
		change     func(t *testing.T, w *emailWorld, p *Provider)
	}{
		{EmailRejected, "the sender is cleared", func(_ *testing.T, w *emailWorld, _ *Provider) { w.noSender() }},
		{EmailRejected, "the template is cleared", func(_ *testing.T, w *emailWorld, _ *Provider) { w.settings["rejectedTemplateId"] = "" }},
		{EmailTerms, "the store no longer requires its terms", func(_ *testing.T, w *emailWorld, _ *Provider) { w.settings["termsRequired"] = false }},
		{EmailTerms, "the terms are accepted", func(t *testing.T, w *emailWorld, p *Provider) {
			if _, err := p.acceptTerms(shopper(), w.acceptArgs(tokenIn(w.lane.sent[0].Fields["termsUrl"])), 0); err != nil {
				t.Fatalf("accept: %v", err)
			}
		}},
		{EmailApproved, "the buyer is removed", func(_ *testing.T, w *emailWorld, _ *Provider) { w.decide(TransitionRevoke) }},
	} {
		t.Run(c.kind+"/"+c.name, func(t *testing.T) {
			w, p := worldIn(c.kind)
			if first := w.automatic(t, p, c.kind); first["state"] != SendSent {
				t.Fatalf("the first call answered %v", first)
			}
			c.change(t, w, p)
			senderReads := len(w.lane.sendersAsked)
			again := w.automatic(t, p, c.kind)
			if again["attempt"] != float64(1) || again["state"] != SendSent || again["sent"] != true || again["recorded"] != true {
				t.Fatalf("after %s the repeat call answered %v, want attempt 1's sent row", c.name, again)
			}
			if len(w.lane.calls) != 1 || len(w.lane.sendersAsked) != senderReads {
				t.Fatalf("the repeat call made %d lane sends in all and %d sender reads, want one and none",
					len(w.lane.calls), len(w.lane.sendersAsked)-senderReads)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// A resend is offered exactly when it would not be refused
// ---------------------------------------------------------------------------

// A RESEND IS OFFERED EXACTLY WHEN IT WOULD NOT BE REFUSED. For every email,
// every way its last attempt can stand and every way the store can stand, the
// page offers the act if and only if the resend then goes through: ActsFor and
// wholesaleResendEmail ask one question.
func TestAResendIsOfferedExactlyWhenItWouldNotBeRefused(t *testing.T) {
	useTestLaneReaders(t)
	lasts := attemptScenarios()
	stores := storeScenarios()
	moved := map[string]func(w *emailWorld, kind string){
		"in the state for it": func(*emailWorld, string) {},
		"moved on": func(w *emailWorld, kind string) {
			switch kind {
			case EmailTerms:
				w.decide(TransitionReject)
			case EmailApproved:
				w.decide(TransitionRevoke)
			case EmailRejected, EmailRevoked:
				// Nothing leads out of rejected or removed; the access of a removed
				// buyer is the one thing left to change, and the email does not wait
				// on it.
			}
		},
	}
	offered, refused := 0, 0
	for _, kind := range EmailKinds {
		for last, makeLast := range lasts {
			for store, makeStore := range stores {
				for where, move := range moved {
					name := kind + "/last attempt " + last + "/" + store + "/" + where
					w, p := worldIn(kind)
					makeLast(t, w, p, kind)
					w.lane.outcome = ""
					makeStore(w, kind)
					move(w, kind)
					offer := w.offers(t, p, resendActOf[kind])
					before := len(w.sends)
					_, err := w.resend(t, p, kind)
					if offer != (err == nil) {
						t.Errorf("%s: the act offered is %v, and the resend answered %v", name, offer, err)
					}
					if err != nil && len(w.sends) != before {
						t.Errorf("%s: a refused resend recorded an attempt", name)
					}
					if offer {
						offered++
					} else {
						refused++
					}
				}
			}
		}
	}
	// THE SWEEP SAW BOTH ANSWERS, many times: it is not agreeing on nothing.
	if offered < 20 || refused < 20 {
		t.Fatalf("the sweep saw %d offers and %d refusals; it should see many of each", offered, refused)
	}
}

// attemptScenarios is every way the last attempt of an email can come to
// stand, each as a function that leaves a world's application so.
func attemptScenarios() map[string]func(t *testing.T, w *emailWorld, p *Provider, kind string) {
	return map[string]func(t *testing.T, w *emailWorld, p *Provider, kind string){
		"none": func(*testing.T, *emailWorld, *Provider, string) {},
		"sent": func(t *testing.T, w *emailWorld, p *Provider, kind string) { w.automatic(t, p, kind) },
		"failed": func(t *testing.T, w *emailWorld, p *Provider, kind string) {
			w.lane.outcome = "fail"
			w.automatic(t, p, kind)
		},
		"held": func(t *testing.T, w *emailWorld, p *Provider, kind string) {
			w.lane.outcome = "hold"
			w.automatic(t, p, kind)
		},
		"uncertain": func(t *testing.T, w *emailWorld, p *Provider, kind string) {
			w.lane.outcome = "uncertain"
			w.automatic(t, p, kind)
		},
		"deferred": func(t *testing.T, w *emailWorld, p *Provider, kind string) {
			w.lane.outcome = "defer"
			w.automatic(t, p, kind)
		},
		"skipped by the address": func(t *testing.T, w *emailWorld, p *Provider, kind string) {
			w.lane.outcome = "suppressed"
			w.automatic(t, p, kind)
		},
		"declined for no template": func(t *testing.T, w *emailWorld, p *Provider, kind string) {
			w.settings[templateSetting[kind]] = ""
			w.automatic(t, p, kind)
			w.settings[templateSetting[kind]] = "tpl-" + kind
		},
		"sent and bounced": func(t *testing.T, w *emailWorld, p *Provider, kind string) {
			w.automatic(t, p, kind)
			w.lane.statuses = []map[string]any{{"requestId": LaneRequestID(kind, linkApp, 1), "status": "sent", "providerStatus": "bounced"}}
		},
		"sent and delivered": func(t *testing.T, w *emailWorld, p *Provider, kind string) {
			w.automatic(t, p, kind)
			w.lane.statuses = []map[string]any{{"requestId": LaneRequestID(kind, linkApp, 1), "status": "sent", "providerStatus": "delivered"}}
		},
		"the lane's record unreadable": func(t *testing.T, w *emailWorld, p *Provider, kind string) {
			w.lane.outcome = "fail"
			w.automatic(t, p, kind)
			w.lane.statusErr = errors.New("campaigns.rowMessageStatus: the read timed out")
		},
	}
}

// storeScenarios is every way a store can stand towards sending an email.
func storeScenarios() map[string]func(w *emailWorld, kind string) {
	return map[string]func(w *emailWorld, kind string){
		"ready":       func(*emailWorld, string) {},
		"no sender":   func(w *emailWorld, _ string) { w.noSender() },
		"no template": func(w *emailWorld, kind string) { w.settings[templateSetting[kind]] = "" },
		"the sender cannot be read just now": func(w *emailWorld, _ string) {
			w.lane.senderErr = errors.New("campaigns.rowSender: the read timed out")
		},
		"the chosen sender is turned off": func(w *emailWorld, _ string) {
			w.settings["senderIdentityId"] = "sender-1"
			w.lane.senders = map[string]map[string]any{"sender-1": {"ready": false, "fromName": "", "address": "",
				"reason": "The sender trade@example.com is turned off. Turn it on to send this email.", "readFailed": false, "noneChosen": false}}
		},
	}
}

// ---------------------------------------------------------------------------
// A bounce (ruling 80)
// ---------------------------------------------------------------------------

// exportedEmails is the store's export's emailSends.
func (w *emailWorld) exportedEmails(t *testing.T, p *Provider) []map[string]any {
	t.Helper()
	nodes, err := p.exportStore(staffCtx(emailStaff), map[string]any{"siteId": linkSite, "storeId": linkStore}, 0)
	var doc struct {
		EmailSends []map[string]any `json:"emailSends"`
	}
	if err := json.Unmarshal([]byte(asString(reply(t, nodes, err)["json"])), &doc); err != nil {
		t.Fatalf("the export: %v", err)
	}
	return doc.EmailSends
}

// A SENT EMAIL THE PROVIDER LATER REPORTS AS NOT DELIVERED STAYS SENT -- sent
// means handed to the email service -- with the provider's report beside it, on
// the application's page and in the export; and since it did not reach the
// applicant a person may send it again: the act is offered and the resend
// records the next attempt. One reported delivered offers no resend.
func TestASentEmailReportedBouncedStaysSentAndMayBeSentAgain(t *testing.T) {
	for _, kind := range []string{EmailRejected, EmailRevoked} {
		t.Run(kind, func(t *testing.T) {
			w, p := worldIn(kind)
			if got := w.automatic(t, p, kind); got["state"] != SendSent {
				t.Fatalf("the %s email answered %v", kind, got)
			}
			id := LaneRequestID(kind, linkApp, 1)
			w.lane.statuses = []map[string]any{{"requestId": id, "status": "sent", "providerStatus": "delivered"}}
			if w.offers(t, p, resendActOf[kind]) {
				t.Fatalf("a delivered %s email offers %s", kind, resendActOf[kind])
			}
			if _, err := w.resend(t, p, kind); err == nil || len(w.lane.calls) != 1 {
				t.Fatalf("a resend of a delivered %s email answered %v after %d lane calls", kind, err, len(w.lane.calls))
			}
			for _, report := range []string{"bounced", "suppressed", "quarantined", "filtered-spam", "failed"} {
				w.lane.statuses[0]["providerStatus"] = report
				shown := emailShown(t, w.application(t, p), kind, 1)
				d, _ := shown["delivery"].(map[string]any)
				if shown["state"] != SendSent || d["providerStatus"] != report {
					t.Fatalf("the page shows an email reported %s as %v, want it sent with the provider's report beside it", report, shown)
				}
				exported := w.exportedEmails(t, p)
				ed, _ := exported[0]["delivery"].(map[string]any)
				if len(exported) != 1 || exported[0]["state"] != SendSent || ed["providerStatus"] != report {
					t.Fatalf("the export shows an email reported %s as %v", report, exported)
				}
				if !w.offers(t, p, resendActOf[kind]) {
					t.Fatalf("an email reported %s does not offer %s: %v", report, resendActOf[kind], w.application(t, p)["acts"])
				}
			}
			// THE ADDRESS IS ONE THE EMAIL SERVICE NO LONGER SENDS TO: the resend is
			// the next attempt, recorded skipped with the service's sentence.
			w.lane.outcome = "suppressed"
			resent, err := w.resend(t, p, kind)
			if err != nil || resent["attempt"] != float64(2) || resent["state"] != SendSkipped || resent["sent"] != false ||
				resent["reason"] != campaigns.SkipSentence("hard_bounce") {
				t.Fatalf("the resend to a bounced address answered %v, %v; want attempt 2 skipped with the service's sentence", resent, err)
			}
			if row := w.sends[EmailSendRowID(kind, linkApp, 2)]; row == nil || row["state"] != SendSkipped {
				t.Fatalf("the resend's row is %v", row)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// An unsettled attempt is read through the lane first
// ---------------------------------------------------------------------------

// unsettled leaves attempt 1 of kind recorded and never settled, age old: the
// node that recorded it handed it over and could not write what came of it.
func (w *emailWorld) unsettled(kind string, age time.Duration) {
	id := EmailSendRowID(kind, linkApp, 1)
	w.putSend(id, map[string]any{"emailSendId": id, "ownerUserId": linkMerchant, "storeId": linkStore, "accountId": "acct-links",
		"applicationId": applicationConcept + ":" + linkApp, "kind": kind, "attempt": 1, "requestId": LaneRequestID(kind, linkApp, 1),
		"templateId": "tpl-" + kind, "state": sendSending, "reason": reasonSending, "sentAt": "", "requestedBy": ""})
	w.sends[id]["createdAt"] = w.clock.Add(-age)
}

// AN UNSETTLED ATTEMPT IS READ THROUGH THE LANE FIRST, at any age. Where the
// lane has a record of the attempt, the lane decides: one it still holds to
// send is on its way and is not sent again -- the lane's replay would make two
// emails; one it failed is failed, not "may have been delivered"; one it sent
// is sent. Only an attempt the lane has no record of turns uncertain once the
// window has passed, and one whose record cannot be read stays on its way.
func TestAnUnsettledAttemptIsReadThroughTheLaneFirst(t *testing.T) {
	old := sendingWindow + 45*time.Minute
	for _, kind := range []string{EmailTerms, EmailRejected} {
		t.Run(kind, func(t *testing.T) {
			w, p := worldIn(kind)
			w.unsettled(kind, old)
			id := LaneRequestID(kind, linkApp, 1)
			shown := func() map[string]any { return emailShown(t, w.application(t, p), kind, 1) }

			// THE LANE HOLDS IT TO SEND LATER.
			w.lane.statuses = []map[string]any{{"requestId": id, "status": "pending"}}
			if e := shown(); e["state"] != SendDeferred || e["reason"] != reasonLaneWaiting {
				t.Fatalf("an unsettled attempt the lane holds reads %v, want it on its way", e)
			}
			if w.offers(t, p, resendActOf[kind]) {
				t.Fatalf("%s is offered for an attempt the lane still holds", resendActOf[kind])
			}
			if _, err := w.resend(t, p, kind); err == nil || !strings.Contains(err.Error(), "waiting to be sent") || len(w.lane.calls) != 0 {
				t.Fatalf("a resend over an attempt the lane still holds answered %v after %d lane calls", err, len(w.lane.calls))
			}
			if again := w.automatic(t, p, kind); again["state"] != SendDeferred || again["attempt"] != float64(1) || len(w.lane.calls) != 0 {
				t.Fatalf("the automatic call answered %v after %d lane calls", again, len(w.lane.calls))
			}

			// THE LANE FAILED IT.
			w.lane.statuses[0]["status"] = "failed"
			if e := shown(); e["state"] != SendFailed || e["reason"] != reasonLaneFailed || strings.Contains(asString(e["reason"]), "may have been delivered") {
				t.Fatalf("an unsettled attempt the lane failed reads %v, want it failed", e)
			}

			// THE LANE SENT IT.
			sentAt := w.clock.Add(-old + time.Minute).UTC().Format(time.RFC3339Nano)
			w.lane.statuses[0]["status"], w.lane.statuses[0]["sentAt"] = "sent", sentAt
			if e := shown(); e["state"] != SendSent || e["sentAt"] != sentAt {
				t.Fatalf("an unsettled attempt the lane sent reads %v", e)
			}

			// THE LANE HAS NO RECORD OF IT: uncertain once the window has passed, on
			// its way before.
			w.lane.statuses = nil
			if e := shown(); e["state"] != SendUncertain || e["reason"] != ReasonUncertain {
				t.Fatalf("an old unsettled attempt the lane has no record of reads %v, want it uncertain", e)
			}
			w.sends[EmailSendRowID(kind, linkApp, 1)]["createdAt"] = w.clock.Add(-time.Minute)
			if e := shown(); e["state"] != SendDeferred || e["reason"] != reasonSending {
				t.Fatalf("a young unsettled attempt reads %v, want it on its way", e)
			}

			// THE LANE'S RECORD CANNOT BE READ: nothing says the lane has no record of
			// it, so it does not turn uncertain, and it is not sent again.
			w.sends[EmailSendRowID(kind, linkApp, 1)]["createdAt"] = w.clock.Add(-old)
			w.lane.statusErr = errors.New("campaigns.rowMessageStatus: the read timed out")
			if e := shown(); e["state"] != SendDeferred || e["deliveryUnreadable"] != true || e["reason"] != reasonLaneUnread {
				t.Fatalf("an old unsettled attempt whose record cannot be read reads %v, want it on its way and unread", e)
			}
			if _, err := w.resend(t, p, kind); err == nil || len(w.lane.calls) != 0 {
				t.Fatalf("a resend over an unsettled attempt whose record cannot be read answered %v after %d lane calls", err, len(w.lane.calls))
			}
		})
	}
}

// THE WINDOW SITS WELL ABOVE THE LANE'S OWN BOUND ON A HAND-OVER: at least ten
// times the default of the send timeout the lane gives one message.
func TestTheSendingWindowIsAtLeastTenTimesTheLanesSendTimeout(t *testing.T) {
	if sendingWindow < 10*campaigns.DefaultSendTimeout || campaigns.DefaultSendTimeout <= 0 {
		t.Fatalf("the window is %s and the lane's default send timeout %s; want the window at least ten times it",
			sendingWindow, campaigns.DefaultSendTimeout)
	}
}

// ---------------------------------------------------------------------------
// The settings and a send give one answer
// ---------------------------------------------------------------------------

// wouldSend is what the settings say of kind: whether it would be sent, and why not.
func (w *emailWorld) wouldSend(t *testing.T, p *Provider, kind string) (bool, string) {
	t.Helper()
	nodes, err := p.storeSettings(staffCtx(emailStaff), map[string]any{"siteId": linkSite, "storeId": linkStore}, 0)
	all, _ := reply(t, nodes, err)["emailsWouldSend"].(map[string]any)
	one, _ := all[kind].(map[string]any)
	would, _ := one["wouldSend"].(bool)
	return would, asString(one["reason"])
}

// THE SETTINGS AND A SEND GIVE ONE ANSWER, because both ask as one person: the
// store's merchant, the owner its settings name. A store whose settings belong
// to one person and whose applications to another: when the store's merchant
// holds the send's authority and the application's owner does not, the settings
// say the email would be sent and the send sends it, as the store's merchant;
// the other way round the settings say it would not be, in the lane's own
// sentence -- never "no sender is chosen", which is for a store with none
// chosen -- and the send sends nothing.
func TestTheSettingsAndASendGiveOneAnswerWhoeverOwnsTheApplication(t *testing.T) {
	const storeMerchant = "v1:identity:user:merchant-of-the-store"
	for _, kind := range EmailKinds {
		for name, c := range map[string]struct {
			unauthorized string
			would        bool
		}{
			"the store's merchant may send, the application's owner may not": {linkMerchant, true},
			"the application's owner may send, the store's merchant may not": {storeMerchant, false},
		} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				w, p := worldIn(kind)
				w.settings["ownerUserId"] = storeMerchant
				w.lane.unauthorized = map[string]bool{c.unauthorized: true}
				would, reason := w.wouldSend(t, p, kind)
				if would != c.would {
					t.Fatalf("the settings say the %s email would be sent: %v (%q), want %v", kind, would, reason, c.would)
				}
				if !would && (reason != laneNoAuthority || reason == NoSenderChosenReason) {
					t.Fatalf("the settings say why not as %q, want the lane's sentence %q", reason, laneNoAuthority)
				}
				for _, actor := range w.lane.senderActors {
					if actor != storeMerchant {
						t.Fatalf("the settings read the sender as %v, want the store's merchant", w.lane.senderActors)
					}
				}
				got := w.automatic(t, p, kind)
				if (got["sent"] == true) != would || (len(w.lane.sent) == 1) != would {
					t.Fatalf("the settings said would send %v; the send answered %v and handed over %d emails", would, got, len(w.lane.sent))
				}
				for _, actor := range append(append([]string{}, w.lane.senderActors...), w.lane.sendActors...) {
					if actor != storeMerchant {
						t.Fatalf("the send asked the lane as %v and %v, want the store's merchant throughout", w.lane.senderActors, w.lane.sendActors)
					}
				}
				if got["reason"] == NoSenderChosenReason || strings.Contains(fmt.Sprint(w.sends), NoSenderChosenReason) {
					t.Fatalf("a send whose person may not read the sender was told no sender is chosen: %v", got)
				}
			})
		}
	}
}

// A STORE WHOSE SETTINGS NAME NO OWNER HAS NOBODY TO SEND AS: the settings say
// so of the sender and of every email, and a due email is recorded as skipped
// with that sentence -- it is not sent as whoever happens to be calling.
func TestAStoreWhoseSettingsNameNoOwnerSendsAsNobody(t *testing.T) {
	w, p := worldIn(EmailRejected)
	w.settings["ownerUserId"] = ""
	nodes, err := p.storeSettings(staffCtx(emailStaff), map[string]any{"siteId": linkSite, "storeId": linkStore}, 0)
	settings := reply(t, nodes, err)
	if sender, _ := settings["sender"].(map[string]any); sender["ready"] != false || sender["reason"] != NoStoreOwnerSentence {
		t.Fatalf("the sender reads %v", settings["sender"])
	}
	if would, reason := w.wouldSend(t, p, EmailRejected); would || reason != NoStoreOwnerSentence {
		t.Fatalf("the settings say the rejection email would be sent: %v (%q)", would, reason)
	}
	got := w.automatic(t, p, EmailRejected)
	if got["state"] != SendSkipped || got["recorded"] != true || got["reason"] != NoStoreOwnerSentence || len(w.lane.calls) != 0 || len(w.lane.sendersAsked) != 0 {
		t.Fatalf("the send answered %v after %d lane sends and %d sender reads", got, len(w.lane.calls), len(w.lane.sendersAsked))
	}
}

// THE PACK DECIDES ON THE LANE'S FIELDS, NEVER ON ITS SENTENCES. The lane says
// "could not be read just now" and "none is chosen" in fields of their own;
// whatever words it puts beside them, the pack's decision does not move: a
// sender that could not be read declines nothing (the send goes to the lane),
// and one the lane says is not chosen is declined, recorded, with the pack's
// own sentence.
func TestThePackDecidesOnTheLanesFieldsNotItsSentences(t *testing.T) {
	for _, words := range []string{laneReadFailed, laneNoneChosen, "Reworded since: nothing could be said of the sender."} {
		// COULD NOT BE READ JUST NOW, in whatever words.
		w, p := worldIn(EmailRejected)
		w.lane.senders = map[string]map[string]any{"": {"ready": false, "fromName": "", "address": "", "reason": words,
			"readFailed": true, "noneChosen": false}}
		if would, reason := w.wouldSend(t, p, EmailRejected); would || reason != SenderUnreadableSentence {
			t.Fatalf("with the lane's sentence %q the settings say %v, %q; want not ready, in the pack's own sentence", words, would, reason)
		}
		if got := w.automatic(t, p, EmailRejected); got["state"] != SendSent || len(w.lane.calls) != 1 {
			t.Fatalf("with the lane's sentence %q a sender that could not be read answered %v after %d lane calls; want the lane to decide",
				words, got, len(w.lane.calls))
		}

		// NONE IS CHOSEN, in whatever words.
		w, p = worldIn(EmailRejected)
		w.lane.senders = map[string]map[string]any{"": {"ready": false, "fromName": "", "address": "", "reason": words,
			"readFailed": false, "noneChosen": true}}
		if would, reason := w.wouldSend(t, p, EmailRejected); would || reason != NoSenderChosenReason {
			t.Fatalf("with the lane's sentence %q the settings say %v, %q; want the pack's no-sender sentence", words, would, reason)
		}
		if got := w.automatic(t, p, EmailRejected); got["state"] != SendSkipped || got["reason"] != NoSenderChosenReason || len(w.lane.calls) != 0 {
			t.Fatalf("with the lane's sentence %q a store with no sender answered %v after %d lane calls", words, got, len(w.lane.calls))
		}

		// NEITHER FIELD: the lane's own sentence, and the lane decides.
		w, p = worldIn(EmailRejected)
		w.lane.senders = map[string]map[string]any{"": {"ready": false, "fromName": "", "address": "", "reason": words,
			"readFailed": false, "noneChosen": false}}
		if would, reason := w.wouldSend(t, p, EmailRejected); would || reason != words {
			t.Fatalf("with neither field the settings say %v, %q; want the lane's sentence %q", would, reason, words)
		}
		if got := w.automatic(t, p, EmailRejected); len(w.lane.calls) != 1 || got["reason"] == NoSenderChosenReason {
			t.Fatalf("with neither field the send answered %v after %d lane calls; want the lane to decide", got, len(w.lane.calls))
		}
	}
}

// ---------------------------------------------------------------------------
// The sentences
// ---------------------------------------------------------------------------

// A SKIPPED SEND'S REASON IS A SENTENCE, NEVER THE LANE'S CODE: the lane says
// why in a sentence of its own beside the code, at the send and in its status
// read, and the pack answers and records that sentence. The code stays where
// the lane's own record is shown, beside it.
func TestASkippedSendsReasonIsTheLanesSentenceNeverItsCode(t *testing.T) {
	w, p := worldIn(EmailRejected)
	w.lane.outcome = "suppressed"
	bounced := campaigns.SkipSentence("hard_bounce")
	got := w.automatic(t, p, EmailRejected)
	if got["state"] != SendSkipped || got["reason"] != bounced {
		t.Fatalf("a send to an address the email service no longer sends to answered %v, want the sentence %q", got, bounced)
	}
	if row := w.sends[EmailSendRowID(EmailRejected, linkApp, 1)]; row["reason"] != bounced {
		t.Fatalf("the row's reason is %q", row["reason"])
	}
	id := LaneRequestID(EmailRejected, linkApp, 1)
	for code, sentence := range map[string]string{
		"hard_bounce": bounced,
		"complaint":   campaigns.SkipSentence("complaint"),
		"manual":      campaigns.SkipSentence("manual"),
	} {
		// THE LANE'S STATUS READ, with its sentence; and one that carries the code
		// alone, which is read as the lane's own words for it.
		for _, status := range []map[string]any{
			{"requestId": id, "status": "skipped", "skipReason": code, "skipSentence": sentence},
			{"requestId": id, "status": "skipped", "skipReason": code},
		} {
			w.lane.statuses = []map[string]any{status}
			shown := emailShown(t, w.application(t, p), EmailRejected, 1)
			d, _ := shown["delivery"].(map[string]any)
			if shown["state"] != SendSkipped || shown["reason"] != sentence || shown["reason"] == code || d["skipReason"] != code {
				t.Fatalf("a %s skip shows %v, want the sentence %q and the code beside it in the delivery", code, shown, sentence)
			}
			if exported := w.exportedEmails(t, p); exported[0]["reason"] != sentence {
				t.Fatalf("the export gives a %s skip the reason %q", code, exported[0]["reason"])
			}
			if again := w.automatic(t, p, EmailRejected); again["reason"] != sentence {
				t.Fatalf("the automatic answer gives a %s skip the reason %q", code, again["reason"])
			}
		}
	}
	// A WITHDRAWAL IS THE ROW'S OWN SENTENCE, as it was given.
	w.lane.statuses = []map[string]any{{"requestId": id, "status": "skipped", "skipReason": notInStateReason(StateRejected),
		"skipSentence": notInStateReason(StateRejected)}}
	if shown := emailShown(t, w.application(t, p), EmailRejected, 1); shown["reason"] != "The application is not rejected." {
		t.Fatalf("a withdrawn email shows %v", shown)
	}
}

// AN UNCERTAIN ATTEMPT ALWAYS CARRIES THE UNCERTAIN SENTENCE, on its row and on
// the application's page: whether the lane's reply said nothing of why or said
// it in words of its own, the reason is exactly the ruled sentence -- never
// empty, never the lane's words.
func TestAnUncertainAttemptAlwaysCarriesTheUncertainSentence(t *testing.T) {
	const ruled = "We could not confirm this email was sent. It may have been delivered."
	for name, laneReply := range map[string]map[string]any{
		"the lane's reply has no reason": {"sent": false, "skipped": false, "uncertain": true, "replayed": true},
		"the lane's reply has a reason of its own": {"sent": false, "skipped": false, "uncertain": true, "replayed": true,
			"reason": "The previous send may have reached the provider. It is not sent again until the provider says whether it accepted it."},
		"the lane's reply has a blank reason": {"sent": false, "skipped": false, "uncertain": true, "reason": "  "},
	} {
		t.Run(name, func(t *testing.T) {
			w, p := worldIn(EmailRejected)
			w.lane.reply = laneReply
			got := w.automatic(t, p, EmailRejected)
			if got["state"] != SendUncertain || got["reason"] != ruled {
				t.Fatalf("the uncertain attempt answered %v", got)
			}
			if row := w.sends[EmailSendRowID(EmailRejected, linkApp, 1)]; row["state"] != SendUncertain || row["reason"] != ruled {
				t.Fatalf("the uncertain attempt's row is %v", row)
			}
			if shown := emailShown(t, w.application(t, p), EmailRejected, 1); shown["state"] != SendUncertain || shown["reason"] != ruled {
				t.Fatalf("the page shows the uncertain attempt as %v", shown)
			}
			if exported := w.exportedEmails(t, p); exported[0]["state"] != SendUncertain || exported[0]["reason"] != ruled {
				t.Fatalf("the export shows the uncertain attempt as %v", exported[0])
			}
		})
	}
	// A ROW AN EARLIER BUILD RECORDED UNCERTAIN WITH THE LANE'S WORDS, OR WITH
	// NONE, is answered with the ruled sentence too.
	for _, stored := range []string{"", "campaigns.sendToRecipient: the connection to the provider closed"} {
		w, p := worldIn(EmailRejected)
		id := EmailSendRowID(EmailRejected, linkApp, 1)
		w.putSend(id, map[string]any{"emailSendId": id, "ownerUserId": linkMerchant, "storeId": linkStore, "accountId": "acct-links",
			"applicationId": applicationConcept + ":" + linkApp, "kind": EmailRejected, "attempt": 1,
			"requestId": LaneRequestID(EmailRejected, linkApp, 1), "state": SendUncertain, "reason": stored})
		if shown := emailShown(t, w.application(t, p), EmailRejected, 1); shown["reason"] != ruled {
			t.Fatalf("a row recorded uncertain with the reason %q shows %v", stored, shown)
		}
	}
}

// EVERY REASON IS A WHOLE SENTENCE: a capital first, a full stop last. Walked
// here: every reason constant of the pack, read from its source so one added
// later is held to the rule too; every sentence the pack composes; and every
// reason the application's emails, the store's settings and the buyers list
// answer, across every way an attempt and a store can stand -- the lane's own
// words and a route's error text among them, which are answered as whole
// sentences whatever form they arrived in. (The account backfill's report says
// why a row was left untied in fragments of its own; it is none of these three
// and is not walked.)
func TestEveryReasonIsAWholeSentence(t *testing.T) {
	useTestLaneReaders(t)
	whole := func(s string) bool {
		first, _ := utf8.DecodeRuneInString(s)
		return s != "" && unicode.IsUpper(first) && strings.HasSuffix(s, ".") && s == strings.TrimSpace(s)
	}
	// 1. EVERY REASON CONSTANT, from the source.
	files, err := parser.ParseDir(token.NewFileSet(), ".", func(f fs.FileInfo) bool { return !strings.HasSuffix(f.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatalf("parse the pack: %v", err)
	}
	isReason := func(name string) bool {
		return strings.Contains(strings.ToLower(name), "reason") || strings.HasSuffix(name, "Sentence") ||
			strings.HasPrefix(name, "withdrawn") || strings.HasPrefix(name, "Note")
	}
	walked := 0
	for _, pkg := range files {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					values := spec.(*ast.ValueSpec)
					for i, name := range values.Names {
						if i >= len(values.Values) || !isReason(name.Name) {
							continue
						}
						lit, ok := values.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						text, err := strconv.Unquote(lit.Value)
						if err != nil {
							t.Fatalf("%s: %v", name.Name, err)
						}
						walked++
						if !whole(text) {
							t.Errorf("the constant %s is %q, want a whole sentence", name.Name, text)
						}
					}
				}
			}
		}
	}
	if walked < 30 {
		t.Fatalf("the walk found %d reason constants; it should find the pack's thirty and more", walked)
	}
	// 2. EVERY SENTENCE THE PACK COMPOSES.
	for _, kind := range EmailKinds {
		if got := noTemplateReason(kind); !whole(got) {
			t.Errorf("noTemplateReason(%s) is %q", kind, got)
		}
	}
	for _, state := range []string{StateApproved, StateRejected, StateRevoked} {
		if got := notInStateReason(state); !whole(got) {
			t.Errorf("notInStateReason(%s) is %q", state, got)
		}
	}
	// 3. EVERY REASON THE THREE SURFACES ANSWER.
	said := func(where, reason string) {
		t.Helper()
		if reason != "" && !whole(reason) {
			t.Errorf("%s answers the reason %q, want a whole sentence", where, reason)
		}
	}
	seen := 0
	for _, kind := range EmailKinds {
		for last, makeLast := range attemptScenarios() {
			for store, makeStore := range storeScenarios() {
				w, p := worldIn(kind)
				makeLast(t, w, p, kind)
				w.lane.outcome = ""
				makeStore(w, kind)
				where := kind + "/" + last + "/" + store
				// THE EMAILS VIEW, the send's own answers and the export.
				emails, _ := w.application(t, p)["emails"].([]any)
				for _, e := range emails {
					said("the emails view of "+where, asString(e.(map[string]any)["reason"]))
					seen++
				}
				for _, e := range w.exportedEmails(t, p) {
					said("the export of "+where, asString(e["reason"]))
				}
				said("the automatic answer of "+where, asString(w.automatic(t, p, kind)["reason"]))
				if resent, err := w.resend(t, p, kind); err == nil {
					said("the resend's answer of "+where, asString(resent["reason"]))
				}
				for _, row := range w.sends {
					said("the row recorded for "+where, asString(row["reason"]))
				}
				// THE SETTINGS: the sender line, and each email's.
				nodes, err := p.storeSettings(staffCtx(emailStaff), map[string]any{"siteId": linkSite, "storeId": linkStore}, 0)
				settings := reply(t, nodes, err)
				sender, _ := settings["sender"].(map[string]any)
				said("the sender line of "+where, asString(sender["reason"]))
				would, _ := settings["emailsWouldSend"].(map[string]any)
				for k, v := range would {
					said("emailsWouldSend."+k+" of "+where, asString(v.(map[string]any)["reason"]))
					seen++
				}
			}
		}
	}
	if seen < 500 {
		t.Fatalf("the walk read %d reasons; it should read hundreds", seen)
	}
	// WORDS THAT ARE NOT THE PACK'S are answered as whole sentences too: the
	// lane's error text on a failed attempt, a sentence of the lane's with no
	// capital or stop, and a store that is not set up.
	w, p := worldIn(EmailRejected)
	w.lane.outcome = "fail"
	w.automatic(t, p, EmailRejected)
	if e := emailShown(t, w.application(t, p), EmailRejected, 1); e["reason"] != "Campaigns.sendRowMessage: the template must belong to the row's organization." {
		t.Errorf("the lane's error text is shown as %q", e["reason"])
	}
	w.settings["senderIdentityId"] = "sender-1"
	w.lane.senders = map[string]map[string]any{"sender-1": {"ready": false, "reason": "choose a sending identity belonging to this organization"}}
	if would, reason := w.wouldSend(t, p, EmailRejected); would || reason != "Choose a sending identity belonging to this organization." {
		t.Errorf("a lane sentence with no capital or stop is answered as %q", reason)
	}
	for _, v := range notSetUpEmails() {
		said("a store that is not set up", asString(v.(map[string]any)["reason"]))
	}
	said("the sender of a store that is not set up", NotSetUpSenderSentence)

	// THE BUYERS LIST: why access is not on is the access record's own text,
	// which a route wrote as an error. It is answered as a whole sentence, the
	// same one the application's page shows.
	w, p = worldIn(EmailApproved)
	w.ents[len(w.ents)-1]["state"] = EntitlementFailed
	w.ents[len(w.ents)-1]["errorCode"] = ErrorCustomerRefused
	w.ents[len(w.ents)-1]["error"] = "wholesale: customerTag refused: the store said no"
	nodes, err := p.buyers(staffCtx(emailStaff), map[string]any{"siteId": linkSite, "storeId": linkStore}, 0)
	rows := rowsOf(t, reply(t, nodes, err))
	ent, _ := w.application(t, p)["entitlement"].(map[string]any)
	if len(rows) != 1 || rows[0]["error"] != "Wholesale: customerTag refused: the store said no." || ent["error"] != rows[0]["error"] {
		t.Errorf("the buyers row says %v and the application's page %q; want one whole sentence on both", rows, ent["error"])
	}
}
