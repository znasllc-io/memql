package campaigns

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
)

// send_row.go -- THE TRANSACTIONAL ROW LANE (U7, E1): one message about one
// person's own request, to the single person a pack's row names.
//
// # One person, never an address
//
// No argument names a recipient or carries the message's fields. A pack
// registers, from Go, a resolver for its row concept (RegisterRowAddress); a DSL
// file cannot register one. The lane asks the resolver only when the message is
// about to be handed to the provider, after the receipt, the template and the
// rate limit have had their say, and the resolver answers the address, the row's
// organization and the {{fields.<key>}} values for that kind -- or withdraws the
// message with a reason. An answer for another organization refuses the send.
//
// # No unsubscribe, and the one way past the hold
//
// The message carries no List-Unsubscribe pair and no footer
// (RenderOptions.Transactional): it is about the person's own request, not mail
// from a list. sendToRecipient holds every transactional template, and a row
// send (singleSend.rowSend, which only Go code in this package sets) is the only
// thing that passes that hold. A row send whose template is not a published
// transactional template is held the other way. A bounce, a complaint or an
// operator's block of the address stops it; an unsubscribe does not
// (BlockingSuppression).
//
// # A skip says why in a sentence
//
// A row send that is skipped answers its reason as the ledger keeps it -- the
// suppression's code, or the row's own withdrawal sentence -- and, beside it,
// skipSentence: the words a person reads (rowSkipSentence, stats.go). The
// status read answers both too. A screen shows the sentence, never the code.
//
// # A refusal and a failure say why in a sentence, beside a code
//
// A row send that is refused, or that fails, answers its caller TWO THINGS AND
// NOTHING ELSE (rowSendFailure, read with RowSendFailure): a short code that
// does not change, for a caller to decide on, and a sentence a person reads,
// written here beside it (rowSendSentences). The answer's whole text is that
// sentence. The lane's own error -- which names the function that refused, may
// quote the engine's message, and from a provider may quote the address the
// message went to -- is NOT answered: it stays with the lane, in its log, and
// for a message the provider did not take in the ledger row of the delivery
// (lastError). A provider's own words are never logged. A hold is answered as
// it always was: its text is its sentence already, written for the
// organization's people (HeldReason). So is an uncertain hand-over
// (UncertainSend), with the lane's sentence in place of the transport's text.
//
// # The ledger and the replay
//
// The ledger row is the order route's shape plus rowConcept, rowId and rowKind,
// and never the fields. A rate-limited row send is deferred like any single
// send, and its replay rebuilds rowSend from those three fields and asks the
// resolver again, so the message carries fields minted when it is finally sent.

// RowMessageRequest is what the lane hands a row's resolver: which row, which
// organization the trusted job named, which message, and its request id.
type RowMessageRequest struct{ Concept, RowID, AccountID, Kind, RequestID string }

// RowMessage is a resolver's answer: the one address the row names, the row's
// organization, and the template's {{fields.<key>}} values for this kind.
type RowMessage struct {
	Email, AccountID string
	Fields           map[string]string // the template's {{fields.<key>}} values
}

// RowWithdrawn is a resolver's answer that the message is no longer due. The
// lane ledgers it skipped with Reason and sends nothing; a value or a pointer is
// a withdrawal.
//
// Reason is shown: in the send's reply, on the ledger row and in the status
// read, where it is also the skip's sentence for a person, word for word
// (rowSkipSentence). So it is a whole sentence about the row's state ("The
// application was withdrawn.") and must never carry an address or anything of
// the message's fields.
type RowWithdrawn struct{ Reason string }

func (w RowWithdrawn) Error() string {
	return "campaigns.sendRowMessage: the message is no longer due: " + w.Reason
}

// RowAddress is one concept's resolver.
//
// Resolve answers the row's OWN organization as RowMessage.AccountID, read from
// the row, and never echoes the AccountID it was asked with: the lane refuses
// an answer for another organization, and that check is what keeps one
// organization's caller from sending about another organization's row. Its
// error is never shown or logged, so it may say what it likes; a withdrawal is
// a RowWithdrawn.
type RowAddress struct {
	Concept string
	Resolve func(ctx context.Context, req RowMessageRequest) (RowMessage, error)
}

// rowAddresses is the process's registry. Registration is Go only, at provider
// build, so a cluster where the pack is disabled registers nothing.
var rowAddresses = struct {
	sync.RWMutex
	byConcept map[string]RowAddress
}{byConcept: map[string]RowAddress{}}

// RegisterRowAddress registers a concept's resolver for the row lane. A DSL file
// cannot register one. A second registration of a concept replaces the first.
// A registration with no concept or no resolver is a wiring mistake and panics.
func RegisterRowAddress(a RowAddress) {
	a.Concept = strings.TrimSpace(a.Concept)
	if a.Concept == "" || a.Resolve == nil {
		panic("campaigns.RegisterRowAddress: a row address needs a concept and a resolver")
	}
	rowAddresses.Lock()
	defer rowAddresses.Unlock()
	rowAddresses.byConcept[a.Concept] = a
}

// rowAddressFor is the resolver registered for concept.
func rowAddressFor(concept string) (RowAddress, bool) {
	rowAddresses.RLock()
	defer rowAddresses.RUnlock()
	a, ok := rowAddresses.byConcept[strings.TrimSpace(concept)]
	return a, ok
}

// rowRecipientID is the stable id of the recipient one row names, for this send
// alone: a bare slug derived as orderRecipientID derives its id. It names no row.
func rowRecipientID(concept, rowID string) string {
	sum := sha256.Sum256([]byte(concept + "\x00" + rowID))
	return "row-" + hex.EncodeToString(sum[:])[:24]
}

// maxRowStatusRequests bounds one status read: the query's page.
const maxRowStatusRequests = 100

// handleSendRowMessage answers campaignSendRowMessage.
func (w *Worker) handleSendRowMessage(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return nil, errors.New("campaigns.sendRowMessage: a message about one person's own request is sent by a trusted job, never by a client")
	}
	concept := strings.TrimSpace(argString(args, "rowConcept"))
	rowID := strings.TrimSpace(argString(args, "rowId"))
	account := memql.BareShortId(strings.TrimSpace(argString(args, "accountId")))
	kind := strings.TrimSpace(argString(args, "kind"))
	templateID := strings.TrimSpace(argString(args, "templateId"))
	requestID := strings.TrimSpace(argString(args, "requestId"))
	if concept == "" || rowID == "" || account == "" || kind == "" || templateID == "" || requestID == "" {
		return nil, errors.New("campaigns.sendRowMessage: rowConcept, rowId, accountId, kind, templateId and requestId are required")
	}
	if _, ok := rowAddressFor(concept); !ok {
		return nil, fmt.Errorf("campaigns.sendRowMessage: %q is not a concept registered for the row lane", concept)
	}
	// Only the arguments the send reads go on: nothing beside the row can name an
	// address.
	send := map[string]any{
		"templateId":  templateID,
		"recipientId": rowRecipientID(concept, rowID),
		"requestId":   requestID,
	}
	if id := strings.TrimSpace(argString(args, "senderIdentityId")); id != "" {
		send["senderIdentityId"] = id
	}
	nodes, err := w.sendToRecipientOnce(ctx, send, singleSend{deferWhenLimited: true, rowSend: &rowSend{Concept: concept, RowID: rowID, Kind: kind, AccountID: account}})
	if err != nil {
		return nil, w.rowSendAnswer(err, rowRecipientID(concept, rowID), requestID)
	}
	return nodes, nil
}

// The codes a row send's refusal or failure carries (RowSendFailure), each with
// its sentence in rowSendSentences. A CODE DOES NOT CHANGE; its sentence may be
// reworded. RowSendNoAuthority and the sender's own three are also what
// campaignRowSender answers as its code, beside RowSenderHeld.
const (
	// RowSendNoAuthority: the person the send is made as holds no write
	// permission in the organization (requireSendAuthority).
	RowSendNoAuthority = "no_authority"
	// RowSendTemplateUnreadable: the template no longer exists, or that person
	// cannot read it.
	RowSendTemplateUnreadable = "template_unreadable"
	// RowSendTemplateOtherOrg: the template is another organization's.
	RowSendTemplateOtherOrg = "template_other_organization"
	// RowSendTemplateInvalid: the template cannot be rendered as it is written.
	RowSendTemplateInvalid = "template_invalid"
	// RowSendSenderNoneChosen: no sender is named, for an organization that is
	// not the installation's.
	RowSendSenderNoneChosen = "sender_none_chosen"
	// RowSendSenderOtherOrg: the sender named is another organization's.
	RowSendSenderOtherOrg = "sender_other_organization"
	// RowSendSenderUnread: the sender could not be read just now.
	RowSendSenderUnread = "sender_unread"
	// RowSendRowUnanswered: the row's resolver did not answer.
	RowSendRowUnanswered = "row_unanswered"
	// RowSendRowOtherOrg: the resolver answered another organization than the
	// call named.
	RowSendRowOtherOrg = "row_other_organization"
	// RowSendNoAddress: the row names no address a message can go to.
	RowSendNoAddress = "no_address"
	// RowSendSuppressionUnread: the suppression list could not be read, so the
	// send was refused rather than made.
	RowSendSuppressionUnread = "suppression_unread"
	// RowSendNoProvider: no email sender is registered on the node.
	RowSendNoProvider = "no_provider"
	// RowSendProviderRefused: the provider did not accept the message.
	RowSendProviderRefused = "provider_refused"
	// RowSendProviderBusy: the provider is limiting what it accepts.
	RowSendProviderBusy = "provider_busy"
	// RowSendNotNow: something the send needed -- a read, a lock, the record that
	// an attempt began -- failed before anything was attempted.
	RowSendNotNow = "not_now"
	// RowSendFailed: anything with no kind of its own.
	RowSendFailed = "failed"

	// RowSenderHeld is campaignRowSender's code for a chosen sender the send
	// would be HELD for (turned off, or no longer there): its reason is the
	// hold's own sentence. A send answers that as a hold, never as a failure.
	RowSenderHeld = "sender_held"
)

// rowSendSentences is each code as a person reads it: what happened, and what
// can be done next or who can do it. WRITTEN FOR A PERSON: no function's name,
// none of the lane's or a provider's own error text, no address, and no promise
// of something nobody will do -- the lane tries a refused send again for no
// caller, so no sentence says it will. The lane does not know who its reader
// is, so where something must be chosen or corrected it says what, and not by
// whom: a pack that knows says so in a sentence of its own.
var rowSendSentences = map[string]string{
	RowSendNoAuthority: "The person on whose behalf this email is sent does not have write permission in this organization, " +
		"so it was not sent. An administrator of the organization can give them that permission.",
	RowSendTemplateUnreadable: "The template chosen for this email no longer exists, or the person on whose behalf it is sent " +
		"cannot read it, so it was not sent. Once another template is chosen, or that person can read this one, it can be sent again.",
	RowSendTemplateOtherOrg: "The template chosen for this email belongs to another organization, so it was not sent. " +
		"Once one of this organization's templates is chosen, it can be sent again.",
	RowSendTemplateInvalid: "The template chosen for this email cannot be sent as it is written, so the email was not sent. " +
		"Once the template is corrected, it can be sent again.",
	RowSendSenderNoneChosen: "No sender is chosen for this organization, so this email was not sent. " +
		"Once a sender is chosen, it can be sent again.",
	RowSendSenderOtherOrg: "The sender chosen for this email belongs to another organization, so it was not sent. " +
		"Once one of this organization's senders is chosen, it can be sent again.",
	RowSendSenderUnread: "The sender of this email could not be read just now, so it was not sent. It can be sent again.",
	RowSendRowUnanswered: "What this email needs from the record it is about could not be read, so it was not sent. " +
		"It can be sent again; if it is still not sent, the cluster's operators can say why.",
	RowSendRowOtherOrg: "The record this email is about belongs to another organization, so it was not sent. " +
		"The cluster's operators can say why.",
	RowSendNoAddress: "The record this email is about names no email address a message can go to, so it was not sent.",
	RowSendSuppressionUnread: "The email service could not check its do-not-send list just now, so this email was not sent. " +
		"It can be sent again.",
	RowSendNoProvider: "No email provider is connected to this installation, so this email was not sent. " +
		"The cluster's operators connect one.",
	RowSendProviderRefused: "The email provider did not accept this email, so it was not sent. " +
		"The cluster's operators can read the provider's answer in the email service's record of it.",
	RowSendProviderBusy: "The email provider is limiting how much email it accepts just now, so this email was not sent. " +
		"It can be sent again later.",
	RowSendNotNow: "The email service could not finish this just now, and nothing was sent. It can be sent again.",
	RowSendFailed: "The email service could not send this email. " +
		"It can be sent again; if it is still not sent, the cluster's operators can say why.",
}

// rowSendUncertainSentence is the text of an uncertain hand-over as a row send
// answers it, in place of the transport's own.
const rowSendUncertainSentence = "The email provider did not say whether it accepted this email, so it may have been sent."

// RowSendSentence is a row send's refusal or failure code as a person reads it:
// the lane's sentence for each code it answers, and for any other the general
// one. It never answers the code.
func RowSendSentence(code string) string {
	if sentence, known := rowSendSentences[code]; known {
		return sentence
	}
	return rowSendSentences[RowSendFailed]
}

// RowSendCodes is every code a row send's refusal or failure carries, in order.
func RowSendCodes() []string {
	codes := make([]string, 0, len(rowSendSentences))
	for code := range rowSendSentences {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

// rowSendFailure is a row send that was refused or that failed, AS ITS CALLER IS
// ANSWERED: a code and a sentence. Its text is the sentence and it wraps
// nothing, so nothing of the lane's own error travels with it.
type rowSendFailure struct{ code, sentence string }

func (f rowSendFailure) Error() string { return f.sentence }

// RowSendFailure reports whether err is a row send the lane refused or failed
// (rowSendFailure), and answers its code and the sentence a person reads. A
// pack that records what the row lane answered reads a failure through here --
// the type is this package's own -- decides on the code, never the sentence,
// and shows its people the sentence. A hold (HeldReason), an uncertain
// hand-over (UncertainSend) and a busy receipt lock are not failures.
func RowSendFailure(err error) (code, sentence string, failed bool) {
	var f rowSendFailure
	if errors.As(err, &f) {
		return f.code, f.sentence, true
	}
	return "", "", false
}

// rowRefusal marks one of the lane's own errors with the code its caller is
// answered. THE ERROR IS UNCHANGED BENEATH IT -- its text is what the lane's log
// keeps and what the replay of a deferred row send writes on the ledger -- and
// it never leaves the lane: rowSendAnswer answers the code's sentence in its
// place.
type rowRefusal struct {
	error
	code string
}

func (r rowRefusal) Unwrap() error { return r.error }

// rowRefused is err, one of the lane's own, marked with its code.
func rowRefused(code string, err error) error { return rowRefusal{error: err, code: code} }

// rowSendCode is the code of a row send's error. One the row lane marked
// carries its own (rowRefused); the rest are read by what they are, never by
// their text: the send's authority, the resolver, the template's read, the
// node's provider, the provider's own refusal, and a failure that decided
// nothing about the message. Anything else is RowSendFailed.
func rowSendCode(err error) string {
	var (
		marked     rowRefusal
		unreadable templateUnreadable
		provider   *email.SendError
	)
	switch {
	case errors.As(err, &marked):
		return marked.code
	case errors.Is(err, errNoSendAuthority):
		return RowSendNoAuthority
	case errors.Is(err, errResolverDidNotAnswer):
		return RowSendRowUnanswered
	case errors.As(err, &unreadable):
		return RowSendTemplateUnreadable
	case errors.Is(err, errNoEmailSender):
		return RowSendNoProvider
	case errors.As(err, &provider):
		if provider.Throttled {
			return RowSendProviderBusy
		}
		return RowSendProviderRefused
	case errors.Is(err, errPriorSendNotAccepted):
		return RowSendProviderRefused
	case isUndecided(err):
		return RowSendNotNow
	}
	return RowSendFailed
}

// rowSendAnswer is a row send's error AS ITS CALLER IS ANSWERED.
//
// A hold is answered as it is: its text is its sentence, written for the
// organization's people. So is a busy receipt lock, which says nothing of the
// message and which a caller reads by what it is (memql.IsWriteGateBusy).
//
// An uncertain hand-over stays one (UncertainSend), with the lane's sentence
// in place of the transport's text: that text may quote the address, and it is
// on the ledger row the hand-over wrote before it was known to be uncertain.
//
// Everything else is answered as its code and the code's sentence
// (rowSendFailure), and the lane's own error goes to the lane's log -- except a
// provider's own words, which may quote the address: of those the log says only
// what kind of answer it was, and the ledger row of the delivery keeps the
// words.
func (w *Worker) rowSendAnswer(err error, recipientID, requestID string) error {
	if _, held := HeldReason(err); held || memql.IsWriteGateBusy(err) {
		return err
	}
	var uncertain uncertainSend
	if errors.As(err, &uncertain) {
		w.logger.Warn("campaigns.sendRowMessage: a row message's hand-over had no answer", "recipient", recipientID, "request", requestID)
		return uncertainSend{error: errors.New(rowSendUncertainSentence), recorded: uncertain.recorded}
	}
	code := rowSendCode(err)
	attrs := []any{"code", code, "recipient", recipientID, "request", requestID}
	var provider *email.SendError
	if errors.As(err, &provider) {
		attrs = append(attrs, "status", provider.StatusCode, "permanent", provider.Permanent, "throttled", provider.Throttled)
	} else {
		attrs = append(attrs, "error", err.Error())
	}
	w.logger.Warn("campaigns.sendRowMessage: a row message was not sent", attrs...)
	return rowSendFailure{code: code, sentence: RowSendSentence(code)}
}

// rowTemplateHeld is the hold of a row send whose template is not a published
// transactional template. It names the template as templateHeld does.
func rowTemplateHeld(t Template) error {
	return sendHeld{reason: templateNamed(t) + " is not a published transactional template."}
}

// templateNamed is how a row lane hold names a template at the start of a
// sentence: by its name, else its subject, quoted, else "This template".
func templateNamed(t Template) string {
	name := strings.TrimSpace(t.Name)
	if name == "" {
		name = strings.TrimSpace(t.Subject)
	}
	if name == "" {
		return "This template"
	}
	return "“" + name + "”"
}

// linkHeld is why a transactional template that still carries an engine link
// is not sent, or "" when it carries none: the row lane holds it, and the
// author's test send of it says the same. It names the template and says each
// kind of link it carries, in the save-time check's words.
func linkHeld(t Template) string {
	faults := transactionalLinkFaults(t)
	if len(faults) == 0 {
		return ""
	}
	return templateNamed(t) + " cannot be sent. " + strings.Join(faults, " ")
}

// sendRow is sendToRecipient for a row send, from the point its template is
// known to be a published transactional template. The order is the decision's:
// the receipt has answered (sendToRecipientHeld), the template is held or not
// (sendToRecipient), then the rate limit, and only then the resolver, the
// suppression list, the render and the hand-over. The resolved address never
// reaches a log line, an error or a reply; the ledger keeps it, as it keeps an
// order message's.
func (w *Worker) sendRow(ctx context.Context, args map[string]any, receipt *singleSendReceipt, opts singleSend, tmpl Template, ownerUserID string) ([]memorynodes.MemoryNode, error) {
	row := *opts.rowSend
	address, ok := rowAddressFor(row.Concept)
	if !ok {
		return nil, fmt.Errorf("campaigns.sendRowMessage: %q is not a concept registered for the row lane", row.Concept)
	}
	account := bare(row.AccountID)
	if held := linkHeld(tmpl); held != "" {
		return nil, sendHeld{reason: held}
	}
	if account == "" || !sameOrganization(tmpl.AccountID, account) {
		return nil, rowRefused(RowSendTemplateOtherOrg, errors.New("campaigns.sendRowMessage: the template must belong to the row's organization"))
	}
	if err := w.requireSendAuthority(ctx, account); err != nil {
		return nil, err
	}
	templateID := memql.BareShortId(strings.TrimSpace(argString(args, "templateId")))
	senderIdentityID := memql.BareShortId(strings.TrimSpace(argString(args, "senderIdentityId")))
	recipient := Recipient{ID: memql.BareShortId(argString(args, "recipientId")), AccountID: account}
	campaign := Campaign{
		AccountID:        account,
		OwnerUserID:      ownerUserID,
		Name:             clipRunes(strings.TrimSpace(tmpl.Subject), maxCampaignNameRunes),
		TemplateID:       templateID,
		SenderIdentityID: senderIdentityID,
		Status:           "sending",
	}
	identity, refusal := w.resolveSendIdentityFor(ctx, campaign, false)
	if refusal.refused() {
		if refusal.Held != "" {
			return nil, sendHeld{reason: refusal.Held}
		}
		// WHICH REFUSAL IT IS, as campaignRowSender tells them apart: one that could
		// not be read (and decides nothing), none named, or another organization's.
		err := fmt.Errorf("campaigns.sendRowMessage: %s", refusal.Reason)
		switch {
		case !refusal.Terminal:
			return nil, undecided(rowRefused(RowSendSenderUnread, err))
		case campaign.SenderIdentityID == "":
			return nil, rowRefused(RowSendSenderNoneChosen, err)
		}
		return nil, rowRefused(RowSendSenderOtherOrg, err)
	}
	// recipient is read when an outcome is recorded: before the resolver has
	// answered it carries no address, after it the one the resolver answered.
	recordOutcome := func(outcome Delivery) error {
		if receipt != nil && receipt.hold != nil {
			if err := receipt.hold.Verify(ctx); err != nil {
				return err
			}
		}
		return detachedWrite(ctx, func(write context.Context) error {
			return w.recordLoneDelivery(write, args, campaign, recipient, "", opts.ledgerRow(outcome))
		})
	}

	// THE RATE LIMIT BEFORE THE RESOLVER: a deferred message mints nothing now.
	// Its replay asks the resolver again, when it is finally sent.
	if !w.allowSend() {
		if opts.deferWhenLimited && receipt != nil {
			if err := recordOutcome(Delivery{Status: "pending", NextAttemptAt: w.nowUTC().Add(time.Minute)}); err != nil {
				return nil, undecided(err)
			}
			return resultNode("campaignRecipientSend", map[string]any{"sent": false, "skipped": false, "deferred": true, "reason": "The send-rate limit is reached; this email will be retried shortly.", "recipientId": recipient.ID, "emailRuleId": ""})
		}
		return nil, errCampaignRateLimited
	}

	resolved, err := address.Resolve(ctx, RowMessageRequest{Concept: row.Concept, RowID: row.RowID, AccountID: account, Kind: row.Kind, RequestID: argString(args, "requestId")})
	if withdrawn, ok := asWithdrawal(err); ok {
		if err := recordOutcome(Delivery{Status: "skipped", SkipReason: withdrawn.Reason}); err != nil {
			w.logger.Warn("campaigns.sendRowMessage: a withdrawn message could not be ledgered", "recipient", recipient.ID, "error", err)
		}
		return resultNode("campaignRecipientSend", map[string]any{"sent": false, "skipped": true, "reason": withdrawn.Reason,
			"skipSentence": rowSkipSentence(withdrawn.Reason), "recipientId": recipient.ID, "emailRuleId": ""})
	}
	if err != nil {
		// The resolver's error is a pack's words, and they may name the row and its
		// address. None of it reaches the caller or a log line: the reply is the
		// lane's own sentence, and the log says only what kind of failure it was.
		w.logger.Warn("campaigns.sendRowMessage: the row's resolver did not answer", "recipient", recipient.ID, "failure", resolverFailure(err))
		return nil, errResolverDidNotAnswer
	}
	if bare(resolved.AccountID) == "" || !sameOrganization(resolved.AccountID, account) {
		return nil, rowRefused(RowSendRowOtherOrg, errors.New("campaigns.sendRowMessage: the row belongs to another organization, so nothing was sent"))
	}
	digest := EmailDigest(resolved.Email)
	if digest == "" {
		return nil, rowRefused(RowSendNoAddress, errors.New("campaigns.sendRowMessage: the row names no usable address"))
	}
	recipient.Email = strings.TrimSpace(resolved.Email)
	recipient.Fields = map[string]any{}
	for key, value := range resolved.Fields {
		if key = strings.TrimSpace(key); key != "" {
			recipient.Fields[key] = value
		}
	}

	// A bounce, a complaint and an operator's block stop the message; an
	// unsubscribe from a list does not (X9).
	if sup, blocked, err := w.store.BlockingSuppression(w.systemActorContext(ctx), account, digest); err != nil {
		return nil, undecided(rowRefused(RowSendSuppressionUnread, fmt.Errorf("campaigns.sendRowMessage: the suppression list could not be consulted, "+
			"so this send is refused rather than made: %w", err)))
	} else if blocked {
		if err := recordOutcome(Delivery{Status: "skipped", SkipReason: sup.Reason}); err != nil {
			w.logger.Warn("campaigns.sendRowMessage: a suppressed message could not be ledgered", "recipient", recipient.ID, "error", err)
		}
		return resultNode("campaignRecipientSend", map[string]any{"sent": false, "skipped": true, "reason": sup.Reason,
			"skipSentence": rowSkipSentence(sup.Reason), "recipientId": recipient.ID, "emailRuleId": ""})
	}

	accountName, err := w.emailOrganizationName(ctx, account)
	if err != nil {
		w.logger.Debug("campaigns.sendRowMessage: could not resolve the account name", "error", err)
	}
	msg, err := renderMessage(campaign, tmpl, recipient, RenderOptions{
		ReplyTo:       replyToFor(campaign, identity),
		AccountName:   accountName,
		SenderName:    identity.SendAs.FromName,
		Transactional: true,
	})
	if err != nil {
		return nil, rowRefused(RowSendTemplateInvalid, fmt.Errorf("campaigns.sendRowMessage: %w", err))
	}
	return w.handOver(ctx, args, receipt, opts, campaign, recipient, "", identity, msg, recordOutcome)
}

// errResolverDidNotAnswer is what a send says when the row's resolver failed:
// the lane's own sentence, never the resolver's.
var errResolverDidNotAnswer = errors.New("campaigns.sendRowMessage: the row's resolver did not answer, so nothing was sent")

// asWithdrawal reads a resolver's withdrawal, answered as a value or a pointer.
func asWithdrawal(err error) (RowWithdrawn, bool) {
	var value RowWithdrawn
	if errors.As(err, &value) {
		return value, true
	}
	var pointer *RowWithdrawn
	if errors.As(err, &pointer) && pointer != nil {
		return *pointer, true
	}
	return RowWithdrawn{}, false
}

// resolverFailure classifies a resolver's error for a log line without its text.
func resolverFailure(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "failed"
}

// handleRowSender answers campaignRowSender: the sender a row send for the
// organization would go out as, resolved exactly as sendRow resolves it
// (resolveSendIdentityFor, with no installation fallback) -- {ready, fromName,
// address, reason, code, readFailed, noneChosen}. Ready answers the identity's
// name and address; otherwise fromName and address are empty and reason is a
// sentence for a person: the hold's own sentence, or one of rowSenderRefusals'
// words, never the campaign worker's refusal text. It sends nothing and checks
// no transport.
//
// WHAT A CALLER DECIDES ON IS A FIELD, NEVER THE SENTENCE: code says which
// answer it is, in the codes a send refused for the same thing carries
// (RowSendNoAuthority, RowSendSenderNoneChosen, RowSendSenderOtherOrg,
// RowSendSenderUnread) and RowSenderHeld for a sender the send would be held
// for; it is empty when the sender is ready. readFailed and noneChosen say two
// of them as fields of their own: the sender could not be read just now
// (nothing is known of it, and the same read made again may answer), and the
// organization has no sender because none is chosen. The sentences may be
// reworded; the code and the fields may not.
//
// THE SEND'S OWN AUTHORITY: the caller must hold what a row send requires of
// it (requireSendAuthority, update data in the organization). A caller without
// it is answered RowSendNoAuthority, exactly as for an organization that does
// not exist -- where nobody holds it either -- so the answer says nothing of the
// organization's senders.
func (w *Worker) handleRowSender(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return nil, errors.New("campaigns.rowSender: read by a trusted job, never by a client")
	}
	account := memql.BareShortId(strings.TrimSpace(argString(args, "accountId")))
	if account == "" {
		return nil, errors.New("campaigns.rowSender: accountId is required: a sender is one organization's")
	}
	// THE CODE SAYS WHICH REFUSAL IT IS; two of them are also fields of their own.
	refused := func(reason, code string) ([]memorynodes.MemoryNode, error) {
		return resultNode("campaignRowSender", map[string]any{"accountId": account, "ready": false, "fromName": "", "address": "",
			"reason": reason, "code": code, "readFailed": code == RowSendSenderUnread, "noneChosen": code == RowSendSenderNoneChosen})
	}
	if err := w.requireSendAuthority(ctx, account); err != nil {
		return refused(rowSenderUnreadable, RowSendNoAuthority)
	}
	campaign := Campaign{AccountID: account, SenderIdentityID: memql.BareShortId(strings.TrimSpace(argString(args, "senderIdentityId")))}
	identity, refusal := w.resolveSendIdentityFor(memql.ContextWithFreshRead(w.systemActorContext(ctx)), campaign, false)
	switch {
	case !refusal.refused():
		return resultNode("campaignRowSender", map[string]any{"accountId": account, "ready": true,
			"fromName": identity.SendAs.FromName, "address": identity.SendAs.Address, "reason": "", "code": "",
			"readFailed": false, "noneChosen": false})
	case refusal.Held != "":
		return refused(refusal.Held, RowSenderHeld)
	case !refusal.Terminal:
		return refused(rowSenderReadFailed, RowSendSenderUnread)
	case campaign.SenderIdentityID == "":
		return refused(rowSenderNoneChosen, RowSendSenderNoneChosen)
	}
	return refused(rowSenderOtherOrganization, RowSendSenderOtherOrg)
}

// The row sender's sentences for a person, in place of the campaign worker's
// refusal text.
const (
	rowSenderUnreadable        = "No sender can be read for this organization."
	rowSenderReadFailed        = "The sender could not be read just now."
	rowSenderNoneChosen        = "No sender is chosen for this organization."
	rowSenderOtherOrganization = "The chosen sender belongs to another organization."
)

// handleRowMessageStatus answers campaignRowMessageStatus: one row per request
// id, {requestId, status, skipReason, skipSentence, providerStatus, sentAt},
// read under the system actor for the one organization the call names, and
// nothing else of the delivery. skipSentence is a skipped message's reason as a
// person reads it (rowSkipSentence), empty for a message that was not skipped.
// Another organization's request id answers nothing, as an unknown one. A
// request id with no message recorded under it answers an empty status.
func (w *Worker) handleRowMessageStatus(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return nil, errors.New("campaigns.rowMessageStatus: read by a trusted job, never by a client")
	}
	account := memql.BareShortId(strings.TrimSpace(argString(args, "accountId")))
	if account == "" {
		return nil, errors.New("campaigns.rowMessageStatus: accountId is required: the read answers one organization's messages")
	}
	raw, _ := args["requestIds"].([]any)
	if len(raw) == 0 || len(raw) > maxRowStatusRequests {
		return nil, fmt.Errorf("campaigns.rowMessageStatus: between 1 and %d request ids are required", maxRowStatusRequests)
	}
	ids := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, v := range raw {
		id, _ := v.(string)
		id = strings.TrimSpace(id)
		if !singleSendRequestID.MatchString(id) {
			return nil, errors.New("campaigns.rowMessageStatus: every request id must be a stable request identifier")
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	system := auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(w.systemActorContext(ctx)))
	rows, err := w.store.rows(system, call("query", "deliveriesByRequestIds", arg{"requestIds", ids},
		arg{"accountIds", []string{account, "v1:accounts:account:" + account}}))
	if err != nil {
		return nil, undecided(fmt.Errorf("campaigns.rowMessageStatus: %w", err))
	}
	// The newest row of each request wins; rows come oldest first.
	latest := map[string]map[string]any{}
	for _, row := range rows {
		latest[str(row, "requestId")] = row
	}
	// One row per request id, in the order asked, keyed by the request id (the
	// node's id is the request id; it names no delivery).
	answers := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		row := latest[id]
		status, sentAt, err := w.rowMessageOutcome(ctx, row)
		if err != nil {
			return nil, undecided(fmt.Errorf("campaigns.rowMessageStatus: %w", err))
		}
		// WHY IT WAS SKIPPED, IN A SENTENCE, and nothing for a message that was not
		// skipped. The query answers row lane deliveries only (rowConcept set), so a
		// reason that is not one of the lane's codes is the row's own withdrawal
		// sentence.
		sentence := ""
		if str(row, "status") == "skipped" {
			sentence = rowSkipSentence(str(row, "skipReason"))
		}
		answers = append(answers, map[string]any{
			"id": id, "requestId": id, "status": status, "skipReason": str(row, "skipReason"),
			"skipSentence": sentence, "providerStatus": str(row, "providerStatus"), "sentAt": sentAt,
		})
	}
	return resultRows("campaignRowMessageStatus", answers)
}

// A failed delivery stops the deferred queue, but does not prove the provider
// refused the message. The durable receipt distinguishes a definite refusal
// from an attempt whose hand-over is still unknown, including on another node.
// Read only the receipt belonging to the already admitted delivery's owner and
// organization; no address, owner or receipt is returned by the status API.
func (w *Worker) rowMessageOutcome(ctx context.Context, row map[string]any) (string, string, error) {
	status, sentAt := str(row, "status"), str(row, "sentAt")
	if status != "failed" {
		return status, sentAt, nil
	}
	owner := bare(str(row, "ownerUserId"))
	if owner == "" {
		return "", "", errors.New("the message's sending identity could not be read")
	}
	actor := auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(w.ownerActorContext(ctx, owner)))
	// Direct callers can carry a canonical user id; deferred workers borrow
	// its bare form. Older receipts hash the spelling the sending actor carried.
	// Read both: a definite refusal under one must not hide an unknown hand-over
	// under the other. These are identities of the same already admitted owner.
	accepted, uncertain := false, false
	for _, identity := range []string{owner, "v1:identity:user:" + owner} {
		rows, err := w.store.rows(actor, call("query", "campaignSingleSendById",
			arg{"receiptId", singleSendReceiptID(identity, str(row, "requestId"))}))
		if err != nil {
			return "", "", err
		}
		if len(rows) == 0 {
			continue
		}
		receipt := rows[0]
		if !sameOrganization(str(receipt, "accountId"), str(row, "accountId")) {
			return "", "", errors.New("the message's saved receipt belongs to another organization")
		}
		switch str(receipt, "status") {
		case receiptFailed:
		case receiptComplete:
			if objectField(receipt, "result")["sent"] == true {
				accepted, sentAt = true, str(receipt, "createdAt")
			} else {
				uncertain = true
			}
		default:
			uncertain = true
		}
	}
	if accepted {
		return "sent", sentAt, nil
	}
	if uncertain {
		return "uncertain", "", nil
	}
	return status, sentAt, nil // No attempt, or every attempt was refused.
}
