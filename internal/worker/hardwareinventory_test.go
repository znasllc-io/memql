package worker

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/hardware"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The scanner and refresh cadence can work while the sender silently drops
// every snapshot. Assert what the cluster receives after envelope encoding.
func TestHardwareInventorySurvivesWorkerEnvelopes(t *testing.T) {
	reportedAt := time.Date(2026, 9, 8, 12, 34, 56, 123456789, time.UTC)
	inv := hardware.Inventory{
		Chip: "13th Gen Intel(R) Core(TM) i9-13900KF", MemoryBytes: 32 << 30,
		GPU:      &hardware.GPU{Name: "NVIDIA GeForce RTX 4090", VRAMBytes: 24 << 30, Backend: hardware.BackendCUDA},
		CPUCores: 32, OSVersion: "Pop!_OS 24.04 LTS", DiskFreeBytes: 491 << 30,
		Runtimes:   []hardware.Runtime{{Name: "ollama", Version: "0.33.3"}, {Name: "docker", Version: "29.7.2"}},
		ReportedAt: reportedAt,
	}
	want := &memqlv1.HardwareInventory{
		Chip: "13th Gen Intel(R) Core(TM) i9-13900KF", MemoryBytes: 32 << 30,
		Gpu:      &memqlv1.GpuInfo{Name: "NVIDIA GeForce RTX 4090", VramBytes: 24 << 30, Backend: "cuda"},
		CpuCores: 32, OsVersion: "Pop!_OS 24.04 LTS", DiskFreeBytes: 491 << 30,
		Runtimes:   []*memqlv1.RuntimeInfo{{Name: "ollama", Version: "0.33.3"}, {Name: "docker", Version: "29.7.2"}},
		ReportedAt: timestamppb.New(reportedAt),
	}
	t.Run("registration", func(t *testing.T) {
		register := buildRegister(Config{Name: "hardware-test", Capabilities: []string{"HEADLESS"}}, nil, models.Inventory{}, inv, tools.PipelinesPolicy{Allow: false})
		decoded := hardwareEnvelopeRoundTrip(t, &memqlv1.WorkerClientMessage{
			Payload: &memqlv1.WorkerClientMessage_Register{Register: register},
		})
		if got := decoded.GetRegister().GetHardware(); !proto.Equal(got, want) {
			t.Fatalf("registration hardware = %v, want %v", got, want)
		}
	})
	t.Run("refresh heartbeat", func(t *testing.T) {
		decoded := hardwareEnvelopeRoundTrip(t, &memqlv1.WorkerClientMessage{
			Payload: &memqlv1.WorkerClientMessage_Heartbeat{Heartbeat: buildHeartbeat(0, nil, nil, &inv)},
		})
		beat := decoded.GetHeartbeat()
		if !beat.GetHardwarePresent() {
			t.Error("refresh heartbeat must mark hardware present so the engine consumes it")
		}
		if got := beat.GetHardware(); !proto.Equal(got, want) {
			t.Fatalf("heartbeat hardware = %v, want %v", got, want)
		}
	})
}

func TestHardwareHeartbeatDistinguishesOmittedAndEmptySnapshot(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inventory *hardware.Inventory
		present   bool
	}{
		{"ordinary beat", nil, false},
		{"empty scan", &hardware.Inventory{}, true},
		{"CPU only", &hardware.Inventory{Chip: "CPU", CPUCores: 8}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded := hardwareEnvelopeRoundTrip(t, &memqlv1.WorkerClientMessage{
				Payload: &memqlv1.WorkerClientMessage_Heartbeat{Heartbeat: buildHeartbeat(0, nil, nil, tc.inventory)},
			})
			beat := decoded.GetHeartbeat()
			if beat.GetHardwarePresent() != tc.present || (beat.GetHardware() != nil) != tc.present {
				t.Fatalf("hardware presence = %v, snapshot = %v; want presence %v", beat.GetHardwarePresent(), beat.GetHardware(), tc.present)
			}
			if hw := beat.GetHardware(); hw != nil {
				if hw.GetGpu() != nil || len(hw.GetRuntimes()) != 0 || hw.GetReportedAt() != nil {
					t.Fatalf("missing GPU, runtimes and scan time must stay absent: %v", hw)
				}
			}
		})
	}
}

func hardwareEnvelopeRoundTrip(t *testing.T, envelope *memqlv1.WorkerClientMessage) *memqlv1.WorkerClientMessage {
	t.Helper()
	raw, err := proto.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	decoded := &memqlv1.WorkerClientMessage{}
	if err := proto.Unmarshal(raw, decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}
