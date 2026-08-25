// Package email is a minimal outbound-mail helper used by MemQL for
// transactional messages (currently just guest invites).
//
// The package intentionally avoids the full integrations.Integration
// lifecycle: email is fire-and-forget, doesn't need a ticker, and the
// SMTP connection is opened per-send. If volume ever grows we can swap
// in a pooled vendor SDK behind the same Sender interface without
// touching callers.
package email

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/smtp"
	"strings"

	"github.com/znasllc-io/memql/core/env"
)

// ComponentName identifies the package in logs + env lookups.
const ComponentName = "email"

// Sender delivers a single email message. Implementations are expected
// to be safe for concurrent use.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// Message is a rendered email ready to go on the wire.
type Message struct {
	To      string
	Subject string
	// TextBody is the plain-text alternative. Required.
	TextBody string
	// HTMLBody is optional; when set the message goes out as
	// multipart/alternative with text first, HTML second.
	HTMLBody string
	// Headers carries additional RFC 5322 headers (memql#3348). Empty
	// for transactional mail; the campaign sender uses it for the RFC
	// 8058 one-click pair, `List-Unsubscribe` and
	// `List-Unsubscribe-Post`.
	//
	// Setting it CHANGES WHICH GRAPH API IS USED. Graph's structured
	// sendMail payload can only carry custom headers whose names begin
	// with `x-`, which `List-Unsubscribe` does not and cannot, so a
	// message with extras is rendered to RFC 5322 and sent through
	// Graph's base64-MIME form instead. See mime.go.
	//
	// Names the renderer composes itself (From / To / Subject /
	// MIME-Version / Content-Type) are REFUSED rather than overridden --
	// an overridden From is a sender the mailbox did not authenticate
	// as, which is the SPF/DKIM alignment the campaign path depends on
	// being structural.
	Headers map[string]string
}

// headerUnsafe reports whether v carries a character that would let a
// caller break out of a single email header line and inject additional
// headers or recipients (RFC 5322 header / SMTP injection): CR, LF, NUL,
// or any other C0 control except tab. To and Subject are written into
// headers verbatim, so both must pass this before a Send. Bodies are
// exempt -- they live after the header/body boundary and legitimately
// contain newlines.
func headerUnsafe(v string) bool {
	return strings.IndexFunc(v, func(r rune) bool {
		return r == '\r' || r == '\n' || r == 0x00 || (r < 0x20 && r != '\t')
	}) >= 0
}

// Validate reports missing or obviously malformed fields.
func (m Message) Validate() error {
	if strings.TrimSpace(m.To) == "" {
		return errors.New("email: To is required")
	}
	if !strings.Contains(m.To, "@") {
		return fmt.Errorf("email: To %q is not a valid address", m.To)
	}
	if headerUnsafe(m.To) {
		return errors.New("email: To contains illegal control characters (header injection)")
	}
	if strings.TrimSpace(m.Subject) == "" {
		return errors.New("email: Subject is required")
	}
	if headerUnsafe(m.Subject) {
		return errors.New("email: Subject contains illegal control characters (header injection)")
	}
	if strings.TrimSpace(m.TextBody) == "" {
		return errors.New("email: TextBody is required")
	}
	// Extra headers are validated HERE as well as in the renderer
	// (memql#3348). Not redundant: Validate is what a caller reaches for
	// to check a message before queueing it, and a header problem
	// discovered at the wire boundary can only be reported as a failed
	// send. The renderer keeps its own check because it is the sink, and
	// a sink that trusts its caller is safe only until the second caller.
	return ValidateExtraHeaders(m.Headers)
}

// SMTPConfig configures the SMTPSender.
type SMTPConfig struct {
	Host     string // e.g. "smtp.sendgrid.net"
	Port     string // e.g. "587"
	Username string
	Password string
	FromAddr string // e.g. "no-reply@example.com"
	FromName string // e.g. "Example App"
}

// SMTPSender sends via plain SMTP over STARTTLS. Use this when the
// operator provides SMTP credentials (typical: SendGrid / SES /
// Postmark relay endpoints).
type SMTPSender struct {
	cfg    SMTPConfig
	logger *slog.Logger
}

// NewSMTPSender returns an SMTPSender that uses net/smtp under the
// hood. It opens a connection per Send.
func NewSMTPSender(cfg SMTPConfig, logger *slog.Logger) *SMTPSender {
	return &SMTPSender{cfg: cfg, logger: logger}
}

// Send delivers msg via SMTP. Blocks until the remote acks or fails.
func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	if err := msg.Validate(); err != nil {
		return err
	}

	from := s.cfg.FromAddr
	fromHeader := FromHeader(from, s.cfg.FromName)

	// Header injection barrier at the wire-format boundary: no header value
	// reaches the SMTP payload without passing headerUnsafe. msg.Validate()
	// already rejects an unsafe To/Subject up front; the renderer re-checks
	// every header value (including the config-derived From and any
	// caller-supplied extras) at the point it is serialized, so the sink is
	// safe by construction regardless of how the message was built. The
	// rendering itself moved to mime.go in memql#3348 so the Graph MIME path
	// produces byte-identical output.
	body, err := RenderRFC5322(fromHeader, msg)
	if err != nil {
		return err
	}

	addr := s.cfg.Host + ":" + s.cfg.Port
	var auth smtp.Auth
	if s.cfg.Username != "" {
		auth = smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
	}

	// net/smtp does not expose a context-aware API. Honor cancellation
	// by checking ctx before the blocking call; the caller's timeout
	// governs overall latency.
	if err := ctx.Err(); err != nil {
		return err
	}

	// `go/email-injection` fires here and is adjudicated a false positive at
	// THIS sink (memql#3483). The query flags untrusted input reaching email
	// content, which is what a mail sender is for -- a campaign exists to put
	// operator-authored text into a message, so no code change can satisfy it.
	// Everything it could concretely mean is closed above: RenderRFC5322 runs
	// msg.Validate(), headerUnsafe(fromHeader) and ValidateExtraHeaders before
	// a byte is serialized, so header injection is barred however the message
	// was assembled; the MIME boundary is 128 bits of crypto/rand, so a body
	// cannot forge a part; and recipient data is escaped in the HTML body.
	//
	// Sited here rather than dismissed only on the security surface so the
	// reasoning travels with the code and the next reader can see it. The
	// query stays live everywhere else -- it caught a real unescaped-HTML
	// defect in memql#3455 that a green test suite missed.
	// codeql[go/email-injection]
	if err := smtp.SendMail(addr, auth, from, []string{msg.To}, body); err != nil {
		// Wrapped as an unclassified SendError rather than left bare
		// (memql#3348): net/smtp gives no status code, so nothing here can
		// honestly call a failure permanent. Zero StatusCode with neither
		// flag set is exactly that statement, and IsPermanent reads it as
		// retryable -- the safe direction, since giving up discards mail.
		return &SendError{Detail: fmt.Sprintf("smtp SendMail to %s: %v", s.cfg.Host, err)}
	}

	if s.logger != nil {
		s.logger.Info("email sent", "to", msg.To, "subject", msg.Subject)
	}
	return nil
}

// LogSender writes the message to the logger instead of sending.
// Useful for local dev where no SMTP relay is wired.
type LogSender struct {
	logger *slog.Logger
	// refusal is non-nil when this install must really deliver mail
	// (delivery.go). Then every Send fails instead of logging a line and
	// returning nil.
	//
	// The check lives HERE, on the type, rather than only in the selection
	// below, because the selection is not the only construction site:
	// component/grpc/guest_handlers.go builds a LogSender directly on three
	// fallback paths that never touch the plug-in factory. A guard placed
	// only at selection would leave those reporting success forever, which
	// is the exact defect (memql#4477).
	refusal error
}

// NewLogSender returns a Sender that logs messages at Info level -- or, on an
// install that must really deliver mail, one that refuses every send.
//
// The decision is taken HERE and cached, not re-read per Send: a process's
// MEMQL_DOMAIN does not change under it, and a per-send read would make the
// behaviour depend on whatever last mutated the environment.
func NewLogSender(logger *slog.Logger) *LogSender {
	l := &LogSender{logger: logger}
	if DeliveryRequired() {
		l.refusal = refusalSendError("send")
	}
	return l
}

// Send logs the message without attempting delivery, or refuses.
func (l *LogSender) Send(_ context.Context, msg Message) error {
	if err := msg.Validate(); err != nil {
		return err
	}
	if l.refusal != nil {
		// Logged as well as returned: the caller may be a path that swallows
		// the error, and an operator grepping the node log for why mail
		// stopped needs the sentence naming the four env vars.
		if l.logger != nil {
			l.logger.Error("email: refusing to log-only a message on an install that must deliver it",
				"to", msg.To,
				"subject", msg.Subject,
				"error", l.refusal)
		}
		return l.refusal
	}
	if l.logger == nil {
		return nil
	}
	l.logger.Info("email (log-only mode, not delivered)",
		"to", msg.To,
		"subject", msg.Subject,
		"text", msg.TextBody)
	return nil
}

// EnvKeys names the env vars consulted by NewSenderFromEnv.
type EnvKeys struct {
	Host     string
	Port     string
	Username string
	Password string
	FromAddr string
	FromName string
}

// DefaultEnvKeys returns the canonical env-var names.
func DefaultEnvKeys() EnvKeys {
	return EnvKeys{
		Host:     "SMTP_HOST",
		Port:     "SMTP_PORT",
		Username: "SMTP_USERNAME",
		Password: "SMTP_PASSWORD",
		FromAddr: "SMTP_FROM_ADDR",
		FromName: "SMTP_FROM_NAME",
	}
}

// NewSenderFromEnv picks a Sender based on environment variables, in
// priority order:
//
//  1. Microsoft Graph (recommended): MEMQL_EMAIL_AZURE_TENANT_ID +
//     MEMQL_EMAIL_AZURE_CLIENT_ID + MEMQL_EMAIL_AZURE_CLIENT_SECRET +
//     MEMQL_EMAIL_SENDER all set → GraphSender. Legacy names
//     (`AZURE_*` / `MAIL_*`) are still accepted as a fallback.
//  2. SMTP (fallback): SMTP_HOST + SMTP_FROM_ADDR set → SMTPSender.
//  3. Neither set → LogSender (dev / no-delivery mode) on a LOCAL install,
//     and an ERROR on one that must really deliver mail (memql#4477).
//
// Case 3 is why this returns an error at all. The plug-in factory hands it
// straight to app.materializePlugins, which fatals -- so a cloud install now
// refuses to boot with a message naming the four env vars, instead of running
// for days in log-only mode telling every layer above that mail was sent.
// That is a deliberate exception to materializePlugins' "registrants that can
// run in degraded mode should return a no-op instance so the app still boots":
// degraded is precisely what must not happen here, because it is
// indistinguishable from working.
//
// Prefix is optional (e.g. "MEMQL_"). Pass "" for no prefix.
func NewSenderFromEnv(prefix string, logger *slog.Logger) (Sender, error) {
	reader := env.NewEnvReader(strings.TrimRight(prefix, "_"))

	// --- Graph path -----------------------------------------------------
	// Try the new EMAIL_*-prefixed names first; fall back per-field to
	// the pre-rename AZURE_* / MAIL_* names so installs that haven't
	// re-seeded (`go run ./scripts/secrets seed`) since the rename keep working.
	graphKeys := DefaultGraphEnvKeys()
	legacyKeys := LegacyGraphEnvKeys()
	readGraph := func(primary, legacy string) string {
		if v, ok := reader.String(primary); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		if v, ok := reader.String(legacy); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	graphCfg := GraphConfig{
		TenantId:     readGraph(graphKeys.TenantId, legacyKeys.TenantId),
		ClientId:     readGraph(graphKeys.ClientId, legacyKeys.ClientId),
		ClientSecret: readGraph(graphKeys.ClientSecret, legacyKeys.ClientSecret),
		SenderAddr:   readGraph(graphKeys.SenderAddr, legacyKeys.SenderAddr),
		FromName:     readGraph(graphKeys.FromName, legacyKeys.FromName),
	}
	graphReady := graphCfg.TenantId != "" &&
		graphCfg.ClientId != "" &&
		graphCfg.ClientSecret != "" &&
		graphCfg.SenderAddr != ""
	if graphReady {
		if logger != nil {
			logger.Info("email: using Microsoft Graph sender",
				"sender", graphCfg.SenderAddr,
				"tenantId", graphCfg.TenantId)
		}
		return NewGraphSender(graphCfg, nil, logger), nil
	}

	// --- SMTP fallback --------------------------------------------------
	smtpKeys := DefaultEnvKeys()
	host, _ := reader.String(smtpKeys.Host)
	host = strings.TrimSpace(host)
	if host == "" {
		if err := refuseLogOnlySelection(logger, "no sender configured"); err != nil {
			return nil, err
		}
		if logger != nil {
			logger.Info("email: no sender configured, using LogSender",
				"hint", "set MEMQL_EMAIL_AZURE_TENANT_ID / MEMQL_EMAIL_AZURE_CLIENT_ID / MEMQL_EMAIL_AZURE_CLIENT_SECRET / MEMQL_EMAIL_SENDER for Microsoft Graph, or SMTP_HOST / SMTP_PORT / SMTP_USERNAME / SMTP_PASSWORD / SMTP_FROM_ADDR for SMTP")
		}
		return NewLogSender(logger), nil
	}

	cfg := SMTPConfig{Host: host}
	if v, ok := reader.String(smtpKeys.Port); ok {
		cfg.Port = strings.TrimSpace(v)
	}
	if cfg.Port == "" {
		cfg.Port = "587"
	}
	if v, ok := reader.String(smtpKeys.Username); ok {
		cfg.Username = strings.TrimSpace(v)
	}
	if v, ok := reader.String(smtpKeys.Password); ok {
		cfg.Password = v
	}
	if v, ok := reader.String(smtpKeys.FromAddr); ok {
		cfg.FromAddr = strings.TrimSpace(v)
	}
	if v, ok := reader.String(smtpKeys.FromName); ok {
		cfg.FromName = strings.TrimSpace(v)
	}
	if cfg.FromAddr == "" {
		// The second fall-through, and the one that reads like a typo rather
		// than an omission: a host is set, so the operator plainly intended
		// to send. Refusing it is if anything more clearly right than the
		// no-configuration case above.
		if err := refuseLogOnlySelection(logger, "SMTP_HOST set but SMTP_FROM_ADDR missing"); err != nil {
			return nil, err
		}
		if logger != nil {
			logger.Warn("email: SMTP_HOST set but SMTP_FROM_ADDR missing; falling back to LogSender")
		}
		return NewLogSender(logger), nil
	}

	return NewSMTPSender(cfg, logger), nil
}

// refuseLogOnlySelection returns the boot refusal when this install must
// really deliver mail, and nil when log-only is a legitimate choice here. The
// nil case still logs, so a `.localhost` cluster and a break-glass cloud
// cluster both say out loud what they settled on.
func refuseLogOnlySelection(logger *slog.Logger, because string) error {
	if !DeliveryRequired() {
		return nil
	}
	err := RefuseLogOnly("boot")
	if logger != nil {
		logger.Error("email: refusing to boot in log-only mode", "because", because, "error", err)
	}
	return err
}
