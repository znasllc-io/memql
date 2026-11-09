package emailstats

import (
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/campaigns"
	"github.com/znasllc-io/memql/component/memql"
)

// facts.go -- what one campaign row counts as (the 2026-11-08 statistics
// design, section 5 and D18). Pure: a row in, its facts out. The walk that
// reads the rows, the two lookups that attribute them and the table they are
// written to are rollup.go's.
//
// # One fact per source record and kind
//
// A row is read again and again: every new version of a delivery, every run
// inside the five-minute overlap, a second replica's run. So a fact is not "a
// row was read" but "this record is a send" or "this record is an open", keyed
// by the record and the kind (email_stat_fact's primary key), and the first
// reading of it is the one kept. A later version of a delivery adds only what
// it newly says -- a provider's verdict beside a send -- and never counts the
// send again.
//
// The cost is a delivery the worker recorded failed and a provider report
// later proved sent: if a run read it while it was failed, it keeps that
// failed fact beside its sent one. That is rare (a lost submission response),
// and a fact once written is never retracted, which is what lets every write
// be a plain ON CONFLICT DO NOTHING.
//
// # Ids and days
//
// The engine answers an in-process caller with canonical ids
// (`v1:campaigns:delivery:<id>`); every id here is bared, because the
// statistics read compares organizations with the bare ids a caller's grant
// resolves to. Every day is the UTC day of the instant the rule names (D5).

// The kinds a fact can be. The statistics read groups on them.
const (
	kindSent        = "sent"
	kindDelivered   = "delivered"
	kindUndelivered = "undelivered"
	kindFailed      = "failed"
	kindSkipped     = "skipped"
	kindSuppressed  = "suppressed"
	kindOpen        = "open"
	kindClick       = "click"
	kindOpenUnique  = "open_unique"
	kindClickUnique = "click_unique"
	kindUnsubscribe = "unsubscribe"
	kindBounce      = "bounce"
	kindComplaint   = "complaint"
	kindSubscribe   = "subscribe"
)

// Fact is one countable thing about one source record, as email_stat_fact
// stores it.
type Fact struct {
	Kind, SourceID, AccountID, CampaignID, RuleID, JourneyID, AudienceID string
	// Day is the fact's UTC day, at midnight.
	Day time.Time
}

// deliveryFacts is what one version of a delivery counts as: the send and how
// it ended, and the provider's verdict on it.
//
//   - status sent: a send, on the day of sentAt. A delivery reaches sent with
//     no sentAt when a provider report proves sent a row the worker recorded
//     failed; it counts, as campaignStats counts it, on the day of the version
//     that says so.
//   - status failed or skipped: that kind, on the day of the version, which is
//     when the row reached it. A skip whose reason is one campaignStats counts
//     as suppressed (campaigns.IsSuppressedSkipReason, the one definition of
//     that set) is a suppression too.
//   - providerStatus: delivered and expanded reached a mailbox, and are a
//     delivery; bounced, suppressed, quarantined, filtered-spam and failed did
//     not. Dated by providerAt, or by the version when the report carried no
//     time.
//
// Every one carries the delivery's organization, campaign and rule. A pending
// delivery is nothing yet: its next version says what became of it.
func deliveryFacts(row map[string]any) []Fact {
	id := bareText(row, "id")
	if id == "" {
		return nil
	}
	version, _ := instantOf(row["createdAt"])
	base := Fact{SourceID: id, AccountID: bareText(row, "accountId"), CampaignID: bareText(row, "campaignId"), RuleID: bareText(row, "emailRuleId")}
	var out []Fact
	add := func(kind string, at time.Time) {
		if at.IsZero() {
			return
		}
		f := base
		f.Kind, f.Day = kind, dayOf(at)
		out = append(out, f)
	}

	switch text(row, "status") {
	case "sent":
		sentAt, ok := instantOf(row["sentAt"])
		if !ok {
			sentAt = version
		}
		add(kindSent, sentAt)
	case "failed":
		add(kindFailed, version)
	case "skipped":
		add(kindSkipped, version)
		if campaigns.IsSuppressedSkipReason(text(row, "skipReason")) {
			add(kindSuppressed, version)
		}
	}

	providerAt, ok := instantOf(row["providerAt"])
	if !ok {
		providerAt = version
	}
	switch text(row, "providerStatus") {
	case "delivered", "expanded":
		add(kindDelivered, providerAt)
	case "bounced", "suppressed", "quarantined", "filtered-spam", "failed":
		add(kindUndelivered, providerAt)
	}
	return out
}

// engagementFacts is what one open or click counts as: a total, keyed by the
// event, and a unique, keyed by the delivery it is about -- so a person who
// opens one message four times is four opens and one unique open, by the
// definition campaignStats uses (distinct deliveryIds, not addresses). Both
// are dated by occurredAt, the engine's clock at the hit; the version's time
// stands in only for a row without one, which the concept does not allow.
//
// Only campaign mail has engagement rows (engagementEvent.campaignId is
// required), so opens and clicks are counted for campaigns alone (D18).
func engagementFacts(row map[string]any) []Fact {
	var total, unique string
	switch text(row, "kind") {
	case "open":
		total, unique = kindOpen, kindOpenUnique
	case "click":
		total, unique = kindClick, kindClickUnique
	default:
		return nil
	}
	at, ok := instantOf(row["occurredAt"])
	if !ok {
		if at, ok = instantOf(row["createdAt"]); !ok {
			return nil
		}
	}
	base := Fact{AccountID: bareText(row, "accountId"), CampaignID: bareText(row, "campaignId"), Day: dayOf(at)}
	var out []Fact
	if id := bareText(row, "id"); id != "" {
		f := base
		f.Kind, f.SourceID = total, id
		out = append(out, f)
	}
	if delivery := bareText(row, "deliveryId"); delivery != "" {
		f := base
		f.Kind, f.SourceID = unique, delivery
		out = append(out, f)
	}
	return out
}

// consentFacts is what one consent event counts as, by the statistics' own
// words: a withdraw is an unsubscribe and a grant a subscribe, a bounce and a
// complaint keep their names, and a suppression -- an operator's or a
// policy's act, not the person's -- is nothing. Dated by occurredAt, which
// a writer may set to an earlier verified time (a provider's report).
//
// A subscribe and an unsubscribe are attributed to the audience of the
// recipient they name, by attribute; one that names no recipient counts in its
// organization's totals only.
func consentFacts(row map[string]any) []Fact {
	id := bareText(row, "id")
	if id == "" {
		return nil
	}
	var kind string
	switch text(row, "kind") {
	case "withdraw":
		kind = kindUnsubscribe
	case "bounce":
		kind = kindBounce
	case "complaint":
		kind = kindComplaint
	case "grant":
		kind = kindSubscribe
	default:
		return nil
	}
	at, ok := instantOf(row["occurredAt"])
	if !ok {
		if at, ok = instantOf(row["createdAt"]); !ok {
			return nil
		}
	}
	return []Fact{{Kind: kind, SourceID: id, AccountID: bareText(row, "accountId"), CampaignID: bareText(row, "campaignId"), Day: dayOf(at)}}
}

// journeyEnrollmentOf is the journey enrollment a delivery's request id names,
// when a journey's step sent it (D18). The form is the one campaigns writes,
// `journey-<enrollment>-<step>`, and it is read back by campaigns' own reader
// of it, beside the writer, so the two cannot drift.
func journeyEnrollmentOf(requestID string) (enrollmentID string, ok bool) {
	return campaigns.JourneyEnrollmentOfRequest(requestID)
}

// pageRefs are the records a page's facts are attributed through, each keyed
// by the SourceID of the facts it attributes: the journey enrollment a
// delivery's request names, and the recipient a subscription change is about.
// attribute reads both in batches, once per page.
type pageRefs struct {
	enrollmentOf map[string]string
	audienceOf   map[string]string
}

func newPageRefs() pageRefs {
	return pageRefs{enrollmentOf: map[string]string{}, audienceOf: map[string]string{}}
}

// deliveryRefs notes the enrollment a journey send names.
func deliveryRefs(row map[string]any, refs pageRefs) {
	id := bareText(row, "id")
	if id == "" {
		return
	}
	if enrollment, ok := journeyEnrollmentOf(text(row, "requestId")); ok {
		refs.enrollmentOf[id] = enrollment
	}
}

// consentRefs notes the recipient a subscribe or an unsubscribe is about. The
// other kinds carry no audience, so they ask for none.
func consentRefs(row map[string]any, refs pageRefs) {
	id := bareText(row, "id")
	if id == "" {
		return
	}
	switch text(row, "kind") {
	case "grant", "withdraw":
		if recipient := bareText(row, "recipientId"); recipient != "" {
			refs.audienceOf[id] = recipient
		}
	}
}

// uniqueFacts keeps one fact per kind and source, the earliest-dated, in the
// order they came. A page can hold two opens of one message, which are two
// candidates for its one unique open; the earlier is the first open.
func uniqueFacts(facts []Fact) []Fact {
	at := make(map[string]int, len(facts))
	out := make([]Fact, 0, len(facts))
	for _, f := range facts {
		key := f.Kind + "\x00" + f.SourceID
		if i, seen := at[key]; seen {
			if f.Day.Before(out[i].Day) {
				out[i] = f
			}
			continue
		}
		at[key] = len(out)
		out = append(out, f)
	}
	return out
}

// text is a row's string field, trimmed; anything else is empty.
func text(row map[string]any, key string) string {
	s, _ := row[key].(string)
	return strings.TrimSpace(s)
}

// bareText is a row's id field, bared.
func bareText(row map[string]any, key string) string {
	return memql.BareShortId(text(row, key))
}

// instantOf reads an instant the way the engine hands one over: row.createdAt
// as a time.Time, payload datetimes as RFC3339 text.
func instantOf(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, !t.IsZero()
	case *time.Time:
		if t != nil && !t.IsZero() {
			return *t, true
		}
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return time.Time{}, false
		}
		if parsed, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// dayOf is the UTC day an instant falls on, at midnight.
func dayOf(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
