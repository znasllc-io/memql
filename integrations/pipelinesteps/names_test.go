package pipelinesteps

import (
	"regexp"
	"strings"
	"testing"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
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

// TestCacheSubPathIsPerOwnerRepositoryAndTrust: a step can rewrite any entry
// of the cache it is given, and Go trusts its build cache by action id, so
// whoever writes a cache decides what a later build reading it gets (rulings
// R15, R15b). Two owners never share a cache, two repositories of one owner
// never do, and a pull request's run never writes the cache a run of the
// default branch, the merge queue or a release reads. The directory is
// derived, so every replica and every version mounts the same one, and its
// owner and repository are hashed, so neither an owner id nor a repository
// name can climb out of owners/ or name a sibling.
func TestCacheSubPathIsPerOwnerRepositoryAndTrust(t *testing.T) {
	widget := pl.Repository{Owner: "acme", Name: "widget"}
	// "owners/" + the first 24 hex of core/id's content address of
	// {"ownerUserId":"user-5d1e"}, "/repos/" + the first 24 hex of that of
	// {"repositoryName":"widget","repositoryOwner":"acme"}, and the trust,
	// derived like names_test.go's other literals.
	for _, tc := range []struct {
		owner string
		repo  pl.Repository
		trust CacheTrust
		want  string
	}{
		{"user-5d1e", widget, CacheTrusted, "owners/0dde18ce172fff450b32bf41/repos/dd0a30e0680ded4f864cf7f8/trusted"},
		{"user-5d1e", widget, CacheUntrusted, "owners/0dde18ce172fff450b32bf41/repos/dd0a30e0680ded4f864cf7f8/untrusted"},
		{"user-5d1e", pl.Repository{Owner: "acme", Name: "gadget"}, CacheTrusted, "owners/0dde18ce172fff450b32bf41/repos/f850af728594b092f8acc209/trusted"},
		{"user-77aa", widget, CacheTrusted, "owners/97d5292f36414d4a6638d794/repos/dd0a30e0680ded4f864cf7f8/trusted"},
	} {
		if got := CacheSubPath(tc.owner, tc.repo, tc.trust); got != tc.want {
			t.Errorf("CacheSubPath(%s, %s, %s) = %q, want %q", tc.owner, tc.repo.FullName(), tc.trust, got, tc.want)
		}
	}

	// Only a trust this package names is a trust: anything else is the
	// untrusted directory, never a path segment a caller chose.
	if got := CacheSubPath("user-5d1e", widget, CacheTrust("../trusted")); got != "owners/0dde18ce172fff450b32bf41/repos/dd0a30e0680ded4f864cf7f8/untrusted" {
		t.Errorf("CacheSubPath with trust ../trusted = %q, want the untrusted directory", got)
	}

	shape := regexp.MustCompile(`^owners/[0-9a-f]{24}/repos/[0-9a-f]{24}/(trusted|untrusted)$`)
	for _, owner := range []string{"user-5d1e", "../../etc", "a/b", ".", strings.Repeat("x", 300)} {
		for _, repo := range []pl.Repository{widget, {Owner: "..", Name: ".."}, {Owner: "a/b", Name: "../../c"}, {}} {
			for _, trust := range []CacheTrust{CacheTrusted, CacheUntrusted, ""} {
				got := CacheSubPath(owner, repo, trust)
				if !shape.MatchString(got) {
					t.Errorf("CacheSubPath(%q, %q, %q) = %q, want owners/<24 hex>/repos/<24 hex>/<trust>: anything else is a path an input chose",
						owner, repo.FullName(), trust, got)
				}
				if again := CacheSubPath(owner, repo, trust); again != got {
					t.Errorf("CacheSubPath(%q, %q, %q) is not deterministic: %q then %q", owner, repo.FullName(), trust, got, again)
				}
			}
		}
	}
	// Owner and name are two fields, not one joined string: "a-b"/"c" and
	// "a"/"b-c" are two repositories, and so are two caches.
	if CacheSubPath("user-5d1e", pl.Repository{Owner: "a-b", Name: "c"}, CacheTrusted) == CacheSubPath("user-5d1e", pl.Repository{Owner: "a", Name: "b-c"}, CacheTrusted) {
		t.Error("two repositories whose owner and name join to the same string share a cache")
	}
}

// TestCacheTrustIsTheRunsEvent (ruling R15b): what a run of the default
// branch, the merge queue or a release builds is what the repository has
// accepted; what a pull request's run builds is what a collaborator proposes.
// The event is the seam's, rendered into the step's contract environment
// (pl.StepRequest.Environment), which no secret and no manifest can set. An
// event this version does not know, or none, is untrusted: a run the runner
// cannot place never writes the cache the default branch reads.
func TestCacheTrustIsTheRunsEvent(t *testing.T) {
	for event, want := range map[string]CacheTrust{
		string(pl.EventPush):        CacheTrusted,
		string(pl.EventMergeGroup):  CacheTrusted,
		string(pl.EventRelease):     CacheTrusted,
		string(pl.EventPullRequest): CacheUntrusted,
		"":                          CacheUntrusted,
		"workflow_dispatch":         CacheUntrusted,
		"PUSH":                      CacheUntrusted,
	} {
		run := StepRun{Env: map[string]string{"MEMQL_EVENT": event}}
		if got := cacheTrustOf(run); got != want {
			t.Errorf("event %q: trust %q, want %q", event, got, want)
		}
	}
	if got := cacheTrustOf(StepRun{}); got != CacheUntrusted {
		t.Errorf("a run with no environment: trust %q, want untrusted", got)
	}
}

// TestIsolationProbeNamesArePerReplica: the isolation probe's Job is named
// from the node id alone, so a replica's next proof finds whatever its last
// one left and never another replica's; and neither it nor its Secret has the
// shape of a step's, so Status, Ack and the orphan-Secret sweep -- which
// address steps -- never reach them.
func TestIsolationProbeNamesArePerReplica(t *testing.T) {
	// "mpi-" + the first 24 hex of core/id's content address of
	// {"isolationProbeOf":"workbench-b"}, as core/id computed it when the
	// probe was written: a rename would leave a crashed proof's probe to its
	// deadline and TTL rather than to the next proof's cleanup.
	name := IsolationProbeName("workbench-b")
	if name != "mpi-2a0defd45f4e74a41bd99849" {
		t.Fatalf("IsolationProbeName(workbench-b) = %q, want mpi-2a0defd45f4e74a41bd99849", name)
	}
	if again := IsolationProbeName("workbench-b"); again != name {
		t.Errorf("IsolationProbeName is not deterministic: %q then %q", name, again)
	}
	if other := IsolationProbeName("workbench-a"); other == name {
		t.Errorf("two replicas share the probe name %q", other)
	}
	shape := regexp.MustCompile(`^mpi-[0-9a-f]{24}$`)
	target := IsolationTargetName(name)
	if !shape.MatchString(name) || target != name+"-target" {
		t.Errorf("names = %q and %q, want mpi- and 24 hex, and that with -target", name, target)
	}
	for _, n := range []string{name, target} {
		if len(n) > 63 || !dnsLabel.MatchString(n) {
			t.Errorf("%q is not a DNS label of at most 63 characters", n)
		}
		if isStepJobName(n) {
			t.Errorf("%q has the shape of a step's Job: Status and Ack would address it", n)
		}
	}
	// Ownerless and a year old, it is still not a step's Secret to sweep.
	old := ObjectMeta{Name: target, CreationTimestamp: time.Now().Add(-365 * 24 * time.Hour)}
	if _, ok := orphanedSecret(old, time.Now()); ok {
		t.Errorf("the orphan sweep judges %q as a step's Secret", target)
	}
}
