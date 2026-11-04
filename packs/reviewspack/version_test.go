package reviewspack

import (
	"encoding/json"
	"testing"
	"time"
)

// version_test.go -- A DECISION COVERS THE WORDS IT WAS MADE ON.
//
// A revision number is not enough: two edits sent through one link at the
// same moment can both become revision 2 with different words, and a publish
// made on one of them must not cover the other. So a decision records a
// fingerprint of the words it covered, and only those words are published.
//
// And a row's createdAt comes back from the engine as a time value, not text:
// every reading of it must work either way, and the public read must send it
// as ISO text a browser can parse.

func TestTheFingerprintIsTheWordsAndNothingElse(t *testing.T) {
	a := map[string]any{"title": "Easy", "body": "Easy to use", "rating": 4, "authorName": "Dana", "createdAt": "x", "revision": 2}
	b := map[string]any{"title": "Easy", "body": "Easy to use", "rating": float64(4), "authorName": "Dana", "createdAt": "y", "revision": 9}
	if contentHashOf(a) != contentHashOf(b) {
		t.Fatal("the same words fingerprint differently: only title, body, rating and name may count")
	}
	b["body"] = "Easy to use!"
	if contentHashOf(a) == contentHashOf(b) {
		t.Fatal("different words fingerprint the same")
	}
}

func TestAPublishOnOtherWordsAtTheSameRevisionDoesNotCover(t *testing.T) {
	seen := map[string]any{"body": "benign", "revision": 2}
	now := map[string]any{"body": "something else", "revision": 2}
	d := dec(ActionPublish, 2, 0)
	d.ContentHash = contentHashOf(seen)
	if got := StateOf(VersionOf(now), []Decision{d}); got != StateWaiting {
		t.Fatalf("a publish made on other words covered these (state %q)", got)
	}
	if got := StateOf(VersionOf(seen), []Decision{d}); got != StatePublished {
		t.Fatalf("a publish on these very words did not cover them (state %q)", got)
	}
}

// A decision recorded before fingerprints existed carries none, and covers
// its revision as it always did.
func TestADecisionWithNoFingerprintCoversItsRevision(t *testing.T) {
	if got := StateOf(VersionOf(map[string]any{"body": "b", "revision": 1}), []Decision{dec(ActionPublish, 1, 0)}); got != StatePublished {
		t.Fatalf("got %q", got)
	}
}

func TestDecideRefusesWhenTheWordsTheModeratorSawChanged(t *testing.T) {
	r := aReview()
	seen := map[string]any{"body": "an older text"}
	_, err := decide(t, moderatorCtx("mod-1"), &decideReader{review: r},
		map[string]any{"reviewId": "r1", "action": "publish", "revision": 2, "contentHash": contentHashOf(seen)})
	if err == nil {
		t.Fatal("a publish named words the review no longer holds and was recorded")
	}
	row, err := decide(t, moderatorCtx("mod-1"), &decideReader{review: r},
		map[string]any{"reviewId": "r1", "action": "publish", "revision": 2, "contentHash": contentHashOf(r)})
	if err != nil {
		t.Fatal(err)
	}
	if row["contentHash"] != contentHashOf(r) {
		t.Fatalf("contentHash = %v, want the fingerprint of the words decided on", row["contentHash"])
	}
}

func TestATimeReadsWhetherTheEngineAnswersAValueOrText(t *testing.T) {
	want := time.Date(2026, 11, 4, 5, 23, 17, 74777000, time.UTC)
	for _, v := range []any{want, &want, want.Format(time.RFC3339Nano), want.String(), "2026-11-04T05:23:17.074777Z"} {
		if got := asTime(v); !got.Equal(want) {
			t.Errorf("asTime(%#v) = %v, want %v", v, got, want)
		}
	}
	if !asTime("not a time").IsZero() || !asTime(nil).IsZero() {
		t.Fatal("an unreadable time must be zero")
	}
}

func TestThePublicReadSendsISOTimesAndOrdersByThem(t *testing.T) {
	older := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	rows := reviewRows("old", "new")
	rows[0]["createdAt"] = older // as the engine answers it: a time value
	rows[1]["createdAt"] = newer
	out := liveRead(t, liveReader(rows, published("old", "new")), nil)
	list := out["reviews"].([]any)
	first := list[0].(map[string]any)
	if first["id"] != "new" {
		t.Fatalf("first = %v, want the newer review first", first["id"])
	}
	if first["createdAt"] != newer.Format(time.RFC3339Nano) {
		raw, _ := json.Marshal(first)
		t.Fatalf("createdAt = %v, want ISO text a browser parses (%s)", first["createdAt"], raw)
	}
}
