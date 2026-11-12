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
// # Once
//
// The send builtins never mint and never send twice. Each computes its attempt
// (1 for the store's automation; one more than the highest recorded for a
// resend) and its lane request id, reads that attempt's row, and when it is
// sent, skipped or pending answers it and calls nothing -- which is what makes a
// re-delivered event, a second granted version of one grant and a version the
// backfill re-wrote send one email. Two nodes that pass that read at once both
// reach the lane, whose receipt lock lets one through to the resolver; the
// other is answered the first's receipt. The row is then written fenced on the
// version read, so the second writer answers the first's row.
//
// # A failure is a row, not an error
//
// What the lane answered is recorded on the email's row -- sent, skipped with
// the lane's sentence, pending, or failed with the lane's words -- and the
// builtin answers the row rather than returning an error, because the store's
// people read why an email did not go out from that row (wholesaleGet's
// emails). Only a caller who may not send is refused.

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
	SendSent    = "sent"
	SendSkipped = "skipped"
	SendFailed  = "failed"
	SendPending = "pending"
)

// The sentences a send that the builtin itself declines answers. Nothing is
// recorded for these: the attempt stays open, so the same call made once the
// store is ready (a template chosen, the access granted) sends.
const (
	ReasonTermsNotRequired  = "terms are not required on this store"
	ReasonNotWaiting        = "the application is not waiting for terms"
	ReasonAccessNotGranted  = "access is not granted yet"
	ReasonNoOrganization    = "the application belongs to no organization, so no email can be sent for it"
	reasonUnreadableAnswer  = "the row lane answered nothing this pack can read"
	withdrawnTerms          = "the application is no longer waiting for terms"
	withdrawnAccess         = "access is not granted"
	withdrawnGone           = "the application no longer exists"
	emailSendConcept        = "v1:wholesale:emailSend"
	emailActor              = "wholesale-email"
	maxEmailReason          = 2000
	maxLaneRequestIDLength  = 160
	laneSendConstruct       = "campaignSendRowMessage"
	laneStatusConstruct     = "campaignRowMessageStatus"
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

// sendTermsEmail answers wholesaleSendTermsEmail.
func (p *Provider) sendTermsEmail(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireInternal(ctx, "wholesaleSendTermsEmail"); err != nil {
		return nil, err
	}
	sys, app, settings, err := p.emailSubject(ctx, trimmed(args["applicationId"]))
	if err != nil {
		return nil, err
	}
	if !settings.TermsRequired {
		return declined(app, EmailTerms, ReasonTermsNotRequired)
	}
	template := settings.TermsTemplateID
	if template == "" {
		return declined(app, EmailTerms, noTemplateReason(EmailTerms))
	}
	waiting, err := p.waitingForTerms(sys, app)
	if err != nil {
		return nil, err
	}
	if !waiting {
		return declined(app, EmailTerms, ReasonNotWaiting)
	}
	return p.sendEmail(ctx, sys, app, EmailTerms, 1, template, settings.SenderIdentityID, "")
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
		granted, err := p.accessGranted(sys, app, state)
		if err != nil {
			return nil, err
		}
		if !granted {
			return declined(app, kind, ReasonAccessNotGranted)
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
	template := settings.TemplateFor(kind)
	if template == "" {
		return declined(app, kind, noTemplateReason(kind))
	}
	return p.sendEmail(ctx, sys, app, kind, 1, template, settings.SenderIdentityID, "")
}

// resendEmail answers wholesaleResendEmail: a person of the store sends the
// terms or the approval email again, as a new attempt.
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
	if kind != EmailTerms && kind != EmailApproved {
		return nil, fmt.Errorf("wholesale: only the terms email and the approval email are sent again; %q is not one of them", kind)
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
	sends, err := p.emailSendsOf(sys, trimmed(app["id"]))
	if err != nil {
		return nil, err
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
	}
	template := settings.TemplateFor(kind)
	if template == "" {
		return nil, fmt.Errorf("wholesale: %s on this store", noTemplateReason(kind))
	}
	return p.sendEmail(ctx, sys, app, kind, highestAttempt(sends, kind)+1, template, settings.SenderIdentityID, userRef(requester))
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

// sendEmail is ONE ATTEMPT OF ONE EMAIL: answered as recorded when the attempt
// already went out, was skipped or is pending; otherwise sent through the row
// lane as the application's merchant and recorded.
func (p *Provider) sendEmail(ctx, sys context.Context, app map[string]any, kind string, attempt int,
	templateID, senderID, requestedBy string) ([]memorynodes.MemoryNode, error) {
	applicationID := trimmed(app["id"])
	rowID := EmailSendRowID(kind, applicationID, attempt)
	requestID := LaneRequestID(kind, applicationID, attempt)
	prior, err := p.emailSendRow(sys, applicationID, rowID)
	if err != nil {
		return nil, err
	}
	if prior != nil && settledSend(trimmed(prior["state"])) {
		return answerSend(prior)
	}
	merchant, err := p.asMerchant(ctx, app)
	if err != nil {
		return nil, err
	}
	state, reason, sentAt := SendFailed, ReasonNoOrganization, ""
	if account := bareID(trimmed(app["accountId"])); account != "" {
		if p.lane == nil {
			return nil, fmt.Errorf("wholesale: the pack has no row lane, so no email can be sent")
		}
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
		state, reason = laneOutcome(reply, sendErr)
		if state == SendSent {
			sentAt = p.clock().UTC().Format(time.RFC3339Nano)
		}
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
		"state":         state,
		"reason":        clipRunes(reason, maxEmailReason),
		"sentAt":        sentAt,
		"requestedBy":   requestedBy,
	}
	// FENCED ON THE VERSION READ ABOVE (absent, or the failed attempt before):
	// of two nodes sending at once, the one whose write loses answers the row
	// the other wrote, never writes its own outcome over a sent one.
	var version time.Time
	if prior != nil {
		version = rowTime(prior["createdAt"])
	}
	fenced := memql.ContextWithRowVersionFence(merchant, emailSendConcept, rowID, version)
	if err := p.write(fenced, "putEmailSendRow", row); err != nil {
		if !errors.Is(err, memql.ErrRowVersionChanged) {
			return nil, err
		}
		current, err := p.emailSendRow(sys, applicationID, rowID)
		if err != nil {
			return nil, err
		}
		if current == nil {
			return nil, fmt.Errorf("wholesale: the email's record changed while it was sent and cannot be read")
		}
		return answerSend(current)
	}
	written := map[string]any{"id": emailSendConcept + ":" + rowID}
	for k, v := range row {
		if k != "emailSendId" {
			written[k] = v
		}
	}
	return answerSend(written)
}

// settledSend reports whether an attempt in state is final: it went out, it
// will not, or the lane holds it to send later. Only a failed attempt is tried
// again.
func settledSend(state string) bool {
	return state == SendSent || state == SendSkipped || state == SendPending
}

// laneOutcome reads what the row lane answered as an emailSend state and its
// sentence. A hold -- configuration the store can fix, such as an unpublished
// template -- and a skip are skipped with the lane's sentence; a deferral, or
// an attempt whose outcome the provider has not reported yet, is pending; any
// other error is failed with the lane's words.
func laneOutcome(reply map[string]any, err error) (string, string) {
	if err != nil {
		if reason, held := campaigns.HeldReason(err); held {
			return SendSkipped, reason
		}
		return SendFailed, laneWords(err)
	}
	switch {
	case reply["sent"] == true:
		return SendSent, ""
	case reply["skipped"] == true:
		return SendSkipped, trimmed(reply["reason"])
	case reply["deferred"] == true, reply["uncertain"] == true:
		return SendPending, trimmed(reply["reason"])
	}
	return SendFailed, reasonUnreadableAnswer
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
		"state":         SendSkipped,
		"reason":        reason,
		"recorded":      false,
	})
}

// answerSend answers an emailSend row as the send's reply.
func answerSend(row map[string]any) ([]memorynodes.MemoryNode, error) {
	attempt, _ := intArg(row["attempt"])
	return confirm("wholesale:email", map[string]any{
		"applicationId": trimmed(row["applicationId"]),
		"kind":          trimmed(row["kind"]),
		"attempt":       attempt,
		"requestId":     trimmed(row["requestId"]),
		"templateId":    trimmed(row["templateId"]),
		"state":         trimmed(row["state"]),
		"reason":        asString(row["reason"]),
		"sentAt":        timeText(row["sentAt"]),
		"requestedBy":   trimmed(row["requestedBy"]),
		"recorded":      true,
	})
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
// first, each with the lane's own record of its delivery.
//
// THE DELIVERY IS READ AFTER THE GATE: wholesaleGet's caller was admitted by
// the organization gate and read the application under their own actor before
// this runs, and the status read names only the application's organization.
// A status that cannot be read leaves each delivery null and says so in one
// line for the operator, naming the application and nothing of the applicant.
func (p *Provider) emailsView(ctx context.Context, app map[string]any, sends []map[string]any) []map[string]any {
	deliveries := map[string]map[string]any{}
	account := bareID(trimmed(app["accountId"]))
	var ids []string
	for _, row := range sends {
		if id := trimmed(row["requestId"]); id != "" && len(ids) < maxLaneStatusRequestIDs {
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 && account != "" && p.lane != nil {
		status, err := p.lane.RowMessageStatus(ctx, account, ids)
		if err != nil {
			p.log().Warn("wholesale: the row lane's record of an application's emails could not be read",
				"component", "wholesale", "applicationId", trimmed(app["id"]), "error", err)
		}
		for _, s := range status {
			if trimmed(s["status"]) != "" {
				deliveries[trimmed(s["requestId"])] = map[string]any{
					"status":         trimmed(s["status"]),
					"skipReason":     asString(s["skipReason"]),
					"providerStatus": trimmed(s["providerStatus"]),
					"sentAt":         timeText(s["sentAt"]),
				}
			}
		}
	}
	out := make([]map[string]any, 0, len(sends))
	for _, row := range sends {
		attempt, _ := intArg(row["attempt"])
		var delivery any
		if d, ok := deliveries[trimmed(row["requestId"])]; ok {
			delivery = d
		}
		out = append(out, map[string]any{
			"kind":        trimmed(row["kind"]),
			"attempt":     attempt,
			"state":       trimmed(row["state"]),
			"reason":      asString(row["reason"]),
			"sentAt":      timeText(row["sentAt"]),
			"requestedBy": trimmed(row["requestedBy"]),
			"delivery":    delivery,
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
