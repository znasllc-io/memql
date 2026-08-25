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

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/magiclink"
	"github.com/znasllc-io/memql/integrations/email"
)

// signin_notice.go -- the new-sign-in notification email (memql#4305).
//
// # What it is for
//
// Every new session mails the account: when, from where, and with what. It
// is the cheapest detection control in the magic-link hardening design and
// the only one that reaches the account holder AFTER somebody else has
// already signed in. Device binding stops a colleague riding your link; it
// does nothing about a colleague requesting their own link to the shared
// address they can also read. This is what turns that from invisible into
// noticed -- and on a shared mailbox it lands in front of everyone, which is
// the point rather than a side effect.
//
// # No action link, and this is a decision
//
// The obvious next line -- "wasn't you? click here to sign out everywhere"
// -- is refused by the design (section 7.1) and must not be added back. An
// unauthenticated revoke link mailed to a shared mailbox is a
// denial-of-service handle: anyone who can read the mailbox can sign
// everybody out, repeatedly, using a link we delivered to them. The copy
// says to sign in and revoke from the profile page instead. A test asserts
// the body contains no URL.
//
// # Not throttled, and it does not need to be
//
// A session is minted at sign-in, not per request, and sessions last for the
// refresh window. Refresh ROTATIONS never reach here -- they create no row --
// so the volume is one message per actual sign-in.

// SendNewSignIn implements identity.SignInNotifier.
func (s *EngineEmailSender) SendNewSignIn(ctx context.Context, in identity.SignInNotice) error {
	if s == nil {
		return errors.New("emailsender: nil")
	}
	if strings.TrimSpace(in.Email) == "" {
		return errors.New("emailsender: new-sign-in notice needs a recipient")
	}

	brand := in.BrandName
	if brand == "" {
		brand = "MemQL"
	}
	subject := fmt.Sprintf("New sign-in to %s", brand)

	sender := s.resolveSender()
	if sender == nil {
		return s.noSender("new-sign-in notice", in.Email, subject,
			slog.String("source", in.Source),
			slog.String("ip", in.SourceIP))
	}

	return sender.Send(ctx, email.Message{
		To:       in.Email,
		Subject:  subject,
		TextBody: buildNewSignInText(brand, in),
		HTMLBody: buildNewSignInHTML(brand, in),
	})
}

// signInFacts renders the three lines a reader actually judges by.
//
// A missing fact prints as "unknown" rather than as an empty line: "IP
// address: " reads like a bug, and a reader deciding whether to be alarmed
// should be able to tell "we don't know" from "we forgot to say".
func signInFacts(in identity.SignInNotice) (when, from, client, how string) {
	at := in.At
	if at.IsZero() {
		at = time.Now()
	}
	when = at.UTC().Format("2 January 2006 at 15:04 MST")
	from = strings.TrimSpace(in.SourceIP)
	if from == "" {
		from = "unknown"
	}
	client = strings.TrimSpace(in.ClientLabel)
	if client == "" {
		client = "unknown"
	}
	how = describeSource(in.Source)
	return when, from, client, how
}

// describeSource turns the authSession source enum into something a person
// can read. An unrecognised value falls back to the raw string rather than
// to silence -- a session whose origin we cannot name is exactly the one a
// reader most wants named.
func describeSource(source string) string {
	switch strings.TrimSpace(source) {
	case "oidc_cookie":
		return "a web browser"
	case "bff_exchange":
		return "an application"
	case "device_code":
		return "a device sign-in code"
	case "":
		return "an unknown client"
	default:
		return source
	}
}

func buildNewSignInText(brand string, in identity.SignInNotice) string {
	when, from, client, how := signInFacts(in)
	var b strings.Builder
	fmt.Fprintf(&b, "New sign-in to %s\n\n", brand)
	fmt.Fprintf(&b, "Someone signed in to this account using %s.\n\n", how)
	fmt.Fprintf(&b, "When:       %s\n", when)
	fmt.Fprintf(&b, "IP address: %s\n", from)
	fmt.Fprintf(&b, "Client:     %s\n\n", client)
	b.WriteString("If this was you, nothing more to do.\n\n")
	b.WriteString("If it was not, sign in and revoke sessions from your profile page, then set up a passkey. ")
	b.WriteString("If this address is a shared mailbox, consider turning on passkey-only sign-in so a sign-in link alone is no longer enough to enter the account.\n")
	return b.String()
}

func buildNewSignInHTML(brand string, in identity.SignInNotice) string {
	when, from, client, how := signInFacts(in)
	tmpl := template.Must(template.New("signin").Parse(htmlNewSignInTemplate))
	var buf bytes.Buffer
	_ = tmpl.Execute(&buf, map[string]any{
		"BrandName": brand,
		"When":      when,
		"From":      from,
		"Client":    client,
		"How":       how,
	})
	return buf.String()
}

// htmlNewSignInTemplate carries NO ANCHOR and no button, unlike every other
// message this package sends. See the file comment: a revoke link mailed to
// a shared mailbox is a weapon handed to the person it is warning about.
const htmlNewSignInTemplate = `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="font-family:Helvetica,Arial,sans-serif;margin:0;padding:24px;background:#f7f7f8;">
  <table cellpadding="0" cellspacing="0" border="0" align="center" width="520" style="background:#ffffff;border-radius:8px;padding:32px;">
    <tr><td style="font-size:20px;font-weight:600;color:#111827;padding-bottom:8px;">New sign-in to {{.BrandName}}</td></tr>
    <tr><td style="font-size:14px;color:#374151;padding-bottom:20px;">Someone signed in to this account using {{.How}}.</td></tr>
    <tr><td style="font-size:14px;color:#374151;padding-bottom:4px;"><strong>When:</strong> {{.When}}</td></tr>
    <tr><td style="font-size:14px;color:#374151;padding-bottom:4px;"><strong>IP address:</strong> {{.From}}</td></tr>
    <tr><td style="font-size:14px;color:#374151;padding-bottom:20px;"><strong>Client:</strong> {{.Client}}</td></tr>
    <tr><td style="font-size:14px;color:#374151;padding-bottom:12px;">If this was you, nothing more to do.</td></tr>
    <tr><td style="font-size:14px;color:#374151;">If it was not, sign in and revoke sessions from your profile page, then set up a passkey. If this address is a shared mailbox, consider turning on passkey-only sign-in so that a sign-in link alone is no longer enough to enter the account.</td></tr>
  </table>
</body>
</html>`

// SendSignInDisabledNotice implements magiclink.Sender's second method: the
// message a passkey-only account gets INSTEAD of a sign-in link
// (memql#4304).
//
// # What it is worth to the wrong reader
//
// Nothing, and that is the requirement. On a shared mailbox this message
// lands in front of everybody who can read the address, exactly as the link
// would have. Where the link was a credential, this is a sentence. It
// carries no token, no URL and no way to act.
//
// # Why it is sent at all
//
// Because the alternative -- silence -- leaves the account holder unable to
// tell "the email is slow" from "somebody is trying to get into my account".
// The request happened either way; the message is the only place that fact
// can surface. And the response the requester sees is identical to an
// ordinary issue (same redirect, same page), so the message is also the ONLY
// difference between the two paths: making it silent would make a
// passkey-only account indistinguishable from a broken one.
func (s *EngineEmailSender) SendSignInDisabledNotice(ctx context.Context, in magiclink.NoticeInput) error {
	if s == nil {
		return errors.New("emailsender: nil")
	}
	brand := in.BrandName
	if brand == "" {
		brand = "MemQL"
	}
	subject := fmt.Sprintf("Sign-in links are turned off for your %s account", brand)

	sender := s.resolveSender()
	if sender == nil {
		return s.noSender("sign-in-disabled notice", in.Email, subject)
	}

	return sender.Send(ctx, email.Message{
		To:       in.Email,
		Subject:  subject,
		TextBody: buildSignInDisabledText(brand),
		HTMLBody: buildSignInDisabledHTML(brand),
	})
}

func buildSignInDisabledText(brand string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Sign-in links are turned off for your %s account\n\n", brand)
	b.WriteString("Someone asked for a sign-in link for this account. Sign-in links are disabled for it, so none was sent.\n\n")
	b.WriteString("To sign in, use your passkey.\n\n")
	b.WriteString("If this wasn't you, nothing has happened -- no link exists and nobody has been signed in. ")
	b.WriteString("If it keeps happening, ask an administrator to look at the sign-in audit trail.\n")
	return b.String()
}

func buildSignInDisabledHTML(brand string) string {
	tmpl := template.Must(template.New("nolink").Parse(htmlSignInDisabledTemplate))
	var buf bytes.Buffer
	_ = tmpl.Execute(&buf, map[string]any{"BrandName": brand})
	return buf.String()
}

// htmlSignInDisabledTemplate carries no anchor, for the reason the file
// comment gives at length.
const htmlSignInDisabledTemplate = `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="font-family:Helvetica,Arial,sans-serif;margin:0;padding:24px;background:#f7f7f8;">
  <table cellpadding="0" cellspacing="0" border="0" align="center" width="520" style="background:#ffffff;border-radius:8px;padding:32px;">
    <tr><td style="font-size:20px;font-weight:600;color:#111827;padding-bottom:8px;">Sign-in links are turned off for your {{.BrandName}} account</td></tr>
    <tr><td style="font-size:14px;color:#374151;padding-bottom:16px;">Someone asked for a sign-in link for this account. Sign-in links are disabled for it, so none was sent.</td></tr>
    <tr><td style="font-size:14px;color:#374151;padding-bottom:16px;">To sign in, use your passkey.</td></tr>
    <tr><td style="font-size:14px;color:#374151;">If this wasn't you, nothing has happened -- no link exists and nobody has been signed in. If it keeps happening, ask an administrator to look at the sign-in audit trail.</td></tr>
  </table>
</body>
</html>`
