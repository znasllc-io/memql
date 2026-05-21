package email

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"
)

// GuestInviteParams carries the per-invite data used to render and
// send the guest-invitation email. All fields are caller-sanitized
// strings; the renderer HTML-escapes anything that lands in the body.
//
// Resend == true switches the subject + body to the "this replaces
// any prior link" copy. The previous URL no longer authenticates
// post-#108 (memql#108 swapped the resend path from "replay the
// stored token" to "mint + rotate"); calling it out in the email
// keeps recipients from getting confused when an older link
// suddenly stops working.
type GuestInviteParams struct {
	To          string    // Guest's email address
	GuestName   string    // Optional suggested name; may be empty
	InviterName string    // Display name of the inviter
	SpaceName   string    // Display name of the space
	JoinURL     string    // Full join link, e.g. https://app.copresent.ai/join/<token>
	ExpiresAt   time.Time // Absolute expiration
	Resend      bool      // true when this is a resend (token was just rotated)
}

// formatRelativeDuration returns a human-friendly "in X minutes"
// style string for a future-pointing duration. Past or zero values
// collapse to "any moment now" so a slightly-stale expiresAt doesn't
// read as a negative duration in the email.
func formatRelativeDuration(d time.Duration) string {
	if d <= 30*time.Second {
		return "any moment now"
	}
	if d < time.Hour {
		mins := int(d.Round(time.Minute).Minutes())
		if mins == 1 {
			return "in 1 minute"
		}
		return fmt.Sprintf("in %d minutes", mins)
	}
	if d < 24*time.Hour {
		hours := int(d.Round(time.Hour).Hours())
		if hours == 1 {
			return "in 1 hour"
		}
		return fmt.Sprintf("in %d hours", hours)
	}
	days := int(d.Round(24*time.Hour).Hours() / 24)
	if days == 1 {
		return "in 1 day"
	}
	return fmt.Sprintf("in %d days", days)
}

// SendGuestInvite renders the guest-invite email and delivers it via
// the supplied Sender. Returns whatever error the sender returns.
func SendGuestInvite(ctx context.Context, sender Sender, p GuestInviteParams) error {
	if sender == nil {
		return fmt.Errorf("email: sender is nil")
	}

	greeting := "Hello"
	if name := strings.TrimSpace(p.GuestName); name != "" {
		greeting = "Hi " + name
	}

	inviter := strings.TrimSpace(p.InviterName)
	if inviter == "" {
		inviter = "Someone"
	}
	space := strings.TrimSpace(p.SpaceName)
	if space == "" {
		space = "a workspace"
	}

	// Expiration copy: prefer a "in X minutes" relative phrase + a
	// short absolute UTC timestamp. The relative phrase is the thing
	// humans actually read, and the absolute timestamp disambiguates
	// once the email has been sitting in an inbox for a while.
	expiresHuman := "soon"
	if !p.ExpiresAt.IsZero() {
		now := time.Now().UTC()
		d := p.ExpiresAt.Sub(now)
		relative := formatRelativeDuration(d)
		absolute := p.ExpiresAt.UTC().Format("Jan 2, 15:04 UTC")
		expiresHuman = fmt.Sprintf("%s (%s)", relative, absolute)
	}

	subject := fmt.Sprintf("%s invited you to %s", inviter, space)
	if p.Resend {
		subject = fmt.Sprintf("Updated link: %s invited you to %s", inviter, space)
	}

	resendTextNote := ""
	resendHTMLNote := ""
	if p.Resend {
		resendTextNote = "This link replaces any prior invitation we sent you. " +
			"If you received an earlier email, please ignore that link and use this one instead.\n\n"
		resendHTMLNote = `<p style="background:#fef3c7; border-left:4px solid #d97706; padding:12px 16px; margin:16px 0; font-size:14px; color:#78350f;">` +
			`<strong>Updated link inside.</strong> This link replaces any prior invitation we sent you. ` +
			`If you received an earlier email, please ignore that link and use this one instead.` +
			`</p>`
	}

	text := fmt.Sprintf(
		"%s,\n\n"+
			"%s has invited you to join \"%s\".\n\n"+
			"%s"+
			"Click the link below to join as a guest. You don't need an account:\n\n"+
			"  %s\n\n"+
			"This link expires %s. If you weren't expecting this invitation, just ignore this email.\n",
		greeting, inviter, space, resendTextNote, p.JoinURL, expiresHuman,
	)

	htmlBody := fmt.Sprintf(`<!doctype html>
<html>
  <body style="font-family: -apple-system, Segoe UI, Helvetica, sans-serif; max-width: 560px; margin: 0 auto; padding: 24px; color: #0f172a;">
    <p>%s,</p>
    <p><strong>%s</strong> has invited you to join <strong>%s</strong>.</p>
    %s
    <p>Click the button below to join as a guest. You don't need an account.</p>
    <p style="margin: 24px 0;">
      <a href="%s" style="display:inline-block; background:#4f46e5; color:#fff; padding:12px 20px; border-radius:9999px; text-decoration:none; font-weight:600;">Join %s</a>
    </p>
    <p style="font-size: 13px; color: #64748b;">Or paste this link into your browser:<br><span style="word-break: break-all;">%s</span></p>
    <p style="font-size: 13px; color: #64748b;">This link expires %s. If you weren't expecting this invitation, just ignore this email.</p>
  </body>
</html>`,
		html.EscapeString(greeting),
		html.EscapeString(inviter),
		html.EscapeString(space),
		resendHTMLNote,
		html.EscapeString(p.JoinURL),
		html.EscapeString(space),
		html.EscapeString(p.JoinURL),
		html.EscapeString(expiresHuman),
	)

	return sender.Send(ctx, Message{
		To:       p.To,
		Subject:  subject,
		TextBody: text,
		HTMLBody: htmlBody,
	})
}
