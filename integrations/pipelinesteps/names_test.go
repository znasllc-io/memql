package pipelinesteps

import (
	"regexp"
	"strings"
	"testing"
)

// dnsLabel is an RFC 1123 label: what a Job name has to be, because the Job
// controller copies it into the job-name label on every pod and pod hostnames
// are derived from it.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// labelValue is a Kubernetes label value: empty, or alphanumeric at both ends
// with dashes, underscores and dots between, at most 63 characters.
var labelValue = regexp.MustCompile(`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`)

// TestJobNameIsDeterministicAndDNSSafe pins the name every party has to arrive
// at independently: the replica that creates the Job, a replica adopting it
// after its runner went stale, and the agent asking for its status. The value
// is a hand-derived literal rather than a recomputation because the name is
// also a contract across VERSIONS -- during a rolling deploy an old replica
// creates the Job and a new one must find it under the same name, or the step
// runs twice.
func TestJobNameIsDeterministicAndDNSSafe(t *testing.T) {
	const run, step = "run-7f3a", "tests/go-tests#2"

	// "mp-" + the first 24 hex of core/id's content address of
	// {"attempt":2,"runId":"run-7f3a","stepKey":"tests/go-tests#2"}, derived by
	// an independent reimplementation of core/id checked against its own
	// documented vectors.
	name := JobName(run, step, 2)
	if name != "mp-a7a72726d5075767e0b6d115" {
		t.Fatalf("JobName = %q, want mp-a7a72726d5075767e0b6d115 (the content address of run, step and attempt)", name)
	}
	if again := JobName(run, step, 2); again != name {
		t.Errorf("JobName is not deterministic: %q then %q -- a resumed driver would start a second Job", name, again)
	}

	// A retried step gets a FRESH Job; the failed attempt's Job is evidence.
	if retried := JobName(run, step, 3); retried != "mp-428198f3726c24d76a94d90f" {
		t.Errorf("JobName(attempt 3) = %q, want mp-428198f3726c24d76a94d90f", retried)
	}
	if other := JobName(run, "tests/go-tests#3", 2); other == name {
		t.Errorf("two shards of one step share the Job name %q", other)
	}
	if other := JobName("run-7f3b", step, 2); other == name {
		t.Errorf("two runs share the Job name %q", other)
	}

	huge := JobName(strings.Repeat("r", 500), strings.Repeat("stage/step#", 100), 1<<30)
	for _, n := range []string{name, huge} {
		if len(n) > 63 || !dnsLabel.MatchString(n) {
			t.Errorf("JobName %q is not a DNS label of at most 63 characters", n)
		}
	}
}

// TestArtifactMarkerIsStablePerJob: the marker is written into the Job by the
// replica that builds it and read back by whichever replica captures the
// output -- possibly a different one, running a different version. Both must
// derive the same line from the Job's name alone.
func TestArtifactMarkerIsStablePerJob(t *testing.T) {
	// "::memql-artifacts::" + the first 16 hex of core/id's content address of
	// {"artifactsOf":"mp-a7a72726d5075767e0b6d115"}.
	got := ArtifactMarker("mp-a7a72726d5075767e0b6d115")
	if got != "::memql-artifacts::4887b49fa27936d6" {
		t.Fatalf("ArtifactMarker = %q, want ::memql-artifacts::4887b49fa27936d6", got)
	}
	if other := ArtifactMarker("mp-428198f3726c24d76a94d90f"); other == got {
		t.Errorf("two Jobs share the artifact marker %q", other)
	}
}

// TestRunLabelValueIsAlwaysALabelValue: the run label is how a cancel finds
// every Job and Secret of a run, so its value has to be legal on every object
// the runner creates. A bare id already is one and stays readable; anything
// else is hashed deterministically, so the selector a cancel builds matches
// the label the Job was given.
func TestRunLabelValueIsAlwaysALabelValue(t *testing.T) {
	for _, bare := range []string{"7f3a2b", "run-7f3a", "0b8e1c52-3f1d-4c7e-9a55-1f2e3d4c5b6a", "general_assistant"} {
		if got := RunLabelValue(bare); got != bare {
			t.Errorf("RunLabelValue(%q) = %q, want the bare id itself (an operator reads it with kubectl -l)", bare, got)
		}
	}

	// "r-" + the first 24 hex of core/id's content address of
	// {"runId":"v1:pipelines:run:7f3a"}.
	if got := RunLabelValue("v1:pipelines:run:7f3a"); got != "r-a52dd336cd341cefd8e6ab57" {
		t.Errorf("RunLabelValue(canonical id) = %q, want r-a52dd336cd341cefd8e6ab57", got)
	}

	for _, unsafe := range []string{strings.Repeat("a", 64), "-leading-dash", "trailing.", "has space", "v1:pipelines:run:7f3a"} {
		got := RunLabelValue(unsafe)
		if got == unsafe || len(got) > 63 || !labelValue.MatchString(got) {
			t.Errorf("RunLabelValue(%q) = %q, want a hashed, legal label value", unsafe, got)
		}
		if again := RunLabelValue(unsafe); again != got {
			t.Errorf("RunLabelValue(%q) is not deterministic: %q then %q -- a cancel would miss the run's Jobs", unsafe, got, again)
		}
	}
}
