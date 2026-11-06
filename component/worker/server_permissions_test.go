package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

// A permission report is written when its evidence changes, never for its
// checked_at alone. The cockpit probes on every beat and stamps each report
// fresh, so writing every report appended a registration version per machine
// per beat. The baseline is per STREAM and seeded from what the register
// wrote, which is what keeps a reconnect -- here onto a second replica sharing
// the same rows -- to at most one write.
func TestHeartbeatPermissionsAreWrittenOnlyWhenTheEvidenceChanges(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := &fakeRegistrationStore{}
	identity := &WorkerIdentity{IdentityId: "ident-1", OwnerUserId: "user-1", Active: true}
	report := func(accessibility memqlv1.PermissionDecision, detail string, at time.Time) *memqlv1.PermissionStatus {
		return &memqlv1.PermissionStatus{AccessibilityState: accessibility, ScreenRecordingState: 2, Detail: detail, CheckedAt: timestamppb.New(at), ProbeContext: "worker-process"}
	}
	beats := func(s *streamSession, status func(time.Time) *memqlv1.PermissionStatus, n int) {
		for i := 0; i < n; i++ {
			now = now.Add(15 * time.Second)
			s.handleHeartbeat(&memqlv1.Heartbeat{Permissions: status(now)}, "")
		}
	}
	granted := func(at time.Time) *memqlv1.PermissionStatus { return report(1, "passive preflight", at) }
	denied := func(at time.Time) *memqlv1.PermissionStatus { return report(2, "passive preflight", at) }

	register := registerMsg([]string{CapabilityHeadless}, "")
	register.Permissions = granted(now)
	first, err := newServer(logger, store, NewRegistry(nil, clock), nil, clock, testNodeId).
		admitRegistration(context.Background(), newFakeCockpitStream(), identity, register, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("admitRegistration: %v", err)
	}
	if len(store.created) != 1 || store.created[0].Permissions["accessibility_state"] != "granted" {
		t.Fatalf("the register did not write the snapshot it carried: %#v", store.created)
	}

	beats(first, granted, 5)
	if len(store.permissionUpdates) != 0 {
		t.Fatalf("a fresh checked_at alone was written %d times; the evidence had not moved", len(store.permissionUpdates))
	}
	if got := first.worker.Permissions[permissionCheckedAtKey]; got != now.Format(time.RFC3339Nano) {
		t.Fatalf("the live registry must still take every report; checked_at = %v", got)
	}

	beats(first, denied, 3)
	if len(store.permissionUpdates) != 1 || store.permissionUpdates[0]["accessibility_state"] != "denied" {
		t.Fatalf("a revoked grant must be written once: %#v", store.permissionUpdates)
	}
	beats(first, func(at time.Time) *memqlv1.PermissionStatus { return report(2, "restart the worker to recheck", at) }, 2)
	if len(store.permissionUpdates) != 2 || store.permissionUpdates[1]["detail"] != "restart the worker to recheck" {
		t.Fatalf("a changed detail is evidence and must be written once: %#v", store.permissionUpdates)
	}
	first.close(nil)

	// THE RECONNECT, on another replica. Its Register carries the machine's
	// opening report, which still says granted, and the refresh writes it; the
	// first beat's denial differs from what that register wrote, so it lands
	// once and its repeats do not.
	store.existing = &RegistrationRow{ID: first.worker.RegistrationId, IdentityId: "ident-1", OwnerUserId: "user-1", Name: "test-worker"}
	reconnect := registerMsg([]string{CapabilityHeadless}, "")
	reconnect.Permissions = granted(now)
	second, err := newServer(logger, store, NewRegistry(nil, clock), nil, clock, "agent-8").
		admitRegistration(context.Background(), newFakeCockpitStream(), identity, reconnect, "10.0.0.2:1234")
	if err != nil {
		t.Fatalf("admitRegistration on the second replica: %v", err)
	}
	defer second.close(nil)
	if len(store.refreshed) != 1 || store.refreshed[0].Permissions["accessibility_state"] != "granted" {
		t.Fatalf("the reconnect did not refresh the row with its own report: %#v", store.refreshed)
	}
	beats(second, func(at time.Time) *memqlv1.PermissionStatus { return report(2, "restart the worker to recheck", at) }, 4)
	if len(store.permissionUpdates) != 3 || store.permissionUpdates[2]["accessibility_state"] != "denied" {
		t.Fatalf("a reconnect must write a report that differs from its register exactly once: %#v", store.permissionUpdates)
	}
}

func TestSamePermissionDecisionIgnoresOnlyCheckedAt(t *testing.T) {
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	base := permissionStatusToMap(&memqlv1.PermissionStatus{AccessibilityState: 1, CheckedAt: timestamppb.New(at), ProbeContext: "worker-process"})
	later := permissionStatusToMap(&memqlv1.PermissionStatus{AccessibilityState: 1, CheckedAt: timestamppb.New(at.Add(time.Hour)), ProbeContext: "worker-process"})
	if !samePermissionDecision(base, later) {
		t.Fatal("two reports differing only in checked_at must be the same evidence")
	}
	for name, other := range map[string]*memqlv1.PermissionStatus{
		"decision":      {AccessibilityState: 2, CheckedAt: timestamppb.New(at), ProbeContext: "worker-process"},
		"detail":        {AccessibilityState: 1, Detail: "restart to recheck", CheckedAt: timestamppb.New(at), ProbeContext: "worker-process"},
		"probe context": {AccessibilityState: 1, CheckedAt: timestamppb.New(at), ProbeContext: "installer"},
		"legacy report": {Accessibility: true},
	} {
		if samePermissionDecision(base, permissionStatusToMap(other)) {
			t.Errorf("a report with a different %s must be written", name)
		}
	}
	if samePermissionDecision(nil, base) || samePermissionDecision(base, nil) {
		t.Fatal("a stream that has written nothing must write its first report")
	}
}
