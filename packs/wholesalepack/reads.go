package wholesalepack

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// reads.go -- THE READS THE WHOLESALE SECTION OPENS (U6): the list, one
// application, the decision log and the buyers. export.go is the fifth.
//
// # Who is admitted, and what is read as whom
//
// Every one of them is gated by the wholesale part. The organization gate
// judges a store-wide read against the SITE's organization and wholesaleGet
// against the APPLICATION's; a store-wide read then checks the site serves the
// store and belongs to the organization its settings are tied to
// (siteForStore) before it reads anything keyed by the store.
//
// THE APPLICATIONS AND THE DECISION LOG ARE READ UNDER THE CALLER. The row gate
// decides which applications a person sees, and a person who cannot read an
// application learns nothing of it here: no row, no count, no line in the log.
//
// AN ADMITTED APPLICATION'S STATE IS JUDGED AGAINST ITS WHOLE LOG (X27). Once
// the caller's own read returned an application, its decisions are folded and
// its entitlement read as the deployment, as wholesaleRecordDecision and
// wholesaleApplicationState do -- a decision written before the account tie
// names no merchant, and a fold over the part the caller reads would show a
// rejected application as ready to review. So the list, the buyers,
// wholesaleGet and wholesaleApplicationState always agree.
//
// A PERSON'S NAME is read through @serverOnly userByIdSystem, at internal
// origin inline on that one read, as the deployment, and only after the
// caller was admitted. Somebody who cannot be found has an empty name.
//
// EVERY REPLY NAMES AN APPLICATION BY THE ROW'S OWN id, as the engine answers
// it, whichever form the caller named it in.

const (
	defaultReadLimit = 50
	maxReadLimit     = 200
	// maxStorePages bounds how many pages one call reads of a store-wide log.
	maxStorePages = 20
	// maxStoreApplications bounds a read over a store's applications: twenty
	// pages of applicationsForStore's fifty. Past it a reply says truncated.
	maxStoreApplications = maxStorePages * 50
	// stageAll lists every stage.
	stageAll = "all"
	// AccessNone is a buyer's access when nothing was ever granted.
	AccessNone = "none"
	// extensionForm is the pack's form a client's own fields extend.
	extensionForm = "application"
)

// Stages is every stage an application is shown in, in the order a reader
// meets them.
var Stages = []string{StageWaitingForTerms, StageReadyToReview, StateApproved, StateRejected, StateRevoked}

// The acts the Wholesale section offers on one application.
const (
	ActReject      = "reject"
	ActApprove     = "approve"
	ActRemove      = "remove"
	ActRetryAccess = "retryAccess"
	// ActResendTerms sends the terms email again (wholesaleResendEmail).
	ActResendTerms = "resendTerms"
	// ActResendApproved sends the approval email again.
	ActResendApproved = "resendApproved"
	// ActResendRejected sends the rejection email again.
	ActResendRejected = "resendRejected"
	// ActResendRevoked sends the removal email again.
	ActResendRevoked = "resendRevoked"
)

// extensionReader is the engine's read of a client's own fields on a row of
// the pack's form (memql.MemQLEngine.ShopperExtensionRow).
type extensionReader interface {
	ShopperExtensionRow(ctx context.Context, pack, form, rowID string) (memql.ExtensionRow, bool, error)
}

// ActsInput is what the acts on one application depend on.
type ActsInput struct {
	Stage, AccessState          string
	EntitlementExists, Operator bool
	// Emails is where each of the applicant's emails stands, by kind
	// (EmailStanding): what a resend of it is judged on. A kind it says nothing
	// of is not offered.
	Emails map[string]EmailStanding
}

// ActsFor is the acts open on one application.
//
// A person of the store decides a waiting application (reject; approve once it
// is ready), removes an approved buyer, and retries access that did not land:
// on an approved application whose access is not granted, or a revoked one
// whose entitlement is still not revoked. A cluster operator decides nothing
// (X4) and is offered the retry under the same two conditions and nothing else
// (ruling 30): a retry is not a decision (X26).
//
// A person of the store also sends an email again (U9): the terms email on an
// application waiting for its terms, after reject; the approval email to a
// buyer whose access is granted, after remove; the rejection or the removal
// email on a rejected or removed application. EACH IS OFFERED EXACTLY WHEN THE
// RESEND WOULD NOT BE REFUSED: the one function wholesaleResendEmail refuses
// by (resendRefusal) is asked here, of the standing the input carries for the
// kind. An operator sends none.
func ActsFor(in ActsInput) []string {
	retry := (in.Stage == StateApproved && in.AccessState != EntitlementGranted) ||
		(in.Stage == StateRevoked && in.EntitlementExists && in.AccessState != EntitlementRevoked)
	acts := []string{}
	if in.Operator {
		if retry {
			acts = append(acts, ActRetryAccess)
		}
		return acts
	}
	resend := func(kind string) bool {
		standing, judged := in.Emails[kind]
		return judged && resendRefusal(kind, in.Stage, in.AccessState, standing) == ""
	}
	switch in.Stage {
	case StageWaitingForTerms:
		acts = append(acts, ActReject)
		if resend(EmailTerms) {
			acts = append(acts, ActResendTerms)
		}
	case StageReadyToReview:
		acts = append(acts, ActReject, ActApprove)
	case StateApproved:
		acts = append(acts, ActRemove)
		if retry {
			acts = append(acts, ActRetryAccess)
		}
		if resend(EmailApproved) {
			acts = append(acts, ActResendApproved)
		}
	case StateRejected:
		if resend(EmailRejected) {
			acts = append(acts, ActResendRejected)
		}
	case StateRevoked:
		if retry {
			acts = append(acts, ActRetryAccess)
		}
		if resend(EmailRevoked) {
			acts = append(acts, ActResendRevoked)
		}
	}
	return acts
}

// resendKindOf is the email a person could send again on an application in
// each stage; a stage it does not name has none.
var resendKindOf = map[string]string{StageWaitingForTerms: EmailTerms, StateApproved: EmailApproved,
	StateRejected: EmailRejected, StateRevoked: EmailRevoked}

// resendStandings is ActsInput.Emails for one application: the standing of the
// one email its stage could send again, read off its emails as the page shows
// them. An operator sends none, so nothing is read for one.
//
// THE STORE'S READINESS IS ASKED LAST, and only when everything else admits the
// resend: it is a read of the store's sites and of the lane (emailBlocker),
// made as the person a send of this application runs as.
func (p *Provider) resendStandings(ctx context.Context, settings Settings, app map[string]any, stage, access string, operator bool,
	emails []map[string]any) map[string]EmailStanding {
	kind := resendKindOf[stage]
	if operator || kind == "" {
		return nil
	}
	standing := emailStandingOf(emails, kind)
	if resendRefusal(kind, stage, access, standing) == "" {
		standing.Blocker, _ = p.emailBlocker(ctx, settings, app, kind)
	}
	return map[string]EmailStanding{kind: standing}
}

// What a person reads of an access record whose cause the route does not name,
// and what is left off the words of one the pack's own routes recorded.
const (
	// storeRefusedLead comes before the store's own words, given as it gave them.
	storeRefusedLead = "The store refused this change. Its answer was: "
	// StoreRefusedSentence is a refusal the store gave no words for.
	StoreRefusedSentence = "The store refused this change."
	// AccessUnconfirmedSentence is anything else that kept a change from being
	// recorded as done: the store could not be reached, or what it answered
	// could not be read. It says what a person can do next, and nothing of the
	// error.
	AccessUnconfirmedSentence = "This change could not be confirmed with the store, so it is not recorded as done. Retry access; if it still does not go through, the cluster's operators can say why."
	// connectorPrefix is how the Shopify connector, which the pack's own two
	// routes ask, begins the text of a refusal: its package's name.
	connectorPrefix = "shopify: "
)

// accessReason is why an application's access is not on, AS A PERSON READS IT.
// The buyers list, the application's page and the export all answer it, so they
// say one thing.
//
// The access record keeps what the route's attempt came to as the text of an
// error (its error field: the pack's own prefix, and for a refusal the route's
// name, before the words) beside the cause it names (errorCode). That text is
// the record's, not a reader's:
//
//   - A CAUSE THE ROUTE NAMES -- no settings, no route, the store's permissions,
//     its plan's ceiling, a refusal for want of a connector or of anything to
//     undo -- reads as the sentence recorded for it, with no prefix of the
//     pack's.
//   - A REFUSAL WHOSE CAUSE THE ROUTE DOES NOT NAME -- the store said no to the
//     change itself (customer_refused, which is also what a refusal under a code
//     this build does not know is recorded as), or a refusal recorded before
//     records named their cause -- reads "The store refused this change. Its
//     answer was: " and the store's words as it gave them.
//   - ANYTHING ELSE -- an outage, an answer nobody could read: what is recorded
//     is an error's chain, which names the constructs that failed. It is never
//     answered; the pack's own sentence is (AccessUnconfirmedSentence).
//
// THE PACK'S OWN TWO ROUTES ASK THE SHOPIFY CONNECTOR, whose refusal is itself
// an error's text and begins with the connector's package name
// (connectorPrefix): that name is no part of the words, and is left off. A
// route of a client's own is given word for word -- the pack cannot tell a
// package's name from a store's own words that begin with a word and a colon.
func accessReason(ent map[string]any) string {
	recorded := strings.TrimSpace(asString(ent["error"]))
	if recorded == "" {
		return ""
	}
	words, prefixed := strings.CutPrefix(recorded, "wholesale: ")
	reason, refusal := "", false
	if prefixed {
		reason, refusal = refusalWords(words)
	}
	if route := trimmed(ent["adapter"]); refusal && (route == AdapterShopifyB2B || route == AdapterCustomerTag) {
		reason = strings.TrimPrefix(reason, connectorPrefix)
	}
	asGiven := func(said string) string {
		if said = strings.TrimSpace(said); said == "" {
			return StoreRefusedSentence
		}
		switch last, _ := utf8.DecodeLastRuneInString(said); last {
		case '.', '!', '?':
			return storeRefusedLead + said
		}
		return storeRefusedLead + said + "."
	}
	// named is a cause the route names, in the sentence recorded for it; one
	// recorded with no words has none to give.
	named := func(sentence string) string {
		if sentence = wholeSentence(sentence); sentence != "" {
			return sentence
		}
		return AccessUnconfirmedSentence
	}
	switch code := trimmed(ent["errorCode"]); {
	case code == ErrorCustomerRefused:
		if refusal {
			return asGiven(reason)
		}
		return asGiven(words)
	case code == ErrorNoSettings, code == ErrorNoRoute, code == ErrorScopeMissing, code == ErrorPlanCeiling:
		if refusal {
			return named(reason)
		}
		return named(words)
	case refusal && code == ErrorShopifyError:
		return named(reason)
	case refusal:
		return asGiven(reason)
	}
	return AccessUnconfirmedSentence
}

// refusalWords reads the words of a route's refusal out of what a record keeps
// of one, once the pack's own prefix is off it: "<the route's name> refused:
// <the words>" (AdapterRefusal). False for anything else.
func refusalWords(recorded string) (string, bool) {
	route, words, found := strings.Cut(recorded, " refused:")
	if !found || route == "" || strings.ContainsAny(route, " \t:") {
		return "", false
	}
	return strings.TrimSpace(words), true
}

// staged is one application the caller reads, with the state its whole log
// gives it.
type staged struct {
	row          map[string]any
	id           string // the row's own id, as the engine answers it
	state, stage string
	at           time.Time // when it was submitted (happenedAt), which the lists order by
}

// storeApplications reads the store's applications under ctx, newest submitted
// first (happenedPages), in every form a store id is held in, at most
// maxStoreApplications of them, and says whether it stopped short -- in which
// case the OLDEST are left out. limited false reads them all.
func (p *Provider) storeApplications(ctx context.Context, storeID string, limited bool) ([]map[string]any, bool, error) {
	var out []map[string]any
	seen := map[string]bool{}
	read := &happenedPages{p: p, ctx: ctx, query: "applicationsForStore", args: map[string]string{"storeId": storeID},
		listName: "storeIds", list: storeIDForms(storeID), field: fieldSubmittedAt}
	for {
		row, err := read.take()
		if err != nil {
			return nil, false, err
		}
		if row == nil {
			return out, false, nil
		}
		id := trimmed(row["id"])
		if id != "" && seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, row)
		if limited && len(out) == maxStoreApplications {
			return out, read.more(), nil
		}
	}
}

// storeIDForms is every form a store id is held in, each once.
func storeIDForms(storeID string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range storeForms(strings.TrimSpace(storeID)) {
		if f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// stagedApplications is the store's applications the caller reads, each staged
// by its whole log, read as the deployment (X27).
func (p *Provider) stagedApplications(ctx context.Context, storeID string) ([]staged, bool, error) {
	apps, truncated, err := p.storeApplications(ctx, storeID, true)
	if err != nil {
		return nil, false, err
	}
	ids := make([]string, 0, len(apps))
	for _, app := range apps {
		ids = append(ids, trimmed(app["id"]))
	}
	sys := auth.ContextWithSystemActor(ctx, provisionActor)
	logs, err := p.rowsForApplications(sys, "decisionsForApplicationIds", ids)
	if err != nil {
		return nil, false, err
	}
	required, accepted, err := p.acceptancesOf(sys, storeID, ids)
	if err != nil {
		return nil, false, err
	}
	out := make([]staged, 0, len(apps))
	for _, app := range apps {
		id := trimmed(app["id"])
		if id == "" {
			continue
		}
		state := FoldState(logs[bareID(id)])
		stage := StageOf(state, required, len(accepted[bareID(id)]) > 0)
		out = append(out, staged{row: app, id: id, state: state, stage: stage, at: happenedAt(app, fieldSubmittedAt)})
	}
	return out, truncated, nil
}

// acceptancesOf answers whether the store requires its terms and, when it does,
// the acceptances of the applications at ids grouped by bare id, read through
// sys IN BATCHES (rowsForApplications) as the decisions are, never one read per
// application. A store that requires none reads no acceptance.
func (p *Provider) acceptancesOf(sys context.Context, storeID string, ids []string) (bool, map[string][]map[string]any, error) {
	required, err := p.termsRequiredAt(sys, storeID)
	if err != nil || !required {
		return required, nil, err
	}
	accepted, err := p.rowsForApplications(sys, "acceptancesForApplicationIds", ids)
	return true, accepted, err
}

// list answers wholesaleList.
func (p *Provider) list(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID, siteID, err := p.admitStore(ctx, args)
	if err != nil {
		return nil, err
	}
	stage := trimmed(args["stage"])
	if stage == "" {
		stage = StageReadyToReview
	}
	if stage != stageAll && !knownStage(stage) {
		return nil, fmt.Errorf("wholesale: %q is not a stage; a stage is %s or %s", stage, strings.Join(Stages, ", "), stageAll)
	}
	search := searchOf(args)
	key := readKey("list", storeID, siteID, stage, search)
	cursor, err := readCursor(trimmed(args["after"]), key)
	if err != nil {
		return nil, err
	}
	apps, truncated, err := p.stagedApplications(ctx, storeID)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, s := range Stages {
		counts[s] = 0
	}
	var matched []staged
	for _, a := range apps {
		counts[a.stage]++
		if stage != stageAll && a.stage != stage {
			continue
		}
		if !searchMatches(search, a.row["companyName"], a.row["applicantName"], a.row["applicantEmail"]) {
			continue
		}
		matched = append(matched, a)
	}
	// The queue is oldest first: how long an application has waited is what
	// the store's people answer to. Every other view is newest first.
	oldestFirst := stage == StageReadyToReview
	page, next := pageOf(matched, cursor, limitOf(args), oldestFirst, key)
	rows := make([]map[string]any, 0, len(page))
	for _, a := range page {
		primary, secondary, err := p.displayFacts(ctx, a.row)
		if err != nil {
			return nil, err
		}
		rows = append(rows, map[string]any{
			"applicationId":  a.id,
			"companyName":    asString(a.row["companyName"]),
			"applicantName":  asString(a.row["applicantName"]),
			"applicantEmail": asString(a.row["applicantEmail"]),
			"stage":          a.stage,
			"primary":        primary,
			"secondary":      secondary,
			"createdAt":      timeText(a.at),
		})
	}
	return confirm("wholesale:list", map[string]any{"rows": rows, "counts": counts, "next": next, "truncated": truncated,
		"operator": isClusterOperator(ctx)})
}

// get answers wholesaleGet.
func (p *Provider) get(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	applicationID := trimmed(args["applicationId"])
	if applicationID == "" {
		return nil, fmt.Errorf("wholesale: applicationId is required")
	}
	// THE CALLER READS THE APPLICATION FIRST; one who cannot is answered
	// nothing, and nothing is read as the deployment for them.
	rows, err := p.rowsFor(ctx, "applicationById", map[string]string{"applicationId": applicationID})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("wholesale: application %q is not readable by this caller", applicationID)
	}
	app := rows[0]
	sys := auth.ContextWithSystemActor(ctx, provisionActor)
	log, err := p.rowsForApplication(sys, "decisionsForApplication", applicationID)
	if err != nil {
		return nil, err
	}
	ent, err := p.entitlementRow(sys, applicationID)
	if err != nil {
		return nil, err
	}
	state := FoldState(log)
	// THE ACCEPTANCE AND THE TERMS LINK, read as the deployment: what the stage
	// waits for, and what the store's people see of it.
	settingsRow, err := p.settingsRowAt(sys, trimmed(app["storeId"]))
	if err != nil {
		return nil, err
	}
	settings := EffectiveSettings(settingsRow)
	termsRequired := settings.TermsRequired
	// THE EMAILS SENT, and what the lane says of each: read once the caller was
	// admitted and read the application above.
	sends, err := p.emailSendsOf(sys, applicationID)
	if err != nil {
		return nil, err
	}
	acceptance, err := p.acceptanceOf(sys, applicationID)
	if err != nil {
		return nil, err
	}
	acceptanceShown, err := p.acceptanceView(sys, acceptance)
	if err != nil {
		return nil, err
	}
	termsLink, err := p.termsLinkView(sys, applicationID, p.clock())
	if err != nil {
		return nil, err
	}
	stage := StageOf(state, termsRequired, acceptance != nil)
	people := p.people(ctx)
	decisions := make([]map[string]any, 0, len(log))
	for _, d := range log {
		decisions = append(decisions, map[string]any{
			"transition":    trimmed(d["transition"]),
			"decidedBy":     trimmed(d["decidedBy"]),
			"decidedByName": people.name(trimmed(d["decidedBy"])),
			"note":          asString(d["note"]),
			"createdAt":     timeText(happenedAt(d, fieldDecidedAt)),
		})
	}
	details, err := p.details(ctx, app)
	if err != nil {
		return nil, err
	}
	access := AccessNone
	var entitlement any
	if ent != nil {
		access = trimmed(ent["state"])
		customerCreated, _ := ent["customerCreated"].(bool)
		entitlement = map[string]any{
			"state":             access,
			"errorCode":         trimmed(ent["errorCode"]),
			"error":             accessReason(ent),
			"adapter":           trimmed(ent["adapter"]),
			"shopifyCustomerId": trimmed(ent["shopifyCustomerId"]),
			"customerCreated":   customerCreated,
			"updatedAt":         timeText(happenedAt(ent, fieldAttemptedAt)),
		}
	}
	operator := isClusterOperator(ctx)
	addedBy := trimmed(app["addedBy"])
	emails := p.emailsView(ctx, app, sends)
	return confirm("wholesale:application", map[string]any{
		"application": map[string]any{
			"id":             trimmed(app["id"]),
			"storeId":        trimmed(app["storeId"]),
			"siteId":         trimmed(app["siteId"]),
			"companyName":    asString(app["companyName"]),
			"applicantName":  asString(app["applicantName"]),
			"applicantEmail": asString(app["applicantEmail"]),
			"source":         sourceOf(app),
			"addedBy":        addedBy,
			"addedByName":    people.name(addedBy),
			"createdAt":      timeText(happenedAt(app, fieldSubmittedAt)),
		},
		"state":       state,
		"stage":       stage,
		"details":     details,
		"decisions":   decisions,
		"entitlement": entitlement,
		"acceptance":  acceptanceShown,
		"termsLink":   termsLink,
		"emails":      emails,
		"operator":    operator,
		// EACH RESEND IS OFFERED EXACTLY WHEN IT WOULD NOT BE REFUSED (resendRefusal).
		"acts": ActsFor(ActsInput{Stage: stage, AccessState: access, EntitlementExists: ent != nil, Operator: operator,
			Emails: p.resendStandings(ctx, settings, app, stage, access, operator, emails)}),
	})
}

// logCursor is where the decision log's next page starts: where each half of
// decisionsForStore stands (happenedPages), the decisions that hold their
// decidedAt and those written before it.
type logCursor struct {
	Version int             `json:"v"`
	Key     string          `json:"key"`
	Halves  [2]happenedHalf `json:"halves"`
}

// decisionLog answers wholesaleDecisionLog.
func (p *Provider) decisionLog(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID, siteID, err := p.admitStore(ctx, args)
	if err != nil {
		return nil, err
	}
	search := searchOf(args)
	limit := limitOf(args)
	key := readKey("decisionLog", storeID, siteID, "", search)
	at := logCursor{Version: 2, Key: key}
	if after := trimmed(args["after"]); after != "" {
		raw, err := base64.RawURLEncoding.DecodeString(after)
		if err != nil || len(after) > 4096 || json.Unmarshal(raw, &at) != nil || at.Version != 2 || at.Key != key ||
			at.Halves[0].Offset < 0 || at.Halves[1].Offset < 0 {
			return nil, fmt.Errorf("wholesale: this page cursor does not match the decision log")
		}
	}
	// Each decision is shown only beside an application the caller reads:
	// the join is the caller's own read of it, by its bare id.
	apps := map[string]map[string]any{}
	appOf := func(applicationID string) (map[string]any, error) {
		bare := bareID(applicationID)
		if app, seen := apps[bare]; seen {
			return app, nil
		}
		rows, err := p.rowsFor(ctx, "applicationById", map[string]string{"applicationId": bare})
		if err != nil {
			return nil, err
		}
		var app map[string]any
		if len(rows) > 0 {
			app = rows[0]
		}
		apps[bare] = app
		return app, nil
	}
	people := p.people(ctx)
	rows := []map[string]any{}
	next := ""
	// NEWEST DECIDED FIRST (happenedPages), at most maxStorePages pages a call.
	read := &happenedPages{p: p, ctx: ctx, query: "decisionsForStore", args: map[string]string{"storeId": storeID},
		listName: "storeIds", list: storeIDForms(storeID), field: fieldDecidedAt, halves: at.Halves, budget: maxStorePages}
	for {
		here := read.position()
		d, err := read.take()
		if err != nil {
			return nil, err
		}
		if d == nil {
			if read.short {
				next = encodeLogCursor(logCursor{Version: 2, Key: key, Halves: here})
			}
			break
		}
		if len(rows) == limit {
			next = encodeLogCursor(logCursor{Version: 2, Key: key, Halves: here})
			break
		}
		app, err := appOf(trimmed(d["applicationId"]))
		if err != nil {
			return nil, err
		}
		if app == nil {
			continue
		}
		decider := trimmed(d["decidedBy"])
		name := people.name(decider)
		if !searchMatches(search, app["companyName"], app["applicantName"], app["applicantEmail"], name, d["note"]) {
			continue
		}
		rows = append(rows, map[string]any{
			"applicationId": trimmed(app["id"]),
			"companyName":   asString(app["companyName"]),
			"transition":    trimmed(d["transition"]),
			"decidedBy":     decider,
			"decidedByName": name,
			"note":          asString(d["note"]),
			"createdAt":     timeText(happenedAt(d, fieldDecidedAt)),
		})
	}
	return confirm("wholesale:decisionLog", map[string]any{"rows": rows, "next": next})
}

func encodeLogCursor(c logCursor) string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// buyers answers wholesaleBuyers.
func (p *Provider) buyers(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID, siteID, err := p.admitStore(ctx, args)
	if err != nil {
		return nil, err
	}
	search := searchOf(args)
	key := readKey("buyers", storeID, siteID, "", search)
	cursor, err := readCursor(trimmed(args["after"]), key)
	if err != nil {
		return nil, err
	}
	apps, truncated, err := p.stagedApplications(ctx, storeID)
	if err != nil {
		return nil, err
	}
	var matched []staged
	for _, a := range apps {
		if a.state == StateApproved && searchMatches(search, a.row["companyName"], a.row["applicantName"], a.row["applicantEmail"]) {
			matched = append(matched, a)
		}
	}
	page, next := pageOf(matched, cursor, limitOf(args), false, key)
	sys := auth.ContextWithSystemActor(ctx, provisionActor)
	rows := make([]map[string]any, 0, len(page))
	for _, a := range page {
		ent, err := p.entitlementRow(sys, a.id)
		if err != nil {
			return nil, err
		}
		// WHY ACCESS IS NOT ON is the entitlement's own sentence, the one the
		// application's page shows (wholesaleGet's entitlement.error): accessReason.
		access, code, reason, grantedAt, customer := AccessNone, "", "", "", ""
		if ent != nil {
			access, code, customer = trimmed(ent["state"]), trimmed(ent["errorCode"]), trimmed(ent["shopifyCustomerId"])
			reason = accessReason(ent)
			if access == EntitlementGranted {
				grantedAt = timeText(happenedAt(ent, fieldGrantedAt))
			}
		}
		rows = append(rows, map[string]any{
			"applicationId":     a.id,
			"companyName":       asString(a.row["companyName"]),
			"applicantEmail":    asString(a.row["applicantEmail"]),
			"accessState":       access,
			"errorCode":         code,
			"error":             reason,
			"grantedAt":         grantedAt,
			"shopifyCustomerId": customer,
		})
	}
	return confirm("wholesale:buyers", map[string]any{"rows": rows, "next": next, "truncated": truncated,
		"operator": isClusterOperator(ctx)})
}

// admitStore reads a store-wide read's store and site and checks the site is
// the store's own (siteForStore), before anything keyed by the store is read.
func (p *Provider) admitStore(ctx context.Context, args map[string]any) (storeID, siteID string, err error) {
	storeID, siteID = trimmed(args["storeId"]), trimmed(args["siteId"])
	if storeID == "" {
		return "", "", fmt.Errorf("wholesale: storeId is required")
	}
	if _, err := p.siteForStore(ctx, siteID, storeID); err != nil {
		return "", "", err
	}
	return storeID, siteID, nil
}

// details is a client's own fields on one application, read under ctx.
//
// none for an application the store's people added (no shopper filled in a
// client's fields), a store whose form no client extends, or an application
// with no extension row; unreadable when the caller reads no row there and the
// deployment does; shown for a row the caller reads, even one with no field to
// show.
func (p *Provider) details(ctx context.Context, app map[string]any) (map[string]any, error) {
	fields := []map[string]any{}
	if sourceOf(app) == SourceStaff || p.extensions == nil || !p.extensionDeclared() {
		return map[string]any{"state": "none", "fields": fields}, nil
	}
	id := bareID(trimmed(app["id"]))
	row, ok, err := p.extensions.ShopperExtensionRow(ctx, Domain, extensionForm, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		_, exists, err := p.extensions.ShopperExtensionRow(auth.ContextWithSystemActor(ctx, provisionActor), Domain, extensionForm, id)
		if err != nil {
			return nil, err
		}
		if exists {
			return map[string]any{"state": "unreadable", "fields": fields}, nil
		}
		// No row at all: an application with no extension row has no further
		// details.
		return map[string]any{"state": "none", "fields": fields}, nil
	}
	for _, f := range row.Fields {
		fields = append(fields, map[string]any{"name": f.Name, "label": f.Label, "value": f.Value})
	}
	return map[string]any{"state": "shown", "fields": fields}, nil
}

// displayFacts is the two facts a client's own fields show first for one
// application, read under ctx; empty when there are none to read.
func (p *Provider) displayFacts(ctx context.Context, app map[string]any) (string, string, error) {
	if sourceOf(app) == SourceStaff || p.extensions == nil || !p.extensionDeclared() {
		return "", "", nil
	}
	row, ok, err := p.extensions.ShopperExtensionRow(ctx, Domain, extensionForm, bareID(trimmed(app["id"])))
	if err != nil || !ok {
		return "", "", err
	}
	return row.Primary, row.Secondary, nil
}

// extensionDeclared reports whether a client extends the pack's form.
func (p *Provider) extensionDeclared() bool {
	if p.hasExtension != nil {
		return p.hasExtension()
	}
	return memql.ShopperExtensionFor(Domain, extensionForm) != nil
}

// sourceOf is how an application arrived; one naming none came through the
// storefront.
func sourceOf(app map[string]any) string {
	if trimmed(app["source"]) == SourceStaff {
		return SourceStaff
	}
	return SourceStorefront
}

// peopleNames answers people's names for one call, each read once.
type peopleNames struct {
	p     *Provider
	ctx   context.Context
	names map[string]string
}

// people is the names reader of one call. Its caller has been admitted.
func (p *Provider) people(ctx context.Context) *peopleNames {
	return &peopleNames{p: p, ctx: auth.ContextWithSystemActor(ctx, provisionActor), names: map[string]string{}}
}

// name is what a person is called, or empty when they cannot be found -- never
// their id.
func (n *peopleNames) name(userID string) string {
	userID = strings.TrimSpace(userID)
	// Only a person's id names a person: bare, or in the user concept's form.
	if userID == "" || (strings.Contains(userID, ":") && !strings.HasPrefix(userID, userIDPrefix)) {
		return ""
	}
	bare := bareID(userID)
	if name, seen := n.names[bare]; seen {
		return name
	}
	name := ""
	rows, err := n.p.rowsFor(auth.ContextWithInternalOrigin(n.ctx), "userByIdSystem", map[string]string{"userId": bare})
	if err == nil && len(rows) > 0 {
		name = strings.TrimSpace(asString(rows[0]["displayName"]))
		if name == "" {
			name = strings.TrimSpace(strings.TrimSpace(asString(rows[0]["firstName"])) + " " + strings.TrimSpace(asString(rows[0]["lastName"])))
		}
	}
	n.names[bare] = name
	return name
}

func knownStage(stage string) bool {
	for _, s := range Stages {
		if s == stage {
			return true
		}
	}
	return false
}

func searchOf(args map[string]any) string { return strings.ToLower(trimmed(args["search"])) }

// searchMatches reports whether search is a part of any one of values.
func searchMatches(search string, values ...any) bool {
	if search == "" {
		return true
	}
	for _, v := range values {
		if strings.Contains(strings.ToLower(asString(v)), search) {
			return true
		}
	}
	return false
}

// limitOf is a page size: defaultReadLimit when absent or not positive, and
// maxReadLimit for anything larger, however large.
func limitOf(args map[string]any) int {
	var n float64
	switch v := args["limit"].(type) {
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	case float64:
		n = v
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return defaultReadLimit
		}
		n = f
	default:
		return defaultReadLimit
	}
	switch {
	case math.IsNaN(n) || n < 1:
		return defaultReadLimit
	case n > maxReadLimit:
		return maxReadLimit
	}
	return int(n)
}

// timeText is a row time as RFC 3339 text, or empty.
func timeText(v any) string {
	at := rowTime(v)
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339Nano)
}

// listCursor is a keyset position in a list, bound to the list it was issued
// for. A deliberate copy of packs/reviewspack/directory_cursor.go's cursor (a
// pack may not import another pack), without its offset form.
type listCursor struct {
	Version int       `json:"v"`
	Key     string    `json:"key"`
	At      time.Time `json:"at"`
	ID      string    `json:"id"`
}

// readKey binds a cursor to the list and the filters it was issued for.
func readKey(kind, store, site, stage, search string) string {
	raw, _ := json.Marshal([]string{kind, store, site, stage, search})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func readCursor(after, key string) (*listCursor, error) {
	if after == "" {
		return nil, nil
	}
	fail := fmt.Errorf("wholesale: this page cursor does not match the list")
	if len(after) > 2048 {
		return nil, fail
	}
	raw, err := base64.RawURLEncoding.DecodeString(after)
	var c listCursor
	if err != nil || json.Unmarshal(raw, &c) != nil || c.Version != 1 || c.Key != key || c.ID == "" {
		return nil, fail
	}
	return &c, nil
}

// pageOf sorts entries, starts after cursor and takes limit of them, answering
// the cursor of the next page ("" when this one is the last).
func pageOf(entries []staged, cursor *listCursor, limit int, oldestFirst bool, key string) ([]staged, string) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].at.Equal(entries[j].at) {
			return entries[i].id < entries[j].id
		}
		if oldestFirst {
			return entries[i].at.Before(entries[j].at)
		}
		return entries[i].at.After(entries[j].at)
	})
	if cursor != nil {
		rest := entries[:0:0]
		for _, e := range entries {
			if e.at.Equal(cursor.At) && e.id > cursor.ID ||
				(oldestFirst && e.at.After(cursor.At)) || (!oldestFirst && e.at.Before(cursor.At)) {
				rest = append(rest, e)
			}
		}
		entries = rest
	}
	if len(entries) <= limit {
		return entries, ""
	}
	last := entries[limit-1]
	raw, _ := json.Marshal(listCursor{Version: 1, Key: key, At: last.at, ID: last.id})
	return entries[:limit], base64.RawURLEncoding.EncodeToString(raw)
}
