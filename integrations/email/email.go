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
	Send(ctx context.Context, msg Message, as SendAs) error
}

// SendAs names the mailbox a message leaves from (design D5, memql#4821).
//
// The ZERO VALUE means "the configured default sender", which is what every
// caller outside the campaigns worker passes -- so plurality is ADDITIVE and
// no transactional caller had to learn about it. A magic link, a guest invite
// and an outbound row all keep sending from the one mailbox the deployment
// authenticated as, and say so by passing nothing.
//
// It is a PARAMETER rather than a field on Message, and that placement is the
// whole design. Message stays From-less: a caller-supplied `From:` header is
// still refused by the renderer (mime.go's reservedHeaders), because an
// arbitrary From is a sender the mailbox did not authenticate as and that is
// exactly the SPF/DKIM alignment the campaign path depends on being
// structural. An identity is not free text the message carries -- it is a
// mailbox the transport must actually be able to send as, which is a fact
// about the TRANSPORT, so it travels beside the message and each Sender
// decides whether it can honour it. Graph can (a different `/users/{addr}`
// path); SMTP cannot (AUTH is bound to one mailbox) and refuses.
//
// Nothing here validates that the address is a mailbox the credential may
// send as. It cannot: that is Exchange ApplicationAccessPolicy state in
// somebody else's tenant, and the honest check is the provider's 403 landing
// on the campaign's lastError (design D7).
type SendAs struct {
	// Address is the mailbox UPN to send as -- the value that becomes both
	// the Graph `/users/{address}/sendMail` path segment and the address
	// half of the From header.
	Address string
	// FromName is the display name half of the From header. May be set on
	// its own: a SendAs carrying only a FromName means "the default mailbox,
	// under this display name", which is what a campaign overriding
	// `fromName` and nothing else asks for.
	FromName string
}

// IsZero reports whether this is the "use the configured default" sentinel.
//
// Whitespace counts as empty. A stored identity row whose address is a stray
// space is not an identity, and treating it as one would build
// `/users/%20/sendMail` and fail with a message about a mailbox nobody named.
func (s SendAs) IsZero() bool {
	return strings.TrimSpace(s.Address) == "" && strings.TrimSpace(s.FromName) == ""
}

// Validate rejects an identity that cannot be put on a header line.
//
// Called by every Sender before the wire, for the same reason Message.Validate
// is: FromName became CALLER-INFLUENCED with D6 (a campaign's `fromName`
// reaches it), and the structured Graph payload does not pass through
// RenderRFC5322's header-injection barrier -- it JSON-encodes the name into a
// body. So the barrier has to exist here too, or one of the two Graph request
// forms is checked and the other is not.
func (s SendAs) Validate() error {
	if headerUnsafe(s.Address) {
		return errors.New("email: SendAs.Address contains illegal control characters (header injection)")
	}
	if headerUnsafe(s.FromName) {
		return errors.New("email: SendAs.FromName contains illegal control characters (header injection)")
	}
	addr := strings.TrimSpace(s.Address)
	if addr != "" && !strings.Contains(addr, "@") {
		return fmt.Errorf("email: SendAs.Address %q is not a valid mailbox address", s.Address)
	}
	return nil
}

// resolveIdentity folds a SendAs over a configured default pair and returns
// the (address, displayName) actually to be used.
//
// ONE function, called once per send, because the alternative is what D6
// found: an address resolved in three places drifts, and the drift is a
// message whose envelope says one mailbox and whose From header says another
// -- which is a DMARC alignment failure that looks like a deliverability
// mystery rather than a bug.
//
// The two halves fall back INDEPENDENTLY. A SendAs carrying only a FromName
// keeps the default address, which is what "send from the usual mailbox under
// this campaign's display name" means; an identity carrying only an address
// takes the deployment's default display name rather than going nameless.
func resolveIdentity(as SendAs, defaultAddr, defaultName string) (string, string) {
	addr := strings.TrimSpace(as.Address)
	if addr == "" {
		addr = defaultAddr
	}
	name := strings.TrimSpace(as.FromName)
	if name == "" {
		name = defaultName
	}
	return addr, name
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
//
// A non-default `as` is REFUSED here rather than honoured (design D5).
// SMTP AUTH binds this connection to exactly one mailbox: the relay
// authenticated s.cfg.Username and will either reject a mismatched envelope
// sender outright or, worse, accept it and let the receiving domain fail
// SPF/DMARC -- a message that leaves successfully and lands in a spam folder,
// which is the failure mode nobody sees. So the refusal is PERMANENT and
// typed: no amount of waiting turns an SMTP relay into a multi-mailbox one,
// and a retryable classification would park a campaign forever on an install
// that can never send it.
//
// A SendAs that carries only a display name is fine and is honoured: the
// address is unchanged, so the authentication story is unchanged, and only
// the From phrase differs.
func (s *SMTPSender) Send(ctx context.Context, msg Message, as SendAs) error {
	if err := msg.Validate(); err != nil {
		return err
	}
	if err := as.Validate(); err != nil {
		return err
	}

	from := s.cfg.FromAddr
	if requested := strings.TrimSpace(as.Address); requested != "" && !strings.EqualFold(requested, strings.TrimSpace(from)) {
		err := fmt.Errorf(
			"email: this node sends over SMTP, whose AUTH is bound to the single mailbox %q, so it cannot send as %q. "+
				"Configure Microsoft Graph (%s, %s, %s, %s -- all four) to send from more than one identity, "+
				"or point this campaign at the default sending identity",
			from, requested,
			DefaultGraphEnvKeys().TenantId, DefaultGraphEnvKeys().ClientId,
			DefaultGraphEnvKeys().ClientSecret, DefaultGraphEnvKeys().SenderAddr)
		return &SendError{Permanent: true, Detail: err.Error(), Cause: err}
	}
	_, fromName := resolveIdentity(as, from, s.cfg.FromName)
	fromHeader := FromHeader(from, fromName)

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
//
// The identity is logged as well as the recipient. On a local install the log
// line IS the delivered message -- it is the only record that a send happened
// -- so omitting which mailbox it would have left from would make the one
// surface an operator can inspect blind to the exact thing D5 added.
func (l *LogSender) Send(_ context.Context, msg Message, as SendAs) error {
	if err := msg.Validate(); err != nil {
		return err
	}
	if err := as.Validate(); err != nil {
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
	sendAsAddr, sendAsName := resolveIdentity(as, "(configured default)", "")
	l.logger.Info("email (log-only mode, not delivered)",
		"to", msg.To,
		"subject", msg.Subject,
		"sendAs", sendAsAddr,
		"fromName", sendAsName,
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

	// ONE walk over the declared manifest (memql#4825). Which lanes exist,
	// which slots each needs, which are secret and which legacy names are
	// still honoured are all declared in emailconfig.go, and the lazy row
	// resolver and the status reporter read the same declaration -- so the
	// three cannot disagree about what "configured" means.
	res := resolveEmailConfig(context.Background(), ConfigResolver{
		Env: func(name string) (string, bool) { return reader.String(name) },
	})
	if sender := senderFor(res, logger); sender != nil {
		return sender, nil
	}

	// Nothing resolved whole. The remaining job is to say WHY in the terms
	// the operator will recognise, and the three cases read very
	// differently.
	//
	// A SPLIT lane first, because it is the one that looks configured: every
	// value is set and the sender is still log-only. It used to produce the
	// same silent fall-through as an empty environment.
	because := "no sender configured"
	for _, lane := range res.Lanes {
		switch {
		case lane.Split:
			because = "the " + lane.Lane.Name + " lane's settings are present but split across the environment and stored rows"
		case lane.Partial:
			// The case that reads like a typo rather than an omission:
			// somebody started this lane, so they plainly intended to send.
			// Refusing it is if anything more clearly right than the
			// no-configuration case. Generalized from the hardcoded
			// "SMTP_HOST set but SMTP_FROM_ADDR missing" (memql#4825), which
			// only ever noticed one of the eleven ways to stop half way.
			because = "the " + lane.Lane.Name + " lane is partly configured; missing " + strings.Join(lane.Missing, ", ")
		default:
			continue
		}
		break
	}
	if err := refuseLogOnlySelection(logger, because); err != nil {
		return nil, err
	}
	if logger != nil {
		if because == "no sender configured" {
			logger.Info("email: no sender configured, using LogSender",
				"hint", "set "+strings.Join(laneEnvVars(res, LaneGraph), " / ")+" for Microsoft Graph, or "+
					strings.Join(laneEnvVars(res, LaneSMTP), " / ")+" for SMTP")
		} else {
			logger.Warn("email: falling back to LogSender", "because", because)
		}
	}
	return NewLogSender(logger), nil
}

// laneEnvVars lists a lane's REQUIRED variables, for the hint an operator
// reads when nothing is configured. Required only: naming the optional ones
// makes a five-item list look like five obligations.
func laneEnvVars(res ConfigResolution, name string) []string {
	out := []string{}
	for _, lane := range res.Lanes {
		if lane.Lane.Name != name {
			continue
		}
		for _, slot := range lane.Slots {
			if slot.Slot.Required {
				out = append(out, slot.Slot.EnvVar)
			}
		}
	}
	return out
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
