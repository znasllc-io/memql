package outbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
)

// Request is the delivery-relevant projection of an outboundRequest row
// handed to a Transport.
type Request struct {
	ID      string
	Medium  string
	Target  string
	Subject string
	Payload string
	// TargetSecret is the v1:platform:globalSecret NAME whose value is the
	// webhook URL (memql#5480). When it is set, the row's Target holds only
	// the descriptor secret:<NAME>; the worker resolves the value and hands
	// the transport a copy whose Target is the URL, so the URL exists in
	// memory for one attempt and never in the row, a log line or an error.
	TargetSecret string
	DedupeKey    string
	Attempts     int
}

// PermanentError marks a delivery failure that must not be retried
// (malformed target, 4xx rejection, policy refusal). Anything else --
// timeouts, connection errors, 408/429/5xx -- is retryable (ADR 4.1).
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent wraps err as a PermanentError.
func Permanent(err error) error { return &PermanentError{Err: err} }

// IsPermanent reports whether err carries a PermanentError.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// Transport performs one delivery attempt. Implementations classify
// non-retryable failures by returning a PermanentError.
type Transport interface {
	Deliver(ctx context.Context, req Request) error
}

// EmailTransport delivers medium="email" rows through the engine's
// email integration plug-in, resolved off the integration registry at
// send time (emailsender precedent) so Graph/SMTP/log selection and
// credential resolution (env / globalSecret rows) come from the
// deployment, never from the staged row.
type EmailTransport struct {
	Engine *memqlengine.MemQLEngine
	Logger *slog.Logger
}

// NewEmailTransport constructs the email transport over the engine's
// integration registry.
func NewEmailTransport(engine *memqlengine.MemQLEngine, logger *slog.Logger) *EmailTransport {
	return &EmailTransport{Engine: engine, Logger: logger}
}

// Deliver sends the payload as the text body to the target address. A
// missing email integration is retryable: on a booting node the plug-in
// registry may not be populated yet.
func (t *EmailTransport) Deliver(ctx context.Context, req Request) error {
	if t == nil || t.Engine == nil {
		return errors.New("email transport: engine not wired")
	}
	sender := t.resolveSender()
	if sender == nil {
		return errors.New("email transport: no email integration registered")
	}
	// The zero SendAs -- the deployment's configured mailbox (email design
	// D5). An outbound row is a product's own delivery, not a campaign, so
	// there is no operator-declared sending identity to resolve and the
	// concept carries no field that could name one.
	return sender.Send(ctx, email.Message{
		To:       req.Target,
		Subject:  req.Subject,
		TextBody: req.Payload,
	}, email.SendAs{})
}

func (t *EmailTransport) resolveSender() email.Sender {
	prov := t.Engine.Integrations().Provider("email")
	if prov == nil {
		return nil
	}
	emailInt, ok := prov.(*email.Integration)
	if !ok || emailInt == nil {
		return nil
	}
	return emailInt.SenderAccess()
}

// WebhookTransport delivers medium="webhook" rows as an HTTPS POST of
// the payload (JSON body) to the target URL. The outbound id and the
// caller's dedupeKey ride as headers so receivers can deduplicate
// at-least-once redelivery (ADR 4.2).
type WebhookTransport struct {
	Client *http.Client
	// Allowlist is the deploy-owned URL prefix set (normalized). The
	// request URL is REBUILT as matched-prefix + remainder, so the
	// scheme and host always come from configuration, never from the
	// staged row -- the row can only extend the path/query under an
	// allowlisted origin (SSRF containment, ADR 4.3; the worker's
	// admit() is the fail-fast policy gate, this is defense in depth).
	Allowlist []string
}

// NewWebhookTransport constructs the webhook transport with the
// deploy-configured request timeout and allowlist.
func NewWebhookTransport() *WebhookTransport {
	cfg := LoadConfig()
	return &WebhookTransport{
		Client:    &http.Client{Timeout: cfg.HTTPTimeout},
		Allowlist: cfg.WebhookAllowlist,
	}
}

// Deliver POSTs the payload. 2xx is success; 408/429/5xx and transport
// errors are retryable; every other status is permanent.
func (t *WebhookTransport) Deliver(ctx context.Context, req Request) error {
	if t == nil || t.Client == nil {
		return errors.New("webhook transport: client not wired")
	}
	prefix, remainder, ok := matchWebhookPrefix(req.Target, t.Allowlist)
	if !ok {
		return Permanent(fmt.Errorf("webhook: target not in allowlist"))
	}
	// Build the request URL from the PARSED trusted prefix: scheme and
	// host come from deployment config; the row-controlled remainder is
	// assigned only to the path/query fields, so it cannot steer the
	// request off the allowlisted origin.
	base, err := url.Parse(prefix)
	if err != nil {
		return Permanent(fmt.Errorf("webhook: allowlist prefix unparseable: %w", err))
	}
	dest := *base
	if i := strings.IndexByte(remainder, '?'); i >= 0 {
		dest.Path = base.Path + remainder[:i]
		dest.RawQuery = remainder[i+1:]
	} else {
		dest.Path = base.Path + remainder
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, dest.String(), strings.NewReader(req.Payload))
	if err != nil {
		// Redacted like the client error below: a URL that fails to parse
		// comes back quoted inside the *url.Error.
		return Permanent(fmt.Errorf("webhook: build request: %w", redactURLError(err)))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Memql-Outbound-Id", req.ID)
	if req.DedupeKey != "" {
		httpReq.Header.Set("X-Memql-Dedupe-Key", req.DedupeKey)
	}
	resp, err := t.Client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("webhook: %w", redactURLError(err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusRequestTimeout,
		resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode >= 500:
		return fmt.Errorf("webhook: status %d", resp.StatusCode)
	default:
		return Permanent(fmt.Errorf("webhook: status %d", resp.StatusCode))
	}
}

// redactURLError drops the request URL net/http embeds in *url.Error: a
// webhook URL can carry its credential in the path (Discord's token), and
// this error is stamped into lastError and logged.
//
// The operation and the cause are kept, so lastError still says what went
// wrong ("Post: dial tcp ...: connection refused"). Only the URL goes, and
// for every row rather than only a secret target's: the transport cannot
// tell which of its URLs is a credential, and a plain row's lastError
// already names its target in the row beside it.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}
