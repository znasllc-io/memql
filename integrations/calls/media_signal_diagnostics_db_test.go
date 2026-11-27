package calls

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Delay only reads and respect the original context. This models sequential
// authority-read latency without changing any runtime gate or deadline.
type mediaSignalDelayedReadEngine struct {
	Engine
	reads atomic.Int32
}

func (e *mediaSignalDelayedReadEngine) Execute(ctx context.Context, query string) (any, error) {
	if strings.HasPrefix(query, "query ") {
		reads := e.reads.Add(1)
		delay := 45 * time.Millisecond
		if reads >= 3 {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return e.Engine.Execute(ctx, query)
}
func TestMediaSignalDatabaseSlowReadsAreDiagnosedWithoutForwardingOrExtendingAuthority(t *testing.T) {
	cw, _, start := mediaSignalFixture(t, "signal-read-diagnostics")
	if cw == nil {
		return
	}
	var output bytes.Buffer
	cw.svc.logger = slog.New(slog.NewJSONHandler(&output, nil))
	ctx := context.Background()
	lease, err := cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, err)
	cw.media.setPresent(start.Call.ID, person(lease.Identity))
	require.NoError(t, lease.Confirm(ctx))
	original := cw.svc.store.engine
	delayed := &mediaSignalDelayedReadEngine{Engine: original}
	cw.svc.store.engine = delayed
	bounded, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	forwarded := false
	err = lease.Forward(bounded, func(context.Context) error { forwarded = true; return nil })
	cancel()
	cw.svc.store.engine = original
	require.Error(t, err)
	require.False(t, forwarded)
	require.GreaterOrEqual(t, delayed.reads.Load(), int32(1))
	require.Empty(t, output.String(), "diagnostics must not log each failed read/frame")
	// Once independent local read latency ends, the original authority path
	// still works and renews only through its existing native lease rules.
	cw.clock.Advance(15 * time.Second)
	require.NoError(t, lease.Forward(ctx, func(context.Context) error { return nil }))
	require.NoError(t, lease.Close(ctx, true))
	lease.Finish()
	lease.Finish()
	require.Equal(t, 1, strings.Count(output.String(), "\n"))
	require.NotContains(t, output.String(), start.Join.Pass)
	require.NotContains(t, output.String(), start.Call.ID)
	require.NotContains(t, output.String(), lease.Identity)
	var receipt struct {
		FirstFailurePhase string             `json:"first_failure_phase"`
		FirstFailure      string             `json:"first_failure"`
		Phases            []mediaSignalPhase `json:"phases"`
	}
	require.NoError(t, json.Unmarshal(output.Bytes(), &receipt))
	require.Equal(t, "native_read", receipt.FirstFailurePhase)
	require.Equal(t, "deadline", receipt.FirstFailure)
	found := false
	for _, phase := range receipt.Phases {
		if phase.Phase == "lease_renewed" {
			found = phase.Count > 0
		}
	}
	require.True(t, found, "successful renewal remains separately observable")
}

func TestMediaSignalDatabaseHeldGateIsDiagnosedWithoutForwarding(t *testing.T) {
	cw, _, start := mediaSignalFixture(t, "signal-gate-diagnostics")
	if cw == nil {
		return
	}
	var output bytes.Buffer
	cw.svc.logger = slog.New(slog.NewJSONHandler(&output, nil))
	ctx := context.Background()
	lease, err := cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, err)
	cw.media.setPresent(start.Call.ID, person(lease.Identity))
	require.NoError(t, lease.Confirm(ctx))
	release, err := cw.svc.gates(ctx, roomGate(start.Call.ID))
	require.NoError(t, err)
	bounded, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	forwarded := false
	err = lease.Forward(bounded, func(context.Context) error { forwarded = true; return nil })
	cancel()
	release()
	require.Error(t, err)
	require.False(t, forwarded)
	require.Empty(t, output.String())
	require.NoError(t, lease.Close(ctx, true))
	lease.Finish()
	var receipt struct {
		FirstFailurePhase string             `json:"first_failure_phase"`
		Phases            []mediaSignalPhase `json:"phases"`
	}
	require.NoError(t, json.Unmarshal(output.Bytes(), &receipt))
	require.Equal(t, "gate_acquire", receipt.FirstFailurePhase)
	found := false
	for _, phase := range receipt.Phases {
		if phase.Phase == "gate_acquire" && phase.Failures > 0 {
			found = phase.LastFailure == "deadline" || phase.LastFailure == "gate_busy"
		}
	}
	require.True(t, found)
	require.NotContains(t, output.String(), start.Call.ID)
	require.NotContains(t, output.String(), start.Join.Pass)
}
