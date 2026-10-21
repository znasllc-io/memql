//go:build agent || planner

package worker

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// capacityRemoteStore is the planner's view of the fleet: rows only, plus the
// admission evidence. It holds no stream for any machine.
type capacityRemoteStore struct {
	*remoteFleetStore
	needs map[string]uint64
}

func (s capacityRemoteStore) ModelMemoryNeeds(context.Context) (map[string]uint64, error) {
	return s.needs, nil
}
func (s capacityRemoteStore) MachineHealth(context.Context, time.Time) (memqlengine.FleetHealthIndex, error) {
	return memqlengine.FleetHealthIndex{}, nil
}

// THE HOP. The planner holds no stream; the machine's stream is on another
// agent. Admission is decided on the planner from the registration row's
// capacity report, so a machine with no room is refused before anything is
// forwarded, and one holding the model is reached across the hop.
func TestAdmissionIsDecidedFromTheRowOnAReplicaWithoutTheStream(t *testing.T) {
	const model = "qwen3.8:27b"
	now := time.Now()
	row := Candidate{RegistrationId: "laptop", ConnectedNodeId: "agent-with-stream", LastSeenAt: now,
		Capabilities: []string{workerservice.ModelCapability}, Labels: map[string]string{workerservice.ModelLabel(model): (ModelAttributes{Embeddings: true}).String()},
		Capacity: reportedCapacity(now, 9*gb)}
	planner := capacityRemoteStore{remoteFleetStore: &remoteFleetStore{candidates: []Candidate{row}},
		needs: memqlengine.FleetMemoryNeeds([]memqlengine.CatalogProfile{{ModelId: model, MemoryNeedBytes: int64(20 * gb)}})}
	registry := workerservice.NewRegistry(slog.Default(), time.Now)
	worker := &workerservice.Worker{RegistrationId: "laptop", OwnerUserId: "alice", Capabilities: []string{workerservice.ModelCapability}, Labels: row.Labels, Concurrency: map[string]uint32{workerservice.ModelCapability: 1}}
	calls := 0
	worker.SetModelCallFunc(func(ctx context.Context, req workerservice.ModelCallRequest) (*workerservice.ModelCallHandle, error) {
		calls++
		handle, _, finish := workerservice.NewModelCallLoopback(req, func(string) {})
		go finish(workerservice.ModelCallOutcome{FinishReason: workerservice.ModelFinishStop, Embeddings: [][]float32{{1}}})
		return handle, nil
	})
	registry.Add(worker)
	link := &plannerModelLink{handler: NewForwardHandler(registry, &remoteFleetStore{candidates: []Candidate{row}}, slog.Default())}
	link.router = newForwardRouter(link, func() (string, string) { return "planner", "planner" }, slog.Default())
	fleet := NewRemoteFleetInference(planner, link.router, "planner", slog.Default())
	authority, err := auth.ForwardedAuthorityForUser(&auth.AccessContext{UserId: "alice", Role: auth.RoleWriter}, "", "", time.Time{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	access, err := auth.VerifyForwardedAuthority(authority, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(auth.BindForwardedContext(context.Background(), authority.Principal().Claims, access, authority), 5*time.Second)
	defer cancel()
	request := memqlengine.FleetCallRequest{ActingUserId: "alice", ModelId: model, Kind: memqlengine.FleetKindEmbedding, EmbeddingInput: []string{"x"}}

	_, err = fleet.Call(ctx, request)
	var unavailable *memqlengine.FleetUnavailable
	if !errors.As(err, &unavailable) || calls != 0 || link.target != "" {
		t.Fatalf("a machine without room was tried: err=%v calls=%d target=%q", err, calls, link.target)
	}
	want := "not enough free memory to load " + model + " (needs 22 GB with headroom, 9 GB free)"
	if got := unavailable.Considered["laptop"]; got != want {
		t.Fatalf("considered[laptop] = %q, want %q", got, want)
	}

	// The next report says the model is loaded: the same planner, still holding
	// no stream, now admits it and the call crosses to the agent that does.
	planner.candidates[0].Capacity = reportedCapacity(now, 1*gb, memqlengine.FleetLoadedModel{Model: model, SizeBytes: 20 * gb})
	if _, err := fleet.Call(ctx, request); err != nil || calls != 1 || link.target != "agent-with-stream" {
		t.Fatalf("a machine holding the model was not reached across the hop: err=%v calls=%d target=%q", err, calls, link.target)
	}
}
