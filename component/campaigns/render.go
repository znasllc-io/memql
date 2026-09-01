package campaigns

import (
	"fmt"
	"html"
	texttemplate "html/template"
	"net/url"
	"strings"

	"github.com/znasllc-io/memql/integrations/email"
)

// render.go -- turning a template plus a recipient into a message.
//
// # SPF / DKIM alignment is STRUCTURAL here, not configured
//
// Alignment means the domain in the visible `From:` matches the domain
// the message was authenticated as. MemQL cannot sign DKIM itself -- the
// relay (Microsoft Graph, or whatever SMTP endpoint is configured) signs
// with the keys published for the mailbox it authenticated as. What MemQL
// CAN guarantee is that it never sets a From address the relay did not
// authenticate as, and it still does -- but the mechanism CHANGED in
// memql#4821 and the old statement of it is no longer true, so it is
// restated exactly:
//
//   - The From ADDRESS is never CALLER-SETTABLE. That is the invariant, and
//     it survives untouched: `integrations/email.Message` has no From field,
//     the MIME renderer refuses a caller-supplied `From:` header outright,
//     and no argument to this package's builtins can name one.
//   - It is NO LONGER true that the address "is not a parameter of this
//     package". Since sending identities exist, the address is chosen HERE,
//     from a `v1:campaigns:senderIdentity` row an operator declared and the
//     campaign explicitly points at -- resolved in identity.go, carried to
//     the transport as `email.SendAs` beside the message, never inside it.
//     The distinction that matters is authored-versus-arbitrary, not
//     present-versus-absent: an identity is a mailbox somebody wrote down in
//     the graph, not free text a request supplied.
//   - The SENDER IMPLEMENTATION still owns the header. This package hands
//     over a mailbox and a display name; Graph builds `/users/{address}/
//     sendMail` and stamps From from it, SMTP refuses a non-default identity
//     because AUTH is bound to one mailbox, and nothing here writes a header
//     line. So the escaping and injection barrier stays in one place.
//   - `fromName` is the DISPLAY NAME only and reaches the header for the
//     first time in memql#4821 (design D6): campaign override, else the
//     identity's, else the transport's own default. It cannot affect
//     alignment, which is a property of the address.
//   - `Reply-To` is settable per campaign and now also per identity, and
//     that is still the correct escape valve -- it steers replies without
//     touching the authenticated identity.
//
// So there is still no "alignment check" to run, and there is now a second
// way to misalign that an operator owns: declaring an identity whose mailbox
// the Graph application may not send as. No engine preflight can verify that
// -- it is Exchange ApplicationAccessPolicy state in the operator's tenant --
// and the honest check is the provider's own 403 landing on the campaign's
// lastError. Both that and the DNS half are written down in
// docs/public/operate/campaign-sending.md rather than pretended away with a
// preflight that could only ever guess.
//
// # The unsubscribe surface is TWO things, and both are required
//
//  1. The RFC 8058 header pair -- `List-Unsubscribe` carrying an https
//     URI, and `List-Unsubscribe-Post: List-Unsubscribe=One-Click`. This
//     is what the mailbox provider's own "unsubscribe" button uses. It is
//     machine-facing, and it is what the large providers now require of
//     bulk senders.
//  2. A visible link in the body. The header serves the provider's UI;
//     a person reading the message in a client that does not surface it
//     needs somewhere to click. Appending it is not optional politeness --
//     the legal requirement is a working opt-out the RECIPIENT can find.
//
// Both are minted from the same signed token, so they cannot disagree
// about who is unsubscribing.

const (
	headerListUnsubscribe     = "List-Unsubscribe"
	headerListUnsubscribePost = "List-Unsubscribe-Post"
	oneClickValue             = "List-Unsubscribe=One-Click"

	// headerCampaignID stamps which send a message belongs to, so a bounce
	// that comes back hours later can be attributed to it (memql#3461).
	//
	// It has to be an `X-` name and it is: Microsoft Graph's structured
	// sendMail payload only carries custom headers with that prefix, and a
	// header the preferred sender cannot set is a header that does not
	// exist. A DSN quotes the original headers back and SES echoes them in
	// `mail.headers`, so both parsers find it in the shape the provider
	// happens to return it.
	//
	// It carries a campaign id and nothing else: no recipient, no owner, no
	// token. The id is already visible to anyone holding the message, and
	// the unsubscribe token -- which is not -- stays in its own header.
	headerCampaignID = "X-Campaign-Id"
)

// UnsubscribePath is the route the one-click endpoint is mounted at. One
// path segment, deliberately: the self-authenticated bypass that lets an
// unauthenticated request reach its own handler is bounded to a single
// segment (memql#3128).
const UnsubscribePath = "/unsubscribe"

// RenderOptions carries what a rendered message needs beyond the campaign,
// template and recipient rows.
//
// A struct rather than more positional parameters because every field here is
// OPTIONAL in the honest sense -- the zero value renders exactly what this
// package rendered before any of them existed -- and because the alternative
// grows a five-argument function into an eight-argument one whose call sites
// nobody can read.
type RenderOptions struct {
	// ReplyTo is the resolved Reply-To for this send: the campaign's own
	// value, else the sending identity's default (memql#4821). Empty falls
	// back to the campaign row's field, which is what a caller passing a
	// zero-valued RenderOptions means.
	ReplyTo string
}

// renderMessage builds the outgoing message for one recipient.
//
// Personalization is exactly ONE substitution: `{{displayName}}`, falling
// back to the address's local part. Not a template engine -- a campaign
// body is operator-authored text that goes to thousands of strangers, and
// an expression evaluator in that position is an injection surface with a
// mailing list attached. A single named placeholder covers the one case
// (a greeting) that actually recurs.
func renderMessage(c Campaign, t Template, r Recipient, unsubscribeURL string, opts RenderOptions) (email.Message, error) {
	name := strings.TrimSpace(r.DisplayName)
	if name == "" {
		if at := strings.Index(r.Email, "@"); at > 0 {
			name = r.Email[:at]
		} else {
			name = "there"
		}
	}
	// TWO replacers, not one. `name` is recipient-supplied -- it arrives on an
	// imported audience roster -- so the HTML path must escape it, while the
	// text path must not (escaping there would render "&amp;" as literal text
	// to the reader). Using one replacer for both is the bug CodeQL caught in
	// memql#3348: the footer below escaped its operator-set values while the
	// substitution above it interpolated recipient data raw.
	subst := strings.NewReplacer("{{displayName}}", name)
	substHTML := strings.NewReplacer("{{displayName}}", htmlEscape(name))

	text := subst.Replace(t.TextBody)
	text += fmt.Sprintf("\r\n\r\n--\r\nYou are receiving this because you subscribed to %s.\r\nUnsubscribe: %s\r\n",
		displayNameFor(c), unsubscribeURL)

	msg := email.Message{
		To:       r.Email,
		Subject:  subst.Replace(t.Subject),
		TextBody: text,
		Headers: map[string]string{
			// Angle brackets are required by RFC 2369 -- a bare URI here
			// is a header some clients silently ignore, which is the same
			// as having no unsubscribe at all.
			headerListUnsubscribe:     "<" + unsubscribeURL + ">",
			headerListUnsubscribePost: oneClickValue,
			headerCampaignID:          c.ID,
		},
	}
	if strings.TrimSpace(t.HTMLBody) != "" {
		// The footer goes through html/template rather than Sprintf + a manual
		// escaper. Not scanner appeasement: `{{.URL}}` sits in an href, and
		// html/template is CONTEXT-aware -- it applies URL escaping there and
		// text escaping around it, and its urlFilter neutralises a `javascript:`
		// scheme, none of which html.EscapeString knows to do. Hand-escaping
		// into an attribute means re-deriving per call site which of those
		// applies; this asks the standard library instead (memql#3348).
		footer, err := footerTemplate(displayNameFor(c), unsubscribeURL)
		if err != nil {
			// Unreachable with a fixed template and string inputs; refusing to
			// send beats mailing a body with no unsubscribe footer, which is the
			// one thing RFC 8058 compliance cannot go out without.
			return email.Message{}, err
		}
		msg.HTMLBody = substHTML.Replace(t.HTMLBody) + footer
	}
	// The campaign's own Reply-To wins over the identity's default, and
	// RenderOptions carries the already-resolved answer. The fall-back to
	// c.ReplyTo is for a caller passing zero options -- a test, or a path
	// with no identity in scope -- and keeps that caller's behaviour exactly
	// what it was.
	replyTo := strings.TrimSpace(opts.ReplyTo)
	if replyTo == "" {
		replyTo = strings.TrimSpace(c.ReplyTo)
	}
	if replyTo != "" {
		msg.Headers["Reply-To"] = replyTo
	}
	return msg, nil
}

func displayNameFor(c Campaign) string {
	if n := strings.TrimSpace(c.FromName); n != "" {
		return n
	}
	return strings.TrimSpace(c.Name)
}

// unsubscribeURL builds the one-click target for a recipient.
func unsubscribeURL(baseURL, token string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return fmt.Sprintf("%s%s?token=%s", base, UnsubscribePath, url.QueryEscape(token))
}

// htmlEscape delegates to the standard library rather than hand-rolling the
// five replacements, which is what this was before memql#3348.
//
// The hand-rolled version was *correct* -- html.EscapeString escapes exactly
// the same five characters -- so this is not a bug fix. It is a maintenance
// and analysis one: a custom escaper is a sanitizer no static analyser
// recognises, so CodeQL reported the one-token unsubscribe page as reflected
// XSS, and any future reader has to re-derive that the five are sufficient for
// the context. The stdlib carries both guarantees for free.
func htmlEscape(s string) string {
	return html.EscapeString(s)
}

// unsubscribeFooter is parsed once. html/template is CONTEXT-aware: it knows
// `{{.URL}}` sits inside an href and applies URL escaping plus its urlFilter
// (which replaces a `javascript:` or `data:` scheme with "#ZgotmplZ"), while
// `{{.Sender}}` in text position gets ordinary HTML escaping. That difference
// is the reason this is a template rather than a Sprintf with a manual
// escaper -- html.EscapeString applies one rule everywhere and cannot know
// which context it landed in.
var unsubscribeFooter = texttemplate.Must(texttemplate.New("unsubscribeFooter").Parse(
	`<hr><p style="font-size:12px;color:#666">You are receiving this because ` +
		`you subscribed to {{.Sender}}. <a href="{{.URL}}">Unsubscribe</a>.</p>`))

// footerTemplate renders the RFC 8058 footer appended to every HTML body.
func footerTemplate(sender, url string) (string, error) {
	var b strings.Builder
	if err := unsubscribeFooter.Execute(&b, struct {
		Sender string
		URL    string
	}{Sender: sender, URL: url}); err != nil {
		return "", fmt.Errorf("campaigns: rendering unsubscribe footer: %w", err)
	}
	return b.String(), nil
}
