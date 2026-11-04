package reviewspack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// state.go -- THE MODERATION LIFECYCLE, AS A READING OF THE DECISION LOG.
//
// A review's state is never stored. It is derived, every time, from the
// append-only v1:reviews:reviewDecision rows recorded against it and from the
// revision the review holds NOW. Storing it would make a second source of
// truth that could disagree with the log; deriving it makes "why is this
// review public" answerable by reading the rows that made it so.
//
// # A decision covers the revision it was made on, and no other
//
// A shopper may edit their review through their link while it is open, and
// each edit is a new revision of the same row. A publish on revision 1 says
// "a person read THESE words and they may go up"; it says nothing about the
// words of revision 2. So a newer revision is waiting again, whatever was
// decided before -- which is also what makes a race between an edit and a
// publish fail closed: nothing is shown until a decision covers the text the
// row holds.
//
// # Two decisions change nothing
//
// askToFix records that the moderator asked the shopper to change something
// (the shopper is contacted outside the system) and escalate records that a
// possible safety concern went to a person. Neither publishes, withholds or
// hides anything: a safety concern is never by itself a reason to withhold a
// review.

// The decision actions. A closed set: there is no "other".
const (
	ActionPublish  = "publish"
	ActionDecline  = "decline"
	ActionAskToFix = "askToFix"
	ActionEscalate = "escalate"
	ActionRemove   = "remove"
	ActionErase    = "erase"
)

// Actions lists every decision action, in the order a screen offers them.
var Actions = []string{ActionPublish, ActionDecline, ActionAskToFix, ActionEscalate, ActionRemove, ActionErase}

// Reasons is the closed set a review may be withheld for. It tracks the
// reasons the FTC's rule on consumer reviews (16 CFR 465.7) allows a business
// to suppress a review for, plus spam, profanity and an undisclosed
// connection to the business. A review's rating or its sentiment is never a
// reason, and the set has no "other" for one to hide behind.
var Reasons = []string{
	"spam_or_advertising",
	"profanity",
	"harassing_or_obscene",
	"discriminatory",
	"personal_information",
	"defamatory",
	"false_or_dangerous",
	"believed_fake",
	"unrelated",
	"confidential",
	"undisclosed_connection",
}

// ReasonReviewerRequest is the one reason only a removal takes: the person who
// wrote the review asked for it to come down.
const ReasonReviewerRequest = "reviewer_request"

// State is a review's place in the lifecycle.
type State string

// The states a review can be in.
const (
	StateWaiting   State = "waiting"
	StatePublished State = "published"
	StateDeclined  State = "declined"
	StateRemoved   State = "removed"
	StateErased    State = "erased"
)

// States lists every state, in the order a screen filters by them.
var States = []State{StateWaiting, StatePublished, StateDeclined, StateRemoved, StateErased}

// Decision is one row of the log, as the lifecycle reads it.
type Decision struct {
	ID       string
	ReviewID string
	Action   string
	Reason   string
	Note     string
	Revision int
	// ContentHash is the fingerprint of the words the decision was made on
	// (contentHashOf). Empty on a decision recorded before fingerprints, which
	// covers its revision as it always did.
	ContentHash string
	DecidedBy   string
	At          time.Time
}

// ReviewVersion is what a decision must match to cover a review: the
// revision it holds and the fingerprint of its words.
type ReviewVersion struct {
	Revision    int
	ContentHash string
}

// VersionOf reads a review row's version.
func VersionOf(row map[string]any) ReviewVersion {
	return ReviewVersion{Revision: asInt(row["revision"]), ContentHash: contentHashOf(row)}
}

// contentHashOf fingerprints the WORDS of a review -- headline, text, rating
// and name -- and nothing else, so a rewrite that changes no word (a field
// added, a version re-stamped) keeps it, and two different texts that both
// arrived as one revision number never share it.
func contentHashOf(row map[string]any) string {
	words := struct {
		Title  string `json:"t"`
		Body   string `json:"b"`
		Rating int    `json:"r"`
		Name   string `json:"a"`
	}{
		Title:  strings.TrimSpace(asString(row["title"])),
		Body:   strings.TrimSpace(asString(row["body"])),
		Rating: asRating(row["rating"]),
		Name:   strings.TrimSpace(asString(row["authorName"])),
	}
	raw, _ := json.Marshal(words)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:32]
}

// covers reports whether a publish or decline covers a review's current
// version: the same revision, and the same words when the decision recorded
// them.
func covers(d Decision, current ReviewVersion) bool {
	if revisionOf(d.Revision) != revisionOf(current.Revision) {
		return false
	}
	return d.ContentHash == "" || current.ContentHash == "" || d.ContentHash == current.ContentHash
}

// asTime reads a time off a row whether the engine answers a time value or
// text: a row's createdAt comes back as a time.Time, a payload datetime as
// RFC 3339 text. Unreadable is zero.
func asTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t.UTC()
	case *time.Time:
		if t == nil {
			return time.Time{}
		}
		return t.UTC()
	case string:
		s := strings.TrimSpace(t)
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.999999999 -0700 MST", "2006-01-02 15:04:05.999999999-07:00", "2006-01-02T15:04:05.999999999"} {
			if parsed, err := time.Parse(layout, s); err == nil {
				return parsed.UTC()
			}
		}
	}
	return time.Time{}
}

// timeText is a time as ISO text a browser parses, or "" when unreadable.
func timeText(v any) string {
	t := asTime(v)
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

// revisionOf normalizes a revision: a row written before revisions existed
// carries none, and it is revision 1.
func revisionOf(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// changesState reports whether an action moves a review between states.
func changesState(action string) bool {
	switch action {
	case ActionPublish, ActionDecline, ActionRemove:
		return true
	}
	return false
}

// StateOf derives a review's state from its decisions and its current version.
func StateOf(current ReviewVersion, decisions []Decision) State {
	var latest *Decision
	ordered := append([]Decision(nil), decisions...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].At.Equal(ordered[j].At) {
			return ordered[i].At.Before(ordered[j].At)
		}
		return ordered[i].ID < ordered[j].ID
	})
	for i := range ordered {
		d := ordered[i]
		if d.Action == ActionErase {
			return StateErased
		}
		if changesState(d.Action) {
			latest = &ordered[i]
		}
	}
	if latest == nil {
		return StateWaiting
	}
	switch latest.Action {
	case ActionRemove:
		return StateRemoved
	case ActionPublish:
		if covers(*latest, current) {
			return StatePublished
		}
	case ActionDecline:
		if covers(*latest, current) {
			return StateDeclined
		}
	}
	return StateWaiting
}

func isListedReason(reason string) bool {
	for _, r := range Reasons {
		if r == reason {
			return true
		}
	}
	return false
}

// ValidateDecision reports whether a decision may be recorded on a review in
// state, by a caller who is (or is not) the store's named removal approver.
// The caller's right to moderate at all is decided before this.
func ValidateDecision(action, reason string, state State, approver bool) error {
	reason = strings.TrimSpace(reason)
	needsListedReason := func() error {
		if reason == "" {
			return fmt.Errorf("reviews: %s needs one of the listed reasons", action)
		}
		if !isListedReason(reason) {
			return fmt.Errorf("reviews: %q is not one of the listed reasons", reason)
		}
		return nil
	}
	noReason := func() error {
		if reason != "" {
			return fmt.Errorf("reviews: %s takes no reason", action)
		}
		return nil
	}
	if state == StateErased {
		return fmt.Errorf("reviews: an erased review takes no further decisions")
	}
	switch action {
	case ActionPublish:
		if state == StatePublished {
			return fmt.Errorf("reviews: the review is already published")
		}
		return noReason()
	case ActionDecline:
		if state != StateWaiting {
			return fmt.Errorf("reviews: only a waiting review can be declined (this one is %s)", state)
		}
		return needsListedReason()
	case ActionAskToFix:
		if state != StateWaiting {
			return fmt.Errorf("reviews: only a waiting review can be sent back to its writer (this one is %s)", state)
		}
		return needsListedReason()
	case ActionEscalate:
		return noReason()
	case ActionRemove:
		if !approver {
			return fmt.Errorf("reviews: only the store's owner can remove a published review")
		}
		if state != StatePublished {
			return fmt.Errorf("reviews: only a published review can be removed (this one is %s)", state)
		}
		if reason == ReasonReviewerRequest {
			return nil
		}
		return needsListedReason()
	case ActionErase:
		if !approver {
			return fmt.Errorf("reviews: only the store's owner can erase a reviewer's data")
		}
		return noReason()
	}
	return fmt.Errorf("reviews: %q is not a decision", action)
}

// reviewIDPrefix is a review row's canonical id prefix.
const reviewIDPrefix = "v1:reviews:review:"

// canonicalReviewID is the ONE spelling a review id is stored and compared
// in: the full engine id, which is what every query answers a row at. A link
// names the bare id, and a decision's reviewId is a plain field compared as
// text (an id filter canonicalizes; a plain field does not), so every writer
// stores, and every reader compares, the full form.
func canonicalReviewID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	return reviewIDPrefix + strings.TrimPrefix(id, reviewIDPrefix)
}
