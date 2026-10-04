package pipelines

import (
	"reflect"
	"strings"
	"testing"
)

// The event-to-mode table (decision 3 of the pipelines-seam plan; design
// record D5, plus `release` from the documentation program's D15). A pull
// request runs what it affects; everything that lands, or ships, runs the
// whole suite once.
func TestModeForIsTheEventTable(t *testing.T) {
	cases := []struct {
		event Event
		mode  Mode
		ok    bool
	}{
		{EventPullRequest, ModeAffected, true},
		{EventMergeGroup, ModeFull, true},
		{EventPush, ModeFull, true},
		{EventRelease, ModeFull, true},
		// A re-requested check run is not an event of its own: it re-runs the
		// original run's event, so the table has no row for it.
		{"check_run", "", false},
		{"check_suite", "", false},
		{"pull_request_review", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		mode, ok := ModeFor(c.event)
		if mode != c.mode || ok != c.ok {
			t.Errorf("ModeFor(%q) = (%q, %v), want (%q, %v)", c.event, mode, ok, c.mode, c.ok)
		}
	}
}

func TestEventsIsTheTableSorted(t *testing.T) {
	want := []Event{EventMergeGroup, EventPullRequest, EventPush, EventRelease}
	got := Events()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Events() = %v, want %v", got, want)
	}
	for _, e := range got {
		if _, ok := ModeFor(e); !ok {
			t.Errorf("Events() lists %q, which has no mode", e)
		}
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

func TestVersionIsTheTagForAReleaseAndTheSHAOtherwise(t *testing.T) {
	const sha = "a944ae33e9bcd7b2f5d1e5d74dcd8acc6a0ae9df"
	cases := []struct {
		event Event
		tag   string
		want  string
	}{
		{EventRelease, "v1.4.0", "v1.4.0"},
		{EventPush, "", sha},
		{EventMergeGroup, "", sha},
		{EventPullRequest, "", sha},
		// A tag on a non-release event is not a version: the event decides.
		{EventPush, "v1.4.0", sha},
		// A release whose tag has not resolved still names the commit.
		{EventRelease, "", sha},
	}
	for _, c := range cases {
		if got := Version(c.event, sha, c.tag); got != c.want {
			t.Errorf("Version(%q, sha, %q) = %q, want %q", c.event, c.tag, got, c.want)
		}
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
