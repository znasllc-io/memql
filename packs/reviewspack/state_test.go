package reviewspack

import (
	"strings"
	"testing"
	"time"
)

// state_test.go -- A REVIEW'S STATE IS DERIVED FROM ITS DECISIONS, NEVER STORED.
//
// The decision log is append-only, so the only honest answer to "is this
// review public" is a reading of that log against the text the review holds
// NOW. These tests pin the reading: a decision covers the revision it was
// made on and no other, a later decision supersedes an earlier one, and the
// two decisions that change nothing (askToFix, escalate) never move a review.

var t0 = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func dec(action string, revision int, minutes int) Decision {
	return Decision{
		ID: action + "-" + time.Duration(minutes).String(), Action: action, Revision: revision,
		At: t0.Add(time.Duration(minutes) * time.Minute),
	}
}

func TestANewReviewIsWaiting(t *testing.T) {
	if got := StateOf(rv(1), nil); got != StateWaiting {
		t.Fatalf("StateOf(no decisions) = %q, want waiting -- nothing is public until a person publishes it", got)
	}
}

func TestPublishOnTheCurrentRevisionPublishes(t *testing.T) {
	if got := StateOf(rv(1), []Decision{dec(ActionPublish, 1, 0)}); got != StatePublished {
		t.Fatalf("got %q, want published", got)
	}
}

// AN EDIT AFTER A DECISION RETURNS THE REVIEW TO THE QUEUE. A decision is
// about the words the moderator read; revision 2 is words nobody has read.
func TestADecisionDoesNotCoverALaterRevision(t *testing.T) {
	cases := map[string]string{ActionPublish: "a publish", ActionDecline: "a decline"}
	for action, what := range cases {
		if got := StateOf(rv(2), []Decision{dec(action, 1, 0)}); got != StateWaiting {
			t.Fatalf("%s on revision 1 with the review at revision 2: got %q, want waiting", what, got)
		}
	}
}

func TestTheLatestVisibilityDecisionWins(t *testing.T) {
	// Declined, then published after a second look: published.
	if got := StateOf(rv(1), []Decision{dec(ActionDecline, 1, 0), dec(ActionPublish, 1, 5)}); got != StatePublished {
		t.Fatalf("decline then publish: got %q, want published", got)
	}
	// Published, then removed: removed.
	if got := StateOf(rv(1), []Decision{dec(ActionPublish, 1, 0), dec(ActionRemove, 1, 5)}); got != StateRemoved {
		t.Fatalf("publish then remove: got %q, want removed", got)
	}
	// Order of the slice must not matter: the log is read by time.
	if got := StateOf(rv(1), []Decision{dec(ActionPublish, 1, 5), dec(ActionDecline, 1, 0)}); got != StatePublished {
		t.Fatalf("unsorted input: got %q, want published", got)
	}
}

func TestAskToFixAndEscalateChangeNothing(t *testing.T) {
	if got := StateOf(rv(1), []Decision{dec(ActionAskToFix, 1, 0), dec(ActionEscalate, 1, 1)}); got != StateWaiting {
		t.Fatalf("askToFix + escalate on a waiting review: got %q, want waiting", got)
	}
	if got := StateOf(rv(1), []Decision{dec(ActionPublish, 1, 0), dec(ActionEscalate, 1, 1)}); got != StatePublished {
		t.Fatalf("escalate on a published review: got %q, want published -- a safety concern is never a reason to withhold", got)
	}
}

func TestEraseIsTerminal(t *testing.T) {
	if got := StateOf(rv(1), []Decision{dec(ActionErase, 1, 0), dec(ActionPublish, 1, 5)}); got != StateErased {
		t.Fatalf("erase then publish: got %q, want erased -- an erased review never comes back", got)
	}
}

// A ROW WRITTEN BEFORE REVISIONS EXISTED CARRIES NONE. It is revision 1, and
// so is a decision that names none.
func TestAnAbsentRevisionIsRevisionOne(t *testing.T) {
	if got := StateOf(rv(0), []Decision{dec(ActionPublish, 0, 0)}); got != StatePublished {
		t.Fatalf("got %q, want published", got)
	}
	if got := StateOf(rv(0), []Decision{dec(ActionPublish, 1, 0)}); got != StatePublished {
		t.Fatalf("got %q, want published", got)
	}
}

func TestTheClosedReasonSetIsTheElevenAndOnlyRemoveTakesReviewerRequest(t *testing.T) {
	want := []string{
		"spam_or_advertising", "profanity", "harassing_or_obscene", "discriminatory",
		"personal_information", "defamatory", "false_or_dangerous", "believed_fake",
		"unrelated", "confidential", "undisclosed_connection",
	}
	if strings.Join(Reasons, ",") != strings.Join(want, ",") {
		t.Fatalf("Reasons = %v\nwant      %v", Reasons, want)
	}
	if err := ValidateDecision(ActionDecline, ReasonReviewerRequest, StateWaiting, false); err == nil {
		t.Fatal("decline accepted reviewer_request; only a removal is at the reviewer's request")
	}
	if err := ValidateDecision(ActionRemove, ReasonReviewerRequest, StatePublished, true); err != nil {
		t.Fatalf("remove at the reviewer's request refused: %v", err)
	}
}

func TestValidateDecision(t *testing.T) {
	cases := []struct {
		name     string
		action   string
		reason   string
		state    State
		approver bool
		ok       bool
	}{
		{"publish a waiting review", ActionPublish, "", StateWaiting, false, true},
		{"publish after a second look", ActionPublish, "", StateDeclined, false, true},
		{"publish what was removed", ActionPublish, "", StateRemoved, false, true},
		{"publish twice", ActionPublish, "", StatePublished, false, false},
		{"publish names no reason", ActionPublish, "spam_or_advertising", StateWaiting, false, false},
		{"decline needs a reason", ActionDecline, "", StateWaiting, false, false},
		{"decline with a listed reason", ActionDecline, "profanity", StateWaiting, false, true},
		{"decline with an unlisted reason", ActionDecline, "negative", StateWaiting, false, false},
		{"decline a published review is a removal", ActionDecline, "profanity", StatePublished, false, false},
		{"ask to fix needs a reason", ActionAskToFix, "", StateWaiting, false, false},
		{"ask to fix a waiting review", ActionAskToFix, "personal_information", StateWaiting, false, true},
		{"ask to fix a published review", ActionAskToFix, "personal_information", StatePublished, false, false},
		{"escalate anything but an erased review", ActionEscalate, "", StatePublished, false, true},
		{"escalate names no reason", ActionEscalate, "profanity", StateWaiting, false, false},
		{"escalate an erased review", ActionEscalate, "", StateErased, false, false},
		{"remove by the owner", ActionRemove, "defamatory", StatePublished, true, true},
		{"remove by anyone else", ActionRemove, "defamatory", StatePublished, false, false},
		{"remove needs a reason", ActionRemove, "", StatePublished, true, false},
		{"remove what is not published", ActionRemove, "defamatory", StateWaiting, true, false},
		{"erase by the owner", ActionErase, "", StatePublished, true, true},
		{"erase by anyone else", ActionErase, "", StateWaiting, false, false},
		{"erase twice", ActionErase, "", StateErased, true, false},
		{"an unknown action", "hide", "", StateWaiting, true, false},
	}
	for _, c := range cases {
		err := ValidateDecision(c.action, c.reason, c.state, c.approver)
		if c.ok && err != nil {
			t.Errorf("%s: refused: %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: accepted, want refused", c.name)
		}
	}
}

func rv(n int) ReviewVersion { return ReviewVersion{Revision: n} }
