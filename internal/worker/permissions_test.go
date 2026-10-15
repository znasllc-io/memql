package worker

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/hardware"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// An independent receiver for the agreed additive engine contract. This
// exercises the real serialized messages even before the engine pin moves.
func permissionContract(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	field := func(name string, number int32, kind descriptorpb.FieldDescriptorProto_Type, typeName string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(number), Type: kind.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()}
		if typeName != "" {
			f.TypeName = proto.String(typeName)
		}
		return f
	}
	const (
		boolean = descriptorpb.FieldDescriptorProto_TYPE_BOOL
		str     = descriptorpb.FieldDescriptorProto_TYPE_STRING
		message = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
		enum    = descriptorpb.FieldDescriptorProto_TYPE_ENUM
	)
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("worker_permission_contract.proto"), Package: proto.String("permissiontest"), Syntax: proto.String("proto3"),
		Dependency: []string{"google/protobuf/timestamp.proto"},
		EnumType: []*descriptorpb.EnumDescriptorProto{{Name: proto.String("PermissionDecision"), Value: []*descriptorpb.EnumValueDescriptorProto{
			{Name: proto.String("PERMISSION_DECISION_UNKNOWN"), Number: proto.Int32(0)},
			{Name: proto.String("PERMISSION_DECISION_GRANTED"), Number: proto.Int32(1)},
			{Name: proto.String("PERMISSION_DECISION_DENIED"), Number: proto.Int32(2)},
		}}},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("PermissionStatus"), Field: []*descriptorpb.FieldDescriptorProto{
				field("accessibility", 1, boolean, ""), field("screen_recording", 2, boolean, ""), field("x11_display", 3, boolean, ""), field("detail", 4, str, ""),
				field("accessibility_state", 5, enum, ".permissiontest.PermissionDecision"), field("screen_recording_state", 6, enum, ".permissiontest.PermissionDecision"), field("x11_display_state", 7, enum, ".permissiontest.PermissionDecision"),
				field("checked_at", 8, message, ".google.protobuf.Timestamp"), field("probe_context", 9, str, ""),
			}},
			{Name: proto.String("Heartbeat"), Field: []*descriptorpb.FieldDescriptorProto{
				field("ts", 1, message, ".google.protobuf.Timestamp"), field("active_calls_total", 2, descriptorpb.FieldDescriptorProto_TYPE_UINT32, ""), field("apps_present", 5, boolean, ""), field("permissions", 8, message, ".permissiontest.PermissionStatus"),
			}},
			{Name: proto.String("Register"), Field: []*descriptorpb.FieldDescriptorProto{
				field("permissions", 6, message, ".permissiontest.PermissionStatus"), field("capability_descriptor_json", 9, str, ""),
			}},
		},
	}, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func decodePermissionContract(t *testing.T, file protoreflect.FileDescriptor, name protoreflect.Name, source proto.Message) protoreflect.Message {
	t.Helper()
	body, err := proto.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	decoded := dynamicpb.NewMessage(file.Messages().ByName(name))
	if err := proto.Unmarshal(body, decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func permissionValue(m protoreflect.Message, name protoreflect.Name) protoreflect.Value {
	return m.Get(m.Descriptor().Fields().ByName(name))
}

func TestPermissionReportsRefreshTransitions(t *testing.T) {
	contract := permissionContract(t)
	granted := false
	accessCalls, screenCalls := 0, 0
	access := func() bool { accessCalls++; return granted }
	screen := func() bool { screenCalls++; return granted }
	for i, step := range []struct {
		name               string
		available, granted bool
		want               permissionDecision
	}{
		{"unknown", false, false, permissionUnknown},
		{"denied", true, false, permissionDenied},
		{"granted", true, true, permissionGranted},
		{"revoked", true, false, permissionDenied},
		{"unknown again", false, false, permissionUnknown},
	} {
		t.Run(step.name, func(t *testing.T) {
			granted = step.granted
			snapshot := measureMacOSPermissions(nil, nil)
			if step.available {
				snapshot = measureMacOSPermissions(access, screen)
			}
			checked := time.Unix(1700000000+int64(i), 123000000)
			status := permissionStatus(snapshot, checked)
			beat := &memqlv1.Heartbeat{ActiveCallsTotal: 3, AppsPresent: true}
			heartbeatPermissions(beat, status)
			decoded := decodePermissionContract(t, contract, "Heartbeat", beat)
			if permissionValue(decoded, "active_calls_total").Uint() != 3 || !permissionValue(decoded, "apps_present").Bool() {
				t.Fatal("permission reporting overwrote other heartbeat fields")
			}
			p := permissionValue(decoded, "permissions").Message()
			for _, name := range []protoreflect.Name{"accessibility_state", "screen_recording_state"} {
				if got := permissionValue(p, name).Enum(); got != protoreflect.EnumNumber(step.want) {
					t.Fatalf("%s = %d, want %d", name, got, step.want)
				}
			}
			for _, name := range []protoreflect.Name{"accessibility", "screen_recording"} {
				if got := permissionValue(p, name).Bool(); got != (step.want == permissionGranted) {
					t.Fatalf("legacy %s = %t", name, got)
				}
			}
			if permissionValue(p, "x11_display_state").Enum() != 0 || permissionValue(p, "x11_display").Bool() {
				t.Fatal("macOS must not claim an X11 grant or denial")
			}
			if permissionValue(p, "probe_context").String() != "worker-process" {
				t.Fatal("missing process attribution")
			}
			ts := permissionValue(p, "checked_at").Message()
			if permissionValue(ts, "seconds").Int() != checked.Unix() || permissionValue(ts, "nanos").Int() != int64(checked.Nanosecond()) {
				t.Fatal("stale or missing check time")
			}
			// Old readers still see only true grants, and the explicit detail.
			body, _ := proto.Marshal(status)
			old := &memqlv1.PermissionStatus{}
			if err := proto.Unmarshal(body, old); err != nil {
				t.Fatal(err)
			}
			if old.GetAccessibility() != step.granted || !strings.Contains(old.GetDetail(), "current-process") {
				t.Fatalf("legacy status = %v", old)
			}
		})
	}
	if accessCalls != 3 || screenCalls != 3 {
		t.Fatalf("probes were cached or not called: %d/%d", accessCalls, screenCalls)
	}
}

func TestPermissionReportsIndependentDecisions(t *testing.T) {
	snapshot := measureMacOSPermissions(func() bool { return true }, func() bool { return false })
	if snapshot.accessibility != permissionGranted || snapshot.screenRecording != permissionDenied {
		t.Fatal("one grant must not complete another permission")
	}
}

func TestLinuxPermissionSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, server     string
		probeState, want permissionDecision
		wantProbe        bool
	}{
		{"reachable X11", "x11", permissionGranted, permissionGranted, true},
		{"X11 refuses connection", "x11", permissionDenied, permissionDenied, true},
		{"missing probe or timeout", "x11", permissionUnknown, permissionUnknown, true},
		{"missing display", "none", permissionGranted, permissionDenied, false},
		{"Wayland with XWayland", "wayland", permissionGranted, permissionUnknown, false},
		{"unknown display", "other", permissionGranted, permissionUnknown, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			snapshot := measureLinuxPermissions(tc.server, ":test", func(display string) (permissionDecision, string) {
				called = true
				if display != ":test" {
					t.Fatalf("display = %q", display)
				}
				return tc.probeState, "fixture result"
			})
			if called != tc.wantProbe || snapshot.x11Display != tc.want {
				t.Fatalf("probe=%v snapshot=%+v", called, snapshot)
			}
			if snapshot.accessibility != permissionUnknown || snapshot.screenRecording != permissionUnknown {
				t.Fatal("Linux cannot report macOS TCC decisions")
			}
		})
	}
}

func TestX11MissingProbeIsUnknown(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	state, detail := probeX11Access(":test")
	if state != permissionUnknown || !strings.Contains(detail, "unavailable") {
		t.Fatalf("%d %s", state, detail)
	}
}

func TestRegisterAndEveryHeartbeatCarryCurrentPermissionSnapshot(t *testing.T) {
	contract := permissionContract(t)
	register := buildRegister(Config{Name: "permission-test", StateDir: t.TempDir()}, nil, models.Inventory{}, hardware.Inventory{}, tools.PipelinesPolicy{Allow: false})
	for _, source := range []struct {
		name    protoreflect.Name
		message proto.Message
	}{
		{"Register", register},
		{"Heartbeat", buildHeartbeat(0, nil, nil, nil)},
		{"Heartbeat", buildHeartbeat(0, nil, nil, nil)},
	} {
		decoded := decodePermissionContract(t, contract, source.name, source.message)
		p := permissionValue(decoded, "permissions").Message()
		if !p.IsValid() || permissionValue(p, "probe_context").String() != "worker-process" || !p.Has(p.Descriptor().Fields().ByName("checked_at")) {
			t.Fatalf("%s omitted permission snapshot", source.name)
		}
		if runtime.GOOS == "darwin" && cockpitBuildTag() == "computeruse" {
			for _, name := range []protoreflect.Name{"accessibility_state", "screen_recording_state"} {
				if permissionValue(p, name).Enum() == 0 {
					t.Fatalf("native %s should be measured", name)
				}
			}
		} else {
			if permissionValue(p, "accessibility_state").Enum() != 0 || !strings.Contains(permissionValue(p, "detail").String(), "unsupported") {
				t.Fatal("unsupported build must report unknown")
			}
		}
	}
	if runtime.GOOS == "darwin" && cockpitBuildTag() == "computeruse" {
		var descriptor tools.CapabilityDescriptor
		if err := json.Unmarshal([]byte(register.GetCapabilityDescriptorJson()), &descriptor); err != nil {
			t.Fatal(err)
		}
		if descriptor.DisplayServer != "quartz" {
			t.Fatalf("macOS display server = %q", descriptor.DisplayServer)
		}
	}
}
