package worker

import (
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The register and heartbeat halves of the hardware inventory (epic memql#5146,
// D1). The unit-level rules -- what is material, what absence means -- are in
// hardware_test.go; what is tested here is the WIRING, which has its own
// decisions and its own way of going wrong.

// hardwareBeat is beatAt carrying an inventory. It moves the server's clock
// for beatAt's reason (design D6): the throttle these tests walk is a
// comparison against the SERVER's now.
func hardwareBeat(s *streamSession, at time.Time, inv *memqlv1.HardwareInventory) {
	s.server.clock = func() time.Time { return at }
	s.handleHeartbeat(&memqlv1.Heartbeat{
		Ts:              timestamppb.New(at),
		Hardware:        inv,
		HardwarePresent: true,
	}, "10.0.0.1:1234")
}

func liveInventory(mut ...func(*memqlv1.HardwareInventory)) *memqlv1.HardwareInventory {
	inv := &memqlv1.HardwareInventory{
		Chip:          "Apple M4 Max",
		MemoryBytes:   64 << 30,
		Gpu:           &memqlv1.GpuInfo{Name: "Apple M4 Max", Backend: "metal"},
		CpuCores:      16,
		OsVersion:     "15.3",
		DiskFreeBytes: 900 << 30,
		Runtimes:      []*memqlv1.RuntimeInfo{{Name: "ollama", Version: "0.5.4"}},
	}
	for _, m := range mut {
		m(inv)
	}
	return inv
}

func TestRegisterRefusesAMalformedInventory(t *testing.T) {
	// A malformed inventory REFUSES the registration. The alternative -- drop
	// the field and register anyway -- leaves a machine that reports its
	// hardware looking exactly like one whose cockpit predates the field, and
	// nobody investigates that state because it is the ordinary one.
	_, err := validateRegister(&memqlv1.Register{
		Capabilities: []string{CapabilityHeadless},
		Hardware:     liveInventory(func(i *memqlv1.HardwareInventory) { i.Gpu.Backend = "vulkan" }),
	})
	if err == nil {
		t.Fatal("a malformed inventory must refuse the registration")
	}
}

func TestRegisterWithNoInventoryStillRegisters(t *testing.T) {
	// The rollout case, and the one that must not regress: every machine in
	// every fleet reports nothing on the day this ships.
	if _, err := validateRegister(&memqlv1.Register{
		Capabilities: []string{CapabilityHeadless},
	}); err != nil {
		t.Fatalf("a cockpit that predates the field must register exactly as before: %v", err)
	}
}

func TestHeartbeatWithoutHardwarePresentLeavesTheInventoryAlone(t *testing.T) {
	// hardware_present is apps_present's twin. A beat that says nothing about
	// hardware must not clear what the machine reported at register -- proto3
	// cannot express the difference between an absent message and an empty one,
	// which is the entire reason the flag exists.
	t0 := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{}
	session := newHeartbeatTestSession(store, func() time.Time { return t0 })
	defer session.cancel()

	known, err := InventoryFromProto(liveInventory())
	if err != nil {
		t.Fatal(err)
	}
	session.worker.SetHardware(known)

	beatAt(session, t0)

	if got := session.worker.Hardware(); !InventoriesEqual(got, known) {
		t.Fatalf("a beat carrying no inventory must leave the stored one alone, got %+v", got)
	}
	if len(store.hardwareUpdates) != 0 {
		t.Fatalf("a beat carrying no inventory must write no inventory, got %d writes", len(store.hardwareUpdates))
	}
}

func TestHeartbeatPersistsAMaterialInventoryChangeInsideTheThrottleWindow(t *testing.T) {
	// The reason the immediate write exists. Installing a runtime moves the
	// `runtime:` labels, which live on the ROW as well as in the registry, and
	// a planner node holds no registry at all -- so waiting for the throttle
	// leaves the router reading labels that disagree with the machine.
	t0 := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{}
	session := newHeartbeatTestSession(store, func() time.Time { return t0 })
	defer session.cancel()

	// The first beat of a stream always flushes, which is what puts the next
	// one inside the throttle window.
	beatAt(session, t0)
	before := len(store.hardwareUpdates)

	hardwareBeat(session, t0.Add(time.Second), liveInventory(func(i *memqlv1.HardwareInventory) {
		i.Runtimes = append(i.Runtimes, &memqlv1.RuntimeInfo{Name: "kokoro", Version: "1.0"})
	}))

	if len(store.hardwareUpdates) != before+1 {
		t.Fatalf("a material inventory change must persist inside the throttle window, got %d writes", len(store.hardwareUpdates)-before)
	}
	got := store.hardwareUpdates[len(store.hardwareUpdates)-1]
	if got.labels["runtime:kokoro"] != "1.0" {
		t.Fatalf("the write must carry the re-derived runtime labels with their versions, got %#v", got.labels)
	}
}

func TestHeartbeatDoesNotPersistAFreeDiskChangeInsideTheThrottleWindow(t *testing.T) {
	// The reason the immediate write is CONDITIONAL. Free disk moves on every
	// report; persisting on any difference would be one write per machine per
	// report forever, which is a write storm nobody would find until it was in
	// production under load.
	t0 := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{}
	session := newHeartbeatTestSession(store, func() time.Time { return t0 })
	defer session.cancel()

	hardwareBeat(session, t0, liveInventory())
	writes := len(store.hardwareUpdates) + len(store.lastSeenFlushes)

	hardwareBeat(session, t0.Add(time.Second), liveInventory(func(i *memqlv1.HardwareInventory) {
		i.DiskFreeBytes = 400 << 30
	}))

	if got := len(store.hardwareUpdates) + len(store.lastSeenFlushes); got != writes {
		t.Fatalf("free disk moving must not buy a write inside the throttle window, writes went %d -> %d", writes, got)
	}
	// It is not lost, though: the registry has it, and the next flush carries
	// it. A change that is not worth a write of its own is still a change.
	if session.worker.Hardware().DiskFreeBytes != 400<<30 {
		t.Fatal("the registry must hold the new figure even when the row does not yet")
	}
}

func TestANonMaterialChangeRidesTheNextFlush(t *testing.T) {
	// The reason hardwarePending is a field on the SESSION and not a local in
	// the beat handler. The cockpit reports hardware on every tenth beat, so a
	// change arriving inside the throttle window is followed by nine beats that
	// carry no inventory at all. A local would be dropped by the first of them
	// and the row would not catch up until the tenth.
	t0 := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{}
	session := newHeartbeatTestSession(store, func() time.Time { return t0 })
	defer session.cancel()

	hardwareBeat(session, t0, liveInventory())
	hardwareBeat(session, t0.Add(time.Second), liveInventory(func(i *memqlv1.HardwareInventory) {
		i.DiskFreeBytes = 400 << 30
	}))
	// A beat with no inventory, past the throttle. This is the one that must
	// carry the pending refresh.
	beatAt(session, t0.Add(HeartbeatBatchInterval+time.Second))

	last := store.lastSeenFlushes[len(store.lastSeenFlushes)-1]
	if last.Hardware == nil {
		t.Fatal("the next flush must carry the pending inventory; a beat that carries no inventory is not a beat with nothing to write")
	}
	if last.Hardware["diskFreeBytes"] != uint64(400<<30) {
		t.Fatalf("the flush must carry the LATEST figure, got %#v", last.Hardware["diskFreeBytes"])
	}
}

func TestHeartbeatKeepsTheStoredInventoryOnAMalformedOne(t *testing.T) {
	// Asymmetric with Register on purpose. Refusing here would drop a live
	// stream and take the machine offline over a field that decides a
	// recommendation, and the previous good inventory is still on the row.
	t0 := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{}
	session := newHeartbeatTestSession(store, func() time.Time { return t0 })
	defer session.cancel()

	good, err := InventoryFromProto(liveInventory())
	if err != nil {
		t.Fatal(err)
	}
	session.worker.SetHardware(good)

	hardwareBeat(session, t0.Add(time.Second), liveInventory(func(i *memqlv1.HardwareInventory) {
		i.Gpu.Backend = "vulkan"
	}))

	if got := session.worker.Hardware(); !InventoriesEqual(got, good) {
		t.Fatalf("a malformed beat must leave the last good inventory in place, got %+v", got)
	}
}

func TestRuntimeLabelsCarryTheVersionAndDropAnUninstalledRuntime(t *testing.T) {
	// The label half of D7. The version is the label's VALUE, never part of the
	// key: fleet labels match exactly and there is no "any value" form, so
	// `runtime:kokoro=1.0` as a key would make every requirement unsatisfiable
	// the day a machine upgraded.
	w := &Worker{RegistrationId: "reg-1", Labels: map[string]string{"os": "darwin"}}

	withKokoro, err := InventoryFromProto(liveInventory(func(i *memqlv1.HardwareInventory) {
		i.Runtimes = append(i.Runtimes, &memqlv1.RuntimeInfo{Name: "kokoro", Version: "1.0"})
	}))
	if err != nil {
		t.Fatal(err)
	}
	w.SetHardware(withKokoro)
	if w.LabelsSnapshot()["runtime:kokoro"] != "1.0" {
		t.Fatalf("the runtime label must carry the version as its value, got %#v", w.LabelsSnapshot())
	}

	withoutKokoro, err := InventoryFromProto(liveInventory())
	if err != nil {
		t.Fatal(err)
	}
	w.SetHardware(withoutKokoro)
	if _, still := w.LabelsSnapshot()["runtime:kokoro"]; still {
		t.Fatal("a runtime the machine no longer reports must stop being advertised, or it claims a capability it lost")
	}
	if w.LabelsSnapshot()["os"] != "darwin" {
		t.Fatal("a non-runtime label must survive the merge untouched")
	}
}

func TestARuntimeLabelTheEngineDoesNotKnowSurvives(t *testing.T) {
	// THE CASE A TEST OF THIS EPIC'S OWN FIELDS WOULD MISS. The cockpit already
	// advertises runtime labels of its own from Register, including names
	// outside the engine's closed six (`openai-compatible` is the documented
	// one). Clearing the whole `runtime:` prefix on the first inventory would
	// delete a real advertisement, and the symptom is a model that stopped
	// being routable for a reason nothing on the page mentions.
	w := &Worker{
		RegistrationId: "reg-1",
		Labels:         map[string]string{"runtime:openai-compatible": "1"},
	}
	inv, err := InventoryFromProto(liveInventory())
	if err != nil {
		t.Fatal(err)
	}
	w.SetHardware(inv)

	if w.LabelsSnapshot()["runtime:openai-compatible"] != "1" {
		t.Fatalf("a runtime label the engine does not model is the cockpit's to make and must survive, got %#v", w.LabelsSnapshot())
	}
	if w.LabelsSnapshot()["runtime:ollama"] != "0.5.4" {
		t.Fatal("a runtime the inventory names must get the version from the inventory")
	}
}

func TestAnAbsentInventoryLeavesTheReportedRuntimeLabelsAlone(t *testing.T) {
	// A cockpit that DOWNGRADED has gone quiet, not uninstalled anything.
	// Reading quiet as removal strips a working machine's labels.
	w := &Worker{
		RegistrationId: "reg-1",
		Labels:         map[string]string{"runtime:ollama": "1"},
	}
	w.SetHardware(Inventory{})
	if w.LabelsSnapshot()["runtime:ollama"] != "1" {
		t.Fatal("an absent inventory must not strip labels the cockpit reported")
	}
}
