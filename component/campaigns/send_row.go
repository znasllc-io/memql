package campaigns

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
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
	return w.sendToRecipientOnce(ctx, send, singleSend{deferWhenLimited: true, rowSend: &rowSend{Concept: concept, RowID: rowID, Kind: kind, AccountID: account}})
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
		return nil, errors.New("campaigns.sendRowMessage: the template must belong to the row's organization")
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
		err := fmt.Errorf("campaigns.sendRowMessage: %s", refusal.Reason)
		if !refusal.Terminal {
			err = undecided(err)
		}
		return nil, err
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
		return nil, errors.New("campaigns.sendRowMessage: the row belongs to another organization, so nothing was sent")
	}
	digest := EmailDigest(resolved.Email)
	if digest == "" {
		return nil, errors.New("campaigns.sendRowMessage: the row names no usable address")
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
		return nil, undecided(fmt.Errorf("campaigns.sendRowMessage: the suppression list could not be consulted, "+
			"so this send is refused rather than made: %w", err))
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
		return nil, fmt.Errorf("campaigns.sendRowMessage: %w", err)
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
// address, reason, readFailed, noneChosen}. Ready answers the identity's name
// and address; otherwise fromName and address are empty and reason is a
// sentence for a person: the hold's own sentence, or one of rowSenderRefusals'
// words, never the campaign worker's refusal text. It sends nothing and checks
// no transport.
//
// WHAT A CALLER DECIDES ON IS A FIELD, NEVER THE SENTENCE: readFailed says the
// sender could not be read just now (nothing is known of it, and the same read
// made again may answer), and noneChosen that the organization has no sender
// because none is chosen. The sentences may be reworded; the fields may not.
//
// THE SEND'S OWN AUTHORITY: the caller must hold what a row send requires of
// it (requireSendAuthority, update data in the organization). A caller without
// it is answered exactly as for an organization that does not exist -- both
// fields false -- so the answer says nothing of the organization's senders.
func (w *Worker) handleRowSender(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return nil, errors.New("campaigns.rowSender: read by a trusted job, never by a client")
	}
	account := memql.BareShortId(strings.TrimSpace(argString(args, "accountId")))
	if account == "" {
		return nil, errors.New("campaigns.rowSender: accountId is required: a sender is one organization's")
	}
	// why is the one thing a refusal may also say in a field: nothing, that the
	// read failed, or that none is chosen.
	const (
		whyNothing = iota
		whyReadFailed
		whyNoneChosen
	)
	refused := func(reason string, why int) ([]memorynodes.MemoryNode, error) {
		return resultNode("campaignRowSender", map[string]any{"accountId": account, "ready": false, "fromName": "", "address": "",
			"reason": reason, "readFailed": why == whyReadFailed, "noneChosen": why == whyNoneChosen})
	}
	if err := w.requireSendAuthority(ctx, account); err != nil {
		return refused(rowSenderUnreadable, whyNothing)
	}
	campaign := Campaign{AccountID: account, SenderIdentityID: memql.BareShortId(strings.TrimSpace(argString(args, "senderIdentityId")))}
	identity, refusal := w.resolveSendIdentityFor(memql.ContextWithFreshRead(w.systemActorContext(ctx)), campaign, false)
	switch {
	case !refusal.refused():
		return resultNode("campaignRowSender", map[string]any{"accountId": account, "ready": true,
			"fromName": identity.SendAs.FromName, "address": identity.SendAs.Address, "reason": "",
			"readFailed": false, "noneChosen": false})
	case refusal.Held != "":
		return refused(refusal.Held, whyNothing)
	case !refusal.Terminal:
		return refused(rowSenderReadFailed, whyReadFailed)
	case campaign.SenderIdentityID == "":
		return refused(rowSenderNoneChosen, whyNoneChosen)
	}
	return refused(rowSenderOtherOrganization, whyNothing)
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
		// WHY IT WAS SKIPPED, IN A SENTENCE, and nothing for a message that was not
		// skipped. The query answers row lane deliveries only (rowConcept set), so a
		// reason that is not one of the lane's codes is the row's own withdrawal
		// sentence.
		sentence := ""
		if str(row, "status") == "skipped" {
			sentence = rowSkipSentence(str(row, "skipReason"))
		}
		answers = append(answers, map[string]any{
			"id": id, "requestId": id, "status": str(row, "status"), "skipReason": str(row, "skipReason"),
			"skipSentence": sentence, "providerStatus": str(row, "providerStatus"), "sentAt": str(row, "sentAt"),
		})
	}
	return resultRows("campaignRowMessageStatus", answers)
}
