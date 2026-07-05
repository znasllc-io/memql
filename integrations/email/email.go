// Package email is a minimal outbound-mail helper used by memQL for
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
	"time"

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
}

// Validate reports missing or obviously malformed fields.
func (m Message) Validate() error {
	if strings.TrimSpace(m.To) == "" {
		return errors.New("email: To is required")
	}
	if !strings.Contains(m.To, "@") {
		return fmt.Errorf("email: To %q is not a valid address", m.To)
	}
	if strings.TrimSpace(m.Subject) == "" {
		return errors.New("email: Subject is required")
	}
	if strings.TrimSpace(m.TextBody) == "" {
		return errors.New("email: TextBody is required")
	}
	return nil
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
	fromHeader := from
	if s.cfg.FromName != "" {
		fromHeader = fmt.Sprintf("%s <%s>", s.cfg.FromName, from)
	}

	var body strings.Builder
	boundary := fmt.Sprintf("memql-email-%d", time.Now().UnixNano())
	writeHeaders := func(h map[string]string) {
		for k, v := range h {
			body.WriteString(k)
			body.WriteString(": ")
			body.WriteString(v)
			body.WriteString("\r\n")
		}
	}

	if msg.HTMLBody == "" {
		writeHeaders(map[string]string{
			"From":         fromHeader,
			"To":           msg.To,
			"Subject":      msg.Subject,
			"MIME-Version": "1.0",
			"Content-Type": "text/plain; charset=UTF-8",
		})
		body.WriteString("\r\n")
		body.WriteString(msg.TextBody)
	} else {
		writeHeaders(map[string]string{
			"From":         fromHeader,
			"To":           msg.To,
			"Subject":      msg.Subject,
			"MIME-Version": "1.0",
			"Content-Type": fmt.Sprintf(`multipart/alternative; boundary="%s"`, boundary),
		})
		body.WriteString("\r\n")
		body.WriteString("--")
		body.WriteString(boundary)
		body.WriteString("\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n")
		body.WriteString(msg.TextBody)
		body.WriteString("\r\n--")
		body.WriteString(boundary)
		body.WriteString("\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n")
		body.WriteString(msg.HTMLBody)
		body.WriteString("\r\n--")
		body.WriteString(boundary)
		body.WriteString("--\r\n")
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

	if err := smtp.SendMail(addr, auth, from, []string{msg.To}, []byte(body.String())); err != nil {
		return fmt.Errorf("smtp SendMail to %s: %w", s.cfg.Host, err)
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
}

// NewLogSender returns a Sender that logs messages at Info level.
func NewLogSender(logger *slog.Logger) *LogSender {
	return &LogSender{logger: logger}
}

// Send logs the message without attempting delivery.
func (l *LogSender) Send(_ context.Context, msg Message) error {
	if err := msg.Validate(); err != nil {
		return err
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
//  3. Neither set → LogSender (dev / no-delivery mode).
//
// Prefix is optional (e.g. "MEMQL_"). Pass "" for no prefix.
func NewSenderFromEnv(prefix string, logger *slog.Logger) Sender {
	reader := env.NewEnvReader(strings.TrimRight(prefix, "_"))

	// --- Graph path -----------------------------------------------------
	// Try the new EMAIL_*-prefixed names first; fall back per-field to
	// the pre-rename AZURE_* / MAIL_* names so installs that haven't
	// run `make secrets-seed` since the rename keep working.
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
		return NewGraphSender(graphCfg, nil, logger)
	}

	// --- SMTP fallback --------------------------------------------------
	smtpKeys := DefaultEnvKeys()
	host, _ := reader.String(smtpKeys.Host)
	host = strings.TrimSpace(host)
	if host == "" {
		if logger != nil {
			logger.Info("email: no sender configured, using LogSender",
				"hint", "set MEMQL_EMAIL_AZURE_TENANT_ID / MEMQL_EMAIL_AZURE_CLIENT_ID / MEMQL_EMAIL_AZURE_CLIENT_SECRET / MEMQL_EMAIL_SENDER for Microsoft Graph, or SMTP_HOST / SMTP_PORT / SMTP_USERNAME / SMTP_PASSWORD / SMTP_FROM_ADDR for SMTP")
		}
		return NewLogSender(logger)
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
		if logger != nil {
			logger.Warn("email: SMTP_HOST set but SMTP_FROM_ADDR missing; falling back to LogSender")
		}
		return NewLogSender(logger)
	}

	return NewSMTPSender(cfg, logger)
}
