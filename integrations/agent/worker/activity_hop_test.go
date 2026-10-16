//go:build agent

package worker

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

type activityWrite struct {
	name, owner string
	args        map[string]any
}
type recordingActivityStore struct {
	*fakeStore
	writes chan activityWrite
}

func (s *recordingActivityStore) writeMachineActivity(ctx context.Context, name string, args map[string]any) error {
	ac, _ := auth.AccessFromContext(ctx)
	s.writes <- activityWrite{name: name, owner: ac.UserId, args: args}
	return nil
}
func activityNext(t *testing.T, writes <-chan activityWrite) activityWrite {
	t.Helper()
	select {
	case w := <-writes:
		return w
	case <-time.After(3 * time.Second):
		t.Fatal("activity did not cross the holding replica")
		return activityWrite{}
	}
}
func TestQueuedRunningAndCompletedActivityOnTheOtherReplica(t *testing.T) {
	finish := make(chan struct{})
	defer close(finish)
	h := newHop(t, func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		<-finish
		return okResult(d.GetCallId()), nil
	}, nil)
	recorder := &recordingActivityStore{fakeStore: h.store, writes: make(chan activityWrite, 8)}
	h.link.handler.store = recorder
	w := h.registry.WorkerById("laptop")
	for range 2 {
		if err := w.Acquire(context.Background(), workerservice.CapabilityHeadless); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan Result, 1)
	go func() { result, _ := h.dispatch.Dispatch(authorityCtx(t, h.owner), h.request()); done <- result }()
	queued := activityNext(t, recorder.writes)
	if queued.name != "createMachineActivity" || queued.owner != h.owner || queued.args["workerId"] != "laptop" {
		t.Fatalf("queued receipt: %+v", queued)
	}
	select {
	case got := <-recorder.writes:
		t.Fatalf("reported running while capacity full: %+v", got)
	default:
	}
	w.Release(workerservice.CapabilityHeadless)
	running := activityNext(t, recorder.writes)
	if running.args["status"] != "running" || running.args["activityId"] != queued.args["activityId"] {
		t.Fatalf("running: %+v", running)
	}
	finish <- struct{}{}
	ended := activityNext(t, recorder.writes)
	if ended.args["status"] != "succeeded" || ended.args["activityId"] != queued.args["activityId"] {
		t.Fatalf("completion: %+v", ended)
	}
	select {
	case result := <-done:
		if !result.OK {
			t.Fatalf("dispatch: %+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dispatch stuck")
	}
	w.Release(workerservice.CapabilityHeadless)
}
func TestActivityCancellationStillWritesWithAnUncancelledOwnerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	recorder := &recordingActivityStore{writes: make(chan activityWrite, 4)}
	a := beginMachineActivity(ctx, recorder, testLogger(), "owner", "machine", "model", "chat", "", time.Minute)
	cancel()
	a.interrupted(ctx)
	a.end("succeeded")
	created, ended := activityNext(t, recorder.writes), activityNext(t, recorder.writes)
	if created.owner != "owner" || ended.owner != "owner" || ended.args["status"] != "cancelled" {
		t.Fatalf("wrong owner or lifecycle: %+v %+v", created, ended)
	}
	select {
	case <-recorder.writes:
		t.Fatal("terminal receipt written twice")
	default:
	}
}

type recordingModelActivityStore struct {
	*remoteFleetStore
	*recordingActivityStore
}

func TestModelActivityTracksCapacityAcrossThePlannerAgentHop(t *testing.T) {
	const model = "local-embedder"
	store := &remoteFleetStore{candidates: []Candidate{{RegistrationId: "laptop", ConnectedNodeId: "agent-with-stream", LastSeenAt: time.Now(), Capabilities: []string{workerservice.ModelCapability}, Labels: map[string]string{workerservice.ModelLabel(model): (ModelAttributes{Embeddings: true}).String()}}}}
	recorder := &recordingModelActivityStore{remoteFleetStore: store, recordingActivityStore: &recordingActivityStore{writes: make(chan activityWrite, 8)}}
	registry := workerservice.NewRegistry(testLogger(), time.Now)
	w := &workerservice.Worker{RegistrationId: "laptop", OwnerUserId: "alice", Capabilities: []string{workerservice.ModelCapability}, Labels: store.candidates[0].Labels, Concurrency: map[string]uint32{workerservice.ModelCapability: 1}}
	finish := make(chan struct{})
	defer close(finish)
	w.SetModelCallFunc(func(_ context.Context, req workerservice.ModelCallRequest) (*workerservice.ModelCallHandle, error) {
		handle, _, end := workerservice.NewModelCallLoopback(req, func(string) {})
		go func() {
			<-finish
			end(workerservice.ModelCallOutcome{FinishReason: workerservice.ModelFinishStop, Embeddings: [][]float32{{1, 0}}})
		}()
		return handle, nil
	})
	registry.Add(w)
	if err := w.Acquire(context.Background(), workerservice.ModelCapability); err != nil {
		t.Fatal(err)
	}
	link := &plannerModelLink{handler: NewForwardHandler(registry, recorder, testLogger())}
	link.router = newForwardRouter(link, func() (string, string) { return "planner", "planner" }, testLogger())
	fleet := NewRemoteFleetInference(store, link.router, "planner", testLogger())
	ctx, cancel := context.WithTimeout(authorityCtx(t, "alice"), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := fleet.Call(ctx, memqlengine.FleetCallRequest{ActingUserId: "alice", ModelId: model, Kind: memqlengine.FleetKindEmbedding, EmbeddingInput: []string{"private input must not enter activity"}})
		done <- err
	}()
	queued := activityNext(t, recorder.writes)
	if queued.name != "createMachineActivity" || queued.owner != "alice" || queued.args["kind"] != "model" || queued.args["label"] != model+" · embedding" {
		t.Fatalf("queued model receipt: %+v", queued)
	}
	for _, key := range []string{"prompt", "embeddingInput", "messages", "output"} {
		if _, present := queued.args[key]; present {
			t.Fatalf("activity contains private %s", key)
		}
	}
	select {
	case got := <-recorder.writes:
		t.Fatalf("model reported running before capacity was available: %+v", got)
	default:
	}
	w.Release(workerservice.ModelCapability)
	running := activityNext(t, recorder.writes)
	if running.args["status"] != "running" || running.args["activityId"] != queued.args["activityId"] {
		t.Fatalf("running model receipt: %+v", running)
	}
	finish <- struct{}{}
	ended := activityNext(t, recorder.writes)
	if ended.args["status"] != "succeeded" || ended.args["activityId"] != queued.args["activityId"] || ended.owner != "alice" {
		t.Fatalf("completed model receipt: %+v", ended)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
