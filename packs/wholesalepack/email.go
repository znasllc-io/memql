package wholesalepack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/campaigns"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// email.go -- THE APPLICANT'S FOUR EMAILS (U9): the terms link, the approval
// with the price-sheet link, the rejection and the removal.
//
// # The lane takes a row, never an address
//
// Every email goes through the engine's transactional row lane
// (campaignSendRowMessage), named by the application and a kind. The pack
// registers, in Go, the resolver the lane asks for the applicant's address and
// the message's fields (resolveRowMessage), and the lane asks it only when it
// is about to hand the message to the provider: after its receipt, the
// template and the rate limit have had their say. So the resolver is the ONLY
// place a link is minted, and a send that is repeated, held or deferred mints
// nothing. A deferred send that is replayed asks the resolver again and is
// given a fresh link. No token is ever stored, logged or answered: the link's
// row keeps its hash, and the emailSend row keeps neither.
//
// # Once: record, hand over, settle
//
// The send builtins never mint and never send twice. Every send, automatic or
// a person's, is ONE ATTEMPT in three steps (sendEmail):
//
//  1. RECORD. The attempt's row is written as sending, fenced on NO ROW AT ITS
//     ID. That write is what admits the hand-over: of two callers for one
//     attempt, the one whose write loses the fence hands over nothing and
//     answers the row the other wrote.
//  2. HAND OVER. The winner calls the row lane as the store's merchant -- the
//     person who owns the store's site now (asStoreMerchant); the lane asks the
//     pack's resolver for the address and mints the link.
//  3. SETTLE. What the lane answered is written over the row, fenced on the
//     version recorded in step 1.
//
// THE AUTOMATIC PATH MAKES ITS ATTEMPT AT MOST ONCE, AND READS ITS ATTEMPTS
// FIRST. Before anything else it reads the application's rows of the kind,
// fresh: if ANY exists -- attempt 1 in any state, sent or not, or a later
// attempt a person sent -- it hands over nothing and answers that row (attempt
// 1's when there is one), whatever the store and the application are now. So a
// re-delivered event, a second granted version, a version the backfill re-wrote
// and the engine running a failed automation again all send nothing, and
// nothing automatic follows a person's resend.
//
// # An email that was due and was not sent is recorded
//
// An email is DUE when the automatic call for its kind arrives for an
// application in the state for it and the kind applies to the store (the terms
// email of a store that requires no terms is never due). An email that is not
// due is declined and nothing is recorded. An email that is due and that THE
// STORE'S OWN CHECKS decline -- no template chosen, no sender chosen, nobody to
// send as, or that person without the permission a send requires
// (emailBlocker) -- is recorded all the same: attempt 1, skipped, with the
// check's own sentence, through step 1's fenced write and with the lane not
// asked. It is then an attempt like any other: the application's page lists
// it, the automatic path answers it and never sends after it, and a person
// sends it again once the store is ready. Without the record such an email
// could never be sent and nothing would say so: the automatic call comes once.
//
// # What happened is a row, in a sentence written for a person
//
// What the lane answered is recorded on the email's row: sent; skipped with the
// lane's sentence (the row withdrew it, or the address is one the email service
// no longer sends to -- the lane's sentence for it, never its code); held with
// the lane's sentence (configuration the store can fix, such as an unpublished
// template: nothing was attempted); uncertain (the hand-over had no answer, and
// the provider may have the message -- always with the one uncertain sentence,
// never the lane's words); failed; or deferred (the send-rate limit: the lane
// replays it). Every outcome is ANSWERED AS A REPLY -- sent true or false, with
// its reason -- and never as an error that would make the engine run the
// automation again; only a fault that kept anything from being recorded is an
// error. The automation's reply carries the pack's own fixed sentences for a
// hold and a failure: the lane's sentence, which can name the store's template
// or its sender, stays on the row for the store's people.
//
// NOTHING REACHES A PERSON AS A REASON THAT WAS NOT WRITTEN FOR A PERSON. A send
// the lane refuses or fails answers a code and a sentence (campaigns
// RowSendFailure), and the sentence is the email's reason: on the row, on the
// application's page, in the export and in a resend's reply. THE PACK DECIDES
// ON THE CODE, NEVER ON THE SENTENCE, and for one code it has a sentence of its
// own (the person a send runs as lacks the permission: NoSendAuthoritySentence).
// THE TEXT OF AN ERROR IS NEVER A REASON -- it names the function that failed,
// and a provider's own message in it can quote the applicant's address: what
// reaches the pack with no sentence is answered in the pack's own
// (ReasonNotSent). Every reason answered is a whole sentence (wholeSentence).
//
// # The lane's status is the truth, except its early word on an uncertain one
//
// The row records what the lane answered at the time; the lane's own record of
// the delivery may say more since (a deferral it replayed and sent, a send the
// provider refused). The application's page, the export, the acts and the
// resend read each attempt through the lane's status read (effectiveSend). An
// attempt recorded UNCERTAIN stays uncertain until the lane says something
// definite -- that it sent it, or a provider's report on it -- because the lane
// writes failed in its ledger before it decides a hand-over was uncertain. For
// the same reason the lane's failed is not final for an attempt recorded and
// NEVER SETTLED either: such an attempt is read through the lane first, at any
// age -- one the lane sent is sent, one it still holds is on its way, one its
// ledger says failed is uncertain -- and only one the lane has no record of
// waits for sendingWindow to pass before it turns uncertain. AN ATTEMPT THE
// PROVIDER REPORTS ON WAS HANDED OVER: delivered or not, it reads sent, with the
// report beside it. SENT MEANS HANDED TO THE EMAIL SERVICE: whether it arrived
// is the delivery's to say (its providerStatus, answered beside the state).
//
// # Sent again by a person
//
// A person of the store sends an email again as a new attempt
// (wholesaleResendEmail). ONE FUNCTION DECIDES IT (resendRefusal), for the
// resend and for the act the application's page offers, so a resend is offered
// exactly when it would not be refused: the store would send the email now, no
// attempt of the kind is on its way, and the application is in the state for
// it -- the terms email while it waits for its terms, the approval email while
// its access is granted and the automation's attempt is recorded, and a
// rejection or removal only while it is still rejected or removed and the last
// attempt did not reach the applicant (skipped, held, uncertain or failed, or
// sent and since reported not delivered). The answer to a resend over an
// uncertain attempt says the applicant may receive it twice.
//
// # One person sends for a store: whoever owns its site now
//
// The lane is asked -- which sender, and the send itself -- as the store's
// merchant: THE CURRENT OWNER OF THE SITE THAT SERVES THE STORE, resolved at the
// moment it is asked (asStoreMerchant). The sends, the resend, the acts and the
// settings all resolve that person through the one function, so what the
// settings say would be sent is what a send then does. Not the application's
// owner, who is fixed when it is submitted, and not the settings row's owner,
// who is fixed when the row is first written: the site that serves a store can
// change, and so can who owns it. When nobody can be resolved, or that person
// lacks the permission, the store's own check declines the email with a
// sentence that says who can put it right. The email's row, and the link the
// resolver mints, stay the application's merchant's, as every row of an
// application is.

// The applicant's four emails, matching the emailSend concept's kind enum.
const (
	EmailTerms    = "terms"
	EmailApproved = "approved"
	EmailRejected = "rejected"
	EmailRevoked  = "revoked"
)

// EmailKinds is every kind, in the order an applicant meets them.
var EmailKinds = []string{EmailTerms, EmailApproved, EmailRejected, EmailRevoked}

// What came of one send, matching the emailSend concept's state enum.
const (
	SendSent      = "sent"
	SendSkipped   = "skipped"
	SendHeld      = "held"
	SendUncertain = "uncertain"
	SendFailed    = "failed"
	SendDeferred  = "deferred"
	// sendSending is an attempt recorded and not yet settled: the lane is being
	// asked. It is read through the lane first (effectiveSend); with nothing of
	// it in the lane's record it is on its way while it is young, and uncertain
	// once sendingWindow has passed (recordedState).
	sendSending = "sending"
	// sendPendingRecorded is the first build's word for a deferral or an
	// uncertain answer, which the concept still accepts for rows it wrote. It is
	// never written now, and read as deferred (recordedState).
	sendPendingRecorded = "pending"
)

// sendingWindow is how long an attempt may stay recorded and unsettled, WITH
// NOTHING OF IT IN THE LANE'S OWN RECORD, before it is read as uncertain.
//
// IT SITS ABOVE THE ONE BOUND A HAND-OVER HAS. The lane gives one message's
// hand-over to the provider its send timeout (campaigns Config.SendTimeout,
// thirty seconds unless the installation sets another:
// campaigns.DefaultSendTimeout), after which the lane has answered and the
// attempt is settled. Fifteen minutes is thirty times that default: room for a
// configured timeout several times longer, a slow database and a busy node. A
// test holds the window at ten times the default or more
// (TestTheSendingWindowIsAtLeastTenTimesTheLanesSendTimeout).
const sendingWindow = 15 * time.Minute

// recordedState is a row's state, and its reason, as this build reads it from
// the row alone. effectiveSend reads the lane's record over it.
func (p *Provider) recordedState(row map[string]any) (string, string) {
	state, reason := trimmed(row["state"]), asString(row["reason"])
	switch state {
	case sendPendingRecorded:
		return SendDeferred, reason
	case sendSending:
		if at := rowTime(row["createdAt"]); !at.IsZero() && p.clock().Sub(at) > sendingWindow {
			return SendUncertain, ReasonUncertain
		}
		return SendDeferred, reasonSending
	case SendUncertain:
		return SendUncertain, ReasonUncertain
	}
	return state, reason
}

// The row lane's readings of its own errors. Seams, so the pack's tests can
// stand in for the lane's unexported hold, uncertain and failure types.
var (
	laneHeldReason = campaigns.HeldReason
	laneUncertain  = campaigns.UncertainSend
	laneFailure    = campaigns.RowSendFailure
)

// The sentences a send answers for an email that is NOT DUE: the kind does not
// apply to the store, or the application is not in the state for it. Nothing is
// recorded for these, so the same call made once the application is there
// sends. (An email that is due and that the store's own checks decline is
// recorded: emailBlocker.) Each is a whole sentence, as every reason the pack
// answers is.
const (
	ReasonTermsNotRequired = "Terms are not required on this store."
	ReasonNotWaiting       = "The application is not waiting for terms."
	ReasonAccessNotGranted = "Access is not granted yet."
	// ReasonAccessPredatesEmail declines the approval email and the access
	// notices for an access record written before the pack sent either.
	ReasonAccessPredatesEmail = "This access record predates the pack's emails and notices, so nothing is sent for it."
)

// The sentences of an attempt that was recorded, the resolver's withdrawals,
// and what this file calls the lane's constructs and the email's row.
const (
	// An attempt that could not be handed to the lane is recorded failed with one
	// of these.
	ReasonNoOrganization = "The application belongs to no organization, so no email can be sent for it."
	reasonNoLane         = "The pack has no row lane, so no email can be sent."
	// reasonSending is an attempt recorded while the lane is being asked.
	reasonSending = "The email is being handed to the email service."
	// reasonFailedForAutomation is a failed attempt as the automation that asked
	// for it is answered; the lane's sentence stays on the row.
	reasonFailedForAutomation = "The email was not sent; the application's page says why."
	// reasonLaneFailed is a delivery the lane recorded as failed after the row
	// said otherwise; the lane's status read does not carry its words.
	reasonLaneFailed = "The email service recorded this email as failed."
	// reasonLaneWaiting is a delivery the lane holds to send later, when the
	// row carries no sentence of its own for it.
	reasonLaneWaiting = "The email service is waiting to send this email."
	// The resolver's withdrawals: the lane ledgers each as the skip's reason and
	// answers it, word for word, as the sentence a person reads.
	withdrawnTerms          = "The application is no longer waiting for terms."
	withdrawnAccess         = "Access is not granted."
	withdrawnGone           = "The application no longer exists."
	emailSendConcept        = "v1:wholesale:emailSend"
	emailActor              = "wholesale-email"
	maxEmailReason          = 2000
	maxLaneRequestIDLength  = 160
	laneSendConstruct       = "campaignSendRowMessage"
	laneStatusConstruct     = "campaignRowMessageStatus"
	laneSenderConstruct     = "campaignRowSender"
	maxLaneStatusRequestIDs = 100
)

// noTemplateReason is the sentence of a kind with no template chosen.
func noTemplateReason(kind string) string { return "No " + kind + " email template is chosen." }

// notInStateReason is the sentence of an email for an application that is not
// in the state it is sent in.
func notInStateReason(state string) string { return "The application is not " + state + "." }

// wholeSentence is text as a sentence a person reads: a capital first, a full
// stop last. Every sentence the pack writes is one already; this is for words
// that are not the pack's -- the lane's sentence for a hold, a skip or a
// failure, a route's own sentence on an access record, a row an earlier build
// recorded -- so that every reason answered is a whole sentence whatever form
// it arrived in. IT MAKES NO SENTENCE OF AN ERROR'S TEXT: that is never
// answered as a reason at all (failedReason, accessReason). A first character
// that is not a small letter (the quotation mark before a template's name) is
// left as it is, and so is a sentence that already ends.
func wholeSentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if first, size := utf8.DecodeRuneInString(text); unicode.IsLower(first) {
		text = string(unicode.ToUpper(first)) + text[size:]
	}
	switch last, _ := utf8.DecodeLastRuneInString(text); last {
	case '.', '!', '?':
		return text
	}
	return text + "."
}

// EmailSendRowID derives the one emailSend row of an application, kind and
// attempt, from the application's bare id whichever spelling the caller named.
func EmailSendRowID(kind, applicationID string, attempt int) string {
	return "wholesale-email-" + slugOf(kind) + "-" + slugOf(bareID(strings.TrimSpace(applicationID))) + "-" + strconv.Itoa(attempt)
}

// LaneRequestID is the request id the row lane receives one email under: the
// lane sends once per request id. It must match the lane's
// ^[A-Za-z0-9_-]{8,160}$, so an application id too long to fit is named by the
// first sixteen hex characters of its SHA-256 instead.
func LaneRequestID(kind, applicationID string, attempt int) string {
	bare := slugOf(bareID(strings.TrimSpace(applicationID)))
	id := "wholesale-" + slugOf(kind) + "-" + bare + "-" + strconv.Itoa(attempt)
	if len(id) > maxLaneRequestIDLength {
		sum := sha256.Sum256([]byte(bare))
		id = "wholesale-" + slugOf(kind) + "-" + hex.EncodeToString(sum[:])[:16] + "-" + strconv.Itoa(attempt)
	}
	return id
}

// rowLane is the transactional row lane as the pack reaches it: the send, as
// the application's merchant, and the status read of the emails it sent.
type rowLane interface {
	SendRowMessage(ctx context.Context, args map[string]any) (map[string]any, error)
	RowMessageStatus(ctx context.Context, accountID string, requestIDs []string) ([]map[string]any, error)
	// RowSender is the sender the lane's messages for the organization go out
	// as, resolved as a send resolves it: {ready, fromName, address, reason}.
	RowSender(ctx context.Context, accountID, senderIdentityID string) (map[string]any, error)
}

// engineLane is the production lane: the two campaigns builtins, each through
// the pack's engineCaller and its one inline stamp.
type engineLane struct{ caller *engineCaller }

func (l *engineLane) SendRowMessage(ctx context.Context, args map[string]any) (map[string]any, error) {
	return l.caller.Call(ctx, laneSendConstruct, args)
}

func (l *engineLane) RowMessageStatus(ctx context.Context, accountID string, requestIDs []string) ([]map[string]any, error) {
	return l.caller.CallRows(ctx, laneStatusConstruct, map[string]any{"accountId": accountID, "requestIds": requestIDs})
}

func (l *engineLane) RowSender(ctx context.Context, accountID, senderIdentityID string) (map[string]any, error) {
	rows, err := l.caller.CallRows(ctx, laneSenderConstruct, map[string]any{"accountId": accountID, "senderIdentityId": senderIdentityID})
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("wholesale: %s answered %d rows", laneSenderConstruct, len(rows))
	}
	return rows[0], nil
}

// blockEffect is what a blocker does to an email that is due.
type blockEffect int

const (
	// blockNothing: nothing blocks the email; the reason is empty.
	blockNothing blockEffect = iota
	// blockNotDue: the kind does not apply to the store -- the terms email of a
	// store that requires no terms. Nothing is sent and nothing is recorded.
	blockNotDue
	// blockDeclined: one of the store's own checks declines the email -- no
	// template chosen, no sender chosen, nobody to send as, or that person
	// without the permission a send requires. The lane's send is not asked, and A
	// DUE EMAIL IS RECORDED as a skipped attempt with the reason, so the
	// application's page shows it and a person sends it again once the store is
	// ready.
	blockDeclined
	// blockLaneDecides: the lane would hold or refuse the email, or could not say
	// just now. An automatic send goes to the lane, whose answer is the
	// attempt's record.
	blockLaneDecides
)

// emailBlocker is the one sentence why the store's email of kind would not be
// sent now, for an application in the right state, or "" when it would --
// THE CHECKS EVERY SEND MAKES OF THE STORE, in order, before the lane is asked:
//
//   - the terms email needs the store to require its terms;
//   - a template must be chosen for the kind;
//   - there must be somebody to send as: the store's merchant, the person who
//     owns its site now (asStoreMerchant, for app's own site when app is not
//     nil);
//   - the lane must resolve a sender for that person (campaignRowSender, as a
//     send resolves it), who must hold what a send requires.
//
// ONE FUNCTION, FOUR CALLERS, so they cannot disagree: the automatic sends, a
// person's resend, the resend acts the application's page offers, and the
// settings' emailsWouldSend. A resend is refused, and its act not offered,
// whenever the reason is not empty.
//
// The effect says what an AUTOMATIC send of a due email does about it:
//
//   - blockNotDue (terms not required): nothing is sent and nothing recorded;
//   - blockDeclined (no template chosen, no sender chosen, nobody to send as,
//     the store's merchant without write permission in its organization): the
//     lane's send is not asked and the attempt is RECORDED, skipped, with the
//     reason. It used to record nothing, "so the same call once the store is
//     ready sends" -- but that call never comes: a client's automation runs on a
//     row's first version, so an email declined that way was lost with nothing
//     to say so. The record is what a person's resend, and the act that offers
//     it, start from;
//   - blockLaneDecides: a sender that IS chosen and that the lane will not
//     send as (turned off, gone, another organization's), or one that could not
//     be read just now. The lane is asked and its answer recorded: held with
//     its sentence, or failed with its sentence.
//
// WHICH OF THESE A SENDER'S ANSWER IS, IS READ FROM THE LANE'S FIELDS AND ITS
// CODE, never from its sentence (readSender): "no sender is chosen" is said
// only of a store with none chosen, by the lane's own word that none is, and
// the permission is said to be missing only by the lane's code for it.
//
// What it cannot know without sending: whether the chosen template is still a
// published transactional template. The pack reads the store's templates only
// under the caller (the Email app's own read), so a template it cannot see may
// be unpublished or merely hidden from the caller; the lane's hold is what
// finds an unpublished one.
func (p *Provider) emailBlocker(ctx context.Context, s Settings, app map[string]any, kind string) (string, blockEffect) {
	return blockerGiven(s, kind, func() senderLine { return p.readSender(ctx, s, trimmed(app["storeId"]), app) })
}

// blockerGiven is emailBlocker over the store's sender line, read only when the
// checks before it pass. emailBlocker reads the line from the lane; the
// settings, which answer the line itself beside all four kinds, read it once
// and ask of that one reading.
func blockerGiven(s Settings, kind string, senderOf func() senderLine) (string, blockEffect) {
	if kind == EmailTerms && !s.TermsRequired {
		return ReasonTermsNotRequired, blockNotDue
	}
	if s.TemplateFor(kind) == "" {
		return noTemplateReason(kind), blockDeclined
	}
	sender := senderOf()
	switch {
	case sender.ready:
		return "", blockNothing
	case sender.nobody, sender.noAuthority:
		// NOBODY TO SEND AS, or that person without the permission: the line's own
		// sentence says which, and who can put it right.
		return sender.reason, blockDeclined
	case sender.readFailed:
		return SenderUnreadableSentence, blockLaneDecides
	case sender.noneChosen:
		return NoSenderChosenReason, blockDeclined
	}
	return sender.reason, blockLaneDecides
}

// emailsWouldSend is, for each of the four kinds, whether an email of it would
// be sent now for an application in the right state, and the one sentence why
// not: emailBlocker's checks, over the sender line the settings read.
func emailsWouldSend(s Settings, sender senderLine) map[string]any {
	out := map[string]any{}
	for _, kind := range EmailKinds {
		reason, _ := blockerGiven(s, kind, func() senderLine { return sender })
		out[kind] = map[string]any{"wouldSend": reason == "", "reason": reason}
	}
	return out
}

// asStoreMerchant is ctx acting as THE PERSON A SEND OF THE STORE RUNS AS: the
// store's merchant, which is THE CURRENT OWNER OF THE SITE THAT SERVES THE
// STORE, read now.
//
//   - For an application: the site it came through (its siteId), while that
//     site still serves the store.
//   - Otherwise, and for the store's settings (app nil): the one site that
//     serves the store. Where several do, the owner they share; or else the
//     owner of the one the store's settings name (the site its program belongs
//     to).
//
// NOT THE APPLICATION'S OWNER AND NOT THE SETTINGS ROW'S OWNER. Each names the
// owner of the site the row was written through, as they stood then -- when the
// application was submitted, when the settings row was first written -- and
// nothing in the pack moves either. The site that serves a store can change,
// and so can who owns it; the person asked is whoever owns the serving site at
// this moment.
//
// ONE FUNCTION FOR EVERY ASKER, asked at the moment of the send and at the
// moment the settings answer. The lane answers which sender a store's emails go
// out as only to a person holding what a send requires, and makes the send as
// the person who asks. The automatic sends, a person's resend, the acts and the
// settings all resolve that person here, so what the settings say would be sent
// is what a send then does.
//
// WHEN NOBODY CAN BE RESOLVED -- no site serves the store; several serve it
// under different owners and nothing names one of them; the site names no owner
// -- nobody is the pack's sentence for it, which says who can put it right, and
// ctx is answered as it came: nothing is asked of the lane as anybody else. An
// error is a read of the store's sites that failed: nothing is known, and
// nothing is decided on it.
func (p *Provider) asStoreMerchant(ctx context.Context, s Settings, storeID string, app map[string]any) (merchant context.Context, nobody string, err error) {
	// A FRESH READ, as the deployment: who owns the site is asked of the rows as
	// they stand at this moment, not as a node last heard of them.
	sys := memql.ContextWithFreshRead(auth.ContextWithSystemActor(ctx, storeLookupActor))
	as := func(site map[string]any) (context.Context, string, error) {
		owner := userRef(trimmed(site["ownerUserId"]))
		if owner == "" {
			return ctx, SiteNamesNoOwnerSentence, nil
		}
		return auth.ContextWithUserActor(ctx, owner), "", nil
	}
	// 1. THE SITE THE APPLICATION CAME THROUGH, while it still serves the store.
	if own := bareID(trimmed(app["siteId"])); own != "" {
		rows, err := p.rowsFor(sys, "siteById", map[string]string{"siteId": own})
		if err != nil {
			return ctx, "", err
		}
		if len(rows) > 0 && servesStore(rows[0], bareID(strings.TrimSpace(storeID))) {
			return as(rows[0])
		}
	}
	// 2. THE ONE SITE THAT SERVES THE STORE.
	sites, err := p.sitesForStore(sys, storeID)
	if err != nil {
		return ctx, "", err
	}
	switch len(sites) {
	case 0:
		return ctx, NoSiteServesSentence, nil
	case 1:
		return as(sites[0])
	}
	// SEVERAL: the owner they share, or else the owner of the one the store's
	// settings name.
	shared := true
	for _, site := range sites[1:] {
		shared = shared && userRef(trimmed(site["ownerUserId"])) == userRef(trimmed(sites[0]["ownerUserId"]))
	}
	if shared {
		return as(sites[0])
	}
	if named := bareID(s.SiteID); named != "" {
		for _, site := range sites {
			if bareID(trimmed(site["id"])) == named {
				return as(site)
			}
		}
	}
	return ctx, SeveralSitesSentence, nil
}

// senderLine is the store's sender as the lane resolves it for the store's
// merchant: the name and address its emails go out as, or why there is none,
// in a sentence.
type senderLine struct {
	ready                     bool
	fromName, address, reason string
	// What a decision is made on, read from the lane's own fields and code and
	// never from its sentence. readFailed: the store's sites or the lane could
	// not be asked, or the lane could not read the sender just now -- nothing is
	// known of it. noneChosen: the lane resolves none because the organization
	// has none chosen. nobody: there is nobody to send as (asStoreMerchant), so
	// the lane was not asked. noAuthority: the store's merchant does not hold
	// what a send requires.
	readFailed, noneChosen, nobody, noAuthority bool
}

// view is the line as wholesaleStoreSettings answers it. The address is the
// store's own sender's, never an applicant's.
func (l senderLine) view() map[string]any {
	return map[string]any{"ready": l.ready, "fromName": l.fromName, "address": l.address, "reason": l.reason}
}

// readSender reads the store's sender line, as a send resolves it (the chosen
// identity, or what a send with none chosen goes out as).
//
// AS THE STORE'S MERCHANT (asStoreMerchant, for app's own site when app is not
// nil): the lane answers a caller holding what a send requires of it, and the
// pack sends as that person. With nobody to send as the line says so and the
// lane is not asked. A merchant WITHOUT WRITE PERMISSION in the store's
// organization is told by the lane's code, and the line says it in the pack's
// own sentence, which names what is missing and who can give it -- never "no
// sender is chosen", and never the lane's words for it. The installation's own
// organization with no sender chosen sends as the installation's default
// mailbox, which has no name or address the lane can show: it says so.
func (p *Provider) readSender(ctx context.Context, s Settings, storeID string, app map[string]any) senderLine {
	unread := senderLine{reason: SenderUnreadableSentence, readFailed: true}
	merchant, nobody, err := p.asStoreMerchant(ctx, s, storeID, app)
	switch {
	case err != nil:
		return unread
	case nobody != "":
		return senderLine{reason: nobody, nobody: true}
	}
	account := bareID(s.AccountID)
	if account == "" || p.lane == nil {
		return unread
	}
	got, err := p.lane.RowSender(merchant, account, s.SenderIdentityID)
	if err != nil {
		return unread
	}
	if ready, _ := got["ready"].(bool); ready {
		line := senderLine{ready: true, fromName: asString(got["fromName"]), address: trimmed(got["address"])}
		if line.address == "" {
			line.fromName, line.reason = "", InstallationDefaultSenderSentence
		}
		return line
	}
	// THE LANE'S FIELDS AND ITS CODE DECIDE. One that could not be read just now is
	// said in the pack's own sentence, whatever words the lane put beside the
	// field; so is a merchant without the permission.
	if failed, _ := got["readFailed"].(bool); failed {
		return unread
	}
	if trimmed(got["code"]) == campaigns.RowSendNoAuthority {
		return senderLine{reason: NoSendAuthoritySentence, noAuthority: true}
	}
	line := senderLine{reason: wholeSentence(asString(got["reason"]))}
	line.noneChosen, _ = got["noneChosen"].(bool)
	if line.reason == "" {
		// NOT READY, AND NO WORD WHY: nothing is known of the sender.
		return unread
	}
	return line
}

// sendTermsEmail answers wholesaleSendTermsEmail.
func (p *Provider) sendTermsEmail(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireInternal(ctx, "wholesaleSendTermsEmail"); err != nil {
		return nil, err
	}
	sys, app, settings, err := p.emailSubject(ctx, trimmed(args["applicationId"]))
	if err != nil {
		return nil, err
	}
	return p.automaticEmail(ctx, sys, app, settings, EmailTerms)
}

// sendBuyerEmail answers wholesaleSendBuyerEmail.
func (p *Provider) sendBuyerEmail(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireInternal(ctx, "wholesaleSendBuyerEmail"); err != nil {
		return nil, err
	}
	kind := trimmed(args["kind"])
	if kind != EmailApproved && kind != EmailRejected && kind != EmailRevoked {
		return nil, fmt.Errorf("wholesale: %q is not a buyer's email; it is approved, rejected or revoked", kind)
	}
	sys, app, settings, err := p.emailSubject(ctx, trimmed(args["applicationId"]))
	if err != nil {
		return nil, err
	}
	return p.automaticEmail(ctx, sys, app, settings, kind)
}

// automaticEmail is the store's automation's send of kind: attempt 1, made at
// most once. In order:
//
//  1. THE ATTEMPTS ARE READ FIRST, fresh. Any attempt of the kind already
//     recorded is answered, whatever the store and the application are now, and
//     nothing else is read: not the application's state, not the lane's sender.
//  2. IS IT DUE (notDueReason). An email that is not due is declined and
//     nothing is recorded.
//  3. THE STORE'S OWN CHECKS (emailBlocker). One that declines a due email is
//     recorded as the attempt, skipped, with the check's sentence, and the lane
//     is not asked. Otherwise the attempt is made (sendEmail).
func (p *Provider) automaticEmail(ctx, sys context.Context, app map[string]any, settings Settings, kind string) ([]memorynodes.MemoryNode, error) {
	sends, err := p.emailSendsOf(memql.ContextWithFreshRead(sys), trimmed(app["id"]))
	if err != nil {
		return nil, err
	}
	if row := automaticAnswerRow(sends, kind); row != nil {
		return p.answerRecorded(ctx, app, row, false, "")
	}
	notDue, err := p.notDueReason(sys, app, settings, kind)
	if err != nil {
		return nil, err
	}
	if notDue != "" {
		return declined(app, kind, notDue)
	}
	attempt := emailAttempt{kind: kind, number: 1, templateID: settings.TemplateFor(kind), automatic: true}
	switch reason, effect := p.emailBlocker(ctx, settings, app, kind); effect {
	case blockNotDue:
		return declined(app, kind, reason)
	case blockDeclined:
		attempt.declined = reason
	}
	return p.sendEmail(ctx, sys, app, settings, attempt)
}

// notDueReason is why the email of kind is not due for app, or "" when it is:
// the kind does not apply to the store, or the application is not in the state
// the email is sent in. Read as sys.
func (p *Provider) notDueReason(sys context.Context, app map[string]any, settings Settings, kind string) (string, error) {
	applicationID := trimmed(app["id"])
	if kind == EmailTerms {
		if !settings.TermsRequired {
			return ReasonTermsNotRequired, nil
		}
		waiting, err := p.waitingForTerms(sys, app)
		if err != nil || waiting {
			return "", err
		}
		return ReasonNotWaiting, nil
	}
	state, err := p.stateOf(sys, applicationID)
	if err != nil {
		return "", err
	}
	switch kind {
	case EmailApproved:
		// THE APPROVAL EMAIL WAITS FOR THE GRANT (W3): it tells the buyer their
		// trade prices are on and carries the price-sheet link, which opens only
		// while they are.
		// THE REASON IS TRUE OF THE APPLICATION: "not granted yet" only of one that
		// is approved, and so may yet be granted.
		if state != StateApproved {
			return notInStateReason(StateApproved), nil
		}
		ent, err := p.entitlementRow(sys, applicationID)
		if err != nil {
			return "", err
		}
		if ent == nil || trimmed(ent["state"]) != EntitlementGranted {
			return ReasonAccessNotGranted, nil
		}
		// NOTHING IS MAILED FOR WHAT HAPPENED BEFORE THIS RELEASE, AND NOTHING
		// RECORDED. Every version the pack writes now carries its attempt, and the
		// backfill carries one forward only when the row holds it, so a granted row
		// with none is an access granted before the pack sent emails -- re-written,
		// perhaps, by the backfill, which a client's automation sees as any other
		// granted version.
		if attemptOf(ent) == 0 {
			return ReasonAccessPredatesEmail, nil
		}
	case EmailRejected:
		if state != StateRejected {
			return notInStateReason(StateRejected), nil
		}
	case EmailRevoked:
		if state != StateRevoked {
			return notInStateReason(StateRevoked), nil
		}
	}
	return "", nil
}

// resendEmail answers wholesaleResendEmail: a person of the store sends one of
// the four emails again, as a new attempt.
//
// THE ORGANIZATION GATE ADMITTED THE CALLER (the part, in the application's
// organization). Before anything is read the caller must be a signed-in person
// who is not one of the cluster's operators; then the application is read
// under the caller, so one they cannot read is not resent.
//
// WHETHER IT IS SENT IS resendRefusal's, the one function the application's
// page offers the act by: the store would send the email now, no attempt of
// the kind is on its way, and the application is in the state for it.
//
// EVERY REFUSAL IS A WHOLE SENTENCE FOR THE PERSON WHO ASKED, with no prefix of
// the pack's: a client shows it as it is.
func (p *Provider) resendEmail(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	requester, err := resenderOf(ctx)
	if err != nil {
		return nil, err
	}
	kind := trimmed(args["kind"])
	if _, known := emailNouns[kind]; !known {
		return nil, errors.New(ResendUnknownKindSentence)
	}
	applicationID := trimmed(args["applicationId"])
	if applicationID == "" {
		return nil, errors.New(ResendNoApplicationSentence)
	}
	rows, err := p.rowsFor(ctx, "applicationById", map[string]string{"applicationId": applicationID})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New(ResendUnreadableSentence)
	}
	app := rows[0]
	sys := auth.ContextWithSystemActor(ctx, emailActor)
	settingsRow, err := p.settingsRowAt(sys, trimmed(app["storeId"]))
	if err != nil {
		return nil, err
	}
	settings := EffectiveSettings(settingsRow)
	stage, access, err := p.stageAndAccess(sys, app, settings)
	if err != nil {
		return nil, err
	}
	sends, err := p.emailSendsOf(memql.ContextWithFreshRead(sys), trimmed(app["id"]))
	if err != nil {
		return nil, err
	}
	// EACH ATTEMPT AS THE LANE NOW SAYS IT STANDS, the reading the application's
	// page shows and offers its acts by; then the store's own readiness.
	standing := emailStandingOf(p.emailsView(ctx, app, sends), kind)
	standing.Blocker, _ = p.emailBlocker(ctx, settings, app, kind)
	if refusal := resendRefusal(kind, stage, access, standing); refusal != "" {
		return nil, errors.New(refusal)
	}
	// A RESEND OVER AN ATTEMPT THAT MAY HAVE BEEN DELIVERED says so in its answer.
	note := ""
	if standing.LastState == SendUncertain {
		note = NoteMaybeSecondCopy
	}
	return p.sendEmail(ctx, sys, app, settings, emailAttempt{kind: kind, number: highestAttempt(sends, kind) + 1,
		templateID: settings.TemplateFor(kind), requestedBy: userRef(requester), note: note})
}

// stageAndAccess is an application's stage and its access, as wholesaleGet
// answers them: the stage its whole log and its acceptance give it, and its
// entitlement's state (AccessNone when it has none). Read as sys.
func (p *Provider) stageAndAccess(sys context.Context, app map[string]any, settings Settings) (stage, access string, err error) {
	applicationID := trimmed(app["id"])
	state, err := p.stateOf(sys, applicationID)
	if err != nil {
		return "", "", err
	}
	accepted := false
	if settings.TermsRequired && state == StateSubmitted {
		acceptance, err := p.acceptanceOf(sys, applicationID)
		if err != nil {
			return "", "", err
		}
		accepted = acceptance != nil
	}
	ent, err := p.entitlementRow(sys, applicationID)
	if err != nil {
		return "", "", err
	}
	access = AccessNone
	if ent != nil {
		access = trimmed(ent["state"])
	}
	return StageOf(state, settings.TermsRequired, accepted), access, nil
}

// EmailStanding is where one kind of email stands on one application, as a
// resend of it is judged: its attempts as the lane says they stand, and the
// store's readiness to send it.
type EmailStanding struct {
	// FirstRecorded is whether attempt 1, the store's automation's, is recorded.
	FirstRecorded bool
	// LastState is the last attempt's state as the lane says it stands, "" when
	// none is recorded; LastBounced whether it was sent and the provider has
	// since reported it not delivered.
	LastState   string
	LastBounced bool
	// OnItsWay is whether an attempt of the kind is still on its way.
	OnItsWay bool
	// Unreadable is whether the lane's record of the kind's attempts could not
	// be read.
	Unreadable bool
	// Blocker is why the store would not send the kind now, "" when it would
	// (emailBlocker).
	Blocker string
}

// emailStandingOf reads kind's standing off an application's emails as
// wholesaleGet shows them (emailsView). Its Blocker is the caller's to fill.
func emailStandingOf(emails []map[string]any, kind string) EmailStanding {
	var s EmailStanding
	last := 0
	for _, e := range emails {
		if trimmed(e["kind"]) != kind {
			continue
		}
		n, _ := intArg(e["attempt"])
		state := trimmed(e["state"])
		if n == 1 {
			s.FirstRecorded = true
		}
		if state == SendDeferred {
			s.OnItsWay = true
		}
		if unread, _ := e["deliveryUnreadable"].(bool); unread {
			s.Unreadable = true
		}
		if n >= last {
			delivery, _ := e["delivery"].(map[string]any)
			last, s.LastState = n, state
			s.LastBounced = state == SendSent && bouncedReports[trimmed(delivery["providerStatus"])]
		}
	}
	return s
}

// notReached reports whether an attempt, as the lane says it stands, did not
// reach the applicant: it was skipped, held, uncertain or failed -- or it was
// sent, handed to the email service, and the provider has since reported it
// not delivered. One still on its way is neither.
func notReached(state string, bounced bool) bool {
	switch state {
	case SendSkipped, SendHeld, SendUncertain, SendFailed:
		return true
	}
	return state == SendSent && bounced
}

// resendRefusal is why a person's resend of kind is refused now, on an
// application in stage whose access is access, or "" when it is not refused.
//
// THE ONE QUESTION BEHIND THE RESEND AND ITS ACT: wholesaleResendEmail refuses
// with this sentence -- a whole one, as it stands -- and ActsFor offers the act
// exactly when it is empty. In order:
//
//   - the store would send the email now (the standing's Blocker, the checks a
//     send makes of the store: emailBlocker);
//   - for a rejection or a removal, the lane's record could be read: it is
//     what says whether the last attempt reached the applicant;
//   - no attempt of the kind is on its way;
//   - the application is in the state for it: the terms email while it waits
//     for its terms, whatever came of the attempts before; the approval email
//     while it is approved, its access granted and the automation's attempt
//     recorded; a rejection or a removal while it is still rejected or removed
//     and the last attempt did not reach the applicant (notReached) -- one that
//     reached them is never sent twice.
func resendRefusal(kind, stage, access string, s EmailStanding) string {
	noun := emailNouns[kind]
	if s.Blocker != "" {
		return s.Blocker
	}
	if (kind == EmailRejected || kind == EmailRevoked) && s.Unreadable {
		return "The email service's record of the last " + noun + " could not be read just now, so it is not sent again. Try again in a moment."
	}
	if s.OnItsWay {
		return "The last " + noun + " is waiting to be sent by the email service, so it is not sent again."
	}
	switch kind {
	case EmailTerms:
		if stage != StageWaitingForTerms {
			return "The terms email is sent again only while the application waits for its terms."
		}
	case EmailApproved:
		if stage != StateApproved || access != EntitlementGranted || !s.FirstRecorded {
			return "The approval email is sent again only to a buyer whose access is granted, " +
				"once the store's automation has sent it."
		}
	case EmailRejected, EmailRevoked:
		want := StateRejected
		if kind == EmailRevoked {
			want = StateRevoked
		}
		if stage != want || !notReached(s.LastState, s.LastBounced) {
			return "The " + noun + " is sent again only while the application is " + strings.ToLower(staffWords[want]) +
				" and its last attempt did not reach the applicant."
		}
	default:
		return ResendUnknownKindSentence
	}
	return ""
}

// The refusals of a resend that is not judged at all: each a whole sentence for
// the person who asked.
const (
	ResendUnknownKindSentence   = "That is not one of the applicant's emails. An email sent again is the terms, approved, rejected or revoked one."
	ResendNoApplicationSentence = "No application is named, so there is no email to send again."
	ResendUnreadableSentence    = "This application is not readable to you, so none of its emails is sent again."
	ResendNobodySentence        = "An email is sent again by a signed-in person."
	ResendOperatorSentence      = "The cluster's operators host the store's applications and send none of their emails. " +
		"A person on the store's side sends one again."
)

// resenderOf is the person a resend is recorded for: the calling user, never
// one of the cluster's operators, who send nothing on a store's behalf (the
// rule deciderOf holds a decision to).
func resenderOf(ctx context.Context) (string, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac.Synthetic || strings.TrimSpace(ac.UserId) == "" {
		return "", errors.New(ResendNobodySentence)
	}
	if auth.IsClusterOperator(ac.Role) {
		return "", errors.New(ResendOperatorSentence)
	}
	return strings.TrimSpace(ac.UserId), nil
}

// emailSubject reads, as the deployment, the application an automation's send
// names and its store's settings.
func (p *Provider) emailSubject(ctx context.Context, applicationID string) (context.Context, map[string]any, Settings, error) {
	if applicationID == "" {
		return nil, nil, Settings{}, fmt.Errorf("wholesale: applicationId is required")
	}
	sys := auth.ContextWithSystemActor(ctx, emailActor)
	rows, err := p.rowsFor(sys, "applicationById", map[string]string{"applicationId": applicationID})
	if err != nil {
		return nil, nil, Settings{}, err
	}
	if len(rows) == 0 {
		return nil, nil, Settings{}, fmt.Errorf("wholesale: application %q does not exist", applicationID)
	}
	settings, err := p.settingsRowAt(sys, trimmed(rows[0]["storeId"]))
	if err != nil {
		return nil, nil, Settings{}, err
	}
	return sys, rows[0], EffectiveSettings(settings), nil
}

// waitingForTerms reports whether app is waiting for its terms: submitted, its
// store requiring them, and no acceptance. Read as sys.
func (p *Provider) waitingForTerms(sys context.Context, app map[string]any) (bool, error) {
	state, err := p.stateOf(sys, trimmed(app["id"]))
	if err != nil || state != StateSubmitted {
		return false, err
	}
	required, accepted, err := p.acceptanceStanding(sys, app)
	if err != nil {
		return false, err
	}
	return required && !accepted, nil
}

// accessGranted reports whether app, in state, is approved with its access
// granted. Read as sys.
func (p *Provider) accessGranted(sys context.Context, app map[string]any, state string) (bool, error) {
	if state != StateApproved {
		return false, nil
	}
	ent, err := p.entitlementRow(sys, trimmed(app["id"]))
	if err != nil {
		return false, err
	}
	return ent != nil && trimmed(ent["state"]) == EntitlementGranted, nil
}

// emailAttempt is one attempt of one email, as sendEmail makes it.
type emailAttempt struct {
	kind   string
	number int
	// templateID is the store's template for the kind; requestedBy the person
	// who asked for a resend, empty for the store's automation.
	templateID, requestedBy string
	// automatic is the store's automation's attempt 1, which is made at most
	// once.
	automatic bool
	// declined, when not empty, is the sentence of the store's own check that
	// declines a due email: the attempt is recorded as skipped with it, and the
	// lane is not asked.
	declined string
	// note is carried by a person's resend answer.
	note string
}

// sendEmail is ONE ATTEMPT OF ONE EMAIL: record, hand over, settle (the file
// note). An attempt the store's own check declined is recorded and goes no
// further.
func (p *Provider) sendEmail(ctx, sys context.Context, app map[string]any, settings Settings, a emailAttempt) ([]memorynodes.MemoryNode, error) {
	applicationID := trimmed(app["id"])
	rowID := EmailSendRowID(a.kind, applicationID, a.number)
	requestID := LaneRequestID(a.kind, applicationID, a.number)
	fresh := memql.ContextWithFreshRead(sys)
	sends, err := p.emailSendsOf(fresh, applicationID)
	if err != nil {
		return nil, err
	}
	// THE AUTOMATIC PATH, AT MOST ONCE: any row of the kind -- this attempt in
	// any state, or a later one a person sent -- is answered, and nothing is
	// handed over.
	if a.automatic {
		if row := automaticAnswerRow(sends, a.kind); row != nil {
			return p.answerRecorded(ctx, app, row, false, "")
		}
	}
	for _, row := range sends {
		if bareID(trimmed(row["id"])) == rowID {
			return p.answerRecorded(ctx, app, row, !a.automatic, a.note)
		}
	}
	// WHO THE SEND RUNS AS, RESOLVED NOW: the person who owns the store's site at
	// this moment (asStoreMerchant). Before anything is recorded: a read of the
	// store's sites that fails is a fault, not an attempt, and with nobody to
	// send as the attempt is recorded as the store's own check declining it.
	var sender context.Context
	if a.declined == "" {
		merchantNow, nobody, err := p.asStoreMerchant(ctx, settings, trimmed(app["storeId"]), app)
		if err != nil {
			return nil, err
		}
		sender, a.declined = merchantNow, nobody
	}
	// THE ROW IS THE APPLICATION'S MERCHANT'S, written as them, like every row of
	// an application.
	merchant, err := p.asMerchant(ctx, app)
	if err != nil {
		return nil, err
	}
	row := map[string]any{
		"emailSendId":   rowID,
		"ownerUserId":   userRef(trimmed(app["ownerUserId"])),
		"storeId":       trimmed(app["storeId"]),
		"accountId":     trimmed(app["accountId"]),
		"applicationId": applicationRef(applicationID),
		"kind":          a.kind,
		"attempt":       a.number,
		"requestId":     requestID,
		"templateId":    a.templateID,
		"state":         sendSending,
		"reason":        reasonSending,
		"sentAt":        "",
		"requestedBy":   a.requestedBy,
	}
	if a.declined != "" {
		// THE STORE'S OWN CHECK DECLINED A DUE EMAIL: the attempt is recorded as
		// what it came to, and the lane is not asked.
		row["state"], row["reason"] = SendSkipped, clipRunes(a.declined, maxEmailReason)
	}
	written := map[string]any{"id": emailSendConcept + ":" + rowID}
	for k, v := range row {
		if k != "emailSendId" {
			written[k] = v
		}
	}
	// 1. RECORD, fenced on no row at this attempt's id: the write that wins is
	// the one caller that hands over.
	if err := p.write(memql.ContextWithRowVersionFence(merchant, emailSendConcept, rowID, time.Time{}), "putEmailSendRow", row); err != nil {
		if !errors.Is(err, memql.ErrRowVersionChanged) {
			return nil, err
		}
		current, err := p.emailSendRow(fresh, applicationID, rowID)
		if err != nil {
			return nil, err
		}
		if current == nil {
			return nil, fmt.Errorf("wholesale: the email's record changed while it was sent and cannot be read")
		}
		return p.answerRecorded(ctx, app, current, !a.automatic, a.note)
	}
	if a.declined != "" {
		return p.answerOutcome(written, !a.automatic, a.note)
	}
	recorded, recordErr := p.emailSendRow(memql.ContextWithFreshRead(sys), applicationID, rowID)

	// 2. HAND OVER, as the store's merchant: the person the store's sender was
	// read as (emailBlocker), so the lane answers the send as it answered that.
	state, reason, sentAt := SendFailed, ReasonNoOrganization, ""
	if account := bareID(trimmed(app["accountId"])); account != "" {
		switch {
		case p.lane == nil:
			state, reason = SendFailed, reasonNoLane
		default:
			laneArgs := map[string]any{
				"rowConcept": applicationConcept,
				"rowId":      bareID(applicationID),
				"accountId":  account,
				"kind":       a.kind,
				"templateId": a.templateID,
				"requestId":  requestID,
			}
			if settings.SenderIdentityID != "" {
				laneArgs["senderIdentityId"] = settings.SenderIdentityID
			}
			reply, sendErr := p.lane.SendRowMessage(sender, laneArgs)
			if memql.IsWriteGateBusy(sendErr) {
				// ANOTHER HOLDER OF THE LANE'S RECEIPT LOCK FOR THIS REQUEST is handing it
				// over (or the wait for the lock ran out): what became of it is not known
				// here, and the lane's status read will say.
				state, reason = SendUncertain, ReasonUncertain
			} else {
				state, reason = laneOutcome(reply, sendErr)
			}
			if state == SendSent {
				sentAt = p.clock().UTC().Format(time.RFC3339Nano)
			}
		}
	}

	// 3. SETTLE, fenced on the version recorded in step 1.
	row["state"], row["reason"], row["sentAt"] = state, clipRunes(reason, maxEmailReason), sentAt
	written["state"], written["reason"], written["sentAt"] = row["state"], row["reason"], row["sentAt"]
	settle := merchant
	if recordErr == nil && recorded != nil {
		settle = memql.ContextWithRowVersionFence(merchant, emailSendConcept, rowID, rowTime(recorded["createdAt"]))
	}
	if err := p.write(settle, "putEmailSendRow", row); err != nil {
		// THE ATTEMPT IS RECORDED (sending) and was handed over; only its outcome
		// could not be written. It is answered as recorded, never as an error that
		// would make the engine run the automation again; the lane's status read
		// says what became of it.
		p.log().Warn("wholesale: an email's outcome could not be recorded", "component", "wholesale",
			"applicationId", applicationID, "kind", a.kind, "attempt", a.number, "state", state)
		written["state"], written["reason"], written["sentAt"] = sendSending, reasonSending, ""
	}
	return p.answerOutcome(written, !a.automatic, a.note)
}

// automaticAnswerRow is the row the automatic path answers for kind instead of
// sending: attempt 1's when there is one, else the latest attempt, or nil when
// none is recorded.
func automaticAnswerRow(sends []map[string]any, kind string) map[string]any {
	for _, row := range sends {
		if n, _ := intArg(row["attempt"]); trimmed(row["kind"]) == kind && n == 1 {
			return row
		}
	}
	return latestSend(sends, kind)
}

// emailNouns is each kind as a sentence names it.
var emailNouns = map[string]string{
	EmailTerms:    "terms email",
	EmailApproved: "approval email",
	EmailRejected: "rejection email",
	EmailRevoked:  "removal email",
}

// answerRecorded answers an attempt already recorded, as the lane now says it
// stands (effectiveSend). A status that cannot be read answers the row as it
// was recorded.
func (p *Provider) answerRecorded(ctx context.Context, app, row map[string]any, forStaff bool, note string) ([]memorynodes.MemoryNode, error) {
	id := trimmed(row["requestId"])
	deliveries, err := p.laneDeliveries(ctx, app, []string{id})
	return p.answerOutcome(p.effectiveSend(row, deliveries[id], err == nil), forStaff, note)
}

// answerOutcome answers one attempt as a reply: sent true only when it went
// out, with its state and reason. A person of the store (forStaff, the resend)
// reads the row's own sentence; the automation that asked is given the pack's
// fixed sentence for a hold or a failure, so nothing of the store's template,
// sender or transport reaches an automation's error or a log through it.
func (p *Provider) answerOutcome(row map[string]any, forStaff bool, note string) ([]memorynodes.MemoryNode, error) {
	state, reason := p.recordedState(row)
	if !forStaff {
		switch state {
		case SendHeld:
			reason = ReasonHeldForAutomation
		case SendFailed:
			reason = reasonFailedForAutomation
		}
	}
	if state == SendDeferred && strings.TrimSpace(reason) == "" {
		reason = reasonLaneWaiting
	}
	answer := copyMap(row)
	answer["state"], answer["reason"] = state, wholeSentence(reason)
	return answerSend(answer, note)
}

// laneOutcome reads what the row lane answered as an emailSend state and its
// sentence: a hold is held, and a skip skipped, each with the lane's sentence;
// an uncertain hand-over is uncertain, with the one uncertain sentence whatever
// the lane said of it; a deferral deferred; any other error is failed, with a
// sentence written for a person (failedReason) and never the error's text.
func laneOutcome(reply map[string]any, err error) (string, string) {
	if err != nil {
		if reason, held := laneHeldReason(err); held {
			return SendHeld, wholeSentence(reason)
		}
		if laneUncertain(err) {
			return SendUncertain, ReasonUncertain
		}
		return SendFailed, failedReason(err)
	}
	switch {
	case reply["sent"] == true:
		return SendSent, ""
	case reply["skipped"] == true:
		return SendSkipped, skipSentenceOf(reply["skipSentence"], reply["reason"])
	case reply["uncertain"] == true:
		// THE LANE'S OWN WORDS FOR AN UNCERTAIN HAND-OVER ARE NOT THE REASON: every
		// uncertain attempt carries the one sentence, never an empty one.
		return SendUncertain, ReasonUncertain
	case reply["deferred"] == true:
		return SendDeferred, wholeSentence(trimmed(reply["reason"]))
	}
	// A REPLY THAT SAYS NONE OF THESE reached the pack with no sentence.
	return SendFailed, ReasonNotSent
}

// failedReason is why a send the lane refused, or that failed, was not sent, AS
// A PERSON READS IT. The lane answers a code and a sentence with every refusal
// and failure of a row send (campaigns.RowSendFailure):
//
//   - THE PACK DECIDES ON THE CODE, never on the sentence. For the one code it
//     has words of its own for -- the person the send ran as does not hold what
//     a send requires -- it answers them (NoSendAuthoritySentence), which name
//     what is missing and who can give it;
//   - any other code: the lane's sentence for it;
//   - anything that reaches the pack with no sentence -- an error that is not
//     the lane's answer, an answer with no words: the pack's own
//     (ReasonNotSent).
//
// THE ERROR'S OWN TEXT IS NEVER READ. It names the functions that failed, and
// what wraps the lane's answer on its way here may quote a provider's message,
// and the applicant's address in it.
func failedReason(err error) string {
	code, sentence, failed := laneFailure(err)
	switch {
	case failed && code == campaigns.RowSendNoAuthority:
		return NoSendAuthoritySentence
	case failed && strings.TrimSpace(sentence) != "":
		return wholeSentence(sentence)
	}
	return ReasonNotSent
}

// skipSentenceOf is why the lane skipped a send, in a sentence: the lane's own
// (its skipSentence), and where it gave none, its words for the reason it gave
// (campaigns.SkipSentence). NEVER THE REASON ITSELF, which for an address the
// email service no longer sends to is a code.
func skipSentenceOf(sentence, reason any) string {
	if said := wholeSentence(asString(sentence)); said != "" {
		return said
	}
	return campaigns.SkipSentence(trimmed(reason))
}

// effectiveSend is one attempt's row as the lane now says it stands. laneRead
// is whether the lane's record could be read at all; delivery is its record of
// the attempt's request id, nil when it has none. The row's state is first
// read as this build reads it from the row alone (recordedState). Then:
//
//   - an attempt recorded and NEVER SETTLED is read THROUGH THE LANE FIRST, at
//     any age: where the lane has a record of it, the lane's status decides (one
//     it still holds to send is on its way, and is not sent again; one it sent
//     is sent; one it skipped is skipped). ITS failed IS NOT FINAL: the lane
//     writes failed in its ledger before it decides a hand-over was uncertain,
//     and this attempt's own outcome was never written, so nothing says which
//     it was -- it reads UNCERTAIN, with ReasonUncertain, and a resend over it
//     says it may be a second copy. Only when the lane was read and has no
//     record of it does the row's age decide -- on its way while it is young,
//     uncertain once sendingWindow has passed. When the lane's record could not
//     be read, nothing says the lane has none, so it does not turn uncertain:
//     it stays on its way;
//   - an UNCERTAIN attempt stays uncertain, with ReasonUncertain, unless the
//     lane says something definite: that it sent it, or A PROVIDER'S REPORT ON
//     IT. An attempt the provider reports on was handed over, whatever the
//     report: delivered or not delivered, it reads sent, with the report beside
//     it, exactly as a sent attempt does. The lane's ledger failed is not final
//     for one, for the reason above;
//   - any other attempt takes the lane's status: sent with the lane's time,
//     failed, skipped with the lane's sentence, pending as deferred; the row's
//     own sentence is kept only when the row already said the same.
//
// THE STATE IS WHAT THE SEND REACHED. sent means HANDED TO THE EMAIL SERVICE:
// whether it arrived is the delivery's to say, and a provider's later report
// that a sent email was not delivered leaves it sent, with the report beside it
// (delivery.providerStatus), which is where a page words it from. Such an email
// did not reach the applicant, and a person may send it again (notReached).
//
// A row the lane has recorded nothing for (a hold, an attempt the store's own
// check declined, one the lane never reached) is answered as it stands. The
// reason answered is always a whole sentence.
func (p *Provider) effectiveSend(row, delivery map[string]any, laneRead bool) map[string]any {
	state, reason := p.recordedState(row)
	sentAt := timeText(row["sentAt"])
	status, provider := trimmed(delivery["status"]), trimmed(delivery["providerStatus"])
	known := status == "sent" || status == "failed" || status == "skipped" || status == "pending"
	if trimmed(row["state"]) == sendSending {
		switch {
		case status == "failed":
			// THE LANE'S RECORD SAYS FAILED, WHICH IT WRITES BEFORE IT DECIDES A
			// HAND-OVER WAS UNCERTAIN, and this attempt's own outcome was never
			// written: it may have been delivered.
			state, reason = SendUncertain, ReasonUncertain
		case known:
			// THE LANE HAS A RECORD OF IT, and decides: none of the row's own reading
			// is kept.
			state, reason = "", ""
		case !laneRead:
			old := state == SendUncertain
			state, reason = SendDeferred, reasonSending
			if old {
				reason = reasonLaneUnread
			}
		}
	}
	if state == SendUncertain {
		// THE LANE SENT IT, OR THE PROVIDER REPORTS ON IT: either way it was handed
		// over. A report of non-delivery leaves it sent with the report beside it
		// (the delivery's providerStatus), as it leaves a sent attempt.
		if status == "sent" || provider != "" {
			state, reason = SendSent, ""
			if at := timeText(delivery["sentAt"]); at != "" {
				sentAt = at
			}
		}
	} else {
		switch status {
		case "sent":
			state, reason = SendSent, ""
			if at := timeText(delivery["sentAt"]); at != "" {
				sentAt = at
			}
		case "failed":
			if state != SendFailed {
				reason = reasonLaneFailed
			}
			state, sentAt = SendFailed, ""
		case "skipped":
			state, reason, sentAt = SendSkipped, skipSentenceOf(delivery["skipSentence"], delivery["skipReason"]), ""
		case "pending":
			if state != SendDeferred {
				reason = reasonLaneWaiting
			}
			state, sentAt = SendDeferred, ""
		}
	}
	out := copyMap(row)
	out["state"], out["reason"], out["sentAt"] = state, wholeSentence(reason), sentAt
	return out
}

// bouncedReports are the provider reports that say a message was not
// delivered (the delivery concept's providerStatus).
var bouncedReports = map[string]bool{"bounced": true, "suppressed": true, "quarantined": true, "filtered-spam": true, "failed": true}

// copyMap is a shallow copy of m.
func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// latestSend is the highest attempt of kind recorded, or nil.
func latestSend(sends []map[string]any, kind string) map[string]any {
	var latest map[string]any
	high := 0
	for _, row := range sends {
		if trimmed(row["kind"]) != kind {
			continue
		}
		if n, _ := intArg(row["attempt"]); latest == nil || n > high {
			latest, high = row, n
		}
	}
	return latest
}

// laneDeliveries is the lane's record of each request id's delivery, for the
// application's organization, keyed by request id: only ids the lane has
// recorded a delivery for are present. The read is made a hundred ids at a time.
func (p *Provider) laneDeliveries(ctx context.Context, app map[string]any, requestIDs []string) (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	account := bareID(trimmed(app["accountId"]))
	var ids []string
	for _, id := range requestIDs {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 || account == "" {
		return out, nil
	}
	if p.lane == nil {
		return out, fmt.Errorf("wholesale: the pack has no row lane, so no delivery can be read")
	}
	for start := 0; start < len(ids); start += maxLaneStatusRequestIDs {
		end := min(start+maxLaneStatusRequestIDs, len(ids))
		status, err := p.lane.RowMessageStatus(ctx, account, ids[start:end])
		if err != nil {
			return out, err
		}
		for _, s := range status {
			if trimmed(s["status"]) != "" {
				out[trimmed(s["requestId"])] = map[string]any{
					"status":     trimmed(s["status"]),
					"skipReason": asString(s["skipReason"]),
					// WHY IT WAS SKIPPED, AS A PERSON READS IT: the lane's sentence, or
					// its words for the reason when the answer carries the reason alone.
					"skipSentence":   skipSentenceIfSkipped(s),
					"providerStatus": trimmed(s["providerStatus"]),
					"sentAt":         timeText(s["sentAt"]),
				}
			}
		}
	}
	return out, nil
}

// skipSentenceIfSkipped is the sentence of one status answer's skip, "" for a
// message that was not skipped.
func skipSentenceIfSkipped(status map[string]any) string {
	if trimmed(status["status"]) != "skipped" {
		return ""
	}
	return skipSentenceOf(status["skipSentence"], status["skipReason"])
}

// declined answers a send of an email that is NOT DUE, recording nothing: the
// kind does not apply to the store, or the application is not in the state for
// it. (A due email the store's own checks decline is recorded: sendEmail.)
func declined(app map[string]any, kind, reason string) ([]memorynodes.MemoryNode, error) {
	return confirm("wholesale:email", map[string]any{
		"applicationId": trimmed(app["id"]),
		"kind":          kind,
		"attempt":       1,
		"sent":          false,
		"state":         SendSkipped,
		"reason":        reason,
		"recorded":      false,
	})
}

// answerSend answers an emailSend row as the send's reply: sent is whether it
// went out; note, when there is one, is a resend's word that it may be the
// applicant's second copy.
func answerSend(row map[string]any, note string) ([]memorynodes.MemoryNode, error) {
	attempt, _ := intArg(row["attempt"])
	answer := map[string]any{
		"applicationId": trimmed(row["applicationId"]),
		"kind":          trimmed(row["kind"]),
		"attempt":       attempt,
		"requestId":     trimmed(row["requestId"]),
		"templateId":    trimmed(row["templateId"]),
		"state":         trimmed(row["state"]),
		"sent":          trimmed(row["state"]) == SendSent,
		"reason":        asString(row["reason"]),
		"sentAt":        timeText(row["sentAt"]),
		"requestedBy":   trimmed(row["requestedBy"]),
		"recorded":      true,
	}
	if note != "" {
		answer["note"] = note
	}
	return confirm("wholesale:email", answer)
}

// clipRunes cuts s to at most max characters.
func clipRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

// emailSendsOf is every emailSend row of an application, either spelling,
// newest first.
func (p *Provider) emailSendsOf(ctx context.Context, applicationID string) ([]map[string]any, error) {
	return p.rowsForApplication(ctx, "emailSendsForApplication", applicationID)
}

// emailSendRow is the application's emailSend row at rowID, or nil.
func (p *Provider) emailSendRow(ctx context.Context, applicationID, rowID string) (map[string]any, error) {
	rows, err := p.emailSendsOf(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if bareID(trimmed(row["id"])) == rowID {
			return row, nil
		}
	}
	return nil, nil
}

// highestAttempt is the highest attempt recorded of kind, 0 when none is.
func highestAttempt(sends []map[string]any, kind string) int {
	high := 0
	for _, row := range sends {
		if trimmed(row["kind"]) != kind {
			continue
		}
		if n, _ := intArg(row["attempt"]); n > high {
			high = n
		}
	}
	return high
}

// resolveRowMessage is the row lane's resolver for an application: the
// applicant's address, the application's organization and the message's
// fields for one kind, or a withdrawal when the application is no longer owed
// it. THE ONE PLACE A LINK IS MINTED: the lane asks only when it is about to
// hand the message over. It reads as the deployment, mints as the
// application's merchant, and answers the closed set of fields every kind
// shares, a link's two empty for a kind that carries none.
func (p *Provider) resolveRowMessage(ctx context.Context, req campaigns.RowMessageRequest) (campaigns.RowMessage, error) {
	if strings.TrimSpace(req.Concept) != applicationConcept {
		return campaigns.RowMessage{}, fmt.Errorf("wholesale: %q is not the application concept", req.Concept)
	}
	sys := auth.ContextWithSystemActor(ctx, emailActor)
	rows, err := p.rowsFor(sys, "applicationById", map[string]string{"applicationId": strings.TrimSpace(req.RowID)})
	if err != nil {
		return campaigns.RowMessage{}, err
	}
	if len(rows) == 0 {
		return campaigns.RowMessage{}, campaigns.RowWithdrawn{Reason: withdrawnGone}
	}
	app := rows[0]
	applicationID := trimmed(app["id"])
	state, err := p.stateOf(sys, applicationID)
	if err != nil {
		return campaigns.RowMessage{}, err
	}
	settingsRow, err := p.settingsRowAt(sys, trimmed(app["storeId"]))
	if err != nil {
		return campaigns.RowMessage{}, err
	}
	host, storeName, err := p.storefrontOf(sys, app, EffectiveSettings(settingsRow))
	if err != nil {
		return campaigns.RowMessage{}, err
	}
	fields := map[string]string{
		"companyName":    asString(app["companyName"]),
		"applicantName":  asString(app["applicantName"]),
		"signInEmail":    trimmed(app["applicantEmail"]),
		"storeName":      storeName,
		"termsUrl":       "",
		"termsExpiresOn": "",
		"priceSheetUrl":  "",
	}
	switch req.Kind {
	case EmailTerms:
		waiting, err := p.waitingForTerms(sys, app)
		if err != nil {
			return campaigns.RowMessage{}, err
		}
		if !waiting {
			return campaigns.RowMessage{}, campaigns.RowWithdrawn{Reason: withdrawnTerms}
		}
		token, expires, err := p.mintLink(ctx, app, LinkPurposeTerms, p.clock())
		if err != nil {
			return campaigns.RowMessage{}, err
		}
		fields["termsUrl"] = "https://" + host + "/wholesale/terms?t=" + token
		// THE INSTANT IN UTC, as the storefront's page prints it, so the email and
		// the page always name the same day.
		fields["termsExpiresOn"] = expires.UTC().Format("January 2, 2006")
	case EmailApproved:
		granted, err := p.accessGranted(sys, app, state)
		if err != nil {
			return campaigns.RowMessage{}, err
		}
		if !granted {
			return campaigns.RowMessage{}, campaigns.RowWithdrawn{Reason: withdrawnAccess}
		}
		token, _, err := p.mintLink(ctx, app, LinkPurposePriceSheet, p.clock())
		if err != nil {
			return campaigns.RowMessage{}, err
		}
		fields["priceSheetUrl"] = "https://" + host + "/wholesale/price-sheet?t=" + token
	case EmailRejected, EmailRevoked:
		want := StateRejected
		if req.Kind == EmailRevoked {
			want = StateRevoked
		}
		if state != want {
			return campaigns.RowMessage{}, campaigns.RowWithdrawn{Reason: notInStateReason(want)}
		}
	default:
		return campaigns.RowMessage{}, fmt.Errorf("wholesale: %q is not an applicant's email", req.Kind)
	}
	return campaigns.RowMessage{Email: trimmed(app["applicantEmail"]), AccountID: trimmed(app["accountId"]), Fields: fields}, nil
}

// storefrontOf is the storefront an application's links open on: the host of
// its site -- the one it arrived on, else the one the store's settings name --
// and the store's name as an email says it (the site's title, else its host).
func (p *Provider) storefrontOf(sys context.Context, app map[string]any, settings Settings) (string, string, error) {
	siteID := trimmed(app["siteId"])
	if siteID == "" {
		siteID = settings.SiteID
	}
	if siteID == "" {
		return "", "", fmt.Errorf("wholesale: application %q names no site, so it has no storefront to link to", trimmed(app["id"]))
	}
	rows, err := p.rowsFor(sys, "siteById", map[string]string{"siteId": bareID(siteID)})
	if err != nil {
		return "", "", err
	}
	if len(rows) == 0 {
		return "", "", fmt.Errorf("wholesale: the site of application %q cannot be read", trimmed(app["id"]))
	}
	host := strings.TrimSuffix(strings.TrimSpace(asString(rows[0]["hostname"])), "/")
	if host == "" || strings.ContainsAny(host, "/?# ") {
		return "", "", fmt.Errorf("wholesale: the site of application %q has no usable hostname", trimmed(app["id"]))
	}
	name := strings.TrimSpace(asString(rows[0]["title"]))
	if name == "" {
		name = host
	}
	return host, name, nil
}

// emailsView is an application's emails as wholesaleGet shows them, newest
// first, each as the lane says it stands (effectiveSend) with the lane's own
// record of its delivery.
//
// THE DELIVERY IS READ AFTER THE GATE: wholesaleGet's caller was admitted by
// the organization gate and read the application under their own actor before
// this runs, and the status read names only the application's organization.
// A status that cannot be read leaves each delivery null and each email as its
// row recorded it, and says so in one line for the operator, naming the
// application and nothing of the applicant.
func (p *Provider) emailsView(ctx context.Context, app map[string]any, sends []map[string]any) []map[string]any {
	ids := make([]string, 0, len(sends))
	for _, row := range sends {
		ids = append(ids, trimmed(row["requestId"]))
	}
	deliveries, err := p.laneDeliveries(ctx, app, ids)
	unreadable := err != nil
	if unreadable {
		p.log().Warn("wholesale: the row lane's record of an application's emails could not be read",
			"component", "wholesale", "applicationId", trimmed(app["id"]))
	}
	out := make([]map[string]any, 0, len(sends))
	for _, row := range sends {
		attempt, _ := intArg(row["attempt"])
		var delivery any
		d, ok := deliveries[trimmed(row["requestId"])]
		if ok {
			delivery = d
		}
		row = p.effectiveSend(row, d, !unreadable)
		out = append(out, map[string]any{
			"kind":        trimmed(row["kind"]),
			"attempt":     attempt,
			"state":       trimmed(row["state"]),
			"reason":      asString(row["reason"]),
			"sentAt":      timeText(row["sentAt"]),
			"requestedBy": trimmed(row["requestedBy"]),
			"delivery":    delivery,
			// THE LANE'S RECORD COULD NOT BE READ: the email is as it was recorded.
			"deliveryUnreadable": unreadable,
		})
	}
	return out
}

// emailTemplate is one template a store may choose for an email.
type emailTemplate struct{ id, name string }

// emailTemplates is the published transactional templates of the organization
// account, read UNDER ctx through the Email app's own templates query (status
// ready) to its end and filtered here: a person who reads none is offered none.
func (p *Provider) emailTemplates(ctx context.Context, account string) ([]emailTemplate, error) {
	account = bareID(account)
	if account == "" {
		return nil, nil
	}
	var out []emailTemplate
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < maxStorePages; page++ {
		rows, next, err := p.rowsPage(ctx, "templates", map[string]string{"status": "ready"}, cursor)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			id := bareID(trimmed(row["id"]))
			if id == "" || seen[id] || bareID(trimmed(row["accountId"])) != account ||
				trimmed(row["purpose"]) != "transactional" || trimmed(row["status"]) != "ready" {
				continue
			}
			seen[id] = true
			name := strings.TrimSpace(asString(row["name"]))
			if name == "" {
				name = strings.TrimSpace(asString(row["subject"]))
			}
			out = append(out, emailTemplate{id: id, name: name})
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return out, nil
}

// NoSenderChosenReason declines every email of a store whose sender the lane
// cannot resolve because none is chosen.
const NoSenderChosenReason = "No sender is chosen for this store's emails; the cluster's operators choose it."

// ReasonUncertain is an attempt whose hand-over the provider never answered:
// it may have been delivered.
const ReasonUncertain = "We could not confirm this email was sent. It may have been delivered."

// ReasonHeldForAutomation is a held attempt as the automation that asked for
// it is answered: the lane's own sentence, which can name the store's template
// or sender, stays on the row for the store's people.
const ReasonHeldForAutomation = "The email service held this email until the store's email settings are fixed; the application's page says what to fix."

// NoteMaybeSecondCopy is carried by the answer to a resend whose last attempt
// may have been delivered.
const NoteMaybeSecondCopy = "The last attempt may have been delivered, so the applicant may receive this email twice."

// reasonLaneUnread is an attempt recorded and never settled whose record at the
// email service could not be read: nothing says what became of it.
const reasonLaneUnread = "The email service could not say just now what became of this email."

// ReasonNotSent is a failed attempt that reached the pack with no sentence for
// a person: an error that is not the lane's answer, or an answer with no words.
// It says what happened and what a person can do next, and nothing of the error.
const ReasonNotSent = "The email could not be sent. Send it again; if it is still not sent, the cluster's operators can say why."

// The sentences of a store with NOBODY TO SEND ITS EMAILS AS (asStoreMerchant):
// each says why, and who can put it right. They are the sender line's reason,
// every email's in emailsWouldSend, the reason a due email is recorded skipped
// with, and a resend's refusal.
const (
	// NoSiteServesSentence: no site's binding names the store.
	NoSiteServesSentence = "No site serves this store now, so there is nobody its emails can be sent as. The cluster's operators connect a site to the store."
	// SeveralSitesSentence: several sites serve the store under different
	// owners, and neither the application nor the store's settings names one of
	// them.
	SeveralSitesSentence = "More than one site serves this store, under different owners, and the store's wholesale settings name none of them, so there is nobody its emails can be sent as. The cluster's operators leave one site serving the store, or say in its wholesale settings which of them its program belongs to."
	// SiteNamesNoOwnerSentence: the site that serves the store names no owner.
	SiteNamesNoOwnerSentence = "The site that serves this store names no owner, so there is nobody its emails can be sent as. The cluster's operators serve the store from a site that a person owns."
)

// NoSendAuthoritySentence is the store's merchant -- the person who owns its
// site now -- without what the lane requires of whoever sends for an
// organization: write permission in it. THE PACK'S OWN SENTENCE, answered
// wherever the lane's code says so (the sender read, and a send the lane
// refuses for it): it names what is missing and who can give it, and is never
// the lane's words for it.
const NoSendAuthoritySentence = "The owner of the site that serves this store does not have write permission in the store's organization, so its emails cannot be sent. An administrator of the organization, or the cluster's operators, can give them that permission."

// SenderUnreadableSentence is the store's sender when the email service could
// not be asked which one its emails go out as.
const SenderUnreadableSentence = "The sender could not be read just now."

// InstallationDefaultSenderSentence is the installation's own organization's
// sender when none is chosen: the installation's default mailbox.
const InstallationDefaultSenderSentence = "The installation's default sender."

// OnlyOperatorsChooseTheSenderSentence refuses a sender named by anybody but a
// cluster operator.
const OnlyOperatorsChooseTheSenderSentence = "Only the cluster's operators choose the sender."

// mergeEmailSettings is the five email settings after a change: the row's,
// with each one the call names. A sender that differs from the row's is
// refused for anybody but a cluster operator; a template must be one of the
// templates the caller is offered (emailTemplates), or empty.
func (p *Provider) mergeEmailSettings(ctx context.Context, args map[string]any, row map[string]any) (heldEmail, error) {
	held := emailHeld(row)
	if v, named := args["senderIdentityId"]; named && v != nil {
		sender := bareID(trimmed(v))
		if sender != held.senderIdentityID && !isClusterOperator(ctx) {
			return heldEmail{}, errors.New(OnlyOperatorsChooseTheSenderSentence)
		}
		// THE SENDER IS ONE OF THE STORE'S ORGANIZATION'S, even when an operator
		// chooses it: the lane would refuse another organization's at send time,
		// after the store's people believed it chosen. Read under the caller, as
		// the send path reads a sender.
		if sender != "" && sender != held.senderIdentityID {
			rows, err := p.rowsFor(ctx, "senderIdentityById", map[string]string{"senderIdentityId": sender})
			if err != nil {
				return heldEmail{}, err
			}
			account := bareID(trimmed(row["accountId"]))
			if len(rows) == 0 || account == "" || bareID(trimmed(rows[0]["accountId"])) != account {
				return heldEmail{}, fmt.Errorf("wholesale: %q is not a sender of this store's organization; choose one "+
					"of the organization's senders, or none", sender)
			}
		}
		held.senderIdentityID = sender
	}
	var offered map[string]bool
	for _, field := range []struct {
		arg  string
		into *string
	}{
		{"termsTemplateId", &held.termsTemplateID},
		{"approvedTemplateId", &held.approvedTemplateID},
		{"rejectedTemplateId", &held.rejectedTemplateID},
		{"revokedTemplateId", &held.revokedTemplateID},
	} {
		v, named := args[field.arg]
		if !named || v == nil {
			continue
		}
		id := bareID(trimmed(v))
		if id != "" && id != *field.into {
			if offered == nil {
				templates, err := p.emailTemplates(ctx, trimmed(row["accountId"]))
				if err != nil {
					return heldEmail{}, err
				}
				offered = map[string]bool{}
				for _, t := range templates {
					offered[t.id] = true
				}
			}
			if !offered[id] {
				return heldEmail{}, fmt.Errorf("wholesale: %q is not a published transactional template of this store's "+
					"organization that you can choose; choose one of the listed templates, or none", id)
			}
		}
		*field.into = id
	}
	return held, nil
}
