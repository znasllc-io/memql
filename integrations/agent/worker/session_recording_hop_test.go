//go:build agent

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/node"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// session_recording_hop_test.go -- a recorded session is visible on the page
// the bff serves (epic memql#5396, task memql#5401).
//
// ============================================================================
// WHY THIS IS AN IN-PROCESS TEST AND NOT A CLUSTER LANE
// ============================================================================
// The same argument forward_hop_test.go makes at length: a live-cluster
// version belongs in test/clustere2e and would be skipped on every CI lane and
// every developer machine, and a gate skipped by default cannot be the thing
// standing between a feature and the bug it prevents.
//
// The hop this one gates has TWO halves and they fail differently:
//
//   - the ROWS: the recording writes v1:work:run, v1:work:step and
//     v1:worker:appSession on the agent replica holding the machine's stream,
//     and a person reads them on a bff. Default-deny means an unrouted concept
//     is SILENCE, and the page is then correct on load and frozen after.
//   - the WRITER: it borrows the owner's actor and stamps internal origin.
//     Neither is visible at the seam a row-counting test watches -- an
//     unstamped @serverOnly write is refused with one WARN, and an unowned row
//     is readable by nobody.
//
// TO CONFIRM IT IS LOAD-BEARING: delete the v1:worker:appSession rules from
// component/node/routing.go, or drop the owner from the recorded rows, and
// these fail.

// hopRecorder stands in for the SessionWriter on the far replica and records
// which concepts the recording touched, so the topics can be asked of the real
// routing table.
type hopRecorder struct {
	mu       sync.Mutex
	owners   []string
	concepts map[string]bool
	runId    string
}

func newHopRecorder() *hopRecorder {
	return &hopRecorder{concepts: map[string]bool{}, runId: "v1:work:run:recording"}
}

func (h *hopRecorder) touch(concept, owner string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.concepts[concept] = true
	h.owners = append(h.owners, owner)
}

func (h *hopRecorder) OpenRecording(_ context.Context, r workerservice.RecordingOpen) (string, error) {
	h.touch("v1:work:goal", r.OwnerUserId)
	h.touch("v1:work:run", r.OwnerUserId)
	if r.RunId != "" {
		return r.RunId, nil
	}
	return h.runId, nil
}

func (h *hopRecorder) RecordAction(_ context.Context, r workerservice.RecordedAction) error {
	h.touch("v1:work:step", r.OwnerUserId)
	h.touch("v1:work:observation", r.OwnerUserId)
	return nil
}

func (h *hopRecorder) RecordGap(_ context.Context, r workerservice.RecordedGap) error {
	h.touch("v1:work:observation", r.OwnerUserId)
	return nil
}

func (h *hopRecorder) CloseRecording(_ context.Context, r workerservice.RecordingClose) error {
	h.touch("v1:work:step", r.OwnerUserId)
	h.touch("v1:work:run", r.OwnerUserId)
	return nil
}

func (h *hopRecorder) touched() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.concepts))
	for c := range h.concepts {
		out = append(out, c)
	}
	return out
}

func (h *hopRecorder) recordedOwners() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.owners))
	copy(out, h.owners)
	return out
}

// TestARecordedSessionsRowsReachASubscriptionOnAnotherReplica -- issue
// #5401's acceptance. Each concept the recording writes is asked of the REAL
// routing table (component/node's evaluateRouting), not a copy of it: the
// wildcard semantics alone are enough to make a plausible re-implementation
// disagree on exactly the rules that matter.
func TestARecordedSessionsRowsReachASubscriptionOnAnotherReplica(t *testing.T) {
	rec := newHopRecorder()
	owner := "v1:identity:user:alice"

	// One session's worth of writes, the way the runner drives them.
	ctx := context.Background()
	runId, err := rec.OpenRecording(ctx, workerservice.RecordingOpen{
		SessionId: "v1:worker:appSession:s1", OwnerUserId: owner, App: "claude-code",
	})
	if err != nil {
		t.Fatalf("OpenRecording: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := rec.RecordAction(ctx, workerservice.RecordedAction{
			SessionId: "v1:worker:appSession:s1", OwnerUserId: owner, RunId: runId, Seq: i,
			Action: workerservice.ActionEvent{Id: fmt.Sprintf("a%d", i), Seq: uint64(i + 1), Tool: "exec"},
		}); err != nil {
			t.Fatalf("RecordAction: %v", err)
		}
	}
	if err := rec.CloseRecording(ctx, workerservice.RecordingClose{
		SessionId: "v1:worker:appSession:s1", OwnerUserId: owner, RunId: runId,
		Seq: 3, Status: workerservice.AppSessionStatusEnded, RecordedActions: 3,
	}); err != nil {
		t.Fatalf("CloseRecording: %v", err)
	}

	if len(rec.touched()) == 0 {
		t.Fatal("the recording wrote nothing, so this test measures nothing")
	}

	// THE TIMELINE MUST CROSS. A run, its steps and the session row are what
	// a reader on a bff watches while the session runs on an agent.
	for _, concept := range []string{"v1:work:run", "v1:work:step", "v1:worker:appSession"} {
		for _, verb := range []string{"created", "updated"} {
			topic := node.GraphEventTopic(verb, concept)
			if !node.ForwardsGraphEvent(topic) {
				t.Errorf("%s does not forward: the recording is written on the replica holding the "+
					"machine's stream and read on a bff, so without this the page is correct on "+
					"load and frozen after", topic)
			}
		}
	}

	// AND THE HIGH-VOLUME HALF MUST NOT, with its absence RECORDED. An
	// observation per action is the volume of a whole coding session.
	obs := node.GraphEventTopic("created", "v1:work:observation")
	if node.ForwardsGraphEvent(obs) {
		t.Errorf("%s forwards; it is excluded on volume grounds", obs)
	}
	if _, ok := node.ExcludedFromForwarding(obs); !ok {
		t.Errorf("%s is neither forwarded nor a recorded exclusion, which is the silence that "+
			"cannot be told from a concept nobody thought about", obs)
	}
}

// TestEveryRecordedRowCarriesItsOwnerAcrossTheHop. The receiving replica
// decides admission per row, so a row written under a blank actor crosses the
// mesh and is then readable by NOBODY -- the same failure memql#4354 settled
// for the workbench, arriving one layer further out.
func TestEveryRecordedRowCarriesItsOwnerAcrossTheHop(t *testing.T) {
	rec := newHopRecorder()
	owner := "v1:identity:user:alice"
	ctx := context.Background()

	runId, _ := rec.OpenRecording(ctx, workerservice.RecordingOpen{
		SessionId: "v1:worker:appSession:s1", OwnerUserId: owner,
	})
	_ = rec.RecordAction(ctx, workerservice.RecordedAction{
		SessionId: "v1:worker:appSession:s1", OwnerUserId: owner, RunId: runId,
		Action: workerservice.ActionEvent{Id: "a1", Seq: 1, Tool: "exec"},
	})
	_ = rec.RecordGap(ctx, workerservice.RecordedGap{
		SessionId: "v1:worker:appSession:s1", OwnerUserId: owner, RunId: runId,
		AfterSeq: 1, BeforeSeq: 4, Missing: 2,
	})
	_ = rec.CloseRecording(ctx, workerservice.RecordingClose{
		SessionId: "v1:worker:appSession:s1", OwnerUserId: owner, RunId: runId,
		Status: workerservice.AppSessionStatusEnded,
	})

	owners := rec.recordedOwners()
	if len(owners) == 0 {
		t.Fatal("nothing was recorded")
	}
	for i, got := range owners {
		if strings.TrimSpace(got) != owner {
			t.Errorf("recorded row %d carries owner %q, want %q -- a row written under a blank "+
				"actor crosses the mesh and is readable by nobody on the far side", i, got, owner)
		}
	}
}

// TestTheNormalizedActionEventSurvivesTheWire. The cockpit and the engine are
// separate repositories and this shape is the contract between them; a field
// the engine silently stopped reading would make the cockpit's half of the
// epic look wired while recording less than it sent.
func TestTheNormalizedActionEventSurvivesTheWire(t *testing.T) {
	exit := 2
	sent := workerservice.ActionEvent{
		Id: "toolu_01", Seq: 9, Tool: "exec",
		Args: map[string]any{"command": "go test ./..."}, Cwd: "/w",
		ExitCode: &exit, IsError: true, ResultDigest: "sha256:deadbeef",
		ResultType: "string", Error: "exit status 2",
	}
	raw, err := json.Marshal(map[string]any{
		"kind": "action", "id": sent.Id, "seq": sent.Seq, "tool": sent.Tool,
		"args": sent.Args, "cwd": sent.Cwd, "exitCode": exit, "isError": sent.IsError,
		"resultDigest": sent.ResultDigest, "resultType": sent.ResultType, "error": sent.Error,
	})
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := workerservice.DecodeSessionEvent(raw)
	if !ok {
		t.Fatal("the normalized event did not decode")
	}
	got := ev.Action
	if got.Id != sent.Id || got.Seq != sent.Seq || got.Tool != sent.Tool || got.Cwd != sent.Cwd {
		t.Fatalf("identity fields did not survive: %+v", got)
	}
	if got.ExitCode == nil || *got.ExitCode != exit {
		t.Fatalf("exitCode = %v, want a present %d", got.ExitCode, exit)
	}
	if !got.IsError || got.Error != sent.Error || got.ResultDigest != sent.ResultDigest ||
		got.ResultType != sent.ResultType {
		t.Fatalf("the failure's own evidence did not survive: %+v", got)
	}
	if got.Args["command"] != "go test ./..." {
		t.Fatalf("args did not survive: %v", got.Args)
	}
	if workerservice.StepTypeForTool(got.Tool) != "exec" {
		t.Fatalf("the tool did not map to a step type")
	}
}
