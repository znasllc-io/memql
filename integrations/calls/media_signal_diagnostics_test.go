package calls

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func TestMediaSignalDiagnosticsClosedProjectionAndSingleTerminalReceipt(t *testing.T) {
	var output bytes.Buffer
	trace := &mediaSignalTrace{logger: slog.New(slog.NewJSONHandler(&output, nil)), started: time.Now()}
	secret := "PRIVATE_TOKEN_URL_QUERY_ID_SENTINEL"
	trace.record(secret, time.Hour, errors.New(secret))
	trace.record("native_read", 300*time.Millisecond, fmt.Errorf("%s: %w", secret, context.DeadlineExceeded))
	trace.record("close", time.Second, context.Canceled)
	trace.record("provider_write", time.Millisecond, callrules.Refuse(callrules.ErrForbidden, secret, secret))
	trace.finish()
	trace.record("forward", time.Second, errors.New(secret))
	trace.finish()
	require.NotContains(t, output.String(), secret)
	require.Equal(t, 1, strings.Count(output.String(), "\n"))
	var receipt map[string]any
	require.NoError(t, json.Unmarshal(output.Bytes(), &receipt))
	require.Equal(t, "media_signal_terminal", receipt["event"])
	require.Equal(t, "native_read", receipt["first_failure_phase"])
	require.Equal(t, "deadline", receipt["first_failure"])
	require.True(t, receipt["slow"].(bool))
	rows := receipt["phases"].([]any)
	require.Len(t, rows, 3)
	require.Equal(t, "operation_failed", trace.phases["provider_write"].LastFailure)
}

func TestMediaSignalDiagnosticsIgnoresPassiveWaitAndBoundsCounters(t *testing.T) {
	var output bytes.Buffer
	trace := &mediaSignalTrace{logger: slog.New(slog.NewJSONHandler(&output, nil)), started: time.Now()}
	trace.record("client_read", time.Hour, nil)
	trace.record("provider_read", time.Hour, nil)
	trace.finish()
	require.Empty(t, output.String(), "passive wait alone must not produce a slow/failure warning")
	trace = &mediaSignalTrace{started: time.Now()}
	for range 65540 {
		trace.record("native_read", 48*time.Hour, errors.New("private"))
	}
	require.Equal(t, 65535, trace.phases["native_read"].Count)
	require.Equal(t, 65535, trace.phases["native_read"].Failures)
	require.Equal(t, int64(24*time.Hour/time.Millisecond), trace.phases["native_read"].TotalMS)
	require.Equal(t, int64(24*time.Hour/time.Millisecond), trace.phases["native_read"].MaxMS)
	var absent *mediaSignalTrace
	require.NotPanics(t, func() { absent.record("forward", time.Second, errors.New("ignored")); absent.finish() })
}

func TestMediaSignalDiagnosticsBudgetLabelsKeepFirstFailureClosed(t *testing.T) {
	reasons := []string{"budget_control_size", "budget_control_rate", "budget_frame_rate", "budget_frame_bytes", "budget_request_decode", "budget_request_unknown", "budget_add_track_shape", "budget_add_track_rate", "budget_add_track_cid_conflict", "budget_add_track_sid_unknown", "budget_add_track_count", "budget_add_track_source", "budget_offer_shape", "budget_offer_pending", "budget_answer_shape", "budget_sync_state_shape", "budget_update_layers_shape", "budget_trickle_shape"}
	for _, reason := range reasons {
		require.Equal(t, reason, mediaSignalFailure(callrules.Refuse(callrules.ErrForbidden, reason, "PRIVATE_DETAIL")))
		require.Equal(t, "operation_failed", mediaSignalFailure(callrules.Refuse(callrules.ErrForbidden, reason+"_PRIVATE_ID", "PRIVATE_DETAIL")))
	}
	var output bytes.Buffer
	trace := &mediaSignalTrace{logger: slog.New(slog.NewJSONHandler(&output, nil)), started: time.Now()}
	trace.record("client_budget", 0, callrules.Refuse(callrules.ErrForbidden, "budget_offer_pending", "PRIVATE_SDP"))
	done := make(chan struct{}, 8)
	for range 8 {
		go func() { trace.record("close", time.Millisecond, context.Canceled); trace.finish(); done <- struct{}{} }()
	}
	for range 8 {
		<-done
	}
	require.NotContains(t, output.String(), "PRIVATE")
	require.Equal(t, 1, strings.Count(output.String(), "\n"))
	var receipt map[string]any
	require.NoError(t, json.Unmarshal(output.Bytes(), &receipt))
	require.Equal(t, "client_budget", receipt["first_failure_phase"])
	require.Equal(t, "budget_offer_pending", receipt["first_failure"])
}
