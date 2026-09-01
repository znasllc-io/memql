package campaigns

import (
	"encoding/json"
	"fmt"
	"html"
	texttemplate "html/template"
	"net/url"
	"regexp"
	"strconv"
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

	// AccountName backs {{accountName}} (memql#4822). Empty is correct and
	// common: a campaign with no account tie renders the tag as an empty
	// string rather than leaving it literal, because "this campaign is for
	// nobody in particular" is an ANSWER, whereas an unknown tag is a typo.
	AccountName string

	// SubjectPrefix is prepended to the rendered subject. Used by the test
	// send for "[Test] " and by nothing else; a campaign cannot set one.
	SubjectPrefix string

	// Tracking rewrites the HTML part's links and appends an open pixel
	// (memql#4823). The zero value tracks nothing, which is what every
	// caller without a delivery row in hand must pass -- there is no
	// engagement to attribute a hit to.
	Tracking TrackingRender
}

// mergeTagPrefix and mergeTagSuffix delimit a merge tag. Named because the
// unresolved-tag scanner has to look for the same delimiters the replacer
// writes, and two spellings of "{{" is how a reporter comes to disagree with
// the thing it reports on.
const (
	mergeTagPrefix = "{{"
	mergeTagSuffix = "}}"

	// mergeFieldsPrefix is the one NAMESPACED tag family: {{fields.<key>}}
	// resolves against the recipient's own imported columns.
	mergeFieldsPrefix = "fields."
)

// mergeReplacers builds the pair of substituters for one message.
//
// # Still not a template engine, and the closed set is what keeps it so
//
// The rule memql#3348 wrote down was "exactly ONE substitution". The set is
// now FIVE things -- {{displayName}}, {{email}}, {{campaignName}},
// {{accountName}} and {{fields.<key>}} for each key the recipient actually
// carries -- and the reason that is not a slide toward an expression
// evaluator is worth stating precisely, because "a closed set" sounds like
// the same argument every template language starts from.
//
// What makes this safe is not the SIZE of the set, it is that every member is
// ENUMERATED BEFORE THE BODY IS READ. strings.NewReplacer is handed a literal
// list of exact strings; the body is never parsed, no path is ever evaluated,
// and a tag that is not on the list is not a lookup that returns nothing --
// it is text nobody looked at. So there is no expression to inject into,
// nothing recursive, no way for one substituted value to become another tag,
// and no attacker-supplied string that can name a value the operator did not
// put there. {{fields.<key>}} widens the LIST from the recipient's own row
// and still cannot widen the GRAMMAR: an unknown key contributes no entry.
//
// A campaign body is operator-authored text that goes to thousands of
// strangers. An expression evaluator in that position is an injection surface
// with a mailing list attached, and it stays out.
//
// # The TWO-REPLACER asymmetry, and why every new tag has to be in both
//
// `displayName` and every `fields.*` value are RECIPIENT-supplied -- they
// arrive on an imported roster, which is the one part of a campaign send the
// operator did not author. The HTML path must escape them; the text path must
// NOT, because text/plain has no markup context and the reader would see
// "&amp;" where an ampersand belongs.
//
// Using one replacer for both is the bug CodeQL caught in memql#3348: the
// footer escaped its operator-set values while the substitution above it
// interpolated recipient data raw. The failure mode of forgetting a tag in
// ONE of the two is silent in both directions -- an unescaped tag in the HTML
// body is an injection, an escaped one in the text body is visible mojibake
// -- so both replacers are built from ONE list here rather than assembled
// separately, and render_escaping_test.go pins each tag in both.
const (
	// baseMergeTagCount is the closed set: displayName, email, campaignName,
	// accountName -- two slice entries each.
	baseMergeTagCount = 8
)

func mergeReplacers(c Campaign, r Recipient, accountName string) (text, html *strings.Replacer) {
	pairs := func(escape bool) []string {
		// CONSTANT capacity, covering the closed base set only. The fields
		// grow the slice through append.
		//
		// It used to be `baseMergeTagCount + 2*len(r.Fields)`, and a
		// recipient's `fields` map is whatever columns a CSV carried -- the
		// one caller-influenced length on the render path. Doubled without a
		// bound it wraps negative, and make with a negative capacity panics.
		// Clamping it first was the obvious repair and is NOT what is here,
		// because CodeQL's range analysis does not follow a clamp on a len()
		// and kept flagging it -- and an unproven bound that a reader believes
		// is worse than no bound at all.
		//
		// So the arithmetic is gone rather than guarded. A capacity hint is
		// only ever an optimization: append reallocates a handful of times for
		// a recipient with many fields, on a path that is already doing string
		// substitution over a whole message body. That is a real cost of
		// approximately nothing, paid for an allocation size that is now a
		// constant anybody can check by reading it.
		out := make([]string, 0, baseMergeTagCount)
		add := func(tag, value string) {
			if escape {
				value = htmlEscape(value)
			}
			out = append(out, mergeTagPrefix+tag+mergeTagSuffix, value)
		}
		add("displayName", greetingNameFor(r))
		add("email", strings.TrimSpace(r.Email))
		add("campaignName", strings.TrimSpace(c.Name))
		add("accountName", strings.TrimSpace(accountName))
		for key, value := range r.Fields {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			add(mergeFieldsPrefix+key, mergeValueString(value))
		}
		return out
	}
	return strings.NewReplacer(pairs(false)...), strings.NewReplacer(pairs(true)...)
}

// greetingNameFor is the {{displayName}} value: the recipient's own name,
// falling back to the address's local part and then to "there". The fallback
// chain exists because the tag is overwhelmingly used in a greeting, and
// "Hi ," is worse than a slightly impersonal one.
func greetingNameFor(r Recipient) string {
	if name := strings.TrimSpace(r.DisplayName); name != "" {
		return name
	}
	if at := strings.Index(r.Email, "@"); at > 0 {
		return r.Email[:at]
	}
	return "there"
}

// mergeValueString renders one imported field value as text.
//
// Strings pass through. Numbers and booleans are rendered in their Go
// spelling, which is what a CSV import produced them from anyway. Anything
// STRUCTURED -- a nested object or list -- renders EMPTY rather than as Go's
// %v, because "map[a:1]" in the middle of a sentence in somebody's inbox is
// worse than a blank, and there is no spelling of a nested object that reads
// as prose.
func mergeValueString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case json.Number:
		return t.String()
	default:
		return ""
	}
}

// unresolvedTagPattern finds anything shaped like a merge tag. Deliberately
// PERMISSIVE about what may sit between the braces -- an imported column can
// be called "Company Name" or "2026 spend", so a tag naming one contains a
// space or a digit-leading segment and an identifier-shaped pattern would
// miss exactly the typos this exists to catch. Bounded so a stray "{{" in a
// body cannot make the scanner walk the whole message as one match.
var unresolvedTagPattern = regexp.MustCompile(`\{\{([^{}\r\n]{1,120}?)\}\}`)

// UnresolvedMergeTags reports the merge tags still sitting literally in a
// rendered body, deduplicated and in first-seen order.
//
// It runs AFTER substitution, which is the only order that can answer the
// question: a tag the replacer resolved is gone, so whatever is left is a tag
// no value existed for. That is a typo'd {{fields.compnay}}, and the test
// send is what puts it in front of an operator before the whole audience gets
// it (design D11).
//
// It reports rather than refuses, deliberately. An unknown tag stays LITERAL
// in the body -- which is a visible defect in one test message and a
// recoverable one -- whereas a hard preflight gate would refuse sends over a
// "{{" that an operator meant as text.
func UnresolvedMergeTags(bodies ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, body := range bodies {
		for _, m := range unresolvedTagPattern.FindAllStringSubmatch(body, -1) {
			tag := mergeTagPrefix + m[1] + mergeTagSuffix
			if seen[tag] {
				continue
			}
			seen[tag] = true
			out = append(out, tag)
		}
	}
	return out
}

// TrackingRender is the per-message half of open/click tracking
// (memql#4823): a base origin plus the minting function that turns one
// (kind, url) pair into a signed token.
//
// The ZERO VALUE TRACKS NOTHING, and that default is deliberate rather than
// incidental. Tracking attributes a hit to a DELIVERY row, so a caller with
// no delivery in hand -- the test send, any future preview -- has nothing to
// attribute to, and the honest answer is to render an untracked body rather
// than a pixel that records against an id nobody has. Making tracking the
// default would mean every such caller had to remember to turn it off.
//
// Mint is injected rather than called directly so this file needs to know
// nothing about the key ring: render.go decides WHERE a tracked URL goes in
// the body, tracking_token.go decides what makes it unforgeable.
type TrackingRender struct {
	// BaseURL is the public origin the recipient's client reaches --
	// MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL, deliberately reused rather than
	// given a variable of its own: it is the same public origin, and a
	// second one is a second thing to get wrong that would present as a
	// broken image in somebody's inbox.
	BaseURL string
	// Opens and Clicks are the campaign's own switches.
	Opens  bool
	Clicks bool
	// Mint signs one tracking token for (kind, url). url is empty for an
	// open. A nil Mint disables tracking entirely, which is what a
	// zero-valued TrackingRender is.
	Mint func(kind, url string) (string, error)
}

// active reports whether anything is tracked at all.
func (tr TrackingRender) active() bool {
	return tr.Mint != nil && strings.TrimSpace(tr.BaseURL) != "" && (tr.Opens || tr.Clicks)
}

// trackedURL builds the public URL a tracking hit is fetched from.
func (tr TrackingRender) trackedURL(path, token string) string {
	base := strings.TrimRight(strings.TrimSpace(tr.BaseURL), "/")
	return base + path + token
}

// hrefPattern matches an href attribute's value in either quoting style.
// Deliberately a regexp over the body rather than an HTML parse: a campaign's
// HTML part is operator-authored fragment markup that no parser round-trips
// unchanged, and re-serializing somebody's carefully-built table layout
// through a DOM is a visible change to every message for the sake of a link
// rewrite. The pattern only ever REPLACES the quoted value, so anything it
// does not match is left exactly as authored.
var hrefPattern = regexp.MustCompile(`(?i)(href\s*=\s*)(["'])([^"']*)(["'])`)

// rewriteLinks points every http(s) href at the signed click endpoint.
//
// HTML PART ONLY, and the text part is deliberately untouched: rewriting a
// URL a reader can SEE is visible mangling of the message for a number
// nobody asked for. A recipient reading the plain-text alternative gets the
// real link.
//
// A non-http scheme is left alone -- mailto:, tel:, an anchor, and in
// particular anything html/template already neutralised. The signature over
// the url is what makes the redirect open-redirect-proof, so a target that
// cannot be signed is a target that is not rewritten rather than one that
// redirects unverified.
func (tr TrackingRender) rewriteLinks(body string) (string, error) {
	if !tr.active() || !tr.Clicks {
		return body, nil
	}
	var mintErr error
	out := hrefPattern.ReplaceAllStringFunc(body, func(match string) string {
		parts := hrefPattern.FindStringSubmatch(match)
		if len(parts) != 5 {
			return match
		}
		target := strings.TrimSpace(parts[3])
		if !isTrackableURL(target) {
			return match
		}
		// The href value in the body is HTML-escaped (an authored `&` in a
		// query string is `&amp;`), and the token has to be signed over the
		// URL the recipient will actually be redirected to. Unescape for
		// signing, re-escape for the attribute.
		token, err := tr.Mint(EngagementClick, html.UnescapeString(target))
		if err != nil {
			mintErr = err
			return match
		}
		return parts[1] + parts[2] + htmlEscape(tr.trackedURL(TrackingClickPath, token)) + parts[4]
	})
	if mintErr != nil {
		return "", fmt.Errorf("campaigns: signing a tracked link: %w", mintErr)
	}
	return out, nil
}

// openPixel returns the 1x1 image tag, or "" when opens are not tracked.
//
// An EMPTY alt attribute, deliberately: a client that blocks images renders
// alt text, and a beacon that announces itself in the middle of a message is
// worse than an invisible gap. Explicit width and height so a client that
// cannot load it reserves one pixel rather than a broken-image placeholder.
func (tr TrackingRender) openPixel() string {
	if !tr.active() || !tr.Opens {
		return ""
	}
	token, err := tr.Mint(EngagementOpen, "")
	if err != nil {
		// A pixel that cannot be signed is simply absent. Unlike a click,
		// there is nothing to degrade: the message is complete without it,
		// and refusing to send a campaign because an open could not be
		// counted would trade the product for the metric.
		return ""
	}
	return `<img src="` + htmlEscape(tr.trackedURL(TrackingOpenPath, token)) + `" width="1" height="1" alt="">`
}

// isTrackableURL reports whether a href value is an absolute http(s) target.
//
// Absolute only. A relative href in an email body is already broken (there is
// no document base to resolve it against), and signing one would produce a
// redirect to a path on the tracking origin itself.
func isTrackableURL(raw string) bool {
	lowered := strings.ToLower(strings.TrimSpace(raw))
	return strings.HasPrefix(lowered, "http://") || strings.HasPrefix(lowered, "https://")
}

// renderMessage builds the outgoing message for one recipient.
//
// Personalization is the closed merge-tag set mergeReplacers documents, in
// two replacers whose escaping asymmetry is the point. Tracking, when the
// campaign asks for it, rewrites the HTML part only.
func renderMessage(c Campaign, t Template, r Recipient, unsubscribeURL string, opts RenderOptions) (email.Message, error) {
	subst, substHTML := mergeReplacers(c, r, opts.AccountName)

	text := subst.Replace(t.TextBody)
	text += fmt.Sprintf("\r\n\r\n--\r\nYou are receiving this because you subscribed to %s.\r\nUnsubscribe: %s\r\n",
		displayNameFor(c), unsubscribeURL)

	msg := email.Message{
		To:       r.Email,
		Subject:  opts.SubjectPrefix + subst.Replace(t.Subject),
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
		// ORDER IS LOAD-BEARING. Substitute, then rewrite links for click
		// tracking, then append the footer, then append the pixel:
		//
		//   - the unsubscribe footer's own href must NOT become a tracked
		//     link. A click on "unsubscribe" is not engagement, and routing
		//     an opt-out through a redirect adds a hop to the one link that
		//     must work;
		//   - the pixel is appended after everything, so nothing rewrites it.
		body, err := opts.Tracking.rewriteLinks(substHTML.Replace(t.HTMLBody))
		if err != nil {
			return email.Message{}, err
		}
		msg.HTMLBody = body + footer + opts.Tracking.openPixel()
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
