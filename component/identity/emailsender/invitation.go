package emailsender

// The invitation email (memql#4584).
//
// # Why this lives beside SendMagicLink rather than in adminops
//
// identity has exactly ONE way to put a message on the wire: the
// engine-resident email integration plug-in, resolved off the integration
// registry at call time by resolveSender() below. SendMagicLink has used it
// since the beginning, which is why an operator tailing a production node sees
// `"msg":"email sent via graph","sender":"noreply@<domain>"` for every sign-in.
//
// An invitation email that reached Graph by any other route -- a second client,
// a second config surface, a direct integrations/email construction inside
// adminops -- would be a second mail path with its own failure modes, its own
// credentials to keep in sync, and its own silence when misconfigured. The
// whole point of memql#4477 was that mail which reports success without
// delivering is worse than mail that refuses. Two paths means two places for
// that to regress. So this file adds a BODY and a SUBJECT, and borrows the
// transport wholesale: resolveSender() and noSender() are the same functions
// SendMagicLink calls, unchanged.
//
// # Why adminops does not import this package
//
// It cannot see the engine. adminops.Service.Engine is identity.EngineExecutor
// -- a two-method interface over Execute -- not *memqlengine.MemQLEngine, and
// the integration registry hangs off the latter. Rather than widen that
// interface (which would give the whole admin surface access to every
// integration in order to send one email), adminops takes a FUNCTION SEAM the
// wiring layer fills, exactly as it already does for IdentityBaseURL and
// RegistrationPolicy. app/transport.go points that seam here.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/integrations/email"
)

// UserInvitationInput is one invitation email.
//
// Every field is supplied by the caller rather than read from config here, for
// the reason magiclink.SendInput carries its own brand fields: this shim is
// constructed on the identity node AND on the bff (which is where an admin
// actually clicks), and the two do not resolve cluster branding from the same
// place. The caller that knows which node it is on answers.
type UserInvitationInput struct {
	// Email is the invitee. The address the invitation authorizes, and the
	// only address this message may go to.
	Email string
	// InviterName is who issued it -- the display name or address recorded on
	// the row. Empty renders as an unattributed invitation rather than a
	// fabricated one.
	InviterName string
	// Role is the cluster role the recipient lands with. Empty means the
	// cluster's default, and the copy says so rather than naming a role the
	// server never promised.
	Role string
	// LinkURL is the redemption link. A CREDENTIAL: it is the product of the
	// issuing call, exists nowhere else, and putting it anywhere but in this
	// message is a leak.
	LinkURL string
	// ExpiresAt is when the link stops working.
	ExpiresAt time.Time
	// BrandName defaults to "MemQL" when empty, matching the magic-link
	// issuer's own default so the two messages do not disagree about what
	// cluster they came from.
	BrandName string
	// BrandPrimaryColor and BrandLogoDataURI style the HTML body. Both
	// optional; the template has a default colour and omits the logo block.
	BrandPrimaryColor string
	BrandLogoDataURI  string
}

// SendUserInvitation delivers the invitation email through the same integration
// plug-in SendMagicLink uses.
//
// Returns the sender's error unchanged. The CALLER decides what a delivery
// failure means, and for an invitation the answer is deliberately not "fail the
// operation" -- see the comment in adminops.IssueUserInvitation. This function
// therefore neither swallows the error nor escalates it.
func (s *EngineEmailSender) SendUserInvitation(ctx context.Context, in UserInvitationInput) error {
	if s == nil {
		return errors.New("emailsender: nil")
	}
	if strings.TrimSpace(in.Email) == "" {
		return errors.New("emailsender: invitation has no recipient")
	}
	if strings.TrimSpace(in.LinkURL) == "" {
		// Refused rather than sent, because an invitation email with no link
		// is an email that tells somebody they have been invited and gives
		// them no way in. That is worse than no email: they will wait for it.
		return errors.New("emailsender: invitation has no link")
	}

	brand := strings.TrimSpace(in.BrandName)
	if brand == "" {
		brand = "MemQL"
	}
	in.BrandName = brand

	subject := fmt.Sprintf("You have been invited to %s", brand)

	sender := s.resolveSender()
	if sender == nil {
		// Same answer SendMagicLink gives, via the same function: on a local
		// install the log IS the inbox and the link has to surface somewhere a
		// developer can copy it from; on an install that must really deliver
		// mail, noSender refuses instead of reporting a send that did not
		// happen (memql#4477).
		return s.noSender("user invitation", in.Email, subject, slog.String("link", in.LinkURL))
	}

	// The zero SendAs -- the deployment's configured mailbox (email D5).
	return sender.Send(ctx, email.Message{
		To:       in.Email,
		Subject:  subject,
		TextBody: buildInvitationText(in),
		HTMLBody: buildInvitationHTML(in),
	}, email.SendAs{})
}

// roleSentence renders the granted role for both bodies. One function so the
// text and HTML halves cannot drift into promising different things.
func roleSentence(role string) string {
	role = strings.TrimSpace(role)
	if role == "" {
		return "You will join with the cluster's default role."
	}
	return "You will join with the " + role + " role."
}

// expirySentence renders the deadline. Absolute UTC rather than "in 7 days",
// because an invitation email can sit unread for days and a relative deadline
// read late is a wrong deadline.
func expirySentence(expiresAt time.Time) string {
	if expiresAt.IsZero() {
		return ""
	}
	return "This invitation expires on " + expiresAt.UTC().Format("2 January 2006 at 15:04 MST") + "."
}

// inviterSentence attributes the invitation, or says nothing. An empty
// InviterName renders no clause at all rather than "invited by " with a blank
// after it: a message that looks broken invites less trust than a shorter one.
func inviterSentence(inviterName, brand string) string {
	if strings.TrimSpace(inviterName) == "" {
		return fmt.Sprintf("You have been invited to join %s.", brand)
	}
	return fmt.Sprintf("%s has invited you to join %s.", strings.TrimSpace(inviterName), brand)
}

// buildInvitationText assembles the plain-text body. Email clients always have
// a text alternative even when they render HTML.
func buildInvitationText(in UserInvitationInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You have been invited to %s\n\n", in.BrandName)
	b.WriteString(inviterSentence(in.InviterName, in.BrandName))
	b.WriteString(" ")
	b.WriteString(roleSentence(in.Role))
	if exp := expirySentence(in.ExpiresAt); exp != "" {
		b.WriteString(" ")
		b.WriteString(exp)
	}
	b.WriteString("\n\nOpen the link below to accept the invitation and sign in:\n\n")
	b.WriteString(in.LinkURL)
	b.WriteString("\n\nIf you were not expecting this invitation, you can safely ignore this email -- the link admits nobody until it is used.\n")
	return b.String()
}

// buildInvitationHTML is the HTML variant, using the same brand chrome as the
// magic-link and bootstrap templates so the three messages from one cluster
// look like they came from one cluster.
func buildInvitationHTML(in UserInvitationInput) string {
	color := in.BrandPrimaryColor
	if color == "" {
		color = "#4f46e5"
	}
	tmpl := template.Must(template.New("invite").Parse(htmlInvitationTemplate))
	var buf bytes.Buffer
	_ = tmpl.Execute(&buf, map[string]any{
		"BrandName": in.BrandName,
		"LinkURL":   in.LinkURL,
		"Color":     color,
		"Logo":      in.BrandLogoDataURI,
		"Inviter":   inviterSentence(in.InviterName, in.BrandName),
		"Role":      roleSentence(in.Role),
		"Expiry":    expirySentence(in.ExpiresAt),
	})
	return buf.String()
}

const htmlInvitationTemplate = `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="font-family:Helvetica,Arial,sans-serif;margin:0;padding:24px;background:#f7f7f8;">
  <table cellpadding="0" cellspacing="0" border="0" align="center" width="520" style="background:#ffffff;border-radius:8px;padding:32px;">
    {{if .Logo}}<tr><td style="padding-bottom:16px;"><img src="{{.Logo}}" alt="{{.BrandName}}" style="max-height:40px;"></td></tr>{{end}}
    <tr><td style="font-size:20px;font-weight:600;color:#111827;padding-bottom:8px;">You have been invited to {{.BrandName}}</td></tr>
    <tr><td style="font-size:14px;color:#374151;padding-bottom:16px;line-height:1.5;">{{.Inviter}} {{.Role}}</td></tr>
    {{if .Expiry}}<tr><td style="font-size:14px;color:#374151;padding-bottom:24px;">{{.Expiry}}</td></tr>{{end}}
    <tr><td style="padding-bottom:24px;"><a href="{{.LinkURL}}" style="display:inline-block;padding:12px 20px;background:{{.Color}};color:#ffffff;text-decoration:none;border-radius:6px;font-weight:500;">Accept the invitation</a></td></tr>
    <tr><td style="font-size:12px;color:#6b7280;">If the button doesn't work, copy and paste this URL into your browser:<br><span style="word-break:break-all;color:#4b5563;">{{.LinkURL}}</span></td></tr>
    <tr><td style="font-size:12px;color:#9ca3af;padding-top:24px;border-top:1px solid #e5e7eb;margin-top:16px;">If you were not expecting this invitation, you can safely ignore this email &mdash; the link admits nobody until it is used.</td></tr>
  </table>
</body>
</html>`
