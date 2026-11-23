package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/safety"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// recordingStructured is a structured client that remembers the context of
// the last call made on it.
type recordingStructured struct {
	ctx context.Context
}

func (r *recordingStructured) CallChatStructured(ctx context.Context, _ []common.ChatMessage, _ common.StructuredSchema) (string, error) {
	r.ctx = ctx
	return `{}`, nil
}

// THE SAFETY CHECK AND THE HEALER ARE THE CLUSTER'S OWN JOBS, SAID (owner
// decision 2026-11-23). Each resolves its model once, when the node starts,
// and serves every later caller. Resolved for nobody, its model would be
// refused the cluster's vendor account; and judged at the call by the caller
// it screens, it would be the screened person's to pay for. So the resolution
// and every call carry the job's name, and the cluster's account pays for the
// job as the cluster's own work, whoever it runs for.
func TestAClusterJobResolvesAndCallsAsTheJob(t *testing.T) {
	for _, job := range []string{safetyClassifierJob, workHealerJob} {
		t.Run(job, func(t *testing.T) {
			var resolvedUnder context.Context
			inner := &recordingStructured{}
			provider, err := resolveForClusterJob(job, healingPatchResolveRequest(),
				func(ctx context.Context, _ airoute.ResolveRequest) (common.ChatStructuredProvider, error) {
					resolvedUnder = ctx
					return inner, nil
				})
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := auth.ClusterWorkFromContext(resolvedUnder); !ok || got != job {
				t.Fatalf("resolved under job %q, %v; want %q", got, ok, job)
			}
			// A context that names nobody: a resolution reads the acting id
			// off the context, and a call that names a person is theirs
			// whatever the stamp says.
			if _, ok := auth.AccessFromContext(resolvedUnder); ok {
				t.Fatal("the job resolved under somebody's access context")
			}
			if _, ok := common.RunFromContext(resolvedUnder); ok {
				t.Fatal("the job resolved inside a run, whose owner the resolution would read")
			}

			alice := auth.ContextWithUserActor(context.Background(), "alice")
			if _, err := provider.CallChatStructured(alice, nil, common.StructuredSchema{}); err != nil {
				t.Fatal(err)
			}
			if !auth.ActsForTheCluster(inner.ctx, "") {
				t.Fatal("a call the job made for a person was not the cluster's own work")
			}
			// The call keeps everything else its caller put on its context.
			if ac, ok := auth.AccessFromContext(inner.ctx); !ok || ac.UserId != "alice" {
				t.Fatal("the call lost its caller's context")
			}
		})
	}
}

// safetyJobModel is the router and the model in one, remembering the context
// of the resolution and of each call.
type safetyJobModel struct {
	resolved chan context.Context
	called   chan context.Context
}

func (m *safetyJobModel) ResolveFor(ctx context.Context, _ airoute.ResolveRequest) (memql.ResolvedProvider, error) {
	m.resolved <- ctx
	return memql.ResolvedProvider{Client: m}, nil
}

func (m *safetyJobModel) CallChatStructured(ctx context.Context, _ []common.ChatMessage, _ common.StructuredSchema) (string, error) {
	m.called <- ctx
	return "", errors.New("fixture: no verdict")
}

// The safety check the node actually builds is the cluster's job: its model is
// resolved under the job's name, and a check it runs for a person's command
// reaches the model as the job, not as that person.
func TestTheSafetyCheckIsWiredAsTheClustersJob(t *testing.T) {
	t.Setenv(EnvSafetyLLMProvider, "fixture")
	model := &safetyJobModel{resolved: make(chan context.Context, 1), called: make(chan context.Context, 1)}
	engine := &memql.MemQLEngine{}
	engine.SetAIResolver(model)
	a := &App{engine: engine, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	classifier := a.buildSafetyClassifier()
	select {
	case ctx := <-model.resolved:
		if job, ok := auth.ClusterWorkFromContext(ctx); !ok || job != safetyClassifierJob {
			t.Fatalf("the check's model was resolved under job %q, %v; want %q", job, ok, safetyClassifierJob)
		}
	default:
		t.Fatal("the check resolved no model: the layer is off")
	}

	alice := auth.ContextWithUserActor(context.Background(), "alice")
	_, _ = classifier.Classify(alice, safety.ActionDescriptor{})
	select {
	case ctx := <-model.called:
		if !auth.ActsForTheCluster(ctx, "") {
			t.Fatal("the check it ran for alice's command reached the model as hers, not the cluster's")
		}
	default:
		t.Fatal("the check never reached its model")
	}
}

// A job whose model cannot be resolved is off, as before: the error comes back
// and no client is handed out.
func TestAClusterJobWithNoModelIsOff(t *testing.T) {
	want := errors.New("no structured-output model is reachable")
	provider, err := resolveForClusterJob(safetyClassifierJob, healingPatchResolveRequest(),
		func(context.Context, airoute.ResolveRequest) (common.ChatStructuredProvider, error) {
			return nil, want
		})
	if !errors.Is(err, want) || provider != nil {
		t.Fatalf("provider = %v, err = %v; want no client and the resolution's error", provider, err)
	}
}
