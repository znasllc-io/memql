package campaigns

import (
	"fmt"
	"regexp"
	"strings"
)

// feedback_parse.go -- turning a provider's bounce report into a
// suppression (memql#3461).
//
// # What was missing, and why it mattered more than "an extension point exists"
//
// memql#3348 built the bounce HANDLING: a hard bounce suppresses the address
// cluster-wide without deleting the audience membership, a soft bounce does
// neither, and campaignRecordFeedback is the door. What did not ship was
// anything that turned an actual provider's feedback into a call to it. So
// the machinery that stops us mailing bad addresses was complete, and the
// wire that says which addresses are bad was absent -- and every other
// deliverability control in the domain is load-bearing on that input. The
// suppression list outranks every audience and was empty by default. The
// hard/soft distinction is the whole design of the bounce decision and is a
// classification only the provider carries.
//
// # Two formats, and the first one is not a vendor
//
// RFC 3464 is the STANDARD delivery status notification: the thing an MTA
// sends back when a message cannot be delivered. Microsoft Graph, Exchange,
// Postfix and essentially every SMTP-era relay produce one, so a DSN parser
// is provider-agnostic in a way a vendor parser cannot be -- it covers the
// engine's own preferred sender without being about it. It cannot express a
// COMPLAINT, though, because a complaint is not a delivery failure: nothing
// bounced, a person pressed "spam".
//
// So the second format is Amazon SES's SNS feedback, which carries both, and
// carries the complaint feedback loop a DSN structurally cannot.
//
// # Classification comes from the provider, never from the text
//
// The acceptance criterion the issue states, and the one worth defending: a
// parser that decided hard-vs-soft by looking for "user unknown" in a
// diagnostic string would be inventing a verdict. Both parsers read the
// provider's own field -- the DSN's `Status:` class and `Action:`, SES's
// `bounceType` -- and a payload carrying neither is UNPARSEABLE rather than
// guessed at.
//
// # An unrecognised payload is refused, not dropped
//
// Silently ignoring what we cannot read recreates the same invisible gap one
// level down: the operator believes bounces are flowing and the list stays
// empty. So a payload that does not parse produces an error, the caller
// stamps the inbound row `failed` with the reason, and it is queryable
// through inboundRequestsByStatus. A payload that parses and legitimately
// carries no bounce -- a delivery receipt, an SNS subscription confirmation
// -- is a different outcome with a different name (Ignored), because
// treating "nothing to do" as a failure trains an operator to ignore
// failures.

// Feedback formats, as named in MEMQL_CAMPAIGNS_FEEDBACK_SOURCES.
const (
	// FormatRFC3464 is the standard delivery status notification. Bounces
	// only; a DSN cannot carry a complaint.
	FormatRFC3464 = "rfc3464"
	// FormatSES is Amazon SES feedback delivered over SNS. Bounces and
	// complaints.
	FormatSES = "ses"
)

// FeedbackReport is one recipient-level verdict from a provider.
type FeedbackReport struct {
	// Email is the address the provider reported. Digested before it
	// reaches the graph; never stored.
	Email string
	// Kind is hard_bounce | soft_bounce | complaint, taken from the
	// provider's own classification field.
	Kind string
	// CampaignID is the send this report is about, when the payload
	// carried it back. Empty otherwise -- attribution is a nice-to-have
	// for a deliverability review, never a precondition for suppressing.
	CampaignID string
	// Note is the provider's own classification string, for the
	// suppression row's provenance. Never the address.
	Note string
}

// FeedbackParse is the outcome of reading one payload.
type FeedbackParse struct {
	// Reports is what to act on. Empty is legitimate -- see Ignored.
	Reports []FeedbackReport
	// Ignored explains a well-formed payload that carried no bounce or
	// complaint: a delivery receipt, an SNS handshake. Empty when Reports
	// is non-empty.
	Ignored string
}

// SupportedFeedbackFormats is what a source may be configured as.
func SupportedFeedbackFormats() []string { return []string{FormatRFC3464, FormatSES} }

// ParseFeedback reads one provider payload in the named format.
//
// An error means the payload could not be understood, which is a condition
// the operator has to see -- not a reason to move on quietly.
func ParseFeedback(format, body string) (FeedbackParse, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case FormatRFC3464:
		return parseDSN(body)
	case FormatSES:
		return parseSESFeedback(body)
	default:
		return FeedbackParse{}, fmt.Errorf(
			"feedback format %q is not one of %s", format, strings.Join(SupportedFeedbackFormats(), " / "))
	}
}

// bounceKindForStatusClass maps a DSN / SMTP status class onto the domain's
// bounce vocabulary. This is THE classification rule, in one place, and it
// is the provider's number rather than our reading of their prose:
//
//	5.x.x  permanent -- the address does not work and will not start
//	4.x.x  transient -- a full mailbox, a greylisting relay, a bad afternoon
//
// Anything else is not a classification we recognise, and the caller says so
// rather than picking one.
func bounceKindForStatusClass(class byte) (string, bool) {
	switch class {
	case '5':
		return "hard_bounce", true
	case '4':
		return "soft_bounce", true
	default:
		return "", false
	}
}

// normalizeReportedAddress trims the shapes a provider wraps an address in
// -- `<user@example.com>`, `rfc822; user@example.com` -- and validates it
// the same way the suppression digest will.
//
// Validation here rather than only at the digest: a report naming an address
// this cannot read is a parse failure the operator should see, not a silent
// zero-report payload.
func normalizeReportedAddress(raw string) string {
	value := strings.TrimSpace(raw)
	if semi := strings.Index(value, ";"); semi >= 0 {
		// `Final-Recipient: rfc822; user@example.com` -- the part before
		// the semicolon is the address TYPE, not the address.
		value = strings.TrimSpace(value[semi+1:])
	}
	value = strings.TrimSpace(strings.Trim(strings.TrimSpace(value), "<>"))
	if NormalizeEmail(value) == "" {
		return ""
	}
	return value
}

// addressLike matches an email address well enough to redact one. Not a
// validator -- deliberately greedy about what counts, because the cost of a
// false positive is a mangled diagnostic string and the cost of a false
// negative is a mailbox in the graph.
var addressLike = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// redactAddressesIn strips mailboxes out of provider prose.
//
// This is load-bearing rather than tidy. A provider's diagnostic routinely
// quotes the address back -- `550 5.1.1 <dead@example.test> User unknown` --
// and that string is headed for v1:campaigns:suppression.note, on a
// CLUSTER-WIDE row whose entire privacy argument is that it stores a digest
// and a domain and never a mailbox. Copying the address into the note beside
// the digest would undo that for every bounce, which is the one place it
// matters most.
//
// The domain survives, because "which domains are bouncing" is the question
// a deliverability review actually asks and it identifies nobody. Caught by
// TestDSNIngestionSuppressesEndToEnd, which asserts the plaintext address
// never reaches the graph.
func redactAddressesIn(text string) string {
	return addressLike.ReplaceAllStringFunc(text, func(match string) string {
		if at := strings.LastIndex(match, "@"); at > 0 {
			return "***@" + match[at+1:]
		}
		return "***"
	})
}
