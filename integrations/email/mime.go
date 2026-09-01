package email

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// mime.go -- RFC 5322 rendering, extracted so BOTH senders produce the
// same bytes (memql#3348).
//
// # Why this exists now and did not before
//
// Until campaigns, every message MemQL sent was transactional and carried
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

// FromHeader renders the `From:` value for a mailbox and an optional display
// name, quoting the name when RFC 5322 requires it (design D6, memql#4821).
//
// # Why this grew a grammar
//
// It used to interpolate the name verbatim, and that was defensible while the
// only name it ever saw was `MEMQL_EMAIL_FROM_NAME` -- an operator-set
// deployment constant, typed once, read by nobody but this function. D6 makes
// the display name CALLER-INFLUENCED: a campaign's `fromName`, and a sending
// identity's, both reach here. That is a different kind of input, and the
// unquoted form is wrong for values a person will legitimately type.
//
// RFC 5322 splits a display name into `atom *(atom)` and `quoted-string`. The
// bare form may not contain a "special" -- `(` `)` `<` `>` `[` `]` `:` `;` `@`
// `\` `,` `.` `"` -- so a name as ordinary as `Acme, Inc.` or
// `Support: Billing` is not a valid unquoted phrase. A strict parser reads
// `Acme, Inc. <a@b>` as a MALFORMED ADDRESS LIST: `Acme` is one address,
// ` Inc. <a@b>` another. That is not a cosmetic defect -- an unparseable From
// is a deliverability failure at the receiving end, and it arrives as mail
// silently not landing rather than as an error anywhere we can see.
//
// So a name carrying anything outside the unquoted-safe set is emitted as a
// quoted-string with `\` and `"` backslash-escaped, which is exactly the
// grammar's own answer.
//
// # What it deliberately does NOT do
//
// It does not RFC 2047 encode a non-ASCII name (`=?UTF-8?B?...?=`). The
// quoted-string carries the raw UTF-8 bytes, which every SMTPUTF8 / RFC 6532
// transport accepts and which Microsoft Graph -- the only transport that can
// send as more than one identity, and therefore the only one this parameter
// really serves -- passes through unchanged. Encoded-words are the stricter
// answer for a 7-bit-only relay and would be a self-contained follow-up;
// quoting is what turns the malformed-address-list class from possible into
// impossible, and that is the defect D6 names.
//
// It also does not refuse a control byte, and that is not an oversight: the
// refusal already exists one layer down and must stay there. RenderRFC5322
// runs headerUnsafe over the composed value at the moment it is serialized,
// and SendAs.Validate covers the Graph structured payload, which never reaches
// the renderer. A second refusal here would be a third place to keep in sync
// for no additional coverage -- and this function returns a string, so its
// only way to refuse would be to silently drop the name.
func FromHeader(addr, displayName string) string {
	name := strings.TrimSpace(displayName)
	if name == "" {
		return addr
	}
	if displayNameNeedsQuoting(name) {
		return fmt.Sprintf("%s <%s>", quoteDisplayName(name), addr)
	}
	return fmt.Sprintf("%s <%s>", name, addr)
}

// displayNameNeedsQuoting reports whether a display name is outside the RFC
// 5322 unquoted `phrase` grammar.
//
// Deliberately CONSERVATIVE in the quoting direction: anything that is not a
// plain ASCII printable, plus every RFC "special", plus every non-ASCII rune,
// gets quoted. Over-quoting is invisible to a recipient (a quoted-string
// decodes to the same characters); under-quoting is a malformed address list.
// Given that asymmetry the only interesting mistake is missing a character,
// so the predicate is written as an ALLOW-LIST of what may stay bare rather
// than as a deny-list of specials -- a deny-list is the shape that silently
// omits the one character nobody thought of.
func displayNameNeedsQuoting(name string) bool {
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			continue
		}
		// The `atext` punctuation an unquoted phrase may carry (RFC 5322
		// §3.2.3), plus the space that separates atoms. Everything else --
		// the specials, the controls, and every rune above US-ASCII --
		// forces the quoted form.
		if strings.ContainsRune(" !#$%&'*+-/=?^_`{|}~", r) {
			continue
		}
		return true
	}
	return false
}

// quoteDisplayName renders a name as an RFC 5322 quoted-string. Only `\` and
// `"` need escaping inside one; every other byte stands for itself.
func quoteDisplayName(name string) string {
	var b strings.Builder
	b.Grow(len(name) + 2)
	b.WriteByte('"')
	for _, r := range name {
		if r == '\\' || r == '"' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
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
	// CRYPTO-RANDOM, not a timestamp. The boundary is the only thing separating
	// operator-authored body content from MIME structure, and a body that
	// contains `\r\n--<boundary>` forges a part. The previous
	// `time.Now().UnixNano()` made that a guessing problem rather than an
	// impossible one; 128 bits of entropy makes it neither (memql#3348).
	boundary, err := randomBoundary()
	if err != nil {
		return nil, fmt.Errorf("email: generating MIME boundary: %w", err)
	}

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

// randomBoundary returns a MIME multipart boundary with 128 bits of entropy.
//
// Boundaries are structure, not content: everything between `--<boundary>` and
// the next one is a body part, so a body able to contain the boundary can
// declare parts of its own. Campaign bodies are operator-authored and reach
// thousands of recipients, which is exactly the position where "an attacker
// would have to guess a nanosecond" is the wrong amount of reassurance.
func randomBoundary() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "memql-email-" + hex.EncodeToString(b[:]), nil
}
