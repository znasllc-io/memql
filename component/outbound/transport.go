package outbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
)

// Request is the delivery-relevant projection of an outboundRequest row
// handed to a Transport.
type Request struct {
	ID        string
	Medium    string
	Target    string
	Subject   string
	Payload   string
	DedupeKey string
	Attempts  int
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
	return sender.Send(ctx, email.Message{
		To:       req.Target,
		Subject:  req.Subject,
		TextBody: req.Payload,
	})
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
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, prefix+remainder, strings.NewReader(req.Payload))
	if err != nil {
		return Permanent(fmt.Errorf("webhook: build request: %w", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Memql-Outbound-Id", req.ID)
	if req.DedupeKey != "" {
		httpReq.Header.Set("X-Memql-Dedupe-Key", req.DedupeKey)
	}
	resp, err := t.Client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
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
