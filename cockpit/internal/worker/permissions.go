package worker

import (
	"fmt"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// These values and field numbers match the additive PermissionDecision /
// PermissionStatus / Heartbeat contract in component/grpc/worker.proto.
// The engine pin predates those fields. Merge valid protobuf bytes rather
// than setting unknown fields directly: this also works after a pin bump,
// when the generated message knows the fields. Older servers ignore them.
type permissionDecision uint64

const (
	permissionUnknown permissionDecision = iota
	permissionGranted
	permissionDenied
)

type permissionSnapshot struct {
	accessibility, screenRecording, x11Display permissionDecision
	detail                                     string
}

// measureMacOSPermissions only calls passive APIs in the reporting process.
// No setup results, parent-process probes, TCC database reads or grant requests
// enter this path. A missing probe is unknown; false from a probe is denied.
func measureMacOSPermissions(accessibility, screenRecording func() bool) permissionSnapshot {
	measure := func(probe func() bool) permissionDecision {
		if probe == nil {
			return permissionUnknown
		}
		if probe() {
			return permissionGranted
		}
		return permissionDenied
	}
	return permissionSnapshot{
		accessibility:   measure(accessibility),
		screenRecording: measure(screenRecording),
		detail:          "Passive current-process preflight: AXIsProcessTrusted (Accessibility), CGPreflightScreenCaptureAccess (Screen Recording); unavailable probes are unknown; X11 is not applicable on macOS. If macOS requires a restart after changing a grant, restart the worker to recheck.",
	}
}

func probePermissions() *memqlv1.PermissionStatus {
	return permissionStatus(currentPermissionSnapshot(), time.Now())
}

func permissionStatus(snapshot permissionSnapshot, checkedAt time.Time) *memqlv1.PermissionStatus {
	status := &memqlv1.PermissionStatus{
		Accessibility:   snapshot.accessibility == permissionGranted,
		ScreenRecording: snapshot.screenRecording == permissionGranted,
		X11Display:      snapshot.x11Display == permissionGranted,
		Detail:          snapshot.detail,
	}
	var wire []byte
	for i, state := range []permissionDecision{snapshot.accessibility, snapshot.screenRecording, snapshot.x11Display} {
		wire = protowire.AppendTag(wire, protowire.Number(5+i), protowire.VarintType)
		wire = protowire.AppendVarint(wire, uint64(state))
	}
	wire = appendPermissionMessage(wire, 8, timestamppb.New(checkedAt))
	wire = protowire.AppendTag(wire, 9, protowire.BytesType)
	wire = protowire.AppendString(wire, "worker-process")
	mergePermissionWire(wire, status)
	return status
}

// Every beat contains a full fresh snapshot, including unknown/denied values.
// Message presence (field 8), rather than a true boolean, requests replacement
// on the server. Otherwise revoking a grant would leave a stale green check.
func heartbeatPermissions(beat *memqlv1.Heartbeat, status *memqlv1.PermissionStatus) {
	mergePermissionWire(appendPermissionMessage(nil, 8, status), beat)
}

func appendPermissionMessage(wire []byte, field protowire.Number, message proto.Message) []byte {
	body, err := proto.Marshal(message)
	if err != nil {
		// Only fixed-schema messages constructed above reach this helper.
		panic(fmt.Sprintf("encode worker permission report: %v", err))
	}
	wire = protowire.AppendTag(wire, field, protowire.BytesType)
	return protowire.AppendBytes(wire, body)
}

func mergePermissionWire(wire []byte, message proto.Message) {
	if err := (proto.UnmarshalOptions{Merge: true}).Unmarshal(wire, message); err != nil {
		panic(fmt.Sprintf("decode worker permission report: %v", err))
	}
}
