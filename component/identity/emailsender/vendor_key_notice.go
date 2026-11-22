package emailsender

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"regexp"
	"strings"
	"time"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
)

// vendor_key_notice.go -- the one email the owner of an API key gets when it
// stops working (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decision K13).
//
// # What it is for
//
// A key that stops working does not stop anybody's work: the call moves to
// the next account that can pay. So nothing tells the key's owner, and
// somebody else pays for calls the owner thought were theirs. This message is
// what tells them. Who gets one, and that it is one per fall, is decided by
// integrations/vendorkeys/notice.go; this file is only the words.
//
// # No link, and this is a decision
//
// The message says where to go in words (Fleet, then API keys) and carries
// nothing a mail client turns into a link. A message about a credential that
// asks its reader to click is exactly what a forged one looks like, and the
// reader of the real one should never have been taught to.
//
// That includes the VENDOR'S OWN ADDRESS. A vendor answers a refused key with
// a sentence that ends in a link to its console, and the sentence is quoted
// here because it is the most useful thing in the message. The address in it
// is replaced, so the quotation keeps its meaning and loses its link.
//
// # Nothing of the key
//
// The input has no field for the key's value or its fingerprint, so neither
// can be put in by mistake. The vendor's words are the one free text, and a
// vendor echoes part of a refused key in them: anything shaped like a key is
// removed again here, whatever the caller already did.
//
// Regression tests: vendor_key_notice_test.go.

// VendorKeyNotice is what the owner of an API key is told when it stops
// working. There is deliberately no field for the key or its fingerprint.
type VendorKeyNotice struct {
	// Email is the owner's address, and OwnerName their name, or "".
	Email, OwnerName string
	// KeyLabel is the owner's own name for the key.
	KeyLabel string
	// Vendor is "anthropic" or "openai".
	Vendor string
	// State is "refused" (the vendor does not accept the key) or "credit"
	// (its account is out of credit or at a spending limit).
	State string
	// Detail is what the vendor answered, or "".
	Detail string
	// At is when the call or check that found it started.
	At        time.Time
	BrandName string
}

// maxVendorWords bounds the vendor's answer quoted in the message.
const maxVendorWords = 300

// vendorAddress matches what a mail client turns into a link inside a vendor's
// answer: anything with a scheme, anything starting www., and a host followed
// by a path.
var vendorAddress = regexp.MustCompile(`(?i)\b(?:(?:https?|ftp)://|www\.)[^\s"'<>)]+|\b(?:[a-z0-9-]+\.)+[a-z]{2,}/[^\s"'<>)]*`)

// SendVendorKeyNotice mails the owner of an API key that stopped working. An
// error means nothing was sent; the caller keeps the notice owed.
func (s *EngineEmailSender) SendVendorKeyNotice(ctx context.Context, in VendorKeyNotice) error {
	if s == nil {
		return errors.New("emailsender: nil")
	}
	in.Email = strings.TrimSpace(in.Email)
	if in.Email == "" {
		return errors.New("emailsender: the API key notice needs a recipient")
	}
	brand := in.BrandName
	if brand == "" {
		brand = "MemQL"
	}
	// The key's name is its owner's own text and stays out of the subject.
	subject := fmt.Sprintf("An API key of yours stopped working on %s", brand)

	sender := s.resolveSender()
	if sender == nil {
		return s.noSender("API key notice", in.Email, subject)
	}
	// The zero SendAs: the deployment's configured mailbox (email D5).
	return sender.Send(ctx, email.Message{
		To:       in.Email,
		Subject:  subject,
		TextBody: buildVendorKeyNoticeText(brand, in),
		HTMLBody: buildVendorKeyNoticeHTML(brand, in),
	}, email.SendAs{})
}

// vendorKeyNoticeCopy is the message's sentences, written once for both bodies.
type vendorKeyNoticeCopy struct {
	Title, Greeting, Key, When, What, Using, Next, Once string
}

func vendorKeyNoticeWords(brand string, in VendorKeyNotice) vendorKeyNoticeCopy {
	if brand == "" {
		brand = "MemQL"
	}
	vendor := vendorKeyVendorName(in.Vendor)
	label := strings.TrimSpace(in.KeyLabel)
	if label == "" {
		label = "(no name)"
	}
	words := vendorKeyNoticeCopy{
		Title: "An API key of yours stopped working",
		Key:   fmt.Sprintf("%s (%s)", label, vendor),
		When:  "unknown",
		Using: fmt.Sprintf("%s is not using this key. A call it would have paid for goes to the next account that can pay, or fails when there is none.", brand),
		Once:  "You will not get another message about this key unless it works again and then stops.",
	}
	if name := strings.TrimSpace(in.OwnerName); name != "" {
		words.Greeting = fmt.Sprintf("Hello %s,", name)
	}
	if !in.At.IsZero() {
		words.When = in.At.UTC().Format("2 Jan 2006, 15:04 UTC")
	}
	switch in.State {
	case "credit":
		words.What = fmt.Sprintf("The account behind this key at %s is out of credit or at its spending limit.", vendor)
		words.Next = fmt.Sprintf("Add credit or raise the limit at %[1]s. %[2]s tries the key again by itself about an hour after it last failed, "+
			"so there is nothing to change in Fleet, under API keys, unless you want to remove the key.", vendor, brand)
	case "refused":
		words.What = fmt.Sprintf("%s refused the key.", vendor)
		words.Next = fmt.Sprintf("If the key was revoked or replaced at %[1]s, remove it and add the new one: open Fleet in %[2]s and go to API keys. "+
			"If it should still work, open the key there and choose Check now.", vendor, brand)
	default:
		words.What = fmt.Sprintf("%s is not accepting the key.", vendor)
		words.Next = fmt.Sprintf("To see where it stands, open Fleet in %s and go to API keys.", brand)
	}
	if said := vendorWords(in.Detail); said != "" {
		words.What += fmt.Sprintf(" %s said: %q", vendor, said)
	}
	return words
}

// vendorWords is the vendor's answer made safe to quote: nothing shaped like a
// key, no address a mail client would link, one line, bounded.
func vendorWords(detail string) string {
	said := memqlengine.ScrubVendorKeyShapes(detail)
	said = vendorAddress.ReplaceAllString(said, "(address removed)")
	said = strings.Join(strings.Fields(said), " ")
	if r := []rune(said); len(r) > maxVendorWords {
		said = string(r[:maxVendorWords])
	}
	return said
}

func vendorKeyVendorName(vendor string) string {
	switch strings.ToLower(strings.TrimSpace(vendor)) {
	case "anthropic":
		return "Anthropic"
	case "openai":
		return "OpenAI"
	}
	return "the vendor"
}

func buildVendorKeyNoticeText(brand string, in VendorKeyNotice) string {
	words := vendorKeyNoticeWords(brand, in)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", words.Title)
	if words.Greeting != "" {
		fmt.Fprintf(&b, "%s\n\n", words.Greeting)
	}
	fmt.Fprintf(&b, "Key:  %s\n", words.Key)
	fmt.Fprintf(&b, "When: %s\n\n", words.When)
	fmt.Fprintf(&b, "%s\n\n%s\n\n%s\n\n%s\n", words.What, words.Using, words.Next, words.Once)
	return b.String()
}

func buildVendorKeyNoticeHTML(brand string, in VendorKeyNotice) string {
	tmpl := template.Must(template.New("vendorkey").Parse(htmlVendorKeyNoticeTemplate))
	var buf bytes.Buffer
	_ = tmpl.Execute(&buf, vendorKeyNoticeWords(brand, in))
	return buf.String()
}

// htmlVendorKeyNoticeTemplate carries NO ANCHOR and no button. See the file
// comment: this message tells its reader where to go in words.
const htmlVendorKeyNoticeTemplate = `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="font-family:Helvetica,Arial,sans-serif;margin:0;padding:24px;background:#f7f7f8;">
  <table cellpadding="0" cellspacing="0" border="0" align="center" width="520" style="background:#ffffff;border-radius:8px;padding:32px;">
    <tr><td style="font-size:20px;font-weight:600;color:#111827;padding-bottom:8px;">{{.Title}}</td></tr>
    {{if .Greeting}}<tr><td style="font-size:14px;color:#374151;padding-bottom:20px;">{{.Greeting}}</td></tr>{{end}}
    <tr><td style="font-size:14px;color:#374151;padding-bottom:4px;"><strong>Key:</strong> {{.Key}}</td></tr>
    <tr><td style="font-size:14px;color:#374151;padding-bottom:20px;"><strong>When:</strong> {{.When}}</td></tr>
    <tr><td style="font-size:14px;color:#374151;padding-bottom:12px;">{{.What}}</td></tr>
    <tr><td style="font-size:14px;color:#374151;padding-bottom:12px;">{{.Using}}</td></tr>
    <tr><td style="font-size:14px;color:#374151;padding-bottom:12px;">{{.Next}}</td></tr>
    <tr><td style="font-size:14px;color:#6b7280;">{{.Once}}</td></tr>
  </table>
</body>
</html>`
