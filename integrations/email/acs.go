package email

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/compose"
	"github.com/znasllc-io/memql/core/id"
)

const acsEmailAPIVersion = "2025-09-01"

// ACSConfig is kept in encrypted organization connection storage. Senders is
// the set of mailboxes verified with Azure, including their provider-managed
// display names. Neither a caller's From header nor a different organization's
// connection can enlarge that set.
type ACSConfig struct {
	AccountID string            `json:"accountId"`
	Endpoint  string            `json:"endpoint"`
	AccessKey string            `json:"accessKey"`
	Senders   map[string]string `json:"senders"`
	Default   string            `json:"defaultSender"`
	ReplyTo   string            `json:"replyTo,omitempty"`
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
	return s.submitOnce(ctx, msg.IntentID, body)
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
		return permanentSendRefusal("Azure email acceptance is unknown; check the Azure delivery log before retrying")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
	if response.StatusCode == http.StatusAccepted {
		return nil
	}
	// Never return provider response bodies: they can echo message contents,
	// addresses or request credentials into the persisted campaign error.
	if response.StatusCode >= 500 || response.StatusCode == http.StatusRequestTimeout {
		return permanentSendRefusal("Azure email acceptance is unknown; check the Azure delivery log before retrying")
	}
	if response.StatusCode < 400 {
		return permanentSendRefusal("Azure returned an unexpected email response; no redirect was followed")
	}
	return classifyHTTPSend(response.StatusCode, response.Header.Get("Retry-After"), "Azure did not accept the message")
}

func (s *ACSSender) submitOnce(ctx context.Context, intent string, body []byte) error {
	if s.operations == nil {
		return permanentSendRefusal("Azure send receipt storage is unavailable")
	}
	if intent == "" {
		intent = "transactional:" + id.NewShortId()
	}
	key := emailStateID("acs-send:" + s.cfg.AccountID + ":" + intent)
	release, err := s.operations.lock(ctx, key)
	if err != nil {
		return err
	}
	defer release()
	op, prior, found, err := s.operations.read(ctx, key)
	if err != nil {
		return err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(body))
	if found {
		if op.AccountID != s.cfg.AccountID || op.Endpoint != s.cfg.Endpoint || op.Digest != digest {
			return permanentSendRefusal("this delivery already has an Azure receipt for a different message or connection; it will not be sent again")
		}
		if op.Status != "throttled" {
			return operationSendOutcome(op)
		}
		if s.now().Before(op.NextPollAt) {
			return &SendError{Throttled: true, RetryAfter: op.NextPollAt.Sub(s.now()), Detail: "Azure asked this delivery to wait"}
		}
	} else {
		op = acsOperation{ID: id.NewShortId(), AccountID: s.cfg.AccountID, Endpoint: s.cfg.Endpoint, Digest: digest, Intent: intent}
	}
	op.Status, op.Detail, op.SubmittedAt, op.NextPollAt = "submitting", "", s.now().UTC(), s.now().UTC().Add(15*time.Second)
	prior, err = s.operations.write(ctx, key, op, prior)
	if err != nil {
		return err
	} // No POST before its durable receipt.
	sendErr := s.post(ctx, op.ID, body)
	op.CheckedAt = s.now().UTC()
	if sendErr == nil {
		op.Status = "accepted"
	} else if wait, throttled := IsThrottled(sendErr); throttled {
		if wait < time.Second {
			wait = time.Minute
		}
		op.Status, op.NextPollAt = "throttled", s.now().UTC().Add(wait)
	} else {
		op.Status, op.Detail = "unknown", "Azure acceptance could not be confirmed. This message will not be submitted again."
		if e, ok := sendErr.(*SendError); ok && e.StatusCode >= 400 && e.StatusCode < 500 && e.StatusCode != 408 {
			op.Status, op.Detail = "rejected", "Azure refused this send request."
		}
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err = s.operations.write(persist, key, op, prior); err != nil {
		return permanentSendRefusal("Azure acceptance needs reconciliation; the saved attempt prevents another submission")
	}
	return sendErr
}

func operationSendOutcome(op acsOperation) error {
	switch op.Status {
	case "accepted", "running", "succeeded":
		return nil
	case "failed", "canceled", "rejected":
		return permanentSendRefusal("Azure did not complete the previous send; this delivery will not be submitted again")
	default:
		return permanentSendRefusal("Azure acceptance is being reconciled; this delivery will not be submitted again")
	}
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

func permanentSendRefusal(detail string) error {
	return &SendError{Permanent: true, Detail: detail}
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
		return "", permanentSendRefusal("choose a verified sender belonging to this organization")
	}
	// ACS controls the display name on the sender username resource; it does
	// not accept a display-name override in emails:send. Refuse a mismatch
	// instead of silently mailing under a different brand.
	if as.FromName != "" && as.FromName != displayName {
		return "", permanentSendRefusal("update this sender's display name in the Azure email connection before sending")
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
