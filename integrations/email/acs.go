package email

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/compose"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

const acsEmailAPIVersion = "2025-09-01"

// ACSConfig is kept in encrypted organization connection storage. Senders is
// the set of mailboxes verified with Azure, including their provider-managed
// display names. Neither a caller's From header nor a different organization's
// connection can enlarge that set.
type ACSConfig struct {
	AccountID  string            `json:"accountId"`
	ResourceID string            `json:"resourceId,omitempty"`
	Endpoint   string            `json:"endpoint"`
	AccessKey  string            `json:"accessKey"`
	Senders    map[string]string `json:"senders"`
	Default    string            `json:"defaultSender"`
	ReplyTo    string            `json:"replyTo,omitempty"`
}

// ACSSender uses ACS's outbound Email API. A successful call means Azure
// ACCEPTED the message, not that the recipient received it. Delivery feedback
// is a separate event. Redirects are refused so credentials never leave the
// resource endpoint discovered through Azure Resource Manager.
type ACSSender struct {
	cfg        ACSConfig
	key        []byte
	client     *http.Client
	now        func() time.Time
	operations acsOperationStore
	// azureOwnDomain is a connection on Azure's own domain for the
	// installation, which has the one sender Azure made and registers no other.
	azureOwnDomain bool
}

func NewACSSender(cfg ACSConfig) (*ACSSender, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.Endpoint))
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" ||
		u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") ||
		!strings.HasSuffix(strings.ToLower(u.Hostname()), ".communication.azure.com") {
		return nil, fmt.Errorf("email: Azure Communication Services endpoint is invalid")
	}
	key, err := base64.StdEncoding.DecodeString(cfg.AccessKey)
	if err != nil || len(key) < 32 {
		return nil, fmt.Errorf("email: Azure Communication Services credential is invalid")
	}
	if strings.TrimSpace(cfg.AccountID) == "" || len(cfg.Senders) == 0 {
		return nil, fmt.Errorf("email: Azure email connection needs an organization and a verified sender")
	}
	senders := make(map[string]string, len(cfg.Senders))
	for address, displayName := range cfg.Senders {
		parsed, err := mail.ParseAddress(address)
		if err != nil || parsed.Address != address || headerUnsafe(displayName) {
			return nil, fmt.Errorf("email: Azure email connection contains an invalid sender")
		}
		senders[strings.ToLower(address)] = displayName
	}
	cfg.Senders = senders
	cfg.Default = strings.ToLower(strings.TrimSpace(cfg.Default))
	if _, ok := senders[cfg.Default]; !ok {
		return nil, fmt.Errorf("email: Azure default sender is not verified")
	}
	if cfg.ReplyTo != "" {
		mailbox, err := mail.ParseAddress(cfg.ReplyTo)
		if err != nil || mailbox.Address != cfg.ReplyTo || headerUnsafe(cfg.ReplyTo) {
			return nil, fmt.Errorf("email: Azure Reply-To address is invalid")
		}
	}
	cfg.Endpoint = strings.TrimRight(u.String(), "/")
	return &ACSSender{cfg: cfg, key: key, now: time.Now, client: &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (s *ACSSender) Send(ctx context.Context, msg Message, as SendAs) error {
	if err := msg.Validate(); err != nil {
		return permanentSendRefusal(err.Error())
	}
	if err := as.Validate(); err != nil {
		return permanentSendRefusal(err.Error())
	}
	address, err := s.identity(as)
	if err != nil {
		return err
	}
	to, err := mail.ParseAddress(msg.To)
	if err != nil {
		return permanentSendRefusal("recipient address is invalid")
	}
	headers := make(map[string]string, len(msg.Headers))
	var replyTo []map[string]string
	for name, value := range msg.Headers {
		if strings.EqualFold(name, "Reply-To") {
			parsed, err := mail.ParseAddress(value)
			if err != nil || len(replyTo) > 0 {
				return permanentSendRefusal("Reply-To must name one valid mailbox")
			}
			replyTo = []map[string]string{{"address": parsed.Address, "displayName": parsed.Name}}
			continue
		}
		if strings.TrimSpace(value) == "" {
			continue // An empty header says nothing (a single send has no campaign to name).
		}
		headers[name] = value
	}
	if len(replyTo) == 0 && s.cfg.ReplyTo != "" {
		replyTo = []map[string]string{{"address": s.cfg.ReplyTo}}
	}
	htmlBody, images, err := compose.ExtractEmailImages(msg.HTMLBody)
	if err != nil {
		return permanentSendRefusal(err.Error())
	}
	payload := map[string]any{
		"senderAddress":                  address,
		"recipients":                     map[string]any{"to": []map[string]string{{"address": to.Address, "displayName": to.Name}}},
		"content":                        map[string]string{"subject": msg.Subject, "plainText": msg.TextBody, "html": htmlBody},
		"headers":                        headers,
		"userEngagementTrackingDisabled": true, // MemQL owns campaign tracking
	}
	if len(images) > 0 {
		attachments := make([]map[string]string, 0, len(images))
		for _, image := range images {
			attachments = append(attachments, map[string]string{"name": image.ContentID + "." + strings.TrimPrefix(image.MIMEType, "image/"), "contentType": image.MIMEType, "contentInBase64": base64.StdEncoding.EncodeToString(image.Data), "contentId": image.ContentID})
		}
		payload["attachments"] = attachments
	}
	if len(replyTo) != 0 {
		payload["replyTo"] = replyTo
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return permanentSendRefusal("could not encode Azure email request")
	}
	if len(body) > 10<<20 {
		return permanentSendRefusal("email exceeds the Azure message size limit")
	}
	return s.submitOnce(withACSMessage(ctx, msg, address), msg.IntentID, body)
}

func (s *ACSSender) post(ctx context.Context, operationID string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.cfg.Endpoint+"/emails:send?api-version="+acsEmailAPIVersion, bytes.NewReader(body))
	if err != nil {
		return permanentSendRefusal("could not construct Azure email request")
	}
	req.Header.Set("Operation-Id", operationID)
	s.sign(req, body)
	response, err := s.client.Do(req)
	if err != nil {
		// A lost response can follow acceptance. Blindly retrying the POST
		// duplicates mail. Leave this delivery for explicit reconciliation.
		return unconfirmedSend("Azure email acceptance is unknown; check the Azure delivery log before retrying")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
	if response.StatusCode == http.StatusAccepted {
		return nil
	}
	// Never return provider response bodies: they can echo message contents,
	// addresses or request credentials into the persisted campaign error.
	if response.StatusCode == http.StatusConflict {
		// Azure already holds an operation under this Operation-Id: an earlier
		// submission of this delivery reached it. Its outcome is Azure's record.
		return unconfirmedSend("Azure already has an operation for this send; its outcome is being confirmed")
	}
	if response.StatusCode < 400 {
		return unconfirmedSend("Azure returned an unexpected email response; no redirect was followed")
	}
	// Any other answer refused the request, a server error included. For a caller
	// that names its delivery with a stable IntentID (the campaign drain, a single
	// send under a receipt), a request Azure did take despite answering with an
	// error is not taken twice: the delivery's next submission goes under the same
	// Operation-Id (submitOnce). A caller that names none (the transactional outbox)
	// gets a fresh intent, and so a fresh Operation-Id, on every Send.
	refusal := classifyHTTPSend(response.StatusCode, response.Header.Get("Retry-After"), "Azure did not accept the message")
	refusal.NotAccepted = true
	return refusal
}

// acsHoldLimit bounds how long one delivery holds its receipt's lock: the POST (the
// client gives up at 30 s) and the write of its outcome (5 s), with room. Far
// inside the gate session's idle limit.
const acsHoldLimit = 90 * time.Second

// acsBusyRetryAfter is how long a delivery that could not take its receipt's lock
// asks its caller to wait. The lock is the pod's, not the provider's, and it clears
// in moments; the caller's provider-throttle handling does the waiting.
const acsBusyRetryAfter = 15 * time.Second

// operationKey is the receipt's key for a delivery: the organization and the
// message's intent.
func (s *ACSSender) operationKey(intent string) string {
	return emailStateID("acs-send:" + s.cfg.AccountID + ":" + intent)
}

// heldOperationKey carries, in a context, the receipt key whose lock the caller
// reserved (Reserve) and holds until the delivery is over.
type heldOperationKey struct{}

type heldOperationScope struct {
	key  string
	hold acsOperationGate
	keys []string
}

func withHeldOperation(ctx context.Context, key string, hold acsOperationGate, keys []string) context.Context {
	return context.WithValue(ctx, heldOperationKey{}, heldOperationScope{key: key, hold: hold, keys: keys})
}
func heldOperation(ctx context.Context) heldOperationScope {
	hold, _ := ctx.Value(heldOperationKey{}).(heldOperationScope)
	return hold
}

// CopyDeliveryGates preserves native reservation evidence on the caller's own
// identity context. It carries no serialized or caller-supplied authority.
func CopyDeliveryGates(target, source context.Context) context.Context {
	return context.WithValue(target, heldOperationKey{}, heldOperation(source))
}

// BorrowDeliveryGate verifies a lock already reserved with the send receipt on
// the same connection. Its lifetime belongs to the reservation's caller.
func BorrowDeliveryGate(ctx context.Context, key string) (func(context.Context) error, bool) {
	scope := heldOperation(ctx)
	for _, held := range scope.keys {
		if held == key && scope.hold != nil {
			return scope.hold.Verify, true
		}
	}
	return nil, false
}

type boundedOperationGate struct {
	acsOperationGate
	cancel context.CancelFunc
}

func (h boundedOperationGate) Release() { h.acsOperationGate.Release(); h.cancel() }

// lockDelivery takes a delivery's receipt lock and returns the function that gives
// it up. The lock is held on a context the caller's CANCELLATION does not end: a
// request that stops waiting while the message is out still has the outcome it
// never saw recorded under the lock, not after the lock was rolled back with the
// caller's transaction. It is bounded (acsHoldLimit), and a caller whose own
// context is done by the time the lock is taken gets that back with nothing
// attempted.
//
// A lock that is busy is a refusal made before anything is attempted, and it says
// so as a throttle: the callers that already know how to wait for a provider
// (the bulk drain parks the job, the transactional worker backs off) wait, and
// it costs no attempt.
func (s *ACSSender) lockDelivery(ctx context.Context, key string, keys ...string) (acsOperationGate, error) {
	hold, cancel := context.WithTimeout(context.WithoutCancel(ctx), acsHoldLimit)
	gate, err := s.operations.lock(hold, key, keys...)
	if err != nil {
		cancel()
		if memql.IsWriteGateBusy(err) {
			return nil, &SendError{
				Throttled: true, RetryAfter: acsBusyRetryAfter, NotAccepted: true,
				Detail: "Azure send coordination is busy, so nothing was submitted; try again in a moment (" + memql.ErrWriteGateBusy.Error() + ")",
				Cause:  err,
			}
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		gate.Release()
		cancel()
		return nil, err
	}
	return boundedOperationGate{acsOperationGate: gate, cancel: cancel}, nil
}

// Reserve takes the receipt's lock for a delivery before its caller records that an
// attempt has begun (Reserver). The Send that follows, called with the context
// returned here, does not ask for the lock again; the caller gives it back when the
// delivery is over.
//
// A delivery Azure throttled is refused here until the time Azure named, with what
// is left of the wait: before its caller records an attempt, so the caller spends
// none, writes nothing, and waits as asked.
func (s *ACSSender) Reserve(ctx context.Context, msg Message, _ SendAs, keys ...string) (context.Context, func(), error) {
	if s.operations == nil || msg.IntentID == "" {
		return ctx, func() {}, nil
	}
	key := s.operationKey(msg.IntentID)
	hold, err := s.lockDelivery(ctx, key, keys...)
	if err != nil {
		return nil, nil, err
	}
	op, _, found, err := s.operations.read(ctx, key)
	if err == nil && found {
		err = s.asksToWait(op)
	}
	if err != nil {
		hold.Release()
		return nil, nil, notSubmitted(err)
	}
	return withHeldOperation(ctx, key, hold, keys), hold.Release, nil
}

// asksToWait refuses a delivery Azure throttled until the time it named. Nothing is
// submitted, and the refusal carries what is left of the wait.
func (s *ACSSender) asksToWait(op acsOperation) error {
	if op.Status == "throttled" && s.now().Before(op.NextPollAt) {
		return &SendError{Throttled: true, NotAccepted: true, RetryAfter: op.NextPollAt.Sub(s.now()), Detail: "Azure asked this delivery to wait"}
	}
	return nil
}

// submitOnce submits one delivery under its Azure receipt.
//
// A delivery Azure definitely did not accept may be submitted again
// (acsMaySubmitAgain): rendered afresh, as a retry is -- its links are minted again,
// so its bytes differ -- and to the connection as it is now. It goes under the
// receipt's Operation-Id, so a request Azure did take despite answering with an
// error is not taken twice; only an operation Azure took and could not finish
// (failed, canceled) goes under a new one, since its own id names that failed
// operation. Azure's repeatable-request headers are not sent: Azure matches a
// repeat by its content, and a retry's content is never the original's.
//
// A delivery Azure may have accepted -- an answer was lost, or is still being
// confirmed -- is never submitted again, by this caller or any other, until
// Azure's record of the operation says it was not accepted (acs_operations.go).
func (s *ACSSender) submitOnce(ctx context.Context, intent string, body []byte) error {
	if s.operations == nil {
		return permanentSendRefusal("Azure send receipt storage is unavailable")
	}
	if intent == "" {
		intent = "transactional:" + id.NewShortId()
	}
	key := s.operationKey(intent)
	// ONE hold, from before the first receipt to after the last. The lock is not
	// given up while the message is out and asked for again afterwards: a request
	// for it that is refused for being busy once the POST has gone would leave a
	// message the provider accepted, whose receipt says "submitting" and so can
	// never be submitted again, and whose caller was told it failed. Nothing that
	// can be refused for being busy happens after the first receipt.
	scope := heldOperation(ctx)
	if scope.key != key {
		hold, err := s.lockDelivery(ctx, key)
		if err != nil {
			return notSubmitted(err)
		}
		defer hold.Release()
		scope = heldOperationScope{key: key, hold: hold}
	}
	op, prior, found, err := s.operations.read(ctx, key)
	if err != nil {
		return notSubmitted(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(body))
	if found {
		again := acsMaySubmitAgain(op.Status)
		if op.AccountID != s.cfg.AccountID || !again && (op.Endpoint != s.cfg.Endpoint || op.Digest != digest) {
			return unconfirmedSend("this delivery already has an Azure receipt for a different message or connection; it will not be sent again")
		}
		if !again {
			return operationSendOutcome(op)
		}
		if err := s.asksToWait(op); err != nil {
			return err
		}
		if op.Status == "failed" || op.Status == "canceled" {
			op.ID = id.NewShortId()
		}
		op.Endpoint, op.ResourceID, op.Digest = s.cfg.Endpoint, s.cfg.ResourceID, digest
	} else {
		op = acsOperation{ID: id.NewShortId(), AccountID: s.cfg.AccountID, Endpoint: s.cfg.Endpoint, Digest: digest, Intent: intent}
		op.ResourceID = s.cfg.ResourceID
	}
	if message, ok := ctx.Value(acsMessageKey{}).(acsMessageIdentity); ok {
		op.DeliveryID, op.RecipientDigest, op.SenderDigest = message.DeliveryID, message.RecipientDigest, message.SenderDigest
	}
	op.Status, op.Detail, op.SubmittedAt, op.NextPollAt = "submitting", "", s.now().UTC(), s.now().UTC().Add(15*time.Second)
	if err = scope.hold.Verify(ctx); err != nil {
		return notSubmitted(err)
	}
	// The engine may finish a dispatched mutation after the caller stops
	// waiting. Keep the reservation until this bounded receipt commit settles.
	commit, finish := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	if err = scope.hold.Verify(commit); err == nil {
		_, err = s.operations.write(commit, key, op, prior)
	}
	finish()
	if err != nil {
		// No POST before its durable receipt. (Should the write have landed after
		// all, the receipt says "submitting" and Azure's record decides.)
		return notSubmitted(err)
	}
	sendErr := s.post(ctx, op.ID, body)
	op.CheckedAt = s.now().UTC()
	switch wait, throttled := IsThrottled(sendErr); {
	case sendErr == nil:
		op.Status = "accepted"
	case throttled:
		if wait < time.Second {
			wait = time.Minute
		}
		op.Status, op.NextPollAt = "throttled", s.now().UTC().Add(wait)
	case IsNotAccepted(sendErr):
		op.Status, op.Detail = "rejected", "Azure refused this send request."
	default:
		op.Status, op.Detail = "unknown", "Azure has not confirmed whether it accepted this message. It is not submitted again unless Azure has no record of it."
	}
	// The outcome is written on a context the caller's cancellation does not end,
	// under the lock this delivery has held since before its first receipt.
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	current, revision, exists, err := s.operations.read(persist, key)
	if err != nil || !exists || current.ID != op.ID {
		return unconfirmedSend("Azure acceptance needs reconciliation; the saved attempt prevents another submission")
	}
	// A background GET may already know more than this delayed POST response.
	if current.Status != "submitting" && current.Status != "unknown" {
		return operationSendOutcome(current)
	}
	prior = revision
	if err = scope.hold.Verify(persist); err != nil {
		return unconfirmedSend("Azure acceptance needs reconciliation; the saved attempt prevents another submission")
	}
	if _, err = s.operations.write(persist, key, op, prior); err != nil {
		return unconfirmedSend("Azure acceptance needs reconciliation; the saved attempt prevents another submission")
	}
	return sendErr
}

// acsMaySubmitAgain says whether a delivery whose receipt has this status was
// definitely not accepted, so it may be submitted again: Azure refused the request
// (rejected, throttled), has no record of the operation (notfound), or took it and
// could not process it (failed, canceled).
func acsMaySubmitAgain(status string) bool {
	switch status {
	case "rejected", "throttled", "notfound", "failed", "canceled":
		return true
	}
	return false
}

// DeliveryOutcome reads what this delivery's Azure receipt says (DeliveryOutcomes).
// It submits nothing and takes no lock: the receipt is written under the
// delivery's lock, and a read that misses a newer version only waits for the next.
//
// A delivery with no receipt is unknown, not "not accepted": a receipt is looked up
// by the identity the caller names, and one asked for under another organization's
// connection would be missing for a message that went. A sender with no receipt
// storage keeps no record at all (OutcomeUnrecorded).
func (s *ACSSender) DeliveryOutcome(ctx context.Context, msg Message, as SendAs) (Outcome, error) {
	if s.operations == nil {
		return OutcomeUnrecorded, nil
	}
	account := strings.TrimSpace(as.AccountID)
	if account == "" {
		account = "self"
	}
	if msg.IntentID == "" || account != s.cfg.AccountID {
		return OutcomeUnknown, nil
	}
	op, _, found, err := s.operations.read(ctx, s.operationKey(msg.IntentID))
	switch {
	case err != nil:
		return OutcomeUnknown, err
	case !found:
		return OutcomeUnknown, nil
	case acsAccepted(op.Status):
		return OutcomeAccepted, nil
	case acsMaySubmitAgain(op.Status):
		return OutcomeNotAccepted, nil
	}
	return OutcomeUnknown, nil
}

// acsAccepted says whether a receipt with this status records a message Azure
// accepted.
func acsAccepted(status string) bool {
	return status == "accepted" || status == "running" || status == "succeeded"
}

func operationSendOutcome(op acsOperation) error {
	if acsAccepted(op.Status) {
		return nil
	}
	return unconfirmedSend("Azure acceptance is being confirmed; this delivery is not submitted again meanwhile")
}

func (s *ACSSender) sign(req *http.Request, body []byte) {
	digest := sha256.Sum256(body)
	contentHash := base64.StdEncoding.EncodeToString(digest[:])
	date := s.now().UTC().Format(http.TimeFormat)
	mac := hmac.New(sha256.New, s.key)
	_, _ = io.WriteString(mac, req.Method+"\n"+req.URL.RequestURI()+"\n"+date+";"+req.URL.Host+";"+contentHash)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-ms-date", date)
	req.Header.Set("x-ms-content-sha256", contentHash)
	req.Header.Set("Authorization", "HMAC-SHA256 SignedHeaders=x-ms-date;host;x-ms-content-sha256&Signature="+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
}

// permanentSendRefusal is a refusal made before anything was submitted: nothing
// can have been accepted, and this exact message cannot succeed on a retry.
func permanentSendRefusal(detail string) error {
	return &SendError{Permanent: true, NotAccepted: true, Detail: detail}
}

// The refusals of a sender that a person fixes on the sender or on its domain,
// not by trying again. Each is its own sentence so SenderRefusal can name it.
const (
	refusalUnregistered   = "choose a verified sender belonging to this organization"
	refusalRenamed        = "Azure sends this sender's address under a different display name; give the sender the display name Azure has"
	refusalAzureOwnDomain = "this organization sends through Azure's own domain, which sends only as its own sender; choose that sender, or set up a sending domain of the organization's own"
)

// SenderRefusal names why an organization's Azure connection refuses to send
// as a sender, when it is one a person fixes rather than one to try again:
// "unregistered" (the domain has not registered the address), "renamed" (it
// has, under another display name, which is the only one Azure sends with) or
// "azureOwnDomain" (Azure's own domain, which registers no other sender). It is
// "" for any other error and for none. Readiness reports it, so a page can say
// what to fix without reading the sentence.
func SenderRefusal(err error) string {
	var se *SendError
	if !errors.As(err, &se) {
		return ""
	}
	switch se.Detail {
	case refusalUnregistered:
		return "unregistered"
	case refusalRenamed:
		return "renamed"
	case refusalAzureOwnDomain:
		return "azureOwnDomain"
	}
	return ""
}

// unconfirmedSend is a delivery Azure may have accepted although nothing said so.
// Permanent, so no caller retries it blindly; not NotAccepted, so nothing submits
// it again until Azure's record of the operation says it was not accepted.
func unconfirmedSend(detail string) error {
	return &SendError{Permanent: true, Detail: detail}
}

// notSubmitted is a failure that came before the POST, so nothing was submitted.
// A failure already classified keeps its own classification.
func notSubmitted(err error) error {
	var se *SendError
	if errors.As(err, &se) {
		return err
	}
	return &SendError{NotAccepted: true, Detail: err.Error(), Cause: err}
}

func (s *ACSSender) identity(as SendAs) (string, error) {
	account := strings.TrimSpace(as.AccountID)
	if account == "" {
		account = "self" // transactional mail belongs to the installation owner
	}
	if account != s.cfg.AccountID {
		return "", permanentSendRefusal("Azure email connection belongs to another organization")
	}
	address := strings.ToLower(strings.TrimSpace(as.Address))
	if address == "" {
		address = s.cfg.Default
	}
	displayName, verified := s.cfg.Senders[address]
	if !verified {
		if s.azureOwnDomain {
			return "", permanentSendRefusal(refusalAzureOwnDomain)
		}
		return "", permanentSendRefusal(refusalUnregistered)
	}
	// ACS controls the display name on the sender username resource; it does
	// not accept a display-name override in emails:send. Refuse a mismatch
	// instead of silently mailing under a different brand.
	if as.FromName != "" && as.FromName != displayName {
		return "", permanentSendRefusal(refusalRenamed)
	}
	return address, nil
}

func (s *ACSSender) CheckSender(_ context.Context, as SendAs) error {
	if err := as.Validate(); err != nil {
		return permanentSendRefusal(err.Error())
	}
	_, err := s.identity(as)
	return err
}
