package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

type setupAdmissionStore struct {
	mu   sync.Mutex
	row  Candidate
	read chan struct{}
}

func (s *setupAdmissionStore) WorkersForOwner(context.Context, string) ([]Candidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case s.read <- struct{}{}:
	default:
	}
	return []Candidate{s.row}, nil
}
func (*setupAdmissionStore) RoutingPolicyForOwner(context.Context, string) (*Policy, error) {
	return nil, nil
}
func (*setupAdmissionStore) TouchWorkerSelected(context.Context, string, string) error { return nil }

func TestModelSetupAdmissionRereadsAfterWaitingAndReleasesRefusals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s := &setupAdmissionStore{row: Candidate{RegistrationId: "machine", ModelSetup: memqlengine.ModelSetup{Revision: "one", Choices: map[string]string{"conversation": "chat"}}}, read: make(chan struct{}, 2)}
	h := &ForwardHandler{store: s}
	w := &workerservice.Worker{OwnerUserId: "owner"}
	release, err := w.AcquireModelSetup(ctx)
	require.NoError(t, err)
	result := make(chan error, 1)
	go func() {
		_, done, err := h.admitModelSetup(ctx, w, "owner", "machine", &memqlv1.ModelCallStart{Model: "chat"})
		if done != nil {
			done()
		}
		result <- err
	}()
	select {
	case <-s.read:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	s.mu.Lock()
	s.row.ModelSetup = memqlengine.ModelSetup{Revision: "two", Choices: map[string]string{"conversation": "other"}}
	s.mu.Unlock()
	release()
	require.ErrorContains(t, <-result, "no longer selected")
	// A refusal must return the memory slot, and an omitted context is bounded.
	start := &memqlv1.ModelCallStart{Model: "other"}
	_, release, err = h.admitModelSetup(ctx, w, "owner", "machine", start)
	require.NoError(t, err)
	require.EqualValues(t, memqlengine.ModelSetupContextLimit, start.GetParams().GetContextTokens())
	release()
	start.Params.ContextTokens = memqlengine.ModelSetupContextLimit + 1
	_, _, err = h.admitModelSetup(ctx, w, "owner", "machine", start)
	require.ErrorContains(t, err, "32K")
	release, err = w.AcquireModelSetup(ctx)
	require.NoError(t, err)
	release()
}
