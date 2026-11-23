package router

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

type resolveContextKey struct{}
type catalogVisit struct {
	user string
	ctx  context.Context
}
type authenticatedFleet struct {
	mu      sync.Mutex
	visits  []catalogVisit
	catalog func(context.Context, string) ([]memql.FleetModel, error)
}

func (f *authenticatedFleet) Catalog(ctx context.Context, user string) ([]memql.FleetModel, error) {
	f.mu.Lock()
	f.visits = append(f.visits, catalogVisit{user, ctx})
	f.mu.Unlock()
	if f.catalog != nil {
		return f.catalog(ctx, user)
	}
	if user != "alice" {
		return nil, nil
	}
	m := sizedModel("qwen3.8:27b", 27_300_000_000, 131072)
	m.Vision = true
	// An embedder of its own: a model a machine advertises as an embedder
	// answers nothing else (spec 2026-10-29, D3).
	return advertisedOnEveryMachine([]memql.FleetModel{m, sizedEmbedder("qwen3-embedding:0.6b", 32768)}), nil
}
func (*authenticatedFleet) ModelPreference(context.Context, string) ([]string, error) {
	return nil, nil
}
func (*authenticatedFleet) Call(ctx context.Context, req memql.FleetCallRequest) (memql.FleetCallResult, error) {
	if req.ActingUserId != "alice" {
		return memql.FleetCallResult{}, errors.New("wrong fleet caller")
	}
	if req.RegistrationId != common.FleetRegistrationFromContext(ctx) {
		return memql.FleetCallResult{}, errors.New("machine pin lost before dispatch")
	}
	if req.OnDelta != nil {
		req.OnDelta("hello from your machine")
	}
	return memql.FleetCallResult{Content: "hello from your machine"}, nil
}
func authenticatedRouter(t *testing.T, fleet *authenticatedFleet) *Router {
	t.Helper()
	registry := registryOpenToEverybody()
	registry.SetFleetInference(fleet)
	return New(registry, memql.NewPolicyRegistryForTest(map[string][]string{"p": {"fleet:strongest"}}), testRules(t, defaultRule("p")), nil, nil)
}

func TestResolveForPreservesAuthenticatedCatalogContext(t *testing.T) {
	for _, modality := range []airoute.Modality{airoute.ModalityChat, airoute.ModalityStreamingChat, airoute.ModalityTools, airoute.ModalityStreamingTools, airoute.ModalityStructured, airoute.ModalityVision, airoute.ModalityEmbedding} {
		for _, provider := range []string{"fleet:qwen3.8:27b", "fleet:strongest", ""} {
			t.Run(string(modality)+"/"+provider, func(t *testing.T) {
				fleet := &authenticatedFleet{}
				r := authenticatedRouter(t, fleet)
				ctx, cancel := context.WithTimeout(auth.ContextWithUserActor(context.Background(), "alice"), time.Minute)
				defer cancel()
				ctx = context.WithValue(ctx, resolveContextKey{}, "request marker")
				ctx = common.ContextWithFleetRegistration(ctx, "v1:worker:registration:my-machine")
				resolved, err := r.ResolveFor(ctx, ResolveRequest{Level: airoute.LevelStrong, Modality: modality, ExplicitProvider: provider})
				if err != nil {
					t.Fatalf("authenticated user's online model was hidden: %v", err)
				}
				if len(fleet.visits) == 0 {
					t.Fatal("real provider registry never read fleet catalog")
				}
				for _, visit := range fleet.visits {
					if visit.user != "alice" || visit.ctx.Value(resolveContextKey{}) != "request marker" || common.FleetRegistrationFromContext(visit.ctx) != "v1:worker:registration:my-machine" {
						t.Fatalf("catalog lost request authority/context: user=%q marker=%v pin=%q", visit.user, visit.ctx.Value(resolveContextKey{}), common.FleetRegistrationFromContext(visit.ctx))
					}
					if deadline, ok := visit.ctx.Deadline(); !ok {
						t.Fatal("catalog lost request deadline")
					} else if want, _ := ctx.Deadline(); !deadline.Equal(want) {
						t.Fatal("catalog changed deadline")
					}
				}
				if modality == airoute.ModalityStreamingChat {
					streamer, ok := resolved.Client.(common.ChatStreamProvider)
					if !ok {
						t.Fatalf("streaming chat resolved to %T, which cannot stream", resolved.Client)
					}
					chunks, err := streamer.CallChatStream(ctx, []common.ChatMessage{{Role: "user", Content: "hello"}})
					if err != nil {
						t.Fatal(err)
					}
					text := ""
					for chunk := range chunks {
						if chunk.Error != nil {
							t.Fatal(chunk.Error)
						}
						text += chunk.Content
					}
					if text != "hello from your machine" {
						t.Fatalf("streamed %q", text)
					}
				}
			})
		}
	}
}

func TestResolveForStreamingChatReturnsStreamingInterface(t *testing.T) {
	fleet := &authenticatedFleet{}
	r := authenticatedRouter(t, fleet)
	resolved, err := r.ResolveFor(context.Background(), ResolveRequest{UserId: "alice", Level: airoute.LevelStrong, Modality: airoute.ModalityStreamingChat, ExplicitProvider: "fleet:qwen3.8:27b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resolved.Client.(common.ChatStreamProvider); !ok {
		t.Fatalf("streaming chat returned %T", resolved.Client)
	}
}

func TestResolveForCancellationReachesCatalog(t *testing.T) {
	ctx, cancel := context.WithCancel(auth.ContextWithUserActor(context.Background(), "alice"))
	defer cancel()
	fleet := &authenticatedFleet{catalog: func(got context.Context, _ string) ([]memql.FleetModel, error) {
		cancel()
		select {
		case <-got.Done():
			return nil, got.Err()
		case <-time.After(100 * time.Millisecond):
			return nil, errors.New("catalog lost cancellation")
		}
	}}
	_, err := authenticatedRouter(t, fleet).ResolveFor(ctx, ResolveRequest{UserId: "alice", Level: airoute.LevelStrong, Modality: airoute.ModalityChat, ExplicitProvider: "fleet:qwen3.8:27b"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want original cancellation, got %v", err)
	}
}

func TestResolveForExpiredRequestDoesNotReadCatalog(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	fleet := &authenticatedFleet{}
	_, err := authenticatedRouter(t, fleet).ResolveFor(ctx, ResolveRequest{UserId: "alice", Level: airoute.LevelStrong, Modality: airoute.ModalityChat, ExplicitProvider: "fleet:qwen3.8:27b"})
	if !errors.Is(err, context.DeadlineExceeded) || len(fleet.visits) != 0 {
		t.Fatalf("expired resolution err=%v catalog reads=%d", err, len(fleet.visits))
	}
}

// A REQUEST BUILT OUTSIDE THE ENGINE, INSIDE A RUN, IS THE RUN OWNER'S. A run's
// owner is a person recorded on the run row, and the run context carries them
// where no access context does -- a step executing on a replica that did not
// take the request. The engine seam already reads the owner there
// (memql applyCallAttribution); the router fills the same way, so a request
// the agent replier builds for itself is not judged as nobody's -- which, now
// that a call naming nobody is refused the cluster's account (owner decision
// 2026-11-23), would refuse a person's own run.
func TestResolveForReadsTheRunsOwnerWhenNoActorIsOnTheContext(t *testing.T) {
	fleet := &authenticatedFleet{}
	ctx := common.ContextWithRun(context.Background(), common.RunContext{
		RunId: "v1:work:run:r1", OwnerUserId: "alice", StepKey: "reply", Mode: common.RunModeLive,
	})
	resolved, err := authenticatedRouter(t, fleet).ResolveFor(ctx, ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityChat})
	if err != nil {
		t.Fatalf("the run owner's call was refused: %v", err)
	}
	if resolved.Resolution.ProviderName != "fleet:qwen3.8:27b" {
		t.Fatalf("resolved %q, want the run owner's own machine", resolved.Resolution.ProviderName)
	}
	for _, visit := range fleet.visits {
		if visit.user != "alice" {
			t.Fatalf("the catalog was read for %q, want the run's owner", visit.user)
		}
	}
	// An access context still answers first: a run's step under its owner's
	// own session reads the session.
	fleet = &authenticatedFleet{}
	if _, err := authenticatedRouter(t, fleet).ResolveFor(auth.ContextWithUserActor(ctx, "bob"), ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityChat}); err == nil {
		t.Fatal("a call under bob's own context was served alice's machine")
	}
}

func TestResolveForPreservesExplicitUserAndDoesNotWidenAnonymousCatalog(t *testing.T) {
	for _, explicit := range []string{"", "bob"} {
		fleet := &authenticatedFleet{}
		ctx := context.WithValue(context.Background(), resolveContextKey{}, "refusal marker")
		if explicit != "" {
			ctx = auth.ContextWithUserActor(ctx, "alice")
		}
		_, err := authenticatedRouter(t, fleet).ResolveFor(ctx, ResolveRequest{UserId: explicit, Level: airoute.LevelStrong, Modality: airoute.ModalityChat, ExplicitProvider: "fleet:qwen3.8:27b"})
		if err == nil {
			t.Fatalf("user %q was incorrectly served alice's machine", explicit)
		}
		for _, visit := range fleet.visits {
			if visit.ctx.Value(resolveContextKey{}) != "refusal marker" {
				t.Fatal("catalog refusal lookup lost request context")
			}
			if visit.user != explicit {
				t.Fatalf("explicit/system catalog scope changed: %q -> %q", explicit, visit.user)
			}
		}
	}
}
