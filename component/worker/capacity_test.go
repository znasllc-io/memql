package worker

import (
	"strings"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCapacityFromProtoBoundsWhatACockpitSays(t *testing.T) {
	received := time.Now()
	if c := CapacityFromProto(nil, received); c.Present() {
		t.Fatal("no report must read as no report")
	}
	if c := CapacityFromProto(&memqlv1.Capacity{MemoryFreeBytes: 5}, received); c.Present() {
		t.Fatal("a report with no pool must read as no report")
	}
	loaded := []*memqlv1.LoadedModel{
		{Model: " qwen3.8:27b ", Runtime: "ollama", SizeBytes: 20 << 30, ActiveCalls: 2, ExpiresAt: timestamppb.New(received.Add(time.Minute))},
		{Model: ""},
		{Model: strings.Repeat("x", maxCapacityModelIdLen+1)},
	}
	for i := 0; i < memqlengine.MaxFleetLoadedModels+5; i++ {
		loaded = append(loaded, &memqlv1.LoadedModel{Model: "m"})
	}
	c := CapacityFromProto(&memqlv1.Capacity{
		MemoryTotalBytes: 32 << 30, MemoryFreeBytes: 64 << 30, MemoryKind: "quantum",
		LoadedModels: loaded, MeasuredAt: timestamppb.New(received.Add(-time.Hour)),
	}, received)
	if c.MemoryFreeBytes != c.MemoryTotalBytes {
		t.Fatalf("free %d beyond the pool %d was not clamped", c.MemoryFreeBytes, c.MemoryTotalBytes)
	}
	if c.MemoryKind != "" {
		t.Fatalf("an unknown pool kind was kept: %q", c.MemoryKind)
	}
	if !c.ReportedAt.Equal(received.UTC()) {
		t.Fatal("freshness must be judged on the cluster's clock, not the machine's")
	}
	if len(c.Loaded) != memqlengine.MaxFleetLoadedModels || c.Loaded[0].Model != "qwen3.8:27b" || c.Loaded[0].ActiveCalls != 2 || c.Loaded[0].ExpiresAt.IsZero() {
		t.Fatalf("loaded models: %d, first %+v", len(c.Loaded), c.Loaded[0])
	}
}
