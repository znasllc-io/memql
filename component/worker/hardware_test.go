package worker

import (
	"strings"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func gpuProto(name, backend string, vram uint64) *memqlv1.GpuInfo {
	return &memqlv1.GpuInfo{Name: name, Backend: backend, VramBytes: vram}
}

func inventoryProto(mut ...func(*memqlv1.HardwareInventory)) *memqlv1.HardwareInventory {
	inv := &memqlv1.HardwareInventory{
		Chip:          "Apple M4 Max",
		MemoryBytes:   64 << 30,
		Gpu:           gpuProto("Apple M4 Max", "metal", 0),
		CpuCores:      16,
		OsVersion:     "15.3",
		DiskFreeBytes: 900 << 30,
		Runtimes:      []*memqlv1.RuntimeInfo{{Name: "ollama", Version: "0.5.4"}},
		ReportedAt:    timestamppb.New(time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)),
	}
	for _, m := range mut {
		m(inv)
	}
	return inv
}

func TestInventoryFromProtoRejectsAnUnknownBackend(t *testing.T) {
	// A backend outside the closed four REFUSES rather than being dropped. An
	// inventory is a fact the cluster acts on -- it decides a machine's class
	// and therefore what the machine is told to pull -- and one silently
	// discarded leaves a machine classed as unsupported forever with nothing
	// anywhere to read.
	_, err := InventoryFromProto(inventoryProto(func(i *memqlv1.HardwareInventory) {
		i.Gpu = gpuProto("Some Card", "vulkan", 8<<30)
	}))
	if err == nil {
		t.Fatal("expected an error for backend \"vulkan\"")
	}
	for _, want := range []string{"vulkan", "metal", "cuda", "rocm", "none"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error must name the offending value and the closed set; %q is missing from %q", want, err)
		}
	}
}

func TestInventoryFromProtoKeepsAnUnknownRuntimeName(t *testing.T) {
	// The engine's runtime set is closed for what it ACTS ON; what a machine
	// REPORTS is the machine's business. A cockpit that has vllm should be able
	// to say so, and a later engine release makes it mean something without a
	// coordinated cockpit rollout. This is the rule AppInfo already follows.
	inv, err := InventoryFromProto(inventoryProto(func(i *memqlv1.HardwareInventory) {
		i.Runtimes = []*memqlv1.RuntimeInfo{{Name: "vllm", Version: "0.7.0"}}
	}))
	if err != nil {
		t.Fatalf("an unknown runtime name must not be an error: %v", err)
	}
	if len(inv.Runtimes) != 1 || inv.Runtimes[0].Name != "vllm" {
		t.Fatalf("the unknown runtime must survive verbatim, got %+v", inv.Runtimes)
	}
	if inv.HasRuntime("vllm") {
		t.Fatal("HasRuntime answers for runtimes the ENGINE knows; an unknown name must not satisfy a recommendation's requirement")
	}
}

func TestInventoryFromProtoCapsTheRuntimeList(t *testing.T) {
	// The bound exists so a misbehaving or compromised cockpit cannot grow the
	// registration row without limit. The cap is well above the six the engine
	// knows, so an honest cockpit reporting extras is never near it.
	many := make([]*memqlv1.RuntimeInfo, MaxRuntimes+1)
	for i := range many {
		many[i] = &memqlv1.RuntimeInfo{Name: "r", Version: "1"}
	}
	_, err := InventoryFromProto(inventoryProto(func(i *memqlv1.HardwareInventory) {
		i.Runtimes = many
	}))
	if err == nil {
		t.Fatalf("expected an error for %d runtimes (cap %d)", len(many), MaxRuntimes)
	}
}

func TestAbsentInventoryIsNotAnEmptyOne(t *testing.T) {
	// THE CENTRAL DISTINCTION OF THIS FIELD. A nil inventory is "this cockpit
	// predates the field", and it must reach the row as NO KEY AT ALL -- not as
	// an object of zeroes, which would be a machine reporting that it has no
	// memory, no cores and no disk. Every downstream reader takes the absence
	// as silence; an object of zeroes is a statement.
	inv, err := InventoryFromProto(nil)
	if err != nil {
		t.Fatalf("an absent inventory is not an error: %v", err)
	}
	if inv.Present() {
		t.Fatal("a nil inventory must not report Present")
	}
	if inv.Row() != nil {
		t.Fatalf("an absent inventory must write no key at all, got %#v", inv.Row())
	}
}

func TestAZeroedInventoryIsStillAbsent(t *testing.T) {
	// A cockpit that sends the message with nothing filled in has told us
	// nothing, and the honest reading is the same silence as sending no message
	// at all. Presence is decided by whether anything was actually reported,
	// never by whether the pointer was non-nil.
	inv, err := InventoryFromProto(&memqlv1.HardwareInventory{})
	if err != nil {
		t.Fatalf("an empty inventory is not an error: %v", err)
	}
	if inv.Present() {
		t.Fatal("an inventory reporting no memory, no chip and no runtimes has said nothing; it must not read as present")
	}
}

func TestMaterialChangeIgnoresFreeDiskAndTheTimestamp(t *testing.T) {
	// The predicate decides whether a heartbeat forces a DB write inside the
	// throttle window. diskFreeBytes changes on essentially every beat, so
	// treating any difference as material would be one write per machine per
	// beat, forever -- a write storm dressed as freshness.
	before, err := InventoryFromProto(inventoryProto())
	if err != nil {
		t.Fatal(err)
	}
	after, err := InventoryFromProto(inventoryProto(func(i *memqlv1.HardwareInventory) {
		i.DiskFreeBytes = 400 << 30
		i.ReportedAt = timestamppb.New(time.Date(2026, 9, 7, 13, 0, 0, 0, time.UTC))
	}))
	if err != nil {
		t.Fatal(err)
	}
	if MaterialChange(before, after) {
		t.Fatal("free disk and the report time must not force a write; they change on every beat")
	}
}

func TestMaterialChangeNoticesANewRuntime(t *testing.T) {
	// The case the predicate exists for. Installing a runtime changes what the
	// machine should be recommended and which `runtime:` labels it carries, and
	// both live on the ROW as well as in the live registry -- a row that
	// disagrees with the registry is exactly the split no reader can detect.
	before, err := InventoryFromProto(inventoryProto())
	if err != nil {
		t.Fatal(err)
	}
	after, err := InventoryFromProto(inventoryProto(func(i *memqlv1.HardwareInventory) {
		i.Runtimes = append(i.Runtimes, &memqlv1.RuntimeInfo{Name: "kokoro", Version: "1.0"})
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !MaterialChange(before, after) {
		t.Fatal("an installed runtime is material: it changes the recommended set and the routing labels")
	}
}

func TestMaterialChangeIgnoresRuntimeOrder(t *testing.T) {
	// Two cockpits, or one cockpit across two beats, may enumerate the same
	// runtimes in a different order. Reading that as a change would force a
	// write on a machine where nothing happened.
	before, err := InventoryFromProto(inventoryProto(func(i *memqlv1.HardwareInventory) {
		i.Runtimes = []*memqlv1.RuntimeInfo{{Name: "ollama", Version: "0.5.4"}, {Name: "mlx", Version: "0.2"}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	after, err := InventoryFromProto(inventoryProto(func(i *memqlv1.HardwareInventory) {
		i.Runtimes = []*memqlv1.RuntimeInfo{{Name: "mlx", Version: "0.2"}, {Name: "ollama", Version: "0.5.4"}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if MaterialChange(before, after) {
		t.Fatal("runtime ORDER is not a fact about the machine; only the set is")
	}
}

func TestMaterialChangeNoticesARuntimeVersionChange(t *testing.T) {
	// An upgraded runtime is material: the machine page reports the version, and
	// a probe's figures were measured against one specific one.
	before, err := InventoryFromProto(inventoryProto())
	if err != nil {
		t.Fatal(err)
	}
	after, err := InventoryFromProto(inventoryProto(func(i *memqlv1.HardwareInventory) {
		i.Runtimes = []*memqlv1.RuntimeInfo{{Name: "ollama", Version: "0.6.0"}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !MaterialChange(before, after) {
		t.Fatal("a runtime version change is material")
	}
}

func TestMaterialChangeNoticesAMemoryChange(t *testing.T) {
	// A VM resized, or a card swapped. Both change the machine's class, which
	// changes what it is told to pull.
	before, err := InventoryFromProto(inventoryProto())
	if err != nil {
		t.Fatal(err)
	}
	after, err := InventoryFromProto(inventoryProto(func(i *memqlv1.HardwareInventory) {
		i.MemoryBytes = 32 << 30
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !MaterialChange(before, after) {
		t.Fatal("a memory change is material: it changes the machine's class")
	}
}

func TestMaterialChangeFromAbsentToPresentIsMaterial(t *testing.T) {
	// A cockpit upgrade is the single most important write in this file's life:
	// it is the beat on which a machine stops being unclassifiable. It must not
	// wait for the throttle.
	var absent Inventory
	present, err := InventoryFromProto(inventoryProto())
	if err != nil {
		t.Fatal(err)
	}
	if !MaterialChange(absent, present) {
		t.Fatal("the first inventory a machine ever reports is material")
	}
}

func TestRowRoundTripsThroughTheStoredShape(t *testing.T) {
	// The row shape is what every reader downstream sees -- the OS, the class
	// function, the recommendation. A field lost here is a field that reads as
	// "not reported" while the machine reported it, which is the one failure
	// mode this whole epic is built to avoid.
	inv, err := InventoryFromProto(inventoryProto())
	if err != nil {
		t.Fatal(err)
	}
	back := InventoryFromRow(inv.Row())
	if !back.Present() {
		t.Fatal("a stored inventory must read back as present")
	}
	if back.Chip != inv.Chip || back.MemoryBytes != inv.MemoryBytes || back.CpuCores != inv.CpuCores {
		t.Fatalf("scalar fields did not round-trip: %+v vs %+v", back, inv)
	}
	if back.Gpu != inv.Gpu {
		t.Fatalf("gpu did not round-trip: %+v vs %+v", back.Gpu, inv.Gpu)
	}
	if len(back.Runtimes) != len(inv.Runtimes) || back.Runtimes[0] != inv.Runtimes[0] {
		t.Fatalf("runtimes did not round-trip: %+v vs %+v", back.Runtimes, inv.Runtimes)
	}
	if !back.ReportedAt.Equal(inv.ReportedAt) {
		t.Fatalf("reportedAt did not round-trip: %v vs %v", back.ReportedAt, inv.ReportedAt)
	}
}

func TestInventoryFromRowOfNothingIsAbsent(t *testing.T) {
	// The read side of the central distinction: a registration written before
	// this field existed has no key, and that must arrive as silence.
	if InventoryFromRow(nil).Present() {
		t.Fatal("a missing hardware key must read as absent")
	}
	if InventoryFromRow(map[string]any{}).Present() {
		t.Fatal("an empty hardware object must read as absent, not as a machine with nothing")
	}
}

// dockerInventory is a reported inventory whose runtimes include Docker.
func dockerInventory(t *testing.T) Inventory {
	t.Helper()
	inv, err := InventoryFromProto(inventoryProto(func(i *memqlv1.HardwareInventory) {
		i.Runtimes = append(i.Runtimes, &memqlv1.RuntimeInfo{Name: "docker", Version: "27.3.1"})
	}))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func TestDockerRuntimeDerivesTheDockerLabel(t *testing.T) {
	// A pipeline step that needs Docker routes on the EXACT pair docker=true
	// (#5494). Fleet labels match exactly and there is no "any value" form, so
	// the label a Docker machine already carries -- runtime:docker, whose VALUE
	// is the version and differs from machine to machine -- can never satisfy
	// that requirement. The routing fact has to be stated as the pair itself,
	// derived from the same report, beside the version-valued one.
	inv := dockerInventory(t)

	derived := RuntimeLabels(inv)
	if derived["docker"] != "true" {
		t.Fatalf("a machine reporting the docker runtime must derive docker=true, got %#v", derived)
	}
	if DockerLabel != "docker" || DockerLabelValue != "true" {
		t.Fatalf("the constants spell %s=%s; the routing requirement is the literal pair docker=true", DockerLabel, DockerLabelValue)
	}
	if derived["runtime:docker"] != "27.3.1" {
		t.Fatalf("the version-valued runtime label is unchanged beside it, got %#v", derived)
	}

	// The live registry entry, which is what a dispatch on this replica routes on.
	w := &Worker{RegistrationId: "reg-1", Labels: map[string]string{"os": "linux"}}
	w.SetHardware(inv)
	if got := w.LabelsSnapshot(); got["docker"] != "true" || got["os"] != "linux" {
		t.Fatalf("the registry entry must carry docker=true beside its other labels, got %#v", got)
	}
	// The ROW, which is what every other replica routes on: the register path
	// writes mergeRuntimeLabels' answer, so a row and a registry entry that
	// disagreed about docker would route the same step two ways.
	if got := mergeRuntimeLabels(map[string]string{"os": "linux"}, inv); got["docker"] != "true" {
		t.Fatalf("the registration row's labels must carry docker=true, got %#v", got)
	}

	// The negative control: the label is DERIVED from the runtime, not stamped
	// on every machine that reports an inventory.
	without, err := InventoryFromProto(inventoryProto())
	if err != nil {
		t.Fatal(err)
	}
	if _, has := RuntimeLabels(without)["docker"]; has {
		t.Fatalf("a machine with no docker runtime must not derive the docker label, got %#v", RuntimeLabels(without))
	}
}

func TestAnUninstalledDockerStopsAdvertisingTheDockerLabel(t *testing.T) {
	// mergeRuntimeLabels' rule, applied to the pair it now derives: it drops
	// only what it could have written. docker=true from a machine whose present
	// inventory no longer lists docker is the uninstalled case, and keeping it
	// would send a step needing Docker to a machine that will fail it.
	without, err := InventoryFromProto(inventoryProto())
	if err != nil {
		t.Fatal(err)
	}

	w := &Worker{RegistrationId: "reg-1", Labels: map[string]string{"os": "linux"}}
	w.SetHardware(dockerInventory(t))
	w.SetHardware(without)
	got := w.LabelsSnapshot()
	if _, still := got["docker"]; still {
		t.Fatalf("docker=true must go when the machine stops reporting docker, got %#v", got)
	}
	if _, still := got["runtime:docker"]; still {
		t.Fatalf("runtime:docker must go with it, got %#v", got)
	}

	// A value the engine could never have written is the cockpit's to make.
	cockpit := &Worker{RegistrationId: "reg-2", Labels: map[string]string{"docker": "rootless"}}
	cockpit.SetHardware(without)
	if cockpit.LabelsSnapshot()["docker"] != "rootless" {
		t.Fatalf("a docker label the engine did not derive must survive, got %#v", cockpit.LabelsSnapshot())
	}

	// An ABSENT inventory is a cockpit gone quiet, not one that uninstalled
	// anything -- the rule TestAnAbsentInventoryLeavesTheReportedRuntimeLabelsAlone
	// states for the runtime labels.
	quiet := &Worker{RegistrationId: "reg-3", Labels: map[string]string{"docker": "true"}}
	quiet.SetHardware(Inventory{})
	if quiet.LabelsSnapshot()["docker"] != "true" {
		t.Fatalf("an absent inventory must leave docker=true alone, got %#v", quiet.LabelsSnapshot())
	}
}
