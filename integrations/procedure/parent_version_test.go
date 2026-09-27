package procedure

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// parent_version_test.go -- a recording is judged by the parent step version
// that delegated it (epic memql#5414, D18 and D23's learning half; plan
// section 1.5).

// versionsEngine answers workStepVersions in the shape a top-level builtin
// really answers in-process -- ONE node set keyed by id, each node's payload
// the folded step row plus `current` -- and hands every other call to the
// recording fake, which records this one too, context included.
type versionsEngine struct {
	*fakeEngine
	// versions are the entries by parent run id.
	versions map[string][]map[string]any
}

func newVersionsEngine() *versionsEngine {
	return &versionsEngine{fakeEngine: newFakeEngine(), versions: map[string][]map[string]any{}}
}

func (e *versionsEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	res, err := e.fakeEngine.Execute(ctx, q)
	if err != nil || !strings.HasPrefix(strings.TrimSpace(q), "builtin workStepVersions(") {
		return res, err
	}
	nodes := map[string]memorynodes.MemoryNode{}
	for parent, entries := range e.versions {
		if !strings.Contains(q, `runId: "`+parent+`"`) {
			continue
		}
		for _, entry := range entries {
			payload, err := json.Marshal(entry)
			if err != nil {
				return nil, err
			}
			id := str(entry, "stepId") + "@v" + strconv.Itoa(intOf(entry, "version"))
			nodes[id] = memorynodes.MemoryNode{ID: id, Concept: "v1:work:step", Type: memorynodes.NodeTypeObject, CreatedAt: testNow, Payload: payload}
		}
	}
	return memql.NewResultWithOutput(nodes), nil
}

// parentOf is the goal run the harness's recordings name as their parent.
func parentOf(r recFixture) string { return "v1:work:run:parent-" + r.file }

// parentVersion is one entry of workStepVersions: a version of the parent's
// delegating step, the child run it opened, and whether it is current.
func parentVersion(key string, n int, childRunId string, current bool) map[string]any {
	return map[string]any{
		"stepId": "parent-" + key, "key": key, "version": float64(n), "attempt": float64(n),
		"childRunId": childRunId, "current": current, "status": "done",
	}
}

// parentFeedback is a verdict a person gave on one version of the parent's
// step, as recordFeedback writes it.
func parentFeedback(verdict, key string, n int, at time.Time) map[string]any {
	return map[string]any{
		"kind": "feedback", "createdAt": at.Format(time.RFC3339Nano),
		"data": map[string]any{"verdict": verdict, "target": map[string]any{"stepKey": key, "version": float64(n)}},
	}
}

func loadJudgedCorpus(t *testing.T, eng *versionsEngine) []recording {
	t.Helper()
	got, err := newTestIntegration(eng).loadCorpus(context.Background(), corpusKeyFor())
	if err != nil {
		t.Fatalf("loadCorpus: %v", err)
	}
	return got
}

func runIdsOf(recs []recording) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.RunId)
	}
	return out
}

func threeRecordings() []recFixture {
	return []recFixture{
		recording1("a.txt", testNow.Add(-3*time.Hour)),
		recording1("b.txt", testNow.Add(-2*time.Hour)),
		recording1("c.txt", testNow.Add(-1*time.Hour)),
	}
}

// TestASupersededRecordingLeavesTheCorpus (D18): the person re-ran the step
// that delegated recording a, so version 1 -- a's -- is no longer the run's
// current version, and a answers a question the run no longer asks. It
// leaves the corpus; b, whose version is current (named by its bare id, the
// way a writer may store it), stays. The versions are read through the
// builtin, as the owner, naming the parent.
func TestASupersededRecordingLeavesTheCorpus(t *testing.T) {
	recs := threeRecordings()
	eng := newVersionsEngine()
	seedCorpus(t, eng.fakeEngine, recs...)
	eng.versions[parentOf(recs[0])] = []map[string]any{
		parentVersion("delegate", 1, recs[0].runId, false),
		parentVersion("delegate", 2, "v1:work:run:rec-a-rerun", true),
	}
	eng.versions[parentOf(recs[1])] = []map[string]any{
		parentVersion("delegate", 1, memql.BareShortId(recs[1].runId), true),
	}

	got := runIdsOf(loadJudgedCorpus(t, eng))
	if strings.Join(got, ",") != recs[1].runId+","+recs[2].runId {
		t.Fatalf("corpus = %v, want the superseded recording out and the other two in", got)
	}
	reads := eng.callsTo("workStepVersions")
	if len(reads) != 3 {
		t.Fatalf("workStepVersions was read %d times, want once per recording's parent", len(reads))
	}
	for _, c := range reads {
		if c.Actor != testOwner || c.Internal {
			t.Errorf("%s ran as %q (internal %v), want the owner's own read", c.Query, c.Actor, c.Internal)
		}
	}
	if !strings.Contains(reads[0].Query, `runId: "`+parentOf(recs[0])+`"`) {
		t.Errorf("the versions read is %s, want it to name the parent run", reads[0].Query)
	}

	// The control: the same version, current, keeps its recording.
	eng.versions[parentOf(recs[0])][0]["current"] = true
	if got := loadJudgedCorpus(t, eng); len(got) != 3 {
		t.Fatalf("a current version must keep its recording; corpus = %v", runIdsOf(got))
	}
}

// TestADislikedParentStepVersionExcludesItsRecording (D23): the newest
// verdict on the version that recorded a is a dislike, so a leaves the
// corpus. A dislike on ANOTHER version says nothing about a, and a like given
// after the dislike is the person's current judgment -- the recording comes
// back, weighing more.
func TestADislikedParentStepVersionExcludesItsRecording(t *testing.T) {
	recs := threeRecordings()
	setup := func(feedback ...map[string]any) *versionsEngine {
		eng := newVersionsEngine()
		seedCorpus(t, eng.fakeEngine, recs...)
		eng.versions[parentOf(recs[0])] = []map[string]any{parentVersion("delegate", 1, recs[0].runId, true)}
		eng.replyWhen("workObservationsForOwnerRun", `"`+parentOf(recs[0])+`"`, feedback...)
		return eng
	}

	disliked := loadJudgedCorpus(t, setup(parentFeedback("dislike", "delegate", 1, testNow.Add(-time.Minute))))
	if got := runIdsOf(disliked); strings.Join(got, ",") != recs[1].runId+","+recs[2].runId {
		t.Fatalf("corpus = %v, want the disliked version's recording out", got)
	}

	otherVersion := loadJudgedCorpus(t, setup(parentFeedback("dislike", "delegate", 2, testNow)))
	if len(otherVersion) != 3 || otherVersion[0].weight() != 1 {
		t.Fatalf("a dislike on another version must not touch a: %v", runIdsOf(otherVersion))
	}

	reconsidered := loadJudgedCorpus(t, setup(
		parentFeedback("dislike", "delegate", 1, testNow.Add(-2*time.Minute)),
		parentFeedback("like", "delegate", 1, testNow.Add(-time.Minute)),
	))
	if len(reconsidered) != 3 || reconsidered[0].RunId != recs[0].runId || reconsidered[0].weight() != likedWeight {
		t.Fatalf("a like after the dislike must bring a back weighing %d: %v", likedWeight, reconsidered)
	}
}

// TestALikedRecordingRanksHigherWithoutChangingTheHash (D23): a like on the
// version that recorded a -- or on a's run itself -- makes a weigh two in the
// corpus, so it wins a ranking tie (component/procedure.MineWeighted). What
// it never does is change the procedure: the instances keep their order, so
// the lift writes the SAME version hash it wrote without the like, and the
// ladder a person has already moved is not reset. And one liked recording is
// still one use: the gate's sentence still counts two.
func TestALikedRecordingRanksHigherWithoutChangingTheHash(t *testing.T) {
	lift := func(t *testing.T, eng *versionsEngine) LearnResult {
		t.Helper()
		i := newTestIntegration(eng)
		i.SetCompiler(&passingGate{})
		return mustLift(t, i)
	}
	plain := newVersionsEngine()
	seedCorpus(t, plain.fakeEngine, twoRecordings()...)
	unliked := lift(t, plain)

	for name, like := range map[string]func(*versionsEngine, recFixture){
		"the parent step version": func(eng *versionsEngine, r recFixture) {
			eng.versions[parentOf(r)] = []map[string]any{parentVersion("delegate", 1, r.runId, true)}
			eng.replyWhen("workObservationsForOwnerRun", `"`+parentOf(r)+`"`, parentFeedback("like", "delegate", 1, testNow))
		},
		"the recording's run": func(eng *versionsEngine, r recFixture) {
			eng.replyWhen("workObservationsForOwnerRun", `"`+r.runId+`"`,
				append(r.observationRows(t), map[string]any{"kind": "feedback", "createdAt": testNow.Format(time.RFC3339Nano),
					"data": map[string]any{"verdict": "like"}})...)
		},
	} {
		t.Run(name, func(t *testing.T) {
			recs := twoRecordings()
			eng := newVersionsEngine()
			like(eng, recs[0])
			seedCorpus(t, eng.fakeEngine, recs...)

			corpus := loadJudgedCorpus(t, eng)
			if len(corpus) != 2 || corpus[0].weight() != likedWeight || corpus[1].weight() != 1 {
				t.Fatalf("weights = %v, want the liked recording at %d and the other at 1", weightsOf(corpus), likedWeight)
			}
			liked := lift(t, eng)
			if liked.ProcedureHash == "" || liked.ProcedureHash != unliked.ProcedureHash {
				t.Fatalf("a like changed the procedure: hash %s, without it %s", liked.ProcedureHash, unliked.ProcedureHash)
			}
			reason, _ := argsOf(t, eng.callTo(t, "recordConstructLadder"))["ladderReason"].(string)
			if !strings.Contains(reason, "used 2 times") {
				t.Fatalf("ladder reason %q: one liked recording must still be one use", reason)
			}
		})
	}
}

func weightsOf(recs []recording) []int {
	out := make([]int, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.weight())
	}
	return out
}

// TestAParentThatCannotBeReadJudgesNothing: a recording whose parent's
// versions cannot be read -- a transient error, a parent the owner does not
// own, a node with no work integration -- is judged by its own verdicts
// alone, and the load goes on: one unreachable parent must not stop every
// procedure of the goal from being learned. The failure is read once per
// parent, not once per recording.
func TestAParentThatCannotBeReadJudgesNothing(t *testing.T) {
	recs := twoRecordings()
	eng := newVersionsEngine()
	seedCorpus(t, eng.fakeEngine, recs...)
	// Both recordings delegated by one parent, newest first as the query
	// answers.
	newer, older := recs[1].runRow(), recs[0].runRow()
	newer["parentRunId"] = parentOf(recs[0])
	eng.reply("workRunsForOwnerGoalSignature", newer, older)
	eng.fail["workStepVersions"] = errWorkStepVersionsDown
	got, err := newTestIntegration(eng).loadCorpus(context.Background(), corpusKeyFor())
	if err != nil {
		t.Fatalf("an unreadable parent failed the corpus: %v", err)
	}
	if len(got) != 2 || got[0].weight() != 1 || got[1].weight() != 1 {
		t.Fatalf("corpus = %v weights %v, want both recordings judged by nothing", runIdsOf(got), weightsOf(got))
	}
	if n := len(eng.callsTo("workStepVersions")); n != 1 {
		t.Fatalf("the unreadable parent was read %d times, want once for both of its recordings", n)
	}
}

var errWorkStepVersionsDown = errors.New("work: stepVersions is not reachable")
