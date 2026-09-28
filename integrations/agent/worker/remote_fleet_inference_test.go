//go:build agent || planner

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/common"
)

type remoteFleetStore struct{ candidates []Candidate }

func (s *remoteFleetStore) WorkersForOwner(_ context.Context, owner string) ([]Candidate, error) {
	if owner != "alice" {
		return nil, nil
	}
	return s.candidates, nil
}
func (*remoteFleetStore) RoutingPolicyForOwner(context.Context, string) (*Policy, error) {
	return nil, nil
}
func (*remoteFleetStore) TouchWorkerSelected(context.Context, string, string) error { return nil }

// This link serializes the real NodeService envelope and starts the agent's
// handler with no planner context or registry. Only the wire crosses the hop.
type plannerModelLink struct {
	handler *ForwardHandler
	router  *ForwardRouter
	target  string
}

func (l *plannerModelLink) Send(target string, msg *nodev1.NodeClientMessage) bool {
	l.target = target
	if target != "agent-with-stream" {
		return false
	}
	raw, err := proto.Marshal(msg)
	if err != nil {
		return false
	}
	var wire nodev1.NodeClientMessage
	if proto.Unmarshal(raw, &wire) != nil {
		return false
	}
	if request := wire.GetModelForwardRequest(); request != nil {
		go l.handler.HandleForwardedModelCall(context.Background(), request, func(reply *nodev1.NodeServerMessage) error {
			if response := reply.GetModelForwardResponse(); response != nil {
				l.router.DispatchModel(response)
			}
			if delta := reply.GetModelForwardDelta(); delta != nil {
				l.router.DispatchModelDelta(delta)
			}
			return nil
		})
	}
	return true
}

func TestRemoteFleetInferenceFromPlannerCrossesActualModelHop(t *testing.T) {
	const model = "local-writer:27b"
	store := &remoteFleetStore{candidates: []Candidate{{RegistrationId: "laptop", ConnectedNodeId: "agent-with-stream", LastSeenAt: time.Now(),
		Capabilities: []string{workerservice.ModelCapability}, Labels: map[string]string{workerservice.ModelLabel(model): (ModelAttributes{ContextWindow: 131072, StructuredOutput: true}).String()}}}}
	registry := workerservice.NewRegistry(slog.Default(), time.Now)
	worker := &workerservice.Worker{RegistrationId: "laptop", OwnerUserId: "alice", Capabilities: []string{workerservice.ModelCapability}, Labels: store.candidates[0].Labels, Concurrency: map[string]uint32{workerservice.ModelCapability: 1}}
	worker.SetModelCallFunc(func(ctx context.Context, req workerservice.ModelCallRequest) (*workerservice.ModelCallHandle, error) {
		access, ok := auth.AccessFromContext(ctx)
		if !ok || access.UserId != "alice" {
			return nil, fmt.Errorf("planner authority did not reach the receiving agent")
		}
		if req.Model != model || len(req.ResponseFormatSchema) == 0 || req.Params.ContextTokens < 8192 {
			return nil, fmt.Errorf("model request lost its schema or context floor: %+v", req)
		}
		// The planner's triage is a `fast` call, and the level is what the
		// cockpit reads to run it with the model's hidden thinking off.
		if req.Level != "fast" {
			return nil, fmt.Errorf("model request lost its level between the planner and the agent: got %q", req.Level)
		}
		handle, emit, finish := workerservice.NewModelCallLoopback(req, func(string) {})
		go func() {
			emit(workerservice.ModelCallDelta{Seq: 1, Content: `{"draft":"local result"}`})
			finish(workerservice.ModelCallOutcome{FinishReason: workerservice.ModelFinishStop, Usage: workerservice.ModelCallUsage{InputTokens: 13, OutputTokens: 7, Model: model, Known: true}})
		}()
		return handle, nil
	})
	registry.Add(worker)
	link := &plannerModelLink{handler: NewForwardHandler(registry, store, slog.Default())}
	link.router = newForwardRouter(link, func() (string, string) { return "planner", "planner" }, slog.Default())
	fleet := NewRemoteFleetInference(store, link.router, "planner", slog.Default())
	if fleet.registry != nil {
		t.Fatal("planner adapter acquired a local worker registry")
	}
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
	result, err := fleet.Call(ctx, memqlengine.FleetCallRequest{ActingUserId: "alice", ModelId: model, Kind: memqlengine.FleetKindChat, ContextTokens: 8192, Level: "fast",
		Messages: []common.ChatMessage{{Role: "user", Content: "Write a draft"}}, Schema: &common.StructuredSchema{Name: "draft", Schema: json.RawMessage(`{"type":"object"}`), Strict: true}})
	if err != nil {
		t.Fatal(err)
	}
	if link.target != "agent-with-stream" || result.Content != `{"draft":"local result"}` || result.Usage.Model != model || result.Usage.InputTokens != 13 || result.Usage.OutputTokens != 7 {
		t.Fatalf("planner's forwarded inference lost output or provenance: %+v target=%q", result, link.target)
	}
}
