package memql

// What a vendor's answer says about the ACCOUNT that paid for a call (design
// record docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions K10 and K11, safety rule S4).
//
// A vendor call is paid by one account: the cluster's own, or a person's API
// key. When the call fails, two readers need to know whether the failure is
// about that account or about something else:
//
//   - the router, which follows an account failure with the SAME model on the
//     next account, and any other failure with the next model (K10);
//   - the key's own state, which a refused key or an exhausted balance changes
//     and a rate limit or an outage does not (K11).
//
// The table below is that one decision, read off the vendors' documentation on
// 2026-11-22 and nowhere else. A status or a code it does not list is NOT an
// account failure: the cost of missing one is a call that moves to the next
// model instead of the next account, and the cost of inventing one is a
// working key marked refused.
//
// OPENAI'S RESTRICTED-KEY ANSWER IS NOT DOCUMENTED. Its error page says only
// that a 401 can mean the key lacks "the required permissions for the endpoint
// you are calling". The wording matched here ("insufficient permissions",
// "missing scopes") is what people report receiving, and was not confirmed
// with a real restricted key. It decides one thing: such a key is passed over
// for the call without being marked refused.
//
// Regression tests: vendor_account_errors_test.go.

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"

	"github.com/znasllc-io/memql/component/metrics"
)

// The two vendors a person can hold an account at. The values are the ones the
// sign-in and its counters already use, so a vendor has one spelling.
const (
	VendorAnthropic = metrics.FederationVendorAnthropic
	VendorOpenAI    = metrics.FederationVendorOpenAI
)

// The kinds of account failure.
const (
	// accountFailureRefused: the vendor does not accept this credential at all.
	accountFailureRefused = "refused"
	// accountFailureCredit: the account has no credit left, or reached a spend
	// limit. It serves again when somebody adds credit or the period turns.
	accountFailureCredit = "credit"
	// accountFailurePermission: the credential is good and may not do this.
	// Another account may be allowed to, and this one may serve another call.
	accountFailurePermission = "permission"
	// accountFailureUnavailable: the account could not be used and the vendor
	// was never asked: a key that was removed, a cluster with no sign-in.
	accountFailureUnavailable = "unavailable"
)

// VendorAccountFailureKinds lists every kind VendorAccountFailure can answer.
// The router words each one on the decision record, and its test asks this
// list so a kind added here cannot be left without words there.
func VendorAccountFailureKinds() []string {
	return []string{accountFailureRefused, accountFailureCredit, accountFailurePermission, accountFailureUnavailable}
}

// maxAccountFailureReason bounds the vendor's words kept on a ledger row and
// on a key's state.
const maxAccountFailureReason = 300

// VendorAccountError is an account failure the ENGINE found before a vendor
// answered: the key was removed, its value could not be unsealed, the cluster
// has no account at this vendor, or this cluster sends the vendor's calls
// somewhere a key must not go.
type VendorAccountError struct {
	Vendor string
	Kind   string
	Reason string
}

func (e *VendorAccountError) Error() string {
	return "vendor account " + e.Kind + ": " + e.Reason
}

// VendorAccountFailure reports whether a failed call failed because of the
// account that paid for it, and how.
//
// ok is false for everything else: a rate limit, an outage, a malformed
// request, a cancelled caller. The router keeps today's fallback for those.
func VendorAccountFailure(err error) (kind string, ok bool) {
	if err == nil {
		return "", false
	}
	var own *VendorAccountError
	if errors.As(err, &own) {
		return own.Kind, own.Kind != ""
	}
	// The cluster's own account cannot serve while its sign-in is refused or
	// waiting for the next identity token (ai_vendor_signin.go).
	var waiting *vendorSignInWaiting
	if errors.As(err, &waiting) {
		return accountFailureUnavailable, true
	}
	var refused *vendorExchangeRefused
	if errors.As(err, &refused) {
		return accountFailureUnavailable, true
	}
	var fromAnthropic *anthropic.Error
	if errors.As(err, &fromAnthropic) {
		kind, _ = classifyVendorAnswer(VendorAnthropic, fromAnthropic.StatusCode, []byte(fromAnthropic.RawJSON()))
		return kind, kind != ""
	}
	var fromOpenAI *openai.Error
	if errors.As(err, &fromOpenAI) {
		kind, _ = classifyVendorAnswer(VendorOpenAI, fromOpenAI.StatusCode, []byte(fromOpenAI.RawJSON()))
		return kind, kind != ""
	}
	return "", false
}

// vendorErrorBody is the part of either vendor's error body this file reads.
// Anthropic wraps it in {"type":"error","error":{...}}; OpenAI wraps it in
// {"error":{...}} on the wire, and its SDK hands back the inner object alone.
type vendorErrorBody struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Details struct {
		ErrorCode string `json:"error_code"`
	} `json:"details"`
}

func readVendorErrorBody(body []byte) vendorErrorBody {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	var out vendorErrorBody
	if json.Unmarshal(body, &envelope) == nil && len(envelope.Error) > 0 && envelope.Error[0] == '{' {
		_ = json.Unmarshal(envelope.Error, &out)
		return out
	}
	_ = json.Unmarshal(body, &out)
	return out
}

// Anthropic's spend limits an owner set answer 400 with a message that starts
// with one of these (its rate-limits page, "Setting your own spend limit").
var anthropicSpendLimitPrefixes = []string{
	"You have reached your specified API usage limits",
	"You have reached your specified workspace API usage limits",
}

// anthropicSpendCapCode marks the tier's monthly spend cap, which answers 429
// like a rate limit and is told apart by this code alone.
const anthropicSpendCapCode = "enforced_spend_limit_reached"

// OpenAI's 429 codes that mean the account is out of money rather than being
// asked to slow down (its error-codes page).
var openaiCreditCodes = map[string]bool{
	"credit_balance_exhausted":          true,
	"organization_spend_limit_exceeded": true,
	"project_spend_limit_exceeded":      true,
	"organization_usage_limit_exceeded": true,
	"insufficient_quota":                true,
}

// classifyVendorAnswer reads one non-2xx answer. kind is "" when the answer
// says nothing about the account. reason is the vendor's own message, scrubbed
// of anything shaped like a key and cut to maxAccountFailureReason.
func classifyVendorAnswer(vendor string, status int, body []byte) (kind, reason string) {
	if status >= 200 && status < 300 {
		return "", ""
	}
	parsed := readVendorErrorBody(body)
	reason = strings.TrimSpace(scrubKeyShapes(parsed.Message))
	if reason == "" {
		reason = fmt.Sprintf("%s answered %d", vendorDisplayName(vendor), status)
	}
	if len(reason) > maxAccountFailureReason {
		reason = reason[:maxAccountFailureReason-3] + "..."
	}

	switch vendor {
	case VendorAnthropic:
		switch status {
		case 401:
			kind = accountFailureRefused
		case 403:
			kind = accountFailurePermission
		case 402:
			kind = accountFailureCredit
		case 400:
			for _, prefix := range anthropicSpendLimitPrefixes {
				if strings.HasPrefix(parsed.Message, prefix) {
					kind = accountFailureCredit
				}
			}
		case 429:
			if parsed.Details.ErrorCode == anthropicSpendCapCode {
				kind = accountFailureCredit
			}
		}
	case VendorOpenAI:
		lower := strings.ToLower(parsed.Message)
		missingPermission := strings.Contains(lower, "insufficient permissions") || strings.Contains(lower, "missing scopes")
		switch status {
		case 401:
			kind = accountFailureRefused
			if missingPermission {
				kind = accountFailurePermission
			}
		case 403:
			kind = accountFailurePermission
		case 429:
			if openaiCreditCodes[parsed.Code] || parsed.Type == "insufficient_quota" {
				kind = accountFailureCredit
			}
		}
	}
	return kind, reason
}

// vendorDisplayName is how a vendor is named to a person.
func vendorDisplayName(vendor string) string {
	switch vendor {
	case VendorAnthropic:
		return "Anthropic"
	case VendorOpenAI:
		return "OpenAI"
	}
	return vendor
}

// keyShape matches anything that reads as a vendor key or a masked part of
// one: `sk-` followed by at least six characters a key or its mask is made of.
// OpenAI echoes part of a rejected key in its answer (safety rule S4).
var keyShape = regexp.MustCompile(`\bsk-[A-Za-z0-9_*.\-]{6,}`)

// scrubKeyShapes removes anything shaped like a key from text that is about to
// be logged, stored or shown.
func scrubKeyShapes(text string) string {
	return keyShape.ReplaceAllString(text, "sk-[removed]")
}
