//go:build identity

package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity"
)

// retryGuardStore is a BootstrapGuardStore whose IsClusterBootstrappedE
// fails the first failTimes calls (simulating the #1858 storm) then
// succeeds, returning bootstrapped state thereafter. HasOwnerUser /
// ReadClusterSettings are inert (fresh cluster) so the post-retry
// action is deterministic.
type retryGuardStore struct {
	failTimes int
	calls     int
}

func (s *retryGuardStore) IsClusterBootstrappedE(_ context.Context) (bool, error) {
	s.calls++
	if s.calls <= s.failTimes {
		return false, errors.New("53300 connection storm")
	}
	return false, nil
}
func (s *retryGuardStore) HasOwnerUser(_ context.Context) (bool, error) { return false, nil }
func (s *retryGuardStore) ReadClusterSettings(_ context.Context) (*identity.ClusterSettingsRow, error) {
	return nil, nil
}

func newTestApp() *App {
	return &App{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestEvaluateAutoBootstrapWithRetry(t *testing.T) {
	// Keep the test fast — the retry gap is a package var.
	orig := autoBootstrapRetryDelay
	autoBootstrapRetryDelay = time.Millisecond
	defer func() { autoBootstrapRetryDelay = orig }()

	t.Run("rides through a transient error then decides Send", func(t *testing.T) {
		a := newTestApp()
		store := &retryGuardStore{failTimes: autoBootstrapReadRetries - 1}
		action, err := a.evaluateAutoBootstrapWithRetry(context.Background(), store)
		if err != nil {
			t.Fatalf("expected retry to ride through transient error, got %v", err)
		}
		if action != identity.BootstrapActionSend {
			t.Fatalf("want Send after recovery, got %v", action)
		}
	})

	t.Run("persistent error fails safe (no email)", func(t *testing.T) {
		a := newTestApp()
		// Fail more than the retry budget -> the guard gives up and the
		// caller must NOT send the email.
		store := &retryGuardStore{failTimes: autoBootstrapReadRetries + 5}
		action, err := a.evaluateAutoBootstrapWithRetry(context.Background(), store)
		if err == nil {
			t.Fatal("expected persistent error to propagate (fail-safe)")
		}
		if action != identity.BootstrapActionSkip {
			t.Fatalf("want Skip on persistent error, got %v", action)
		}
	})

	t.Run("context cancellation aborts retry", func(t *testing.T) {
		a := newTestApp()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		store := &retryGuardStore{failTimes: autoBootstrapReadRetries + 5}
		if _, err := a.evaluateAutoBootstrapWithRetry(ctx, store); err == nil {
			t.Fatal("expected error when context is cancelled mid-retry")
		}
	})
}
