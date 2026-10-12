package campaigns

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	texttemplate "html/template"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	nethtml "golang.org/x/net/html"

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
//
// # The footer is the default, and a template can carry its own
//
// A message gets the footer below unless its template already links to the
// unsubscribe page, which is what {{unsubscribeUrl}} is for: a layout with a
// footer of its own puts the tag in it and is not given a second one. The header
// pair is there either way. Deciding that is a judgement about what a person
// would SEE, so it is made per part and from the tokenized HTML, not from a
// search of the text (see planHTML and textCarriesTag).
//
// The footer names who the person subscribed to: the sending identity's display
// name, else the organization, else "this mailing list" (footerSender). It never
// names the template's subject, which a single send used to do.
//
// # Where the footer sits
//
// Inside the body, at the start of the template's last real closing body tag
// (else its last closing html tag, else the end), and nowhere else is touched:
// every other byte of the template is sent as written (see completeHTML). A
// footer written after the last byte of an ordinary document lands after
// </html>, and one written after a template that ends in an open tag, an
// unterminated comment or a style element lands INSIDE the unfinished construct,
// where no client shows it. The second is refused instead, with a sentence for
// the template's author, because a message with no footer is the one thing this
// package must not send.

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
	//
	// It is also the footer's second choice of who the person subscribed to,
	// after SenderName.
	AccountName string

	// SenderName is who the footer says the person subscribed to: the display
	// name of the identity the message is sent as, which callers take from
	// identity.SendAs.FromName. Empty falls back to AccountName and then to
	// "this mailing list" (footerSender).
	SenderName string

	// Links is every address the message can point at. Links.Unsubscribe is
	// required: it is the footer's link and the List-Unsubscribe header, and
	// renderMessage refuses a message without one. The other three back
	// {{preferencesUrl}}, {{viewUrl}} and {{confirmUrl}}, and an empty one
	// renders its tag empty.
	Links MessageLinks

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
// now EIGHT fixed tags -- {{displayName}}, {{email}}, {{campaignName}},
// {{accountName}} and the four link tags {{unsubscribeUrl}},
// {{preferencesUrl}}, {{viewUrl}} and {{confirmUrl}} -- plus
// {{fields.<key>}} for each key the recipient actually carries. The reason
// that is not a slide toward an expression evaluator is worth stating
// precisely, because "a closed set" sounds like the same argument every
// template language starts from.
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
	// accountName and the four link tags -- two slice entries each.
	baseMergeTagCount = 16
)

// The four link tags. Each resolves to the message's own link of that name
// (MessageLinks) and renders empty when the message was not given one.
//
// Their values are URLs this package built, and they are escaped in the HTML
// replacer like everything else: the origin they start with is configuration,
// and a value that reaches markup unescaped is the bug this file's two-replacer
// layout exists to prevent.
const (
	tagUnsubscribeURL = "unsubscribeUrl"
	tagPreferencesURL = "preferencesUrl"
	tagViewURL        = "viewUrl"
	tagConfirmURL     = "confirmUrl"
)

func mergeReplacers(c Campaign, r Recipient, accountName string, links MessageLinks) (text, html *strings.Replacer) {
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
		add(tagUnsubscribeURL, links.Unsubscribe)
		add(tagPreferencesURL, links.Preferences)
		add(tagViewURL, links.View)
		add(tagConfirmURL, links.Confirm)
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
//
// (The footer and the open pixel are placed with the HTML tokenizer, after this,
// because where they go is a question about which "</body>" is a tag and no
// pattern can answer it; see planHTML. It reads the part and does not rewrite
// it, so the same goes for it: every byte outside the fragment is the author's.)
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
//
// keep names URLs that are never rewritten: the message's own links
// (MessageLinks.exempt). An href is compared by the value a browser reads from
// it, so one spelled with entities (`&amp;`) matches, and a kept href is left
// exactly as it was written, quotes and all.
func (tr TrackingRender) rewriteLinks(body string, keep map[string]bool) (string, error) {
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
		if !isTrackableURL(target) || keep[html.UnescapeString(target)] {
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

// errNoUnsubscribeLink is the refusal for a message with no unsubscribe link. A
// message the engine sends without one is the thing this package exists to make
// impossible, so it is an error at the last place it could be caught and not a
// footer that quietly says nothing.
var errNoUnsubscribeLink = errors.New("campaigns: a message needs its unsubscribe link")

// renderMessage builds the outgoing message for one recipient.
//
// Personalization is the closed merge-tag set mergeReplacers documents, in
// two replacers whose escaping asymmetry is the point. Tracking, when the
// campaign asks for it, rewrites the HTML part only.
//
// opts.Links.Unsubscribe is required (errNoUnsubscribeLink). It is the
// List-Unsubscribe header, the footer's link and {{unsubscribeUrl}}; the
// header pair is on every message, whatever the body carries.
func renderMessage(c Campaign, t Template, r Recipient, opts RenderOptions) (email.Message, error) {
	if strings.TrimSpace(opts.Links.Unsubscribe) == "" {
		return email.Message{}, errNoUnsubscribeLink
	}
	subst, substHTML := mergeReplacers(c, r, opts.AccountName, opts.Links)

	text := subst.Replace(t.TextBody)
	if !textCarriesTag(t.TextBody, mergeTagPrefix+tagUnsubscribeURL+mergeTagSuffix) {
		text += fmt.Sprintf("\r\n\r\n--\r\nYou are receiving this because you subscribed to %s.\r\nUnsubscribe: %s\r\n",
			footerSender(opts), opts.Links.Unsubscribe)
	}

	msg := email.Message{
		To:       r.Email,
		Subject:  opts.SubjectPrefix + subst.Replace(t.Subject),
		TextBody: text,
		Headers: map[string]string{
			// Angle brackets are required by RFC 2369 -- a bare URI here
			// is a header some clients silently ignore, which is the same
			// as having no unsubscribe at all.
			headerListUnsubscribe:     "<" + opts.Links.Unsubscribe + ">",
			headerListUnsubscribePost: oneClickValue,
			headerCampaignID:          c.ID,
		},
	}
	if strings.TrimSpace(t.HTMLBody) != "" {
		// ORDER IS LOAD-BEARING. Substitute, then rewrite links for click
		// tracking, then place the footer and the pixel:
		//
		//   - the message's own links (the footer's, and any the template
		//     reaches through a link tag) must NOT become tracked links. A
		//     click on "unsubscribe" is not engagement, and routing an opt-out
		//     through a redirect adds a hop to the one link that must work;
		//   - the footer and the pixel are placed last, so nothing rewrites them.
		body, err := opts.Tracking.rewriteLinks(substHTML.Replace(t.HTMLBody), opts.Links.exempt())
		if err != nil {
			return email.Message{}, err
		}
		if msg.HTMLBody, err = completeHTML(body, opts); err != nil {
			return email.Message{}, err
		}
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

// footerSender is who the footer says the person subscribed to: the display name
// the message is sent under, else the organization, else "this mailing list".
//
// It is never built from the campaign, and above all never from a template's
// subject, which is what a single send's synthetic campaign was named after.
// Whitespace is collapsed so a name with a line break in it stays one line in a
// plain-text footer.
func footerSender(opts RenderOptions) string {
	for _, name := range []string{opts.SenderName, opts.AccountName} {
		if name = strings.Join(strings.Fields(name), " "); name != "" {
			return name
		}
	}
	return "this mailing list"
}

// templateFault is a refusal whose text is for the person who wrote the
// template. It is a sentence that says what is wrong and what to do, and the
// places that show a render failure (a failed campaign, a test send) show it as
// it is, with no wrapper that would turn it back into a diagnostic.
type templateFault string

func (f templateFault) Error() string { return string(f) }

const (
	// errTemplateEndsOpen: the HTML stops inside a comment, a tag or an element
	// whose content is text (<style>, <script>, <textarea>, <title>, <xmp>,
	// <iframe>, <noembed>, <noframes>, <noscript>, <plaintext>). Anything
	// written after the last byte of such a template is part of the unfinished
	// construct, and no client shows it. The same sentence is the answer when the
	// tokenizer found a clean spot and a client's parser still would not show what
	// is put there (checkShown): a comment that only the tree builder sees, an
	// element left open whose content is not the page's.
	errTemplateEndsOpen = templateFault("This template's HTML ends inside a comment or a tag, so its unsubscribe link cannot be placed. Fix the HTML and save it again.")

	// errTemplateFrames: a frameset document has no body, and a message built
	// from one shows nothing a footer could be part of.
	errTemplateFrames = templateFault("This template uses frames, which email cannot show.")

	// errTemplateOutlookOpen: a section that Outlook does not show (<![if !mso]>
	// or <!--[if !mso]><!-->, see readSection) is still open where the footer
	// would go, so Outlook would not show the footer.
	errTemplateOutlookOpen = templateFault("This template leaves an Outlook-only section open at its end, so its unsubscribe link cannot be placed. Close the section and save it again.")
)

// completeHTML adds to a substituted HTML part what every message needs and the
// template may not carry: the unsubscribe footer, unless the template already
// links to the unsubscribe page, and the open pixel when opens are tracked.
//
// # The template's bytes are the template's
//
// The footer and the pixel are one fragment, put in at one byte offset
// (planHTML); every other byte of the part is the byte the author wrote. The
// part is not parsed into a tree and written back out. That would change what a
// client renders: a downlevel-revealed conditional comment (<![if !mso]>) is
// read as an ordinary comment and written as one, so Outlook shows what the
// author hid from it; a table gains a tbody; a fragment gains an html, a head
// and a body; entities and attributes are respelled. Layouts are held to a
// pixel budget against the emails they reproduce, and conditional comments
// around buttons, columns and media queries are how they get there.
//
// # A fault is refused, not repaired
//
// The same pass finds the templates a fragment cannot be put in. Those return a
// templateFault whether or not this message would have anything placed in it:
// whether a message needs a footer depends on the template and whether it needs
// a pixel depends on the campaign, and a template that passed a test send, which
// tracks nothing, only to fail the campaign, which tracks opens, would teach the
// author nothing.
//
// # What a client shows is a tree's answer
//
// The tokenizer reads a template alone, with scripting on and no namespaces. A
// mail client builds a tree, with scripting off, and the two can disagree about
// what is markup: a <noscript> whose content opens a comment, an svg <style> or
// <![CDATA[ section, an element left open whose content is not the page's. So
// the spot is checked again, after the pass and before anything is placed, in a
// tree built the client's way (checkShown), and a template that fails is refused
// with the same sentence as one that ends inside a comment. The bytes sent are
// still the splice's: the tree only judges.
func completeHTML(body string, opts RenderOptions) (string, error) {
	plan, err := planHTML(body, opts.Links.Unsubscribe)
	if err != nil {
		return "", err
	}
	if err := checkShown(body, plan, opts); err != nil {
		return "", err
	}
	var fragment strings.Builder
	if !plan.linksToUnsubscribe {
		// The footer goes through html/template rather than Sprintf + a manual
		// escaper. Not scanner appeasement: `{{.URL}}` sits in an href, and
		// html/template is CONTEXT-aware -- it applies URL escaping there and
		// text escaping around it, and its urlFilter neutralises a `javascript:`
		// scheme, none of which html.EscapeString knows to do. Hand-escaping
		// into an attribute means re-deriving per call site which of those
		// applies; this asks the standard library instead (memql#3348).
		footer, err := footerTemplate(footerSender(opts), opts.Links.Unsubscribe)
		if err != nil {
			// Unreachable with a fixed template and string inputs; refusing to
			// send beats mailing a body with no unsubscribe footer, which is the
			// one thing RFC 8058 compliance cannot go out without.
			return "", err
		}
		fragment.WriteString(footer)
	}
	fragment.WriteString(opts.Tracking.openPixel())
	if fragment.Len() == 0 {
		return body, nil
	}
	return body[:plan.insertAt] + fragment.String() + body[plan.insertAt:], nil
}

// The addresses checkShown puts in the footer and the pixel it tries at the
// spot. They are never sent. The check asks whether a client would show a footer
// and a pixel put there, and asks it of every template, whatever is to be placed:
// the real footer goes in only when the template has no link of its own, and the
// real pixel only when the campaign tracks opens. The same question for every
// message is what lets a test send, which tracks nothing, and a campaign, which
// tracks opens, agree about a template.
const (
	probeUnsubscribeURL = "https://probe.invalid/unsubscribe"
	probePixelSrc       = "https://probe.invalid/pixel"
)

// checkShown builds, the way a mail client does, the message the template would
// make with a footer and a pixel at the spot, and refuses the template when a
// person would not be shown them. It is read-only: the bytes that are sent are the
// splice's, and the tree only judges.
//
// A client parses with the HTML5 algorithm and scripting off. What is shown is an
// element of the HTML namespace with no <template>, <noscript> or foreign-content
// (svg, math) ancestor; a link or an image that a comment swallowed, that sits
// in a template, or that is an svg element, is not shown. The template's own link,
// when it has one, has to be shown too, in the same tree: the tokenizer counted
// it, and a client may see otherwise.
//
// The cost is one more pass over the part, about 0.3 ms for a 33 kB message
// (BenchmarkCheckShown), against the round trip to the provider, so there is
// nothing worth caching per revision.
func checkShown(body string, plan htmlPlan, opts RenderOptions) error {
	footer, err := footerTemplate(footerSender(opts), probeUnsubscribeURL)
	if err != nil {
		return err
	}
	probe := footer + `<img src="` + probePixelSrc + `" width="1" height="1" alt="">`
	doc, err := nethtml.ParseWithOptions(
		strings.NewReader(body[:plan.insertAt]+probe+body[plan.insertAt:]),
		nethtml.ParseOptionEnableScripting(false))
	if err != nil {
		return fmt.Errorf("campaigns: reading the HTML part: %w", err)
	}
	if !clientShows(doc, opts.Links.Unsubscribe).enough(plan.linksToUnsubscribe) {
		return errTemplateEndsOpen
	}
	return nil
}

// shownByClient is what a client's tree shows of the probe and of the
// template's own link.
type shownByClient struct {
	footer  bool // the probe footer's link
	pixel   bool // the probe pixel
	ownLink bool // a link to the message's own unsubscribe address
}

// clientShows reads a client's tree for what checkShown asks about.
func clientShows(doc *nethtml.Node, ownURL string) (got shownByClient) {
	eachShownElement(doc, func(n *nethtml.Node) {
		switch n.Data {
		case "a":
			if href := attrValue(n, "href"); href == probeUnsubscribeURL {
				got.footer = true
			} else if ownURL != "" && href == ownURL {
				got.ownLink = true
			}
		case "img":
			if attrValue(n, "src") == probePixelSrc {
				got.pixel = true
			}
		}
	})
	return got
}

// enough is whether a message with this much shown has what every message needs:
// a spot where the footer is shown, one where the pixel is, and, when the
// template was counted as having a link of its own, that link.
func (got shownByClient) enough(templateHasOwnLink bool) bool {
	return got.footer && got.pixel && (!templateHasOwnLink || got.ownLink)
}

// eachShownElement calls visit for every element of the tree that a client
// shows: in the HTML namespace, with no <template>, <noscript> or foreign-content
// ancestor.
func eachShownElement(doc *nethtml.Node, visit func(*nethtml.Node)) {
	type frame struct {
		node   *nethtml.Node
		hidden bool
	}
	stack := []frame{{doc, false}}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		hidden := f.hidden
		if f.node.Type == nethtml.ElementNode {
			if !hidden && f.node.Namespace == "" {
				visit(f.node)
			}
			hidden = hidden || f.node.Namespace != "" || f.node.Data == "template" || f.node.Data == "noscript"
		}
		for c := f.node.FirstChild; c != nil; c = c.NextSibling {
			stack = append(stack, frame{c, hidden})
		}
	}
}

// attrValue is the value of an element's first attribute of that name, trimmed
// the way a browser trims a URL.
func attrValue(n *nethtml.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return strings.TrimFunc(a.Val, isURLPadding)
		}
	}
	return ""
}

// htmlPlan is what one pass over a template's HTML finds.
type htmlPlan struct {
	// insertAt is the byte offset the footer and the pixel go at: the start of
	// the last real closing body tag, else of the last real closing html tag,
	// else the end of the part. A closing tag inside a <template> is not real: it
	// closes nothing of the document's, and a footer put before it is inside the
	// template, where no client shows it.
	insertAt int

	// linksToUnsubscribe is whether the template has a link a client would show
	// to the unsubscribe URL: a real <a> whose href is exactly that URL (the
	// space and control characters around the value are dropped, as a browser
	// drops them before it reads a URL).
	//
	// It is how a template that links to {{unsubscribeUrl}} itself is
	// recognised, and it counts nothing else. The tag in a hidden preheader, an
	// alt or title attribute, a comment, a script, the text of a link to
	// somewhere else or the middle of a longer URL shows nobody an opt-out, so
	// none of them stops the footer. Neither does a link inside a <template>,
	// whose content a client does not render, or inside <svg> or <math>, whose
	// <a> is not an HTML link. When an <a> repeats href, the first is the one a
	// browser uses, and the tokenizer keeps only that one.
	linksToUnsubscribe bool
}

// rawTextElements are the elements the HTML tokenizer reads the content of as
// text, up to their own end tag; <plaintext> has none and runs to the end of
// the input. Markup inside them is not markup: a closing body tag in a script is
// a few characters of script. This is the tokenizer's own list.
var rawTextElements = map[string]bool{
	"iframe": true, "noembed": true, "noframes": true, "noscript": true,
	"plaintext": true, "script": true, "style": true, "textarea": true,
	"title": true, "xmp": true,
}

// planHTML reads a template's HTML once, with the HTML tokenizer, and says where
// the footer goes and whether the template has its own link. It returns a
// templateFault for a template the footer cannot be put in.
//
// # Why the tokenizer
//
// The tokenizer's tokens partition the bytes (x/net/html documents it): there is
// no gap and no overlap, so the offset of a token is the sum of the lengths of
// the tokens before it. It also knows what is a tag and what is not -- a
// comment, a bogus comment (<![if !mso]>, <?xml ?>), the content of a script or
// a style, an attribute value -- which no search of the string for "</body>"
// can know. It does not build a tree, which is the point.
//
// # What it cannot know, and what it does about it
//
// It does not know which elements are still open when the document ends, so the
// fragment lands inside whatever is. A table cell or a division left open is
// where the footer is shown, and that is fine; an anchor left open nests the
// footer in a link, which a client sorts out. An element whose content a client
// does not show as HTML (<template>, <svg>, <math>) left open would hide it, and
// so would a comment or a style that only a tree builder sees, and the tokenizer
// cannot see that. checkShown does, in a tree built the way a client builds one,
// and refuses what it finds. What a division with display:none hides is not
// something either can see. What this pass refuses itself is the end of the
// input falling inside something that swallows what follows it:
//
//   - a comment, a bogus comment or a doctype with no closing ">" (a comment
//     needs "-->" or "--!>", or to be "<!-->" or "<!--->");
//   - a start or end tag cut short, which the tokenizer reports as an error with
//     the partial tag still in its raw bytes, or a "<" or "</" with nothing
//     after it (a lone "<" would not swallow a tag written after it, but it is
//     the start of a tag the template did not finish, and the template is
//     refused for it all the same);
//   - a raw-text element with no end tag, <plaintext> always.
//
// A frameset is refused for a different reason: it has no body to show a footer
// in. A template whose spot is inside a section Outlook does not show (see
// readSection) is refused for a third, when a footer is to go there. The pass
// also checks that the tokens added up to the whole part, because an offset that
// is off by one is a footer in the middle of a tag.
func planHTML(source, unsubscribeURL string) (htmlPlan, error) {
	z := nethtml.NewTokenizer(strings.NewReader(source))
	plan := htmlPlan{insertAt: len(source)}
	closingHTML := -1
	var (
		offset     int
		rawText    string // the raw-text element the tokenizer is inside, if any
		inTemplate int
		inForeign  int
		cutShort   bool
		frames     bool
		bracket    bool // the token just read is text ending in "<" or "</"

		// The Outlook sections open at this point, innermost last, each true when
		// Outlook does not show it; hiding counts the true ones. spotHiding is
		// whether one was open where the footer will go.
		sections   []bool
		hiding     int
		spotHiding bool
		htmlHiding bool // hiding at the closing html tag closingHTML records
	)
	for {
		kind := z.Next()
		raw := z.Raw()
		start := offset
		offset += len(raw)

		if kind == nethtml.ErrorToken {
			if err := z.Err(); err != io.EOF {
				return htmlPlan{}, fmt.Errorf("campaigns: reading the HTML part: %w", err)
			}
			// The end of the input. Whatever the last token left in its raw
			// bytes is a tag the end cut short, and a "<" or "</" that ended the
			// last text is the start of one.
			cutShort = cutShort || len(raw) > 0 || bracket
			break
		}

		// Only the last token can be cut short by the end of the input, so a
		// bracket is judged when nothing follows it. "a<" is also a whole text
		// token when another "<" starts a tag right after it, as in
		// <<First Name>>, a merge tag some other tools write.
		bracket = false
		switch kind {
		case nethtml.TextToken:
			bracket = rawText == "" && (bytes.HasSuffix(raw, []byte("<")) || bytes.HasSuffix(raw, []byte("</")))
		case nethtml.CommentToken:
			cutShort = cutShort || !commentTerminated(raw)
			switch section, hidesFromOutlook := readSection(raw); section {
			case sectionOpens:
				sections = append(sections, hidesFromOutlook)
				if hidesFromOutlook {
					hiding++
				}
			case sectionCloses:
				if n := len(sections); n > 0 {
					if sections[n-1] {
						hiding--
					}
					sections = sections[:n-1]
				}
			}
		case nethtml.DoctypeToken:
			cutShort = cutShort || !bytes.HasSuffix(raw, []byte(">"))
		case nethtml.StartTagToken, nethtml.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			tag := string(name)
			// The tokenizer reads what follows these as text, a self-closing
			// slash notwithstanding (a start tag is a start tag).
			if rawTextElements[tag] {
				rawText = tag
			}
			frames = frames || tag == "frameset"
			switch tag {
			case "template":
				// A slash on a template is ignored by the HTML parser: it opens one.
				inTemplate++
			case "svg", "math":
				// In foreign content the slash is honoured: <svg/> is an empty svg.
				if kind == nethtml.StartTagToken {
					inForeign++
				}
			}
			if tag == "a" && inTemplate == 0 && inForeign == 0 && hiding == 0 && !plan.linksToUnsubscribe {
				plan.linksToUnsubscribe = hrefIs(z, hasAttr, unsubscribeURL)
			}
		case nethtml.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if tag == rawText {
				rawText = ""
			}
			switch tag {
			case "body":
				if inTemplate == 0 {
					plan.insertAt, spotHiding = start, hiding > 0
				}
			case "html":
				if inTemplate == 0 {
					closingHTML, htmlHiding = start, hiding > 0
				}
			case "template":
				if inTemplate > 0 {
					inTemplate--
				}
			case "svg", "math":
				if inForeign > 0 {
					inForeign--
				}
			}
		}
	}

	switch {
	case offset != len(source):
		return htmlPlan{}, fmt.Errorf("campaigns: reading the HTML part: the tokens cover %d of its %d bytes", offset, len(source))
	case frames:
		return htmlPlan{}, errTemplateFrames
	case cutShort || rawText != "":
		return htmlPlan{}, errTemplateEndsOpen
	}
	// No closing body tag: before the closing html tag if there is one, so the
	// footer is not written after the document, else at the end.
	if plan.insertAt == len(source) {
		spotHiding = hiding > 0
		if closingHTML >= 0 {
			plan.insertAt, spotHiding = closingHTML, htmlHiding
		}
	}
	// Outlook would not show a footer put where a section it hides is still open.
	// Only a footer that is to be put there: a template with a link of its own gets
	// none, and a pixel Outlook does not load changes no one's opt-out.
	if spotHiding && !plan.linksToUnsubscribe {
		return htmlPlan{}, errTemplateOutlookOpen
	}
	return plan, nil
}

// Outlook sections.
//
// A layout hides markup from Outlook, and shows it to every other client, with a
// downlevel-revealed conditional: <![if !mso]> ... <![endif]>, or the same
// written so that an HTML parser reads it as two ordinary comments,
// <!--[if !mso]><!--> ... <!--<![endif]-->. To the tokenizer each of the four is
// a comment, and what stands between them is markup like any other. Outlook
// shows the markup between them only when the expression is true for it, so the
// section is one Outlook hides when the expression is !mso.
//
// A footer put where such a section is still open is a footer Outlook does not
// show, and so is a link the template has inside one: neither is a way out for
// the person reading in Outlook. Only these two forms of !mso are read. Layouts
// close their sections around buttons, columns and media queries, and a section
// that is closed before the spot is no obstacle; the forms with other
// expressions ((gte mso 9), (!mso)|(IE), !IE) either show Outlook the markup or
// are not how layouts hide markup from it.
var (
	revealedSectionOpen = regexp.MustCompile(`(?is)^<!\[\s*if\s+([^\]]*?)\s*\]>$`)
	wrappedSectionOpen  = regexp.MustCompile(`(?is)^<!--\s*\[\s*if\s+([^\]]*?)\s*\]\s*><!.*-->$`)
	sectionClose        = regexp.MustCompile(`(?is)^(?:<!\[\s*endif\s*\]>|<!--\s*<!\[\s*endif\s*\]\s*-->)$`)

	sectionOpenForms = []*regexp.Regexp{revealedSectionOpen, wrappedSectionOpen}
)

type sectionToken int

const (
	notASection sectionToken = iota
	sectionOpens
	sectionCloses
)

// readSection reads one comment token of the tokenizer as a boundary of an
// Outlook section, and says whether the section it opens is one Outlook hides.
func readSection(raw []byte) (sectionToken, bool) {
	if bytes.IndexByte(raw, '[') < 0 {
		return notASection, false
	}
	if sectionClose.Match(raw) {
		return sectionCloses, false
	}
	for _, open := range sectionOpenForms {
		if m := open.FindSubmatch(raw); m != nil {
			return sectionOpens, hidesFromOutlook(string(m[1]))
		}
	}
	return notASection, false
}

// hidesFromOutlook is whether an expression in <![if EXPRESSION]> is false for
// Outlook, which is true for the mso feature.
func hidesFromOutlook(expression string) bool {
	switch strings.ToLower(strings.Join(strings.Fields(expression), "")) {
	case "!mso", "!(mso)", "(!mso)":
		return true
	}
	return false
}

// commentTerminated reports whether the raw bytes of a comment token, as the
// tokenizer read them, end the comment. A comment ends with "-->" or "--!>" after
// its "<!--", or is "<!-->" or "<!--->"; a bogus comment (<!x ...>, <?x ...>,
// </ ...>, and "<![if ...]>") ends at the first ">". Bytes that merely end in
// "-->" because the opening's own dashes are part of them ("<!--!>") do not.
func commentTerminated(raw []byte) bool {
	const opening = "<!--"
	if !bytes.HasPrefix(raw, []byte(opening)) {
		return bytes.HasSuffix(raw, []byte(">"))
	}
	body := raw[len(opening):]
	return string(body) == ">" || string(body) == "->" ||
		bytes.HasSuffix(body, []byte("-->")) || bytes.HasSuffix(body, []byte("--!>"))
}

// hrefIs reports whether the tag the tokenizer is at has an href equal to want.
func hrefIs(z *nethtml.Tokenizer, more bool, want string) bool {
	for more {
		var key, val []byte
		key, val, more = z.TagAttr()
		if string(key) == "href" {
			return strings.TrimFunc(string(val), isURLPadding) == want
		}
	}
	return false
}

// isURLPadding is what a browser strips from both ends of a URL attribute: the
// C0 controls and the space.
func isURLPadding(r rune) bool { return r <= ' ' }

// textCarriesTag reports whether the template's plain-text part shows tag as a
// URL on its own: between whitespace or the ends of the text, or alone inside a
// pair of angle brackets, which is how a plain-text message delimits one.
//
// Anything else is the tag inside other text -- glued to a label or a word, in
// parentheses, at the end of a sentence -- and a client may not make it a link.
// That is stricter than it has to be, on purpose. When this says no, the worst
// case is a second unsubscribe line; when it says yes it stands in for the
// footer, so a yes has to be one a reader can follow.
func textCarriesTag(text, tag string) bool {
	const edge rune = -1
	for from := 0; from < len(text); {
		i := strings.Index(text[from:], tag)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(tag)
		before, after := edge, edge
		if start > 0 {
			before, _ = utf8.DecodeLastRuneInString(text[:start])
		}
		if end < len(text) {
			after, _ = utf8.DecodeRuneInString(text[end:])
		}
		bounded := (before == edge || unicode.IsSpace(before)) && (after == edge || unicode.IsSpace(after))
		if bounded || (before == '<' && after == '>') {
			return true
		}
		from = end
	}
	return false
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
