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
//  2. HAND OVER. The winner calls the row lane as the application's merchant;
//     the lane asks the pack's resolver for the address and mints the link.
//  3. SETTLE. What the lane answered is written over the row, fenced on the
//     version recorded in step 1.
//
// THE AUTOMATIC PATH MAKES ITS ATTEMPT AT MOST ONCE. Before step 1 it reads
// the application's rows of the kind, fresh: if ANY exists -- attempt 1 in any
// state, sent or not, or a later attempt a person sent -- it hands over
// nothing and answers that row (attempt 1's when there is one). So a
// re-delivered event, a second granted version, a version the backfill
// re-wrote and the engine running a failed automation again all send nothing,
// and nothing automatic follows a person's resend.
//
// # What happened is a row, in the lane's words
//
// What the lane answered is recorded on the email's row: sent; skipped with the
// lane's sentence (the row withdrew it, or the address is suppressed); held
// with the lane's sentence (configuration the store can fix, such as an
// unpublished template: nothing was attempted); uncertain (the hand-over had no
// answer, and the provider may have the message); failed with the lane's words;
// or deferred (the send-rate limit: the lane replays it). Every outcome is
// ANSWERED AS A REPLY -- sent true or false, with its reason -- and never as an
// error that would make the engine run the automation again; only a fault that
// kept anything from being recorded is an error. The automation's reply
// carries the pack's own fixed sentences for a hold and a failure: the lane's
// words, which can name the store's template, its sender or a transport's
// text, stay on the row for the store's people.
//
// # The lane's status is the truth, except its early word on an uncertain one
//
// The row records what the lane answered at the time; the lane's own record of
// the delivery may say more since (a deferral it replayed and sent, a send the
// provider refused). The application's page, the export, the acts and the
// resend read each attempt through the lane's status read (effectiveSend). An
// attempt recorded UNCERTAIN stays uncertain until the lane says something
// definite -- sent, or a provider report of delivery or a bounce -- because the
// lane writes failed in its ledger before it decides a hand-over was uncertain.
//
// # Sent again by a person
//
// A person of the store sends an email again as a new attempt
// (wholesaleResendEmail): the terms and approval emails as before, and a
// rejection or removal only while the application is still rejected or
// removed and its last attempt did not reach the provider (held, uncertain or
// failed) -- one that was sent is never sent twice. Nothing is sent again while
// the last attempt is on its way, and the answer to a resend over an uncertain
// attempt says the applicant may receive it twice.

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
	// asked. It is read as deferred while it is young (on its way) and as
	// uncertain once sendingWindow has passed with nothing settled (recordedState).
	sendSending = "sending"
	// sendPendingRecorded is the first build's word for a deferral or an
	// uncertain answer, which the concept still accepts for rows it wrote. It is
	// never written now, and read as deferred (recordedState).
	sendPendingRecorded = "pending"
)

// sendingWindow is how long an attempt may stay recorded and unsettled before
// it is read as uncertain: well past any hand-over the lane would still be in.
const sendingWindow = 15 * time.Minute

// recordedState is a row's state, and its reason, as this build reads it.
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
// stand in for the lane's unexported hold and uncertain types.
var (
	laneHeldReason = campaigns.HeldReason
	laneUncertain  = campaigns.UncertainSend
)

// The sentences a send that the builtin itself declines answers. Nothing is
// recorded for these: the attempt stays open, so the same call made once the
// store is ready (a template chosen, the access granted) sends.
const (
	ReasonTermsNotRequired = "terms are not required on this store"
	ReasonNotWaiting       = "the application is not waiting for terms"
	ReasonAccessNotGranted = "access is not granted yet"
	// ReasonAccessPredatesEmail declines the approval email and the access
	// notices for an access record written before the pack sent either.
	ReasonAccessPredatesEmail = "this access record predates the pack's emails and notices, so nothing is sent for it"
	ReasonNoOrganization      = "the application belongs to no organization, so no email can be sent for it"
	reasonUnreadableAnswer    = "the row lane answered nothing this pack can read"
	// reasonSending is an attempt recorded while the lane is being asked.
	reasonSending = "The email is being handed to the email service."
	// reasonFailedForAutomation is a failed attempt as the automation that asked
	// for it is answered; the lane's words stay on the row.
	reasonFailedForAutomation = "The email was not sent; the application's page says why."
	// reasonLaneFailed is a delivery the lane recorded as failed after the row
	// said otherwise; the lane's status read does not carry its words.
	reasonLaneFailed = "The email service recorded this email as failed."
	// reasonLaneWaiting is a delivery the lane holds to send later, when the
	// row carries no sentence of its own for it.
	reasonLaneWaiting       = "The email service is waiting to send this email."
	withdrawnTerms          = "the application is no longer waiting for terms"
	withdrawnAccess         = "access is not granted"
	withdrawnGone           = "the application no longer exists"
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
func noTemplateReason(kind string) string { return "no " + kind + " email template is chosen" }

// notInStateReason is the sentence of a rejection or removal email for an
// application that is not in that state.
func notInStateReason(state string) string { return "the application is not " + state }

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

// emailBlocker is the one sentence why the store's email of kind would not be
// sent now, for an application in the right state, or "" when it would --
// THE CHECKS EVERY SEND MAKES OF THE STORE, in order, before the lane is asked:
//
//   - the terms email needs the store to require its terms;
//   - a template must be chosen for the kind;
//   - the lane must resolve a sender (campaignRowSender, as a send resolves it).
//
// declines says whether a send declines on it -- recording nothing, so the
// same call once the store is ready sends. Terms not required, no template and
// NO SENDER CHOSEN decline. A sender that IS chosen and that the lane will not
// send as (turned off, gone, another organization's) is answered with the
// lane's sentence but does not decline: the lane holds the send, the row says
// held, and a person sends it again once it is fixed. A sender read that fails
// declines nothing either: the lane decides at the send.
//
// What it cannot know without sending: whether the chosen template is still a
// published transactional template. The pack reads the store's templates only
// under the caller (the Email app's own read), so a template it cannot see may
// be unpublished or merely hidden from the caller; the lane's hold is what
// finds an unpublished one.
func (p *Provider) emailBlocker(merchant context.Context, s Settings, kind string) (string, bool) {
	if kind == EmailTerms && !s.TermsRequired {
		return ReasonTermsNotRequired, true
	}
	if s.TemplateFor(kind) == "" {
		return noTemplateReason(kind), true
	}
	sender := p.senderView(merchant, s)
	if ready, _ := sender["ready"].(bool); ready {
		return "", false
	}
	if asString(sender["reason"]) == SenderUnreadableSentence {
		return SenderUnreadableSentence, false
	}
	if s.SenderIdentityID == "" {
		return NoSenderChosenReason, true
	}
	return asString(sender["reason"]), false
}

// emailsWouldSend is, for each of the four kinds, whether an email of it would
// be sent now for an application in the right state, and the one sentence why
// not (emailBlocker).
func (p *Provider) emailsWouldSend(merchant context.Context, s Settings) map[string]any {
	out := map[string]any{}
	for _, kind := range EmailKinds {
		reason, _ := p.emailBlocker(merchant, s, kind)
		out[kind] = map[string]any{"wouldSend": reason == "", "reason": reason}
	}
	return out
}

// senderView is the store's sender as a person reads it: the name and address
// its emails go out as, resolved by the lane as a send resolves it (the chosen
// identity, or what a send with none chosen goes out as), or why there is none,
// in a sentence. The address is the store's own sender's, never an applicant's.
//
// AS THE MERCHANT: the lane answers a caller holding what a send requires of
// it, and the pack sends as the store's merchant. The installation's own
// organization with no sender chosen sends as the installation's default
// mailbox, which has no name or address the lane can show: it says so.
func (p *Provider) senderView(merchant context.Context, s Settings) map[string]any {
	unread := map[string]any{"ready": false, "fromName": "", "address": "", "reason": SenderUnreadableSentence}
	account := bareID(s.AccountID)
	if account == "" || p.lane == nil {
		return unread
	}
	got, err := p.lane.RowSender(merchant, account, s.SenderIdentityID)
	if err != nil {
		return unread
	}
	ready, _ := got["ready"].(bool)
	view := map[string]any{"ready": ready, "fromName": asString(got["fromName"]), "address": trimmed(got["address"]),
		"reason": asString(got["reason"])}
	if ready && trimmed(got["address"]) == "" {
		view["fromName"], view["reason"] = "", InstallationDefaultSenderSentence
	}
	if !ready {
		view["fromName"], view["address"] = "", ""
		if strings.TrimSpace(asString(got["reason"])) == "" {
			view["reason"] = SenderUnreadableSentence
		}
	}
	return view
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
	merchant, err := p.asMerchant(ctx, app)
	if err != nil {
		return nil, err
	}
	if reason, declines := p.emailBlocker(merchant, settings, EmailTerms); declines {
		return declined(app, EmailTerms, reason)
	}
	template := settings.TermsTemplateID
	waiting, err := p.waitingForTerms(sys, app)
	if err != nil {
		return nil, err
	}
	if !waiting {
		return declined(app, EmailTerms, ReasonNotWaiting)
	}
	return p.sendEmail(ctx, sys, app, EmailTerms, 1, template, settings.SenderIdentityID, "", true, "")
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
	state, err := p.stateOf(sys, trimmed(app["id"]))
	if err != nil {
		return nil, err
	}
	switch kind {
	case EmailApproved:
		// THE APPROVAL EMAIL WAITS FOR THE GRANT (W3): it tells the buyer their
		// trade prices are on and carries the price-sheet link, which opens only
		// while they are.
		// THE REASON IS TRUE OF THE APPLICATION: "not granted yet" only of one that
		// is approved, and so may yet be granted.
		if state != StateApproved {
			return declined(app, kind, notInStateReason(StateApproved))
		}
		granted, err := p.accessGranted(sys, app, state)
		if err != nil {
			return nil, err
		}
		if !granted {
			return declined(app, kind, ReasonAccessNotGranted)
		}
		// NOTHING IS MAILED FOR WHAT HAPPENED BEFORE THIS RELEASE. Every version
		// the pack writes now carries its attempt, and the backfill carries one
		// forward only when the row holds it, so a granted row with none is an
		// access granted before the pack sent emails -- re-written, perhaps, by the
		// backfill, which a client's automation sees as any other granted version.
		ent, err := p.entitlementRow(sys, trimmed(app["id"]))
		if err != nil {
			return nil, err
		}
		if attemptOf(ent) == 0 {
			return declined(app, kind, ReasonAccessPredatesEmail)
		}
	case EmailRejected:
		if state != StateRejected {
			return declined(app, kind, notInStateReason(StateRejected))
		}
	case EmailRevoked:
		if state != StateRevoked {
			return declined(app, kind, notInStateReason(StateRevoked))
		}
	}
	merchant, err := p.asMerchant(ctx, app)
	if err != nil {
		return nil, err
	}
	if reason, declines := p.emailBlocker(merchant, settings, kind); declines {
		return declined(app, kind, reason)
	}
	return p.sendEmail(ctx, sys, app, kind, 1, settings.TemplateFor(kind), settings.SenderIdentityID, "", true, "")
}

// resendEmail answers wholesaleResendEmail: a person of the store sends one of
// the four emails again, as a new attempt.
//
// THE ORGANIZATION GATE ADMITTED THE CALLER (the part, in the application's
// organization). Before anything is read the caller must be a signed-in person
// who is not one of the cluster's operators; then the application is read
// under the caller, so one they cannot read is not resent.
func (p *Provider) resendEmail(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	requester, err := resenderOf(ctx)
	if err != nil {
		return nil, err
	}
	kind := trimmed(args["kind"])
	if _, known := emailNouns[kind]; !known {
		return nil, fmt.Errorf("wholesale: %q is not one of the applicant's emails; it is terms, approved, rejected or revoked", kind)
	}
	applicationID := trimmed(args["applicationId"])
	if applicationID == "" {
		return nil, fmt.Errorf("wholesale: applicationId is required")
	}
	rows, err := p.rowsFor(ctx, "applicationById", map[string]string{"applicationId": applicationID})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("wholesale: application %q is not readable by this caller, so no email of it is sent again", applicationID)
	}
	app := rows[0]
	sys := auth.ContextWithSystemActor(ctx, emailActor)
	settingsRow, err := p.settingsRowAt(sys, trimmed(app["storeId"]))
	if err != nil {
		return nil, err
	}
	settings := EffectiveSettings(settingsRow)
	// THE STORE'S OWN READINESS FIRST, the checks a send makes (emailBlocker):
	// an email the store's automation would decline is not sent again either.
	merchant, err := p.asMerchant(ctx, app)
	if err != nil {
		return nil, err
	}
	if reason, declines := p.emailBlocker(merchant, settings, kind); declines {
		return nil, errors.New("wholesale: " + reason)
	}
	sends, err := p.emailSendsOf(sys, trimmed(app["id"]))
	if err != nil {
		return nil, err
	}
	// WHAT THE LANE SAYS OF THE LAST ATTEMPT. For a rejection or removal the
	// answer decides whether anything is sent, so a status that cannot be read
	// refuses the resend; for the other two it only says whether the last attempt
	// is still on its way, and the row's own record answers when it cannot.
	latest := latestSend(sends, kind)
	if latest != nil {
		deliveries, err := p.laneDeliveries(ctx, app, []string{trimmed(latest["requestId"])})
		if err != nil && (kind == EmailRejected || kind == EmailRevoked) {
			return nil, fmt.Errorf("wholesale: the email service's record of the last %s could not be read, so it is not "+
				"sent again now", emailNouns[kind])
		}
		latest = p.effectiveSend(latest, deliveries[trimmed(latest["requestId"])])
		if trimmed(latest["state"]) == SendDeferred {
			return nil, fmt.Errorf("wholesale: the last %s is waiting to be sent by the email service, so it is not sent again", emailNouns[kind])
		}
	}
	switch kind {
	case EmailTerms:
		waiting, err := p.waitingForTerms(sys, app)
		if err != nil {
			return nil, err
		}
		if !settings.TermsRequired || !waiting {
			return nil, errors.New("wholesale: the terms email is sent again only while the application waits for its terms")
		}
	case EmailApproved:
		state, err := p.stateOf(sys, trimmed(app["id"]))
		if err != nil {
			return nil, err
		}
		granted, err := p.accessGranted(sys, app, state)
		if err != nil {
			return nil, err
		}
		if !granted || !hasAttempt(sends, EmailApproved, 1) {
			return nil, errors.New("wholesale: the approval email is sent again only to a buyer whose access is granted, " +
				"once the store's automation has sent it")
		}
	case EmailRejected, EmailRevoked:
		want := StateRejected
		if kind == EmailRevoked {
			want = StateRevoked
		}
		state, err := p.stateOf(sys, trimmed(app["id"]))
		if err != nil {
			return nil, err
		}
		if state != want || latest == nil || !notReachedProvider(trimmed(latest["state"])) {
			return nil, fmt.Errorf("wholesale: the %s is sent again only while the application is %s and its last "+
				"attempt did not reach the email service", emailNouns[kind], strings.ToLower(staffWords[want]))
		}
	}
	// A RESEND OVER AN ATTEMPT THAT MAY HAVE BEEN DELIVERED says so in its answer.
	note := ""
	if latest != nil && trimmed(latest["state"]) == SendUncertain {
		note = NoteMaybeSecondCopy
	}
	return p.sendEmail(ctx, sys, app, kind, highestAttempt(sends, kind)+1, settings.TemplateFor(kind), settings.SenderIdentityID,
		userRef(requester), false, note)
}

// resenderOf is the person a resend is recorded for: the calling user, never
// one of the cluster's operators, who send nothing on a store's behalf (the
// rule deciderOf holds a decision to).
func resenderOf(ctx context.Context) (string, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac.Synthetic || strings.TrimSpace(ac.UserId) == "" {
		return "", errors.New("wholesale: an email is sent again by a signed-in person")
	}
	if auth.IsClusterOperator(ac.Role) {
		return "", errors.New("wholesale: the cluster's operators host the store's applications and send none of " +
			"their emails; a person on the store's side sends one again")
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

// sendEmail is ONE ATTEMPT OF ONE EMAIL: record, hand over, settle (the file
// note). automatic is the store's automation's attempt 1, which is made at
// most once; note is carried by a person's resend answer.
func (p *Provider) sendEmail(ctx, sys context.Context, app map[string]any, kind string, attempt int,
	templateID, senderID, requestedBy string, automatic bool, note string) ([]memorynodes.MemoryNode, error) {
	applicationID := trimmed(app["id"])
	rowID := EmailSendRowID(kind, applicationID, attempt)
	requestID := LaneRequestID(kind, applicationID, attempt)
	fresh := memql.ContextWithFreshRead(sys)
	sends, err := p.emailSendsOf(fresh, applicationID)
	if err != nil {
		return nil, err
	}
	// THE AUTOMATIC PATH, AT MOST ONCE: any row of the kind -- this attempt in
	// any state, or a later one a person sent -- is answered, and nothing is
	// handed over.
	if automatic {
		if row := automaticAnswerRow(sends, kind); row != nil {
			return p.answerRecorded(ctx, app, row, false, "")
		}
	}
	for _, row := range sends {
		if bareID(trimmed(row["id"])) == rowID {
			return p.answerRecorded(ctx, app, row, !automatic, note)
		}
	}
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
		"kind":          kind,
		"attempt":       attempt,
		"requestId":     requestID,
		"templateId":    templateID,
		"state":         sendSending,
		"reason":        reasonSending,
		"sentAt":        "",
		"requestedBy":   requestedBy,
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
		return p.answerRecorded(ctx, app, current, !automatic, note)
	}
	recorded, recordErr := p.emailSendRow(memql.ContextWithFreshRead(sys), applicationID, rowID)

	// 2. HAND OVER.
	state, reason, sentAt := SendFailed, ReasonNoOrganization, ""
	if account := bareID(trimmed(app["accountId"])); account != "" {
		if p.lane == nil {
			state, reason = SendFailed, "the pack has no row lane, so no email can be sent"
		} else {
			laneArgs := map[string]any{
				"rowConcept": applicationConcept,
				"rowId":      bareID(applicationID),
				"accountId":  account,
				"kind":       kind,
				"templateId": templateID,
				"requestId":  requestID,
			}
			if senderID != "" {
				laneArgs["senderIdentityId"] = senderID
			}
			reply, sendErr := p.lane.SendRowMessage(merchant, laneArgs)
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
	settle := merchant
	if recordErr == nil && recorded != nil {
		settle = memql.ContextWithRowVersionFence(merchant, emailSendConcept, rowID, rowTime(recorded["createdAt"]))
	}
	written := map[string]any{"id": emailSendConcept + ":" + rowID}
	for k, v := range row {
		if k != "emailSendId" {
			written[k] = v
		}
	}
	if err := p.write(settle, "putEmailSendRow", row); err != nil {
		// THE ATTEMPT IS RECORDED (sending) and was handed over; only its outcome
		// could not be written. It is answered as recorded, never as an error that
		// would make the engine run the automation again; the lane's status read
		// says what became of it.
		p.log().Warn("wholesale: an email's outcome could not be recorded", "component", "wholesale",
			"applicationId", applicationID, "kind", kind, "attempt", attempt, "state", state)
		written["state"], written["reason"], written["sentAt"] = sendSending, reasonSending, ""
	}
	return p.answerOutcome(written, !automatic, note)
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

// notReachedProvider reports whether an attempt in state did not reach the
// provider, so a rejection or removal may be sent again: held, uncertain or
// failed.
func notReachedProvider(state string) bool {
	return state == SendHeld || state == SendUncertain || state == SendFailed
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
	if err != nil {
		deliveries = nil
	}
	return p.answerOutcome(p.effectiveSend(row, deliveries[id]), forStaff, note)
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
	answer["state"], answer["reason"] = state, reason
	return answerSend(answer, note)
}

// laneOutcome reads what the row lane answered as an emailSend state and its
// sentence: a hold is held, and a skip skipped, each with the lane's sentence;
// an uncertain hand-over is uncertain; a deferral deferred; any other error is
// failed with the lane's words.
func laneOutcome(reply map[string]any, err error) (string, string) {
	if err != nil {
		if reason, held := laneHeldReason(err); held {
			return SendHeld, reason
		}
		if laneUncertain(err) {
			return SendUncertain, ReasonUncertain
		}
		return SendFailed, laneWords(err)
	}
	switch {
	case reply["sent"] == true:
		return SendSent, ""
	case reply["skipped"] == true:
		return SendSkipped, trimmed(reply["reason"])
	case reply["uncertain"] == true:
		return SendUncertain, ReasonUncertain
	case reply["deferred"] == true:
		return SendDeferred, trimmed(reply["reason"])
	}
	return SendFailed, reasonUnreadableAnswer
}

// effectiveSend is one attempt's row as the lane now says it stands. The row's
// state is first read as this build reads it (recordedState). Then, where the
// lane has recorded a delivery for the attempt's request id:
//
//   - an UNCERTAIN attempt stays uncertain, with ReasonUncertain, unless the
//     lane says something definite: sent, or a provider report of delivery
//     (sent) or of a bounce (failed, reasonBounced). The lane writes failed in
//     its ledger before it decides a hand-over was uncertain, so its failed is
//     not final for one;
//   - any other attempt takes the lane's status: sent with the lane's time,
//     failed, skipped with the lane's sentence, pending as deferred; the row's
//     own sentence is kept only when the row already said the same.
//
// A row the lane has recorded nothing for (a hold, an attempt the lane never
// reached) is answered as it stands.
func (p *Provider) effectiveSend(row, delivery map[string]any) map[string]any {
	state, reason := p.recordedState(row)
	sentAt := timeText(row["sentAt"])
	status, provider := trimmed(delivery["status"]), trimmed(delivery["providerStatus"])
	if state == SendUncertain {
		switch {
		case status == "sent" || provider == "delivered":
			state, reason = SendSent, ""
			if at := timeText(delivery["sentAt"]); at != "" {
				sentAt = at
			}
		case bouncedReports[provider]:
			state, reason, sentAt = SendFailed, reasonBounced, ""
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
			state, reason, sentAt = SendSkipped, asString(delivery["skipReason"]), ""
		case "pending":
			if state != SendDeferred {
				reason = reasonLaneWaiting
			}
			state, sentAt = SendDeferred, ""
		}
	}
	out := copyMap(row)
	out["state"], out["reason"], out["sentAt"] = state, reason, sentAt
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
					"status":         trimmed(s["status"]),
					"skipReason":     asString(s["skipReason"]),
					"providerStatus": trimmed(s["providerStatus"]),
					"sentAt":         timeText(s["sentAt"]),
				}
			}
		}
	}
	return out, nil
}

// laneWords is the lane's error as the store's people read it: without the
// pack's own prefix for the call. The lane's errors never carry an address
// (campaigns/send_row.go), and the call carries none to quote.
func laneWords(err error) string {
	text := strings.TrimSpace(err.Error())
	return strings.TrimPrefix(text, "wholesale: "+laneSendConstruct+": ")
}

// declined answers a send the builtin itself declined, recording nothing: the
// attempt stays open for the call that finds the store ready.
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

// hasAttempt reports whether attempt of kind is recorded.
func hasAttempt(sends []map[string]any, kind string, attempt int) bool {
	for _, row := range sends {
		if n, _ := intArg(row["attempt"]); trimmed(row["kind"]) == kind && n == attempt {
			return true
		}
	}
	return false
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
		row = p.effectiveSend(row, d)
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
const NoSenderChosenReason = "no sender is chosen for this store's emails; the cluster's operators choose it"

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

// reasonBounced is an attempt the provider reported it could not deliver.
const reasonBounced = "The email service reports this email could not be delivered."

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
