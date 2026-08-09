package email

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// mime.go -- RFC 5322 rendering, extracted so BOTH senders produce the
// same bytes (memql#3348).
//
// # Why this exists now and did not before
//
// Until campaigns, every message memQL sent was transactional and carried
// exactly the five headers a sender composes for itself. Bulk mail cannot
// be: RFC 8058 one-click unsubscribe is delivered as TWO HEADERS on the
// message (`List-Unsubscribe` with an https URI, and
// `List-Unsubscribe-Post: List-Unsubscribe=One-Click`), and the large
// mailbox providers treat their absence as a bulk-sender defect.
//
// That is a problem specifically for the GRAPH sender, and it is the reason
// this file is not just a `Headers` map on Message. Microsoft Graph's
// structured `sendMail` payload exposes custom headers through
// `internetMessageHeaders`, which REFUSES any header name not beginning with
// `x-`. `List-Unsubscribe` is not an `x-` header and never can be, so the
// structured payload structurally cannot carry it. Graph's other sendMail
// form -- POST the whole RFC 5322 message, base64-encoded, with
// `Content-Type: text/plain` -- has no such restriction, so a message with
// extra headers goes out through that path instead.
//
// Rendering therefore has to exist in one place rather than two: the SMTP
// sender already built a message body inline, and letting Graph build a
// second one by hand is how the two drift into disagreeing about the
// multipart boundary, the header-injection barrier, or which body part comes
// first.
//
// # The header-injection barrier is in the RENDERER, not at the caller
//
// Every header value -- including the ones this package composes from
// configuration, and including caller-supplied extras -- passes headerUnsafe
// at the moment it is serialized. That placement is deliberate and predates
// this file (see SMTPSender.Send's comment): validating at the sink makes the
// sink safe by construction no matter how the message was assembled, whereas
// validating at the entry point is a guarantee that lasts until someone adds
// a second entry point. Header NAMES are checked too, which the inline
// version did not need to do because it wrote a fixed set of literals.

// reservedHeaders are the header names the renderer composes itself. A
// caller-supplied header of the same name is REFUSED rather than
// overriding or duplicating, because both of the other options are
// silently wrong: a duplicate `To:` is a second recipient, and an
// override of `From:` is a sender the configured mailbox did not
// authenticate as -- which is precisely the SPF/DKIM alignment the
// campaign path relies on being structural.
var reservedHeaders = map[string]struct{}{
	"from":         {},
	"to":           {},
	"subject":      {},
	"mime-version": {},
	"content-type": {},
}

// headerNameUnsafe reports whether a header NAME is outside the RFC 5322
// field-name grammar (printable US-ASCII except colon). A name is never
// remote-derived today, but it is caller-derived, and a name carrying a
// colon or a newline splits the header exactly as a value would.
func headerNameUnsafe(name string) bool {
	if strings.TrimSpace(name) == "" {
		return true
	}
	for _, r := range name {
		if r <= 0x20 || r >= 0x7f || r == ':' {
			return true
		}
	}
	return false
}

// ValidateExtraHeaders reports the first problem with a caller-supplied
// header set: an unsafe name, an unsafe value, or a name the renderer
// reserves. Exported so a caller can refuse early with a message naming
// the header, rather than discovering it at the wire boundary where the
// only honest answer is a failed send.
func ValidateExtraHeaders(headers map[string]string) error {
	for name, value := range headers {
		if headerNameUnsafe(name) {
			return fmt.Errorf("email: header name %q is not a valid RFC 5322 field name", name)
		}
		if _, reserved := reservedHeaders[strings.ToLower(name)]; reserved {
			return fmt.Errorf("email: header %q is composed by the renderer and may not be supplied by a caller", name)
		}
		if headerUnsafe(value) {
			return fmt.Errorf("email: header %q contains illegal control characters (header injection)", name)
		}
	}
	return nil
}

// FromHeader renders the `From:` value for a configured mailbox.
func FromHeader(addr, displayName string) string {
	if strings.TrimSpace(displayName) == "" {
		return addr
	}
	return fmt.Sprintf("%s <%s>", displayName, addr)
}

// RenderRFC5322 serializes msg as a complete RFC 5322 message, ready for
// SMTP DATA or for Graph's base64 MIME sendMail form.
//
// Header ORDER is deterministic (the composed five in a fixed order, then
// the extras sorted by name) rather than map-iteration order. Not
// cosmetic: a message whose bytes differ run to run cannot be compared in
// a test, and DKIM signing -- done by the relay, over the headers it is
// handed -- is easier to reason about when the input is stable.
func RenderRFC5322(fromHeader string, msg Message) ([]byte, error) {
	if err := msg.Validate(); err != nil {
		return nil, err
	}
	if headerUnsafe(fromHeader) {
		return nil, errors.New("email: From contains illegal control characters (header injection)")
	}
	if err := ValidateExtraHeaders(msg.Headers); err != nil {
		return nil, err
	}

	var b strings.Builder
	write := func(name, value string) {
		b.WriteString(name)
		b.WriteString(": ")
		b.WriteString(value)
		b.WriteString("\r\n")
	}

	multipart := strings.TrimSpace(msg.HTMLBody) != ""
	boundary := fmt.Sprintf("memql-email-%d", time.Now().UnixNano())

	write("From", fromHeader)
	write("To", msg.To)
	write("Subject", msg.Subject)
	write("MIME-Version", "1.0")
	if multipart {
		write("Content-Type", fmt.Sprintf(`multipart/alternative; boundary="%s"`, boundary))
	} else {
		write("Content-Type", "text/plain; charset=UTF-8")
	}

	extras := make([]string, 0, len(msg.Headers))
	for name := range msg.Headers {
		extras = append(extras, name)
	}
	sort.Strings(extras)
	for _, name := range extras {
		write(name, msg.Headers[name])
	}

	b.WriteString("\r\n")
	if !multipart {
		b.WriteString(msg.TextBody)
		return []byte(b.String()), nil
	}
	b.WriteString("--")
	b.WriteString(boundary)
	b.WriteString("\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n")
	b.WriteString(msg.TextBody)
	b.WriteString("\r\n--")
	b.WriteString(boundary)
	b.WriteString("\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n")
	b.WriteString(msg.HTMLBody)
	b.WriteString("\r\n--")
	b.WriteString(boundary)
	b.WriteString("--\r\n")
	return []byte(b.String()), nil
}
