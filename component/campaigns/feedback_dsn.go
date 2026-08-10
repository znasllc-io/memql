package campaigns

import (
	"fmt"
	"strings"
)

// feedback_dsn.go -- RFC 3464 delivery status notifications (memql#3461).
//
// A DSN is what an MTA sends back when it cannot deliver: a
// `multipart/report; report-type=delivery-status` message whose middle part
// is `message/delivery-status`, a sequence of RFC 822-style field groups --
// one per-message group, then one per recipient:
//
//	Reporting-MTA: dns; mx.example.com
//
//	Final-Recipient: rfc822; user@example.com
//	Action: failed
//	Status: 5.1.1
//	Diagnostic-Code: smtp; 550 5.1.1 <user@example.com> User unknown
//
// # Scanned rather than MIME-parsed, deliberately
//
// This walks the whole body for groups containing a Final-Recipient rather
// than decoding the MIME tree to find the delivery-status part. Two reasons,
// and the second is the real one:
//
//  1. The per-recipient groups are unambiguous on their own. A group with a
//     Final-Recipient IS a delivery-status group; nothing else in a DSN has
//     that field.
//  2. Real DSNs in the wild vary in ways a strict MIME walk trips over --
//     boundary quoting, a nested message/rfc822 part, transfer encodings a
//     relay applied on the way. Being strict about the envelope buys nothing
//     here and loses reports, and a lost bounce is an address we keep
//     mailing.
//
// The cost is that a per-recipient group appearing inside a quoted original
// message would be read as a report. That is a DSN carrying a DSN, which is
// not a shape a relay produces for a campaign send, and the failure mode if
// it happened is suppressing an address that had genuinely bounced once.
//
// # The classification is theirs
//
// `Status:` first -- its class digit is the provider's permanent/transient
// verdict. `Action:` second, because RFC 3464 defines `failed` as permanent
// and `delayed` as transient, so it is also a classification rather than an
// inference. The diagnostic text is recorded as provenance and never read
// for meaning.

// parseDSN reads every per-recipient group in a delivery status
// notification.
func parseDSN(body string) (FeedbackParse, error) {
	groups := dsnFieldGroups(body)
	if len(groups) == 0 {
		return FeedbackParse{}, fmt.Errorf(
			"no RFC 3464 per-recipient group found (expected a `Final-Recipient:` field); this does not look like a delivery status notification")
	}

	campaignID := dsnCampaignID(body)
	var out FeedbackParse
	var undeliverable []string

	for _, g := range groups {
		addr := normalizeReportedAddress(firstOf(g, "original-recipient", "final-recipient"))
		if addr == "" {
			undeliverable = append(undeliverable, "a per-recipient group named no address this can read")
			continue
		}
		kind, why := dsnKind(g)
		if kind == "" {
			if why == "" {
				// A delivered / relayed / expanded group. A DSN reports
				// successes too, and one is not a fault.
				continue
			}
			undeliverable = append(undeliverable, why)
			continue
		}
		out.Reports = append(out.Reports, FeedbackReport{
			Email:      addr,
			Kind:       kind,
			CampaignID: campaignID,
			Note:       dsnNote(g),
		})
	}

	if len(out.Reports) == 0 {
		if len(undeliverable) > 0 {
			// Every group we found was unreadable. Refusing is the point:
			// a payload that parses to nothing looks identical to a healthy
			// deployment with no bounces.
			return FeedbackParse{}, fmt.Errorf(
				"the delivery status notification carried %d recipient group(s) and none could be classified: %s",
				len(undeliverable), strings.Join(undeliverable, "; "))
		}
		out.Ignored = "the delivery status notification reports only successful delivery"
	}
	return out, nil
}

// dsnKind reads the provider's own verdict off one group.
//
// Returns ("", "") for a group that reports success, and ("", reason) for one
// that reports a failure it does not classify -- which is a payload the
// operator has to see rather than a bounce we invent a severity for.
func dsnKind(g map[string]string) (string, string) {
	action := strings.ToLower(strings.TrimSpace(g["action"]))
	switch action {
	case "delivered", "relayed", "expanded":
		return "", ""
	}

	if status := strings.TrimSpace(g["status"]); status != "" {
		if kind, ok := bounceKindForStatusClass(status[0]); ok {
			return kind, ""
		}
		return "", fmt.Sprintf("Status %q is not a 4.x.x or 5.x.x class", status)
	}

	// RFC 3464 defines these two as the permanent / transient split, so
	// falling back to them is still reading the provider's classification.
	switch action {
	case "failed":
		return "hard_bounce", ""
	case "delayed":
		return "soft_bounce", ""
	case "":
		return "", "a per-recipient group carried neither Status nor Action, so the provider stated no verdict"
	default:
		return "", fmt.Sprintf("Action %q is not an RFC 3464 action value", action)
	}
}

// dsnNote is the provenance recorded on the suppression row: the provider's
// own diagnostic, never the address.
func dsnNote(g map[string]string) string {
	parts := make([]string, 0, 3)
	if v := strings.TrimSpace(g["status"]); v != "" {
		parts = append(parts, "status="+v)
	}
	if v := strings.TrimSpace(g["action"]); v != "" {
		parts = append(parts, "action="+v)
	}
	if v := strings.TrimSpace(g["diagnostic-code"]); v != "" {
		// REDACTED, because a provider's diagnostic routinely quotes the
		// address back and this string lands on the cluster-wide suppression
		// row, whose whole privacy argument is that it holds a digest and a
		// domain and never a mailbox.
		parts = append(parts, "diagnostic="+redactAddressesIn(collapseWhitespace(v)))
	}
	return strings.Join(parts, " ")
}

// dsnCampaignID recovers the send a bounce belongs to.
//
// A DSN usually quotes the original message's headers back (the third part
// of the report), so the X-Campaign-Id the campaign sender stamps comes home
// with it. Best-effort by design: attribution improves a deliverability
// review and is never a precondition for suppressing an address.
func dsnCampaignID(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < len(headerCampaignID)+1 {
			continue
		}
		if !strings.EqualFold(line[:len(headerCampaignID)], headerCampaignID) || line[len(headerCampaignID)] != ':' {
			continue
		}
		return strings.TrimSpace(line[len(headerCampaignID)+1:])
	}
	return ""
}

// dsnFieldGroups splits the body into blank-line-separated groups and keeps
// the ones carrying a recipient. Field names are lowercased; a continuation
// line (leading whitespace, RFC 5322 folding) is appended to the field
// before it, which is how a long Diagnostic-Code arrives.
func dsnFieldGroups(body string) []map[string]string {
	var groups []map[string]string
	current := map[string]string{}
	lastKey := ""

	flush := func() {
		if _, hasRecipient := current["final-recipient"]; hasRecipient {
			groups = append(groups, current)
		} else if _, hasOriginal := current["original-recipient"]; hasOriginal {
			groups = append(groups, current)
		}
		current = map[string]string{}
		lastKey = ""
	}

	for _, raw := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(raw) == "" {
			flush()
			continue
		}
		if (raw[0] == ' ' || raw[0] == '\t') && lastKey != "" {
			current[lastKey] += " " + strings.TrimSpace(raw)
			continue
		}
		colon := strings.Index(raw, ":")
		if colon <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(raw[:colon]))
		if key == "" {
			continue
		}
		// FIRST value wins within a group. A DSN that repeats a field is
		// malformed, and taking the first keeps the read deterministic.
		if _, seen := current[key]; !seen {
			current[key] = strings.TrimSpace(raw[colon+1:])
		}
		lastKey = key
	}
	flush()
	return groups
}

func firstOf(g map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(g[k]); v != "" {
			return v
		}
	}
	return ""
}

func collapseWhitespace(v string) string { return strings.Join(strings.Fields(v), " ") }
