package app

import (
	"context"
	"errors"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
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
