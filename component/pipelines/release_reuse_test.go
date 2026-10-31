package pipelines

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var (
	sameTreeHash  = strings.Repeat("7", 40)
	sameTreeStart = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
)

// sameTreePair is a release and a pull request run of one pipeline that tested
// one tree at different commits, the case the owner asked for.
func sameTreePair() (SameTreeRun, SameTreeRun) {
	reuser := SameTreeRun{ID: "release", OwnerUserID: "owner", PipelineID: "pipeline", Repository: "acme/engine", Event: string(EventRelease),
		TreeHash: sameTreeHash, Status: "in_progress", StartedAt: sameTreeStart}
	source := SameTreeRun{ID: "pull", OwnerUserID: "owner", PipelineID: "pipeline", Repository: "acme/engine", Event: string(EventPullRequest),
		TreeHash: sameTreeHash, Status: "completed", Conclusion: "success", FinishedAt: sameTreeStart.Add(-time.Hour)}
	return reuser, source
}

func TestSameTreeProblemAdmitsASuccessfulRunOfTheSameTree(t *testing.T) {
	reuser, source := sameTreePair()
	if problem := SameTreeProblem(reuser, source, sameTreeStart); problem != "" {
		t.Fatalf("a successful run of the same tree was refused: %s", problem)
	}
	source.FinishedAt = sameTreeStart.Add(-ReleaseReuseWindow)
	if problem := SameTreeProblem(reuser, source, sameTreeStart); problem != "" {
		t.Fatalf("a source exactly at the window was refused: %s", problem)
	}
	// A source that finished while the release was starting is not stale. The
	// proof has no clock but the run's start, so it must not read this as an
	// impossible time and refuse what the runner accepted.
	source.FinishedAt = sameTreeStart.Add(time.Minute)
	if problem := SameTreeProblem(reuser, source, sameTreeStart); problem != "" {
		t.Fatalf("a source that finished after the release started was refused: %s", problem)
	}
}

func TestSameTreeProblemRefusesEveryOtherSource(t *testing.T) {
	for name, change := range map[string]func(reuser, source *SameTreeRun){
		"itself":             func(_, s *SameTreeRun) { s.ID = "release" },
		"no source id":       func(_, s *SameTreeRun) { s.ID = "" },
		"another owner":      func(_, s *SameTreeRun) { s.OwnerUserID = "stranger" },
		"no owner":           func(r, s *SameTreeRun) { r.OwnerUserID, s.OwnerUserID = "", "" },
		"another pipeline":   func(_, s *SameTreeRun) { s.PipelineID = "other" },
		"no pipeline":        func(r, s *SameTreeRun) { r.PipelineID, s.PipelineID = "", "" },
		"another repository": func(_, s *SameTreeRun) { s.Repository = "acme/other" },
		"no repository":      func(r, s *SameTreeRun) { r.Repository, s.Repository = "", "" },
		"another tree":       func(_, s *SameTreeRun) { s.TreeHash = strings.Repeat("8", 40) },
		"source tree unread": func(_, s *SameTreeRun) { s.TreeHash = "" },
		"both trees unread":  func(r, s *SameTreeRun) { r.TreeHash, s.TreeHash = "", "" },
		"malformed tree":     func(r, s *SameTreeRun) { r.TreeHash, s.TreeHash = "tree", "tree" },
		"still running":      func(_, s *SameTreeRun) { s.Status = "in_progress"; s.Conclusion = "" },
		"failed":             func(_, s *SameTreeRun) { s.Conclusion = "failure" },
		"cancelled":          func(_, s *SameTreeRun) { s.Conclusion = "cancelled" },
		"refused":            func(_, s *SameTreeRun) { s.Conclusion = "refused" },
		"cancel asked for":   func(_, s *SameTreeRun) { s.CancelRequested = true },
		"scheduled scan":     func(_, s *SameTreeRun) { s.Event = string(EventSchedule) },
		"never finished":     func(_, s *SameTreeRun) { s.FinishedAt = time.Time{} },
		"too old":            func(_, s *SameTreeRun) { s.FinishedAt = sameTreeStart.Add(-ReleaseReuseWindow - time.Second) },
		"reuser never began": func(r, _ *SameTreeRun) { r.StartedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			reuser, source := sameTreePair()
			change(&reuser, &source)
			if problem := SameTreeProblem(reuser, source, sameTreeStart); problem == "" {
				t.Fatalf("source accepted: %+v for %+v", source, reuser)
			}
		})
	}
}

// The runner judges at the later of now and the run's start, the proof at the
// run's start. So a source the runner accepted is one the proof accepts, which is
// the property that keeps a release's version from being lost to a reuse the
// candidate then refuses.
func TestSameTreeWindowOfTheRunnerImpliesTheProofs(t *testing.T) {
	reuser, source := sameTreePair()
	for _, age := range []time.Duration{0, time.Hour, ReleaseReuseWindow - time.Second, ReleaseReuseWindow, ReleaseReuseWindow + time.Second} {
		// Later than the start, and earlier too: the replica planning the run may
		// have a clock behind the one that started it.
		for _, late := range []time.Duration{-time.Minute, 0, time.Minute, 2 * time.Hour} {
			now := sameTreeStart.Add(late)
			source.FinishedAt = now.Add(-age)
			if SameTreeProblem(reuser, source, SameTreeJudgedAt(now, reuser.StartedAt)) == "" && SameTreeProblem(reuser, source, reuser.StartedAt) != "" {
				t.Fatalf("the runner accepted a source the proof refuses: age %s, %s after the start", age, late)
			}
		}
	}
}

func TestValidTreeHash(t *testing.T) {
	for value, want := range map[string]bool{
		strings.Repeat("a", 40):       true,
		strings.Repeat("a", 64):       true,
		"":                            false,
		"tree":                        false,
		strings.Repeat("a", 39):       false,
		strings.Repeat("A", 40):       false,
		strings.Repeat("g", 40):       false,
		" " + strings.Repeat("a", 40): false,
	} {
		if got := ValidTreeHash(value); got != want {
			t.Errorf("ValidTreeHash(%q) = %v, want %v", value, got, want)
		}
	}
}

// reusedSource is a source run's executed, artifact-free receipt, bound to its
// tree through the work run's input.
func reusedSource() (map[string]any, map[string]any) {
	r, s := artifactFreeReleaseWork()
	r["input"].(map[string]any)["event"] = "pull_request"
	r["input"].(map[string]any)["mode"] = "affected"
	r["input"].(map[string]any)["treeHash"] = sameTreeHash
	r["triggeredBy"] = WorkTriggerPrefix + "affected"
	return r, s
}

// reusedSourceRun is the run row reusedSource's journal belongs to.
func reusedSourceRun() SameTreeRun {
	return SameTreeRun{ID: "pipeline-run", OwnerUserID: "owner", PipelineID: "pipeline", Repository: "acme/engine", SHA: strings.Repeat("b", 40),
		Event: "pull_request", TreeHash: sameTreeHash, Status: "completed", Conclusion: "success"}
}

func TestReusedReleaseReceiptAdmitsAnExecutedArtifactFreePass(t *testing.T) {
	r, s := reusedSource()
	got, err := InspectReusedReleaseReceipt("owner", "run", "test.full", r, s, reusedSourceRun())
	if err != nil {
		t.Fatal(err)
	}
	if got.TreeHash != sameTreeHash || got.Status != "done" || got.StepKind != string(StepCommand) || got.Event != "pull_request" {
		t.Fatalf("receipt lost its facts: %+v", got)
	}
	// One step receipt has one release identity, whichever inspector reads it.
	released, err := InspectReleaseStepReceipt("owner", "run", "test.full", r, s)
	if err != nil || released.ReceiptDigest != got.ReceiptDigest {
		t.Fatalf("reused digest %s differs from the release digest %s: %v", got.ReceiptDigest, released.ReceiptDigest, err)
	}
}

func TestReusedReleaseReceiptRefusesWhatCannotStandForARelease(t *testing.T) {
	for name, change := range map[string]func(map[string]any, map[string]any){
		"artifacts": func(_, s map[string]any) {
			s["result"].(map[string]any)["metadata"].(map[string]any)["artifactIntentIds"] = []string{strings.Repeat("c", 64)}
		},
		"carried skip": func(_, s map[string]any) {
			s["call"].(map[string]any)["skip"] = map[string]any{"code": CodePassedEarlier, "reason": "Passed in attempt 1."}
			s["status"], s["errorCode"] = "skipped", CodePassedEarlier
			s["result"] = map[string]any{"code": CodePassedEarlier, "reason": "Passed in attempt 1."}
		},
		"planned skip": func(_, s map[string]any) {
			s["call"].(map[string]any)["skip"] = map[string]any{"code": CodeNotAffected, "reason": "No packages."}
			s["status"], s["errorCode"] = "skipped", CodeNotAffected
			s["result"] = map[string]any{"code": CodeNotAffected, "reason": "No packages."}
		},
		"notification": func(_, s map[string]any) {
			s["call"].(map[string]any)["pipelineKind"] = "notify"
			s["result"] = map[string]any{"status": "succeeded", "notificationStatus": "delivered", "channel": "owners", "kind": "email", "requestIds": []string{"request-1"}}
		},
		"nonzero exit":     func(_, s map[string]any) { s["result"].(map[string]any)["metadata"].(map[string]any)["exitCode"] = 1 },
		"failed step":      func(_, s map[string]any) { s["status"] = "failed" },
		"source failed":    func(r, _ map[string]any) { r["status"] = "failed" },
		"source cancelled": func(r, _ map[string]any) { r["status"] = "cancelled" },
		"source abandoned": func(r, _ map[string]any) { r["status"] = "abandoned" },
		"source running":   func(r, _ map[string]any) { r["status"] = "running" },
		"cancel requested": func(r, _ map[string]any) { r["cancelRequested"] = true },
		"caller payload":   func(r, _ map[string]any) { r["callerSuppliedPayload"] = true },
		"replay":           func(r, _ map[string]any) { r["mode"] = "replay" },
		"other owner":      func(_, s map[string]any) { s["ownerUserId"] = "stranger" },
		"no tree":          func(r, _ map[string]any) { delete(r["input"].(map[string]any), "treeHash") },
		"malformed tree":   func(r, _ map[string]any) { r["input"].(map[string]any)["treeHash"] = "tree" },
	} {
		t.Run(name, func(t *testing.T) {
			r, s := reusedSource()
			change(r, s)
			if got, err := InspectReusedReleaseReceipt("owner", "run", "test.full", r, s, reusedSourceRun()); err == nil {
				t.Fatalf("receipt that cannot stand for a release was accepted: %+v", got)
			}
		})
	}
}

// The journal says which run a receipt executed for, and the row must agree. A row
// that names a tree the journal does not hold is refused, so the tree a candidate
// records is always the one the receipt's digest covers.
func TestReusedReleaseReceiptMustBeTheSourceRunsOwn(t *testing.T) {
	for name, change := range map[string]func(*SameTreeRun){
		"another run":        func(s *SameTreeRun) { s.ID = "other" },
		"another pipeline":   func(s *SameTreeRun) { s.PipelineID = "other" },
		"another repository": func(s *SameTreeRun) { s.Repository = "acme/other" },
		"another commit":     func(s *SameTreeRun) { s.SHA = strings.Repeat("9", 40) },
		"another tree":       func(s *SameTreeRun) { s.TreeHash = strings.Repeat("8", 40) },
	} {
		t.Run(name, func(t *testing.T) {
			r, s := reusedSource()
			source := reusedSourceRun()
			change(&source)
			if got, err := InspectReusedReleaseReceipt("owner", "run", "test.full", r, s, source); err == nil {
				t.Fatalf("a receipt that disagrees with its run was accepted: %+v", got)
			}
		})
	}
}

// A rollback must not refuse a candidate that reused nothing, and an engine from
// before tree hashes computes a receipt's digest without one. So the input's tree
// stays out of the digest: the same rows have one digest on every engine.
func TestAReceiptsDigestIgnoresItsInputTree(t *testing.T) {
	r, s := reusedSource()
	with, err := InspectReleaseStepReceipt("owner", "run", "test.full", r, s)
	if err != nil {
		t.Fatal(err)
	}
	delete(r["input"].(map[string]any), "treeHash")
	without, err := InspectReleaseStepReceipt("owner", "run", "test.full", r, s)
	if err != nil {
		t.Fatal(err)
	}
	if with.ReceiptDigest != without.ReceiptDigest {
		t.Fatal("the input's tree moved the receipt's digest, so an older engine would refuse this candidate")
	}
	if with.TreeHash != sameTreeHash || without.TreeHash != "" {
		t.Fatalf("the tree was not read beside the digest: %q, %q", with.TreeHash, without.TreeHash)
	}
}

// The digest commits to the tree through the step's own declaration.
func TestAReceiptsDigestBindsItsTreeFingerprint(t *testing.T) {
	r, s := reusedSource()
	s["call"].(map[string]any)["treeFingerprint"] = "work-definition-v2:" + strings.Repeat("1", 64)
	first, err := InspectReleaseStepReceipt("owner", "run", "test.full", r, s)
	if err != nil {
		t.Fatal(err)
	}
	s["call"].(map[string]any)["treeFingerprint"] = "work-definition-v2:" + strings.Repeat("2", 64)
	second, err := InspectReleaseStepReceipt("owner", "run", "test.full", r, s)
	if err != nil {
		t.Fatal(err)
	}
	if first.ReceiptDigest == second.ReceiptDigest {
		t.Fatal("a different tree fingerprint kept the receipt's digest")
	}
}

// Every candidate and overlay already saved re-verifies its receipts by digest,
// so a receipt written before tree hashes existed must keep the digest it had.
// These values were read from the engine before this change.
func TestReceiptDigestsWithoutATreeKeepTheirEarlierValues(t *testing.T) {
	r, s := releaseWorkFixture()
	released, err := InspectReleaseWorkReceipt("owner", "run", "test.full", r, s)
	if err != nil || released.ReceiptDigest != "sha256:a10f5dd053346d2082613b57282f4f60aaa8a62785ab1998f35df01b363e83c3" {
		t.Errorf("release digest moved: %s, %v", released.ReceiptDigest, err)
	}
	if released.TreeHash != "" {
		t.Errorf("a receipt with no tree reports one: %q", released.TreeHash)
	}
	r, s = artifactFreeReleaseWork()
	carried, err := InspectCarriedReleaseReceipt("owner", "run", "test.full", r, s)
	if err != nil || carried.ReceiptDigest != "sha256:78387e819d31986eafcfac4d859fd5b15e73561450f5120f07fb0f3c537e1804" {
		t.Errorf("carried digest moved: %s, %v", carried.ReceiptDigest, err)
	}
	r, s = releaseWorkFixture()
	scanner, err := InspectScannerReceipt("owner", "run", "test.full", r, s)
	if err != nil || scanner.ReceiptDigest != "sha256:c54b84199a09bc0f374000002d9eafcaadb0774399a8c3ee2434c3b94e76fb50" {
		t.Errorf("scanner digest moved: %s, %v", scanner.ReceiptDigest, err)
	}
}

func reusedFromFixture() *ReleaseReusedFrom {
	return &ReleaseReusedFrom{PipelineRunID: "pull", WorkRunID: "pull-work", Attempt: 1, Event: "pull_request", Commit: strings.Repeat("9", 40), TreeHash: sameTreeHash, ReceiptDigest: "sha256:" + strings.Repeat("e", 64)}
}

func TestReleaseCandidateRecordsReusedProvenance(t *testing.T) {
	plain := releaseCandidateFixture()
	plainBody, plainID, err := CanonicalReleaseCandidate(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plainBody), "reusedFrom") {
		t.Fatalf("a candidate with no reuse names it, which would move every saved candidate's identity: %s", plainBody)
	}
	with := releaseCandidateFixture()
	with.Evidence[0].ArtifactIntentIDs = []string{}
	with.Evidence[0].ReusedFrom = reusedFromFixture()
	body, id, err := CanonicalReleaseCandidate(with)
	if err != nil {
		t.Fatal(err)
	}
	if id == plainID || !strings.Contains(string(body), `"reusedFrom":{"pipelineRunId":"pull"`) {
		t.Fatalf("provenance is not part of the candidate: %s", body)
	}
	for name, change := range map[string]func(*ReleaseReusedFrom){
		"run":     func(f *ReleaseReusedFrom) { f.PipelineRunID = "other" },
		"work":    func(f *ReleaseReusedFrom) { f.WorkRunID = "other" },
		"attempt": func(f *ReleaseReusedFrom) { f.Attempt = 2 },
		"event":   func(f *ReleaseReusedFrom) { f.Event = "push" },
		"commit":  func(f *ReleaseReusedFrom) { f.Commit = strings.Repeat("6", 40) },
		"tree":    func(f *ReleaseReusedFrom) { f.TreeHash = strings.Repeat("6", 40) },
		"receipt": func(f *ReleaseReusedFrom) { f.ReceiptDigest = "sha256:" + strings.Repeat("6", 64) },
	} {
		t.Run("binds "+name, func(t *testing.T) {
			c := releaseCandidateFixture()
			c.Evidence[0].ArtifactIntentIDs = []string{}
			c.Evidence[0].ReusedFrom = reusedFromFixture()
			change(c.Evidence[0].ReusedFrom)
			_, changed, err := CanonicalReleaseCandidate(c)
			if err != nil || changed == id {
				t.Fatalf("a changed %s kept the candidate's identity: %v", name, err)
			}
		})
	}
}

func TestReleaseCandidateRefusesInvalidReusedProvenance(t *testing.T) {
	for name, change := range map[string]func(*ReleaseEvidence){
		"no run":      func(e *ReleaseEvidence) { e.ReusedFrom.PipelineRunID = "" },
		"no work":     func(e *ReleaseEvidence) { e.ReusedFrom.WorkRunID = "" },
		"no attempt":  func(e *ReleaseEvidence) { e.ReusedFrom.Attempt = 0 },
		"no event":    func(e *ReleaseEvidence) { e.ReusedFrom.Event = "" },
		"bad commit":  func(e *ReleaseEvidence) { e.ReusedFrom.Commit = "main" },
		"bad tree":    func(e *ReleaseEvidence) { e.ReusedFrom.TreeHash = "tree" },
		"bad receipt": func(e *ReleaseEvidence) { e.ReusedFrom.ReceiptDigest = "receipt" },
		// A reused step produced nothing. A candidate that says otherwise would
		// let an artifact stand on a run it did not cover.
		"artifacts": func(e *ReleaseEvidence) { e.ArtifactIntentIDs = []string{strings.Repeat("d", 64)} },
	} {
		t.Run(name, func(t *testing.T) {
			c := releaseCandidateFixture()
			c.Evidence[0].ArtifactIntentIDs = []string{}
			c.Evidence[0].ReusedFrom = reusedFromFixture()
			change(&c.Evidence[0])
			if _, _, err := CanonicalReleaseCandidate(c); err == nil {
				t.Fatal("invalid reuse provenance was accepted")
			}
		})
	}
}

// The native overlay hashes these receipts as JSON, and its saved evidence must read
// the same on any engine, so a tree never enters that encoding.
func TestAReceiptsEncodingNeverNamesItsTree(t *testing.T) {
	r, s := reusedSource()
	got, err := InspectReleaseStepReceipt("owner", "run", "test.full", r, s)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if got.TreeHash == "" || strings.Contains(string(body), "TreeHash") || strings.Contains(string(body), sameTreeHash) {
		t.Fatalf("the receipt's encoding names its tree: %s", body)
	}
}

// A row spells some ids canonically; the rule compares them bare.
func TestSameTreeRunFromRowReadsARunRow(t *testing.T) {
	bare := func(id string) string { return id[strings.LastIndex(id, ":")+1:] }
	got, err := SameTreeRunFromRow(map[string]any{
		"id": "v1:pipelines:run:pull", "ownerUserId": "v1:identity:user:owner", "pipelineId": "v1:pipelines:pipeline:pipeline", "repository": "acme/engine",
		"sha": strings.Repeat("9", 40), "mode": "affected", "event": "pull_request", "treeHash": sameTreeHash, "status": "completed", "conclusion": "success",
		"workRunId": "v1:work:run:pull-work", "attempt": 2, "cancelRequested": true, "startedAt": "2026-10-29T11:00:00Z", "finishedAt": "2026-10-29T11:30:00Z",
	}, bare)
	if err != nil {
		t.Fatal(err)
	}
	want := SameTreeRun{ID: "pull", OwnerUserID: "owner", PipelineID: "pipeline", Repository: "acme/engine", SHA: strings.Repeat("9", 40), Mode: "affected", Event: "pull_request",
		TreeHash: sameTreeHash, Status: "completed", Conclusion: "success", WorkRunID: "pull-work", Attempt: 2, CancelRequested: true,
		StartedAt: time.Date(2026, 10, 29, 11, 0, 0, 0, time.UTC), FinishedAt: time.Date(2026, 10, 29, 11, 30, 0, 0, time.UTC)}
	if got != want {
		t.Fatalf("read %+v\nwant %+v", got, want)
	}
}

// reusedProofCase is a release that reused test.full from a pull request, and the
// journal the proof reads, as coverage reads it.
type reusedProofCase struct {
	reuser     SameTreeRun
	reuserStep map[string]any
	source     SameTreeRun
	sourceRun  map[string]any
	sourceStep map[string]any
}

func newReusedProofCase() *reusedProofCase {
	fingerprint := "work-definition-v2:" + strings.Repeat("f", 64)
	definition := "work-definition-v2:" + strings.Repeat("e", 64)
	reuser, source := sameTreePair()
	reuser.Mode = string(ModeFull)
	source = reusedSourceRun()
	source.StartedAt, source.FinishedAt = sameTreeStart.Add(-2*time.Hour), sameTreeStart.Add(-time.Hour)
	source.Attempt, source.WorkRunID = 1, "run"
	r, s := reusedSource()
	s["call"].(map[string]any)["treeFingerprint"] = fingerprint
	s["call"].(map[string]any)["packages"] = []any{"example.test/a"}
	reason := "Verified on the same source tree in run pipeline-run."
	return &reusedProofCase{reuser: reuser, source: source, sourceRun: r, sourceStep: s, reuserStep: map[string]any{
		"call": map[string]any{"definitionFingerprint": definition, "treeFingerprint": fingerprint, "evidenceRunId": source.ID, "packages": []any{"example.test/a"},
			"skip": map[string]any{"code": CodePassedEarlier, "reason": reason}},
	}}
}

func (c *reusedProofCase) prove() (ReleaseReusedFrom, error) {
	return ProveReusedStep(ReusedStepReads{
		PipelineRun: func(id string) (SameTreeRun, error) {
			if id != c.source.ID {
				return SameTreeRun{}, errors.New("no such run")
			}
			return c.source, nil
		},
		WorkRun:  func(string) (map[string]any, error) { return c.sourceRun, nil },
		WorkStep: func(string, string) (map[string]any, error) { return c.sourceStep, nil },
	}, "owner", c.reuser, c.reuserStep, "test.full")
}

func TestProveReusedStepNamesTheReceiptItStandsOn(t *testing.T) {
	c := newReusedProofCase()
	got, err := c.prove()
	if err != nil {
		t.Fatal(err)
	}
	released, err := InspectReleaseStepReceipt("owner", "run", "test.full", c.sourceRun, c.sourceStep)
	if err != nil {
		t.Fatal(err)
	}
	want := ReleaseReusedFrom{PipelineRunID: c.source.ID, WorkRunID: "run", Attempt: 1, Event: "pull_request", Commit: c.source.SHA, TreeHash: sameTreeHash, ReceiptDigest: released.ReceiptDigest}
	if got != want {
		t.Fatalf("proved %+v\nwant %+v", got, want)
	}
}

// Each for its own reason: a later refusal must not pass for this one.
func TestProveReusedStepRefusesWhatItCannotProve(t *testing.T) {
	for name, tc := range map[string]struct {
		want   string
		change func(*reusedProofCase)
	}{
		"the reuser is not a release": {"only a full release", func(c *reusedProofCase) { c.reuser.Event = string(EventPush) }},
		"the reuser is not full":      {"only a full release", func(c *reusedProofCase) { c.reuser.Mode = string(ModeAffected) }},
		"it names no run":             {"cannot be read", func(c *reusedProofCase) { delete(c.reuserStep["call"].(map[string]any), "evidenceRunId") }},
		"another tree":                {"different tree", func(c *reusedProofCase) { c.source.TreeHash = strings.Repeat("8", 40) }},
		"no work run":                 {"no work run", func(c *reusedProofCase) { c.source.WorkRunID = "" }},
		"the receipt is another run's": {"the source run's own", func(c *reusedProofCase) {
			c.sourceRun["input"].(map[string]any)["pipelineRunId"] = "other"
		}},
		"another check": {"different check", func(c *reusedProofCase) {
			c.sourceStep["call"].(map[string]any)["treeFingerprint"] = "work-definition-v2:" + strings.Repeat("9", 64)
		}},
		"no check to compare": {"different check", func(c *reusedProofCase) {
			delete(c.reuserStep["call"].(map[string]any), "treeFingerprint")
			delete(c.sourceStep["call"].(map[string]any), "treeFingerprint")
		}},
		"other packages": {"different packages", func(c *reusedProofCase) {
			c.sourceStep["call"].(map[string]any)["packages"] = []any{"example.test/b"}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			c := newReusedProofCase()
			tc.change(c)
			got, err := c.prove()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %+v, %v; want a refusal saying %q", got, err, tc.want)
			}
		})
	}
}

// A row whose plan reused a step says so however it settled, because a failed stage
// may have blocked it first. Anything else names no run.
func TestReusedEvidenceRunReadsThePlansDeclaration(t *testing.T) {
	call := func(code string) map[string]any {
		return map[string]any{"call": map[string]any{"evidenceRunId": "pull", "skip": map[string]any{"code": code, "reason": "Why."}}}
	}
	if got := ReusedEvidenceRun(call(CodePassedEarlier)); got != "pull" {
		t.Fatalf("got %q", got)
	}
	if got := ReusedEvidenceRun(call(CodeNotAffected)); got != "" {
		t.Fatalf("a skip for another reason named a run: %q", got)
	}
	if got := ReusedEvidenceRun(map[string]any{"status": "done"}); got != "" {
		t.Fatalf("a row with no call named a run: %q", got)
	}
}
