// Package emailsender bridges the identity service's magic-link
// issuer to the engine-resident email integration plug-in. Lives in
// its own subpackage (rather than the identity root) so it can import
// both component/identity/magiclink (for the Sender interface) and
// integrations/email (for the Sender implementation) without creating
// an import cycle through the parent package.
package emailsender

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"strings"
	"time"

	"github.com/visionarys-io/memql/component/identity"
	"github.com/visionarys-io/memql/component/identity/magiclink"
	memqlengine "github.com/visionarys-io/memql/component/memql"
	"github.com/visionarys-io/memql/integrations/email"
)

// EngineEmailSender satisfies magiclink.Sender by resolving the email
// integration plug-in off the engine's integration registry at call
// time. If the registry doesn't have one wired (dev / pre-plugin
// builds), the Sender falls back to a slog-only LogSender so the
// magic link still surfaces somewhere developers can copy it from.
type EngineEmailSender struct {
	Engine *memqlengine.MemQLEngine
	Logger *slog.Logger
	Cfg    identity.Config
}

// New constructs the shim. Passing the engine (rather than just a
// Sender) lets us late-resolve the integration plug-in after the
// engine has finished its bus-bootstrap.
func New(engine *memqlengine.MemQLEngine, logger *slog.Logger, cfg identity.Config) *EngineEmailSender {
	return &EngineEmailSender{Engine: engine, Logger: logger, Cfg: cfg}
}

// SendMagicLink implements magiclink.Sender. The bootstrap variant
// of the email (claim-ownership wording) is selected when
// SendInput.Bootstrap is true; that flag is only set on /setup
// wizard owner-mints, never on regular sign-in or access-request
// paths, so existing users always see the standard sign-in copy.
func (s *EngineEmailSender) SendMagicLink(ctx context.Context, in magiclink.SendInput) error {
	if s == nil {
		return errors.New("emailsender: nil")
	}

	var subject string
	var textBody, htmlBody string
	if in.Bootstrap {
		subject = fmt.Sprintf("Claim ownership of %s", in.BrandName)
		textBody = buildBootstrapText(in)
		htmlBody = buildBootstrapHTML(in)
	} else {
		subject = fmt.Sprintf("Sign in to %s", in.BrandName)
		textBody = buildMagicLinkText(in)
		htmlBody = buildMagicLinkHTML(in)
	}

	// Dev-mode escape hatch: when BaseURL is HTTP (localhost dev), log
	// the magic-link URL to slog INFO. Email delivery to public domains
	// can be flaky during dev (spam filters, sender-reputation warnups,
	// etc.), and the operator running the wizard against a fresh cluster
	// can't bootstrap if the email never arrives. Logging the URL is a
	// no-op in production (HTTPS BaseURL skips this branch).
	if s.Logger != nil && isDevBaseURL(s.Cfg.BaseURL) {
		s.Logger.Info("identity: DEV magic link (also sent via configured email)",
			slog.String("to", in.Email),
			slog.String("link", in.LinkURL),
		)
	}

	sender := s.resolveSender()
	if sender == nil {
		// Last-resort fallback: log to slog at info level. The link
		// shows up in operator logs so magic-link auth still "works"
		// in dev when no email plug-in is wired.
		if s.Logger != nil {
			s.Logger.Info("identity: no email sender configured, logging magic link",
				slog.String("to", in.Email),
				slog.String("subject", subject),
				slog.String("link", in.LinkURL),
			)
		}
		return nil
	}

	return sender.Send(ctx, email.Message{
		To:       in.Email,
		Subject:  subject,
		TextBody: textBody,
		HTMLBody: htmlBody,
	})
}

// isDevBaseURL returns true when the configured public origin is plain
// HTTP (localhost or other non-TLS dev). In production the BaseURL is
// always HTTPS, so this function returns false and the magic-link URL
// is never logged.
func isDevBaseURL(baseURL string) bool {
	return strings.HasPrefix(strings.ToLower(baseURL), "http://")
}

// resolveSender pulls the email Sender out of the engine's integration
// registry. Returns nil if the integration isn't registered.
func (s *EngineEmailSender) resolveSender() email.Sender {
	if s == nil || s.Engine == nil {
		return nil
	}
	prov := s.Engine.Integrations().Provider("email")
	if prov == nil {
		return nil
	}
	emailInt, ok := prov.(*email.Integration)
	if !ok || emailInt == nil {
		return nil
	}
	return emailInt.SenderAccess()
}

// buildMagicLinkText assembles the plain-text body. Email clients
// always have a text alternative even when they render HTML.
func buildMagicLinkText(in magiclink.SendInput) string {
	expiresMin := int(in.ExpiresIn / time.Minute)
	if expiresMin <= 0 {
		expiresMin = 10
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Sign in to %s\n\n", in.BrandName)
	b.WriteString("Click the link below to finish signing in. The link expires in ")
	fmt.Fprintf(&b, "%d minutes.\n\n", expiresMin)
	b.WriteString(in.LinkURL)
	b.WriteString("\n\nIf you did not request this link, you can safely ignore this email.\n")
	return b.String()
}

// buildMagicLinkHTML assembles a minimally-styled HTML body using the
// configured brand color + optional logo.
func buildMagicLinkHTML(in magiclink.SendInput) string {
	expiresMin := int(in.ExpiresIn / time.Minute)
	if expiresMin <= 0 {
		expiresMin = 10
	}
	color := in.BrandPrimaryColor
	if color == "" {
		color = "#4f46e5"
	}

	tmpl := template.Must(template.New("ml").Parse(htmlMagicLinkTemplate))
	var buf bytes.Buffer
	_ = tmpl.Execute(&buf, map[string]any{
		"BrandName":  in.BrandName,
		"LinkURL":    in.LinkURL,
		"ExpiresMin": expiresMin,
		"Color":      color,
		"Logo":       in.BrandLogoDataURI,
	})
	return buf.String()
}

// buildBootstrapText assembles the plain-text body for a /setup
// wizard owner-mint email. Distinguished from the regular sign-in
// copy by claim-ownership language: this email says "you're about
// to take ownership of a brand-new cluster," not "welcome back."
func buildBootstrapText(in magiclink.SendInput) string {
	expiresMin := int(in.ExpiresIn / time.Minute)
	if expiresMin <= 0 {
		expiresMin = 10
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Claim ownership of %s\n\n", in.BrandName)
	fmt.Fprintf(&b, "Someone (probably you) just ran the first-run setup wizard for %s and entered this email as the cluster-owner address. Click the link below to confirm and finish setup -- you'll be signed in as the cluster owner. The link expires in %d minutes.\n\n", in.BrandName, expiresMin)
	b.WriteString(in.LinkURL)
	b.WriteString("\n\nIf you did not run the setup wizard, you can safely ignore this email -- the cluster won't be claimed without this confirmation.\n")
	return b.String()
}

// buildBootstrapHTML is the HTML variant of buildBootstrapText.
// Same template + brand chrome as the regular magic link, just
// with claim-ownership copy and a more prominent confirm button.
func buildBootstrapHTML(in magiclink.SendInput) string {
	expiresMin := int(in.ExpiresIn / time.Minute)
	if expiresMin <= 0 {
		expiresMin = 10
	}
	color := in.BrandPrimaryColor
	if color == "" {
		color = "#0433ff"
	}
	tmpl := template.Must(template.New("bootstrap").Parse(htmlBootstrapTemplate))
	var buf bytes.Buffer
	_ = tmpl.Execute(&buf, map[string]any{
		"BrandName":  in.BrandName,
		"LinkURL":    in.LinkURL,
		"ExpiresMin": expiresMin,
		"Color":      color,
		"Logo":       in.BrandLogoDataURI,
	})
	return buf.String()
}

const htmlBootstrapTemplate = `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="font-family:Helvetica,Arial,sans-serif;margin:0;padding:24px;background:#f7f7f8;">
  <table cellpadding="0" cellspacing="0" border="0" align="center" width="520" style="background:#ffffff;border-radius:8px;padding:32px;">
    {{if .Logo}}<tr><td style="padding-bottom:16px;"><img src="{{.Logo}}" alt="{{.BrandName}}" style="max-height:40px;"></td></tr>{{end}}
    <tr><td style="font-size:20px;font-weight:600;color:#111827;padding-bottom:8px;">Claim ownership of {{.BrandName}}</td></tr>
    <tr><td style="font-size:14px;color:#374151;padding-bottom:16px;line-height:1.5;">Someone (probably you) just ran the first-run setup wizard for <strong>{{.BrandName}}</strong> and entered this email as the cluster-owner address. Click the button below to confirm and finish setup &mdash; you'll be signed in as the cluster owner.</td></tr>
    <tr><td style="font-size:14px;color:#374151;padding-bottom:24px;">This link expires in {{.ExpiresMin}} minutes.</td></tr>
    <tr><td style="padding-bottom:24px;"><a href="{{.LinkURL}}" style="display:inline-block;padding:12px 20px;background:{{.Color}};color:#ffffff;text-decoration:none;border-radius:6px;font-weight:500;">Confirm and claim ownership</a></td></tr>
    <tr><td style="font-size:12px;color:#6b7280;">If the button doesn't work, copy and paste this URL into your browser:<br><span style="word-break:break-all;color:#4b5563;">{{.LinkURL}}</span></td></tr>
    <tr><td style="font-size:12px;color:#9ca3af;padding-top:24px;border-top:1px solid #e5e7eb;margin-top:16px;">If you did not run the setup wizard, you can safely ignore this email &mdash; the cluster won't be claimed without this confirmation.</td></tr>
  </table>
</body>
</html>`

const htmlMagicLinkTemplate = `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="font-family:Helvetica,Arial,sans-serif;margin:0;padding:24px;background:#f7f7f8;">
  <table cellpadding="0" cellspacing="0" border="0" align="center" width="520" style="background:#ffffff;border-radius:8px;padding:32px;">
    {{if .Logo}}<tr><td style="padding-bottom:16px;"><img src="{{.Logo}}" alt="{{.BrandName}}" style="max-height:40px;"></td></tr>{{end}}
    <tr><td style="font-size:20px;font-weight:600;color:#111827;padding-bottom:8px;">Sign in to {{.BrandName}}</td></tr>
    <tr><td style="font-size:14px;color:#374151;padding-bottom:24px;">Click the button below to finish signing in. This link will expire in {{.ExpiresMin}} minutes.</td></tr>
    <tr><td style="padding-bottom:24px;"><a href="{{.LinkURL}}" style="display:inline-block;padding:12px 20px;background:{{.Color}};color:#ffffff;text-decoration:none;border-radius:6px;font-weight:500;">Sign in</a></td></tr>
    <tr><td style="font-size:12px;color:#6b7280;">If the button doesn't work, copy and paste this URL into your browser:<br><span style="word-break:break-all;color:#4b5563;">{{.LinkURL}}</span></td></tr>
    <tr><td style="font-size:12px;color:#9ca3af;padding-top:24px;border-top:1px solid #e5e7eb;margin-top:16px;">If you did not request this link, you can safely ignore this email.</td></tr>
  </table>
</body>
</html>`
