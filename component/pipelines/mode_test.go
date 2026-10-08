package pipelines

import (
	"reflect"
	"strings"
	"testing"
)

func TestEventsIsTheTableSorted(t *testing.T) {
	want := []Event{EventMergeGroup, EventPullRequest, EventPush, EventRelease}
	got := Events()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Events() = %v, want %v", got, want)
	}

	// The caller owns what it gets back.
	got[0] = "mutated"
	if Events()[0] != EventMergeGroup {
		t.Error("Events() handed out its own backing array")
	}
}

// Decision 1: the merge queue's full run and the default branch's push run
// carry the same SHA once merged and the same mode, so the event is what keeps
// the push's deploy and notify stages from collapsing into the queue's run.
func TestRunKeyKeepsTheQueueAndThePushApart(t *testing.T) {
	const sha = "a944ae33e9bcd7b2f5d1e5d74dcd8acc6a0ae9df"
	queue := RunKey("acme-corp/storefront", sha, ModeFull, EventMergeGroup)
	push := RunKey("acme-corp/storefront", sha, ModeFull, EventPush)
	if queue == push {
		t.Fatalf("RunKey is %q for both the merge group and the push of one SHA", queue)
	}
	if want := "acme-corp/storefront@" + sha + ":full:push"; push != want {
		t.Errorf("RunKey = %q, want %q", push, want)
	}
}

// A webhook and a poll for one head must be ONE run (Review Focus 1), so the
// key is a pure function of its inputs, and two spellings of one repository
// or one SHA are one key.
func TestRunKeyIsOneKeyForOneHead(t *testing.T) {
	const sha = "c5fa05f142af4f7c6bd9b18c24924ebed165a4ab"
	a := RunKey("acme-corp/storefront", sha, ModeAffected, EventPullRequest)
	b := RunKey("acme-corp/storefront", sha, ModeAffected, EventPullRequest)
	if a != b {
		t.Fatalf("RunKey is not deterministic: %q then %q", a, b)
	}
	if c := RunKey("Acme-Corp/Storefront", strings.ToUpper(sha), ModeAffected, EventPullRequest); c != a {
		t.Errorf("RunKey(%q, upper-case SHA) = %q, want %q: a webhook and a poll that spell the head differently would open two runs",
			"Acme-Corp/Storefront", c, a)
	}
	if d := RunKey("acme-corp/storefront", sha, ModeFull, EventPullRequest); d == a {
		t.Errorf("RunKey ignores the mode: %q", d)
	}
}

func TestScheduledRunKeyDeduplicatesOneUTCDateAndRescansUnchangedHeadsNextDate(t *testing.T) {
	const sha = "c5fa05f142af4f7c6bd9b18c24924ebed165a4ab"
	one, err := ScheduledRunKey("Acme-Corp/Storefront", strings.ToUpper(sha), ModeFull, "2026-10-07")
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := ScheduledRunKey("acme-corp/storefront", sha, ModeFull, "2026-10-07")
	if err != nil || duplicate != one {
		t.Fatalf("same day scheduled keys = %q and %q, err %v", one, duplicate, err)
	}
	nextDay, err := ScheduledRunKey("acme-corp/storefront", sha, ModeFull, "2026-10-08")
	if err != nil || nextDay == one {
		t.Fatalf("next day key = %q, err %v; same-head daily scans must be distinct", nextDay, err)
	}
	if day, ok := ScheduledDayFromRunKey(one); !ok || day != "2026-10-07" {
		t.Fatalf("ScheduledDayFromRunKey(%q) = %q, %v", one, day, ok)
	}
	if _, err := ScheduledRunKey("acme-corp/storefront", sha, ModeFull, "2026-10-7"); err == nil {
		t.Fatal("accepted a non-canonical scheduled date")
	}
	if _, err := ScheduledRunKey("acme-corp/storefront", sha, ModeAffected, "2026-10-07"); err == nil {
		t.Fatal("accepted an affected-mode scheduled run")
	}
	if _, ok := ScheduledDayFromRunKey("acme-corp/storefront@" + sha + ":affected:schedule:2026-10-07"); ok {
		t.Fatal("accepted a malformed scheduled run key")
	}
}

// Decision 14: the check run's details link is the OS run page, read at boot
// from one query parameter.
func TestRunPageURLEscapesTheRunID(t *testing.T) {
	cases := []struct{ origin, id, want string }{
		{"https://os.lab.example.test", "k3x9q2", "https://os.lab.example.test/?pipelineRun=k3x9q2"},
		// A trailing slash on the origin is not a second path segment.
		{"https://os.lab.example.test/", "k3x9q2", "https://os.lab.example.test/?pipelineRun=k3x9q2"},
		// An id is a value, never URL syntax.
		{"https://os.lab.example.test", "a&b=c d", "https://os.lab.example.test/?pipelineRun=a%26b%3Dc+d"},
	}
	for _, c := range cases {
		if got := RunPageURL(c.origin, c.id); got != c.want {
			t.Errorf("RunPageURL(%q, %q) = %q, want %q", c.origin, c.id, got, c.want)
		}
	}
}

func TestCheckRunNameNamesThePipeline(t *testing.T) {
	if got, want := CheckRunName("storefront"), "MemQL / storefront"; got != want {
		t.Errorf("CheckRunName = %q, want %q", got, want)
	}
}
