package worker

import (
	"errors"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPermissionSnapshotEvidence(t *testing.T) {
	at := time.Date(2026, 9, 15, 18, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		report    *memqlv1.PermissionStatus
		a, screen string
	}{
		{"empty report is unmeasured", &memqlv1.PermissionStatus{}, "unknown", "unknown"},
		{"MVP is unmeasured", &memqlv1.PermissionStatus{Detail: "permission probe not yet implemented (MVP)"}, "unknown", "unknown"},
		{"legacy measured booleans", &memqlv1.PermissionStatus{Accessibility: true}, "granted", "denied"},
		{"explicit unknown clears legacy grant", &memqlv1.PermissionStatus{Accessibility: true, ScreenRecording: true, CheckedAt: timestamppb.New(at), ProbeContext: "worker-process"}, "unknown", "unknown"},
		{"explicit states override booleans", &memqlv1.PermissionStatus{AccessibilityState: 1, ScreenRecordingState: 2, ScreenRecording: true}, "granted", "denied"},
		{"unrecognized enum is unmeasured", &memqlv1.PermissionStatus{AccessibilityState: 99}, "unknown", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := permissionStatusToMap(tc.report)
			if got["accessibility_state"] != tc.a || got["screen_recording_state"] != tc.screen {
				t.Fatalf("states: %#v", got)
			}
			if got["accessibility"] != (tc.a == "granted") || got["screen_recording"] != (tc.screen == "granted") {
				t.Fatalf("legacy flags overstate evidence: %#v", got)
			}
			if tc.report.CheckedAt != nil && (got["checked_at"] != at.Format(time.RFC3339Nano) || got["probe_context"] != "worker-process") {
				t.Fatalf("missing evidence metadata: %#v", got)
			}
		})
	}
	if permissionStatusToMap(nil) != nil {
		t.Fatal("absent report must remain absent")
	}
}

func TestHeartbeatPermissionsRefreshOutsideThrottleAndRetry(t *testing.T) {
	now := time.Now()
	store := &fakeRegistrationStore{}
	s := newHeartbeatTestSession(store, func() time.Time { return now })
	defer s.cancel()
	for i, decision := range []memqlv1.PermissionDecision{1, 2, 0} {
		s.handleHeartbeat(&memqlv1.Heartbeat{Permissions: &memqlv1.PermissionStatus{AccessibilityState: decision, CheckedAt: timestamppb.New(now), ProbeContext: "worker-process"}}, "")
		want := []string{"granted", "denied", "unknown"}[i]
		if len(store.permissionUpdates) != i+1 || store.permissionUpdates[i]["accessibility_state"] != want {
			t.Fatalf("transition %s was not persisted: %#v", want, store.permissionUpdates)
		}
		if s.worker.Permissions["accessibility_state"] != want {
			t.Fatal("live registry did not refresh")
		}
	}
	if len(store.lastSeenAts) != 1 {
		t.Fatal("fixture must be inside the lastSeen throttle")
	}
	s.handleHeartbeat(&memqlv1.Heartbeat{}, "")
	if len(store.permissionUpdates) != 3 || s.worker.Permissions["accessibility_state"] != "unknown" {
		t.Fatal("absent report changed evidence")
	}
	store.permissionErr = errors.New("database unavailable")
	report := &memqlv1.Heartbeat{Permissions: &memqlv1.PermissionStatus{AccessibilityState: 1, ProbeContext: "worker-process"}}
	s.handleHeartbeat(report, "")
	store.permissionErr = nil
	s.handleHeartbeat(report, "")
	if got := store.permissionUpdates[len(store.permissionUpdates)-1]["accessibility_state"]; got != "granted" {
		t.Fatalf("next heartbeat did not retry: %v", got)
	}
}
