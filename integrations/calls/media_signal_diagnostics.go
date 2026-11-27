package calls

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

// One bounded terminal receipt per authenticated connection. No identity,
// credential, URL, query, SDP or error text is accepted by the projection.
// This observes existing deadlines; it grants no authority and changes no wait.
type mediaSignalTrace struct {
	mu                              sync.Mutex
	logger                          *slog.Logger
	started                         time.Time
	finished, failed, slow          bool
	phases                          map[string]*mediaSignalPhase
	firstFailurePhase, firstFailure string
	firstFailureOffsetMS            int64
}
type mediaSignalPhase struct {
	Phase       string `json:"phase"`
	Count       int    `json:"count"`
	TotalMS     int64  `json:"total_ms"`
	MaxMS       int64  `json:"max_ms"`
	Failures    int    `json:"failures"`
	LastFailure string `json:"last_failure,omitempty"`
}
type mediaSignalTraceKey struct{}

func mediaSignalPhaseAllowed(phase string) bool {
	switch phase {
	case "begin", "authenticate", "forward", "confirm", "close", "native_read", "native_write",
		"gate_keys_before", "gate_acquire", "gate_room_read", "gate_keys_after", "authority", "grant",
		"journal_read", "provider_write", "lease_renewed", "upstream_dial", "upstream_initial",
		"upstream_resume", "downstream_upgrade", "downstream_initial", "client_frame", "client_heartbeat",
		"client_read", "provider_read", "provider_frame", "downstream_write", "client_budget", "provider_budget":
		return true
	}
	return false
}
func mediaSignalFailure(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case memql.IsWriteGateBusy(err):
		return "gate_busy"
	case errors.Is(err, memql.ErrRowVersionChanged):
		return "row_changed"
	}
	switch reason := callrules.ReasonOf(err); reason {
	case "media_barrier_unavailable", "media_connection_expired", "media_cleanup_pending", "media_connection_limit",
		"media_admission_invalid", "media_admission_closed", "media_session_closed", "media_account_closed",
		"media_permissions_changed", "media_permissions_pending", "media_server_unavailable", "media_authority_changed",
		"media_connection_closed", "media_processing_lease_closed",
		"budget_control_size", "budget_control_rate", "budget_frame_rate", "budget_frame_bytes",
		"budget_request_decode", "budget_request_unknown", "budget_add_track_shape", "budget_add_track_rate",
		"budget_add_track_cid_conflict", "budget_add_track_sid_unknown", "budget_add_track_count", "budget_add_track_source",
		"budget_offer_shape", "budget_offer_pending", "budget_answer_shape", "budget_sync_state_shape",
		"budget_update_layers_shape", "budget_trickle_shape":
		return reason
	}
	return "operation_failed"
}
func (t *mediaSignalTrace) record(phase string, elapsed time.Duration, err error) {
	if t == nil || !mediaSignalPhaseAllowed(phase) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	if t.phases == nil {
		t.phases = make(map[string]*mediaSignalPhase)
	}
	v := t.phases[phase]
	if v == nil {
		v = &mediaSignalPhase{Phase: phase}
		t.phases[phase] = v
	}
	ms := min(max(elapsed.Milliseconds(), 0), int64(24*time.Hour/time.Millisecond))
	v.Count = min(v.Count+1, 65535)
	v.TotalMS = min(v.TotalMS+ms, int64(24*time.Hour/time.Millisecond))
	v.MaxMS = max(v.MaxMS, ms)
	if err != nil {
		v.Failures = min(v.Failures+1, 65535)
		v.LastFailure = mediaSignalFailure(err)
		t.failed = true
		if t.firstFailurePhase == "" {
			t.firstFailurePhase, t.firstFailure = phase, v.LastFailure
			t.firstFailureOffsetMS = min(max(time.Since(t.started).Milliseconds(), 0), int64(24*time.Hour/time.Millisecond))
		}
	}
	// Waiting for a peer to send its next frame is not active server work.
	if phase != "client_read" && phase != "provider_read" {
		t.slow = t.slow || elapsed >= 250*time.Millisecond
	}
}
func (t *mediaSignalTrace) finish() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	t.finished = true
	if t.logger == nil || (!t.failed && !t.slow) {
		return
	}
	rows := make([]mediaSignalPhase, 0, len(t.phases))
	for _, row := range t.phases {
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Phase < rows[j].Phase })
	t.logger.Warn("calls media signalling terminal receipt", "component", "calls", "event", "media_signal_terminal",
		"duration_ms", min(max(time.Since(t.started).Milliseconds(), 0), int64(24*time.Hour/time.Millisecond)),
		"failed", t.failed, "slow", t.slow,
		"first_failure_phase", t.firstFailurePhase, "first_failure", t.firstFailure,
		"first_failure_offset_ms", t.firstFailureOffsetMS, "phases", rows)
}
func mediaSignalTraceContext(ctx context.Context, trace *mediaSignalTrace) context.Context {
	if trace == nil {
		return ctx
	}
	return context.WithValue(ctx, mediaSignalTraceKey{}, trace)
}
func mediaSignalTraceFrom(ctx context.Context) *mediaSignalTrace {
	t, _ := ctx.Value(mediaSignalTraceKey{}).(*mediaSignalTrace)
	return t
}
func mediaSignalMeasure(ctx context.Context, phase string, fn func() error) error {
	started := time.Now()
	err := fn()
	mediaSignalTraceFrom(ctx).record(phase, time.Since(started), err)
	return err
}
