package worker

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/num"
)

// hardware.go owns the machine inventory a cockpit reports (epic memql#5146,
// design D1) and the one predicate the heartbeat path asks of it.
//
// ===========================================================================
// ABSENT IS NOT EMPTY, AND THAT IS THE WHOLE FILE
// ===========================================================================
// Before this field existed the cluster was never told what a machine IS. It
// was told what a machine OFFERS -- a model with a parameter count -- which is
// a fact about weights rather than about hardware, and which a machine that has
// pulled nothing yet cannot state at all. That machine is exactly the one the
// scanner is for.
//
// So the field arrives into a fleet where every existing machine reports
// nothing, and the reading of that silence decides whether the feature is
// useful or insulting on the day it ships:
//
//   - ABSENT means "this cockpit predates the field". The machine is fine, it
//     is working, and there is nothing to fix. Its class is the EMPTY STRING.
//   - `unsupported` means "this machine reported its hardware and the hardware
//     is under the floor". That is an accusation, and levelling it at a fleet
//     that simply has not upgraded its cockpits would be wrong about every
//     machine at once.
//
// Present() is therefore decided by whether anything was actually REPORTED, not
// by whether a pointer was non-nil, and Row() returns nil for an absent
// inventory so the mutation writes no key rather than an object of zeroes.
//
// ===========================================================================
// WHAT THE ENGINE STORES AND WHAT IT DERIVES
// ===========================================================================
// This is stored. The machine CLASS and the RECOMMENDED SET are not: they are
// pure functions of these numbers (component/memql/fleet_class.go and
// fleet_recommend.go), and a stored copy of a derived value is a second answer
// that ages while the first one cannot.

// Gpu is the accelerator a machine can actually reach.
type Gpu struct {
	Name string
	// VramBytes is dedicated video memory. ZERO on a unified-memory machine,
	// where the pool is Inventory.MemoryBytes -- not a card with no memory.
	// Backend is what tells those apart, and reading a zero here as "no
	// accelerator" would class every Apple Silicon machine as unsupported.
	VramBytes uint64
	// Backend is what the machine can RUN, not what is installed: metal, cuda,
	// rocm, or none. A discrete card whose driver does not work reports `none`,
	// because a backend no runtime can use is not a capability this cluster may
	// count -- and the two states have different repairs.
	Backend string
}

// Runtime is one model runtime present on the machine, with its version.
type Runtime struct {
	Name    string
	Version string
}

// Inventory is what a machine IS, as its cockpit reported it.
type Inventory struct {
	Chip          string
	MemoryBytes   uint64
	Gpu           Gpu
	CpuCores      int
	OsVersion     string
	DiskFreeBytes uint64
	// Runtimes is what the machine reported, VERBATIM, including names this
	// engine does not know. HasRuntime answers only for the closed set the
	// engine acts on, so an unknown name is visible to an operator without ever
	// satisfying a recommendation's requirement.
	Runtimes   []Runtime
	ReportedAt time.Time
}

const (
	// GpuBackendMetal is Apple's, on unified memory.
	GpuBackendMetal = "metal"
	// GpuBackendCuda is NVIDIA's, on dedicated VRAM.
	GpuBackendCuda = "cuda"
	// GpuBackendRocm is AMD's, on dedicated VRAM.
	GpuBackendRocm = "rocm"
	// GpuBackendNone is a machine with no accelerator a runtime can reach. It
	// is NOT the same as "no GPU": a card whose driver does not work reports
	// this, and the repair lives on that machine.
	GpuBackendNone = "none"
)

// Runtime names the engine knows how to act on. A cockpit may report others.
const (
	RuntimeOllama     = "ollama"
	RuntimeMlx        = "mlx"
	RuntimeWhisperCpp = "whispercpp"
	RuntimeKokoro     = "kokoro"
	RuntimeMflux      = "mflux"
	RuntimeDocker     = "docker"
)

// DockerLabel and DockerLabelValue are the routing pair a machine that can run
// containers carries: exactly docker=true (#5494). Derived from a reported
// docker runtime beside the version-valued runtime:docker (RuntimeLabels), and
// required by a pipeline step that needs Docker -- integrations/agent/worker's
// EnvironmentNeedsLabels spells the same pair for the docker need.
const (
	DockerLabel      = "docker"
	DockerLabelValue = "true"
)

// MaxRuntimes bounds the reported runtime list. Well above the six the engine
// knows, so an honest cockpit reporting extras is never near it; the bound
// exists only so a misbehaving one cannot grow the registration row.
const MaxRuntimes = 16

// maxHardwareStringLen bounds every operator-facing string a machine reports.
// Nothing parses them, so the only job is to stop a malformed cockpit parking
// kilobytes on the row.
const maxHardwareStringLen = 128

// GpuBackends returns the closed backend set, in the order the error names it.
func GpuBackends() []string {
	return []string{GpuBackendMetal, GpuBackendCuda, GpuBackendRocm, GpuBackendNone}
}

// KnownRuntimes returns the runtime names the engine acts on.
func KnownRuntimes() []string {
	return []string{RuntimeOllama, RuntimeMlx, RuntimeWhisperCpp, RuntimeKokoro, RuntimeMflux, RuntimeDocker}
}

// InventoryFromProto validates and decodes a reported inventory.
//
// A nil message is the ABSENT case and is not an error -- a cockpit that
// predates the field registers exactly as before. A malformed one IS an error
// and refuses the registration: an inventory decides a machine's class and
// therefore what it is told to pull, so one silently discarded leaves a machine
// unclassifiable forever with nothing anywhere to read.
func InventoryFromProto(p *memqlv1.HardwareInventory) (Inventory, error) {
	if p == nil {
		return Inventory{}, nil
	}
	backend := strings.TrimSpace(p.GetGpu().GetBackend())
	if backend != "" && !validGpuBackend(backend) {
		return Inventory{}, fmt.Errorf(
			"hardware inventory: unknown gpu backend %q (want %s)",
			backend, strings.Join(GpuBackends(), "|"))
	}
	if n := len(p.GetRuntimes()); n > MaxRuntimes {
		return Inventory{}, fmt.Errorf("hardware inventory: %d runtimes exceeds the cap of %d", n, MaxRuntimes)
	}

	inv := Inventory{
		Chip:          clampHardwareString(p.GetChip()),
		MemoryBytes:   p.GetMemoryBytes(),
		CpuCores:      num.ClampInt64(int64(p.GetCpuCores())),
		OsVersion:     clampHardwareString(p.GetOsVersion()),
		DiskFreeBytes: p.GetDiskFreeBytes(),
	}
	if g := p.GetGpu(); g != nil {
		inv.Gpu = Gpu{
			Name:      clampHardwareString(g.GetName()),
			VramBytes: g.GetVramBytes(),
			Backend:   backend,
		}
	}
	for _, r := range p.GetRuntimes() {
		name := clampHardwareString(r.GetName())
		if name == "" {
			continue
		}
		inv.Runtimes = append(inv.Runtimes, Runtime{
			Name:    name,
			Version: clampHardwareString(r.GetVersion()),
		})
	}
	sortRuntimes(inv.Runtimes)
	if ts := p.GetReportedAt(); ts != nil {
		inv.ReportedAt = ts.AsTime()
	}
	return inv, nil
}

// Present reports whether the machine actually told us anything.
//
// Decided by CONTENT, never by whether a message arrived: a cockpit that sends
// the message with nothing filled in has said as much as one that sent none,
// and the honest reading of both is silence. DiskFreeBytes and ReportedAt are
// deliberately not evidence of presence -- a cockpit reporting only free disk
// has still not said what the machine is.
func (i Inventory) Present() bool {
	return i.Chip != "" || i.MemoryBytes > 0 || i.CpuCores > 0 ||
		i.Gpu.Backend != "" || i.Gpu.Name != "" || len(i.Runtimes) > 0
}

// HasRuntime reports whether the machine has a runtime the ENGINE knows.
//
// An unrecognised name never satisfies this, which is what keeps an unknown
// runtime visible on the machine page without ever making a recommendation
// pullable that this cluster cannot serve.
func (i Inventory) HasRuntime(name string) bool {
	name = strings.TrimSpace(strings.ToLower(name))
	if !knownRuntime(name) {
		return false
	}
	for _, r := range i.Runtimes {
		if strings.EqualFold(r.Name, name) {
			return true
		}
	}
	return false
}

// RuntimeVersion returns the reported version of a runtime, or "".
func (i Inventory) RuntimeVersion(name string) string {
	for _, r := range i.Runtimes {
		if strings.EqualFold(r.Name, name) {
			return r.Version
		}
	}
	return ""
}

// Row is the shape stored on v1:worker:registration.hardware.
//
// NIL FOR AN ABSENT INVENTORY, so the mutation writes no key at all. Writing an
// object of zeroes would turn "this cockpit has not said" into "this machine
// has no memory, no cores and no disk", which every reader downstream would be
// right to believe.
func (i Inventory) Row() map[string]any {
	if !i.Present() {
		return nil
	}
	runtimes := make([]any, 0, len(i.Runtimes))
	for _, r := range i.Runtimes {
		runtimes = append(runtimes, map[string]any{"name": r.Name, "version": r.Version})
	}
	reported := i.ReportedAt
	if reported.IsZero() {
		reported = time.Now().UTC()
	}
	return map[string]any{
		"chip":        i.Chip,
		"memoryBytes": i.MemoryBytes,
		"gpu": map[string]any{
			"name":      i.Gpu.Name,
			"vramBytes": i.Gpu.VramBytes,
			"backend":   i.Gpu.Backend,
		},
		"cpuCores":      i.CpuCores,
		"osVersion":     i.OsVersion,
		"diskFreeBytes": i.DiskFreeBytes,
		"runtimes":      runtimes,
		"reportedAt":    reported.UTC().Format(time.RFC3339),
	}
}

// InventoryFromRow reads a stored inventory back.
//
// A missing key and an empty object both read as ABSENT, for the reason at the
// top of this file: a registration written before the field existed has not
// reported, and that is not the same as a machine with nothing.
func InventoryFromRow(v any) Inventory {
	row, ok := v.(map[string]any)
	if !ok || len(row) == 0 {
		return Inventory{}
	}
	inv := Inventory{
		Chip:          rowHardwareString(row, "chip"),
		MemoryBytes:   rowHardwareUint64(row, "memoryBytes"),
		CpuCores:      rowHardwareInt(row, "cpuCores"),
		OsVersion:     rowHardwareString(row, "osVersion"),
		DiskFreeBytes: rowHardwareUint64(row, "diskFreeBytes"),
	}
	if gpu, ok := row["gpu"].(map[string]any); ok {
		inv.Gpu = Gpu{
			Name:      rowHardwareString(gpu, "name"),
			VramBytes: rowHardwareUint64(gpu, "vramBytes"),
			Backend:   rowHardwareString(gpu, "backend"),
		}
	}
	if list, ok := row["runtimes"].([]any); ok {
		for _, item := range list {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name := rowHardwareString(entry, "name")
			if name == "" {
				continue
			}
			inv.Runtimes = append(inv.Runtimes, Runtime{
				Name:    name,
				Version: rowHardwareString(entry, "version"),
			})
		}
		sortRuntimes(inv.Runtimes)
	}
	if s := rowHardwareString(row, "reportedAt"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			inv.ReportedAt = t
		}
	}
	return inv
}

// MaterialChange reports whether the difference between two inventories is one
// the cluster ACTS on, and it is the predicate the heartbeat path asks.
//
// ===========================================================================
// WHY THIS IS NOT `old != new`
// ===========================================================================
// A heartbeat's DB write is throttled to one per HeartbeatBatchInterval. An
// inventory change that alters what a machine can be asked to do must not wait
// for that window, because the derived `runtime:` labels live on the ROW as
// well as in the live registry, and a row that disagrees with the registry is
// exactly the split no reader can detect.
//
// But DiskFreeBytes changes on essentially every beat. Forcing a write on any
// difference would therefore be one write per machine per beat forever -- a
// write storm dressed up as freshness, and one that would only be found in
// production under load.
//
// So: the parts that change what the machine can RUN force a write. Free disk
// and the report timestamp ride the ordinary throttle and arrive within the
// interval, which is soon enough for a number nothing decides on.
func MaterialChange(before, after Inventory) bool {
	if before.Present() != after.Present() {
		// The first inventory a machine ever reports is the most important
		// write in this field's life: it is the beat on which the machine stops
		// being unclassifiable. It must not wait.
		return true
	}
	if before.Chip != after.Chip ||
		before.MemoryBytes != after.MemoryBytes ||
		before.CpuCores != after.CpuCores ||
		before.OsVersion != after.OsVersion ||
		before.Gpu != after.Gpu {
		return true
	}
	if len(before.Runtimes) != len(after.Runtimes) {
		return true
	}
	// Both slices are sorted on construction, so this compares SETS: two
	// cockpits, or one cockpit across two beats, may enumerate the same
	// runtimes in a different order, and reading that as a change would force a
	// write on a machine where nothing happened.
	for i := range before.Runtimes {
		if before.Runtimes[i] != after.Runtimes[i] {
			return true
		}
	}
	return false
}

// RuntimeLabel is the label for one runtime name.
//
// The prefix is the EXISTING one from modelcall.go, deliberately: the cockpit
// already advertises `runtime:<name>` and the fleet already reads it
// (Candidate.Runtimes). What this epic adds is the VERSION, as the label's
// value, and a second vocabulary for the same fact would have left two answers
// to "which runtimes does this machine have" that could disagree.
//
// The version goes in the VALUE and never in the key. Fleet labels match
// exactly and there is no "any value" form, so `runtime:kokoro=1.0` as a key
// would make every requirement unsatisfiable the day a machine upgraded.
func RuntimeLabel(name string) string {
	return RuntimeLabelPrefix + strings.TrimSpace(strings.ToLower(name))
}

// RuntimeLabels derives the `runtime:` label set from an inventory.
//
// ONLY THE RUNTIMES THE ENGINE KNOWS produce a label. A machine that reports
// vllm keeps it on the row, where an operator can see it, and derives nothing
// -- because a label is a routing fact, and this engine has no way to serve a
// model through a runtime it does not model. That is the rule `apps` already
// follows for an app id outside the closed runnable set.
//
// A runtime with no version reported gets the label with an EMPTY value rather
// than no label: the machine has the runtime, which is the routing fact, and
// the version is operator-facing detail it happened not to state.
//
// DOCKER ALSO DERIVES docker=true (#5494), the one label here outside the
// prefix. A step that needs Docker routes on that exact pair, and the
// version-valued runtime:docker can never satisfy it: there is no "any value"
// form, and the value differs from machine to machine. It is derived from the
// same report rather than left for a cockpit to state, so the two cannot
// disagree about whether a machine has Docker.
func RuntimeLabels(inv Inventory) map[string]string {
	if !inv.Present() {
		return nil
	}
	out := map[string]string{}
	for _, r := range inv.Runtimes {
		name := strings.TrimSpace(strings.ToLower(r.Name))
		if !knownRuntime(name) {
			continue
		}
		out[RuntimeLabel(name)] = r.Version
		if name == RuntimeDocker {
			out[DockerLabel] = DockerLabelValue
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mergeRuntimeLabels folds an inventory's runtimes into a label map.
//
// ===========================================================================
// IT DROPS ONLY WHAT IT COULD HAVE WRITTEN
// ===========================================================================
// The obvious implementation -- clear every `runtime:` key, then write the
// inventory's -- is wrong here, and wrong in a way no test of this epic's own
// fields would catch. The cockpit ALREADY advertises runtime labels of its own,
// including names outside the engine's closed six (`openai-compatible` is the
// documented one), and it advertises them from Register rather than from the
// inventory. Clearing the whole prefix would delete a real advertisement the
// moment a machine first reported its hardware, and the symptom would be a
// model that stopped being routable for a reason nothing on the page mentions.
//
// So the rule is narrow: a `runtime:` key is removed only when the engine KNOWS
// that runtime and the present inventory does not list it -- which is the
// uninstalled case, and the only one this function is entitled to have an
// opinion about. Everything else is left exactly as the cockpit reported it.
//
// docker=true follows the same rule (#5494): it is removed only when the present
// inventory no longer lists docker, and only with the one value this function
// writes. A `docker` label carrying any other value is the cockpit's.
func mergeRuntimeLabels(base map[string]string, inv Inventory) map[string]string {
	// An ABSENT inventory leaves the map exactly as it was. A cockpit that
	// stopped reporting has not uninstalled anything -- it has gone quiet, and
	// reading quiet as removal would strip a working machine's labels the
	// moment it downgraded.
	if !inv.Present() {
		return base
	}
	derived := RuntimeLabels(inv)
	out := make(map[string]string, len(base)+len(derived))
	for k, v := range base {
		if strings.HasPrefix(k, RuntimeLabelPrefix) {
			name := strings.TrimPrefix(k, RuntimeLabelPrefix)
			_, stillListed := derived[k]
			if knownRuntime(name) && !stillListed {
				// The engine knows this runtime, the machine has just said what
				// it has, and this is not in it. That is the uninstalled case,
				// and the only one this function may act on.
				continue
			}
		}
		if k == DockerLabel && v == DockerLabelValue {
			if _, stillDerived := derived[DockerLabel]; !stillDerived {
				// Docker uninstalled. Kept, it would send a step that needs
				// Docker to a machine that will fail it.
				continue
			}
		}
		out[k] = v
	}
	for k, v := range derived {
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// InventoriesEqual reports whether two inventories are identical in every
// field, free disk and report time included.
//
// It answers a different question from MaterialChange and both are needed on
// the heartbeat path: this one decides whether there is anything to store at
// all, MaterialChange decides whether storing it can wait. An inventory whose
// free disk moved is a CHANGE (worth writing, eventually) and not MATERIAL
// (not worth its own write now).
func InventoriesEqual(a, b Inventory) bool {
	if a.Chip != b.Chip || a.MemoryBytes != b.MemoryBytes || a.Gpu != b.Gpu ||
		a.CpuCores != b.CpuCores || a.OsVersion != b.OsVersion ||
		a.DiskFreeBytes != b.DiskFreeBytes || !a.ReportedAt.Equal(b.ReportedAt) {
		return false
	}
	if len(a.Runtimes) != len(b.Runtimes) {
		return false
	}
	for i := range a.Runtimes {
		if a.Runtimes[i] != b.Runtimes[i] {
			return false
		}
	}
	return true
}

func sortRuntimes(rs []Runtime) {
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].Name != rs[j].Name {
			return rs[i].Name < rs[j].Name
		}
		return rs[i].Version < rs[j].Version
	})
}

func validGpuBackend(v string) bool {
	for _, b := range GpuBackends() {
		if v == b {
			return true
		}
	}
	return false
}

func knownRuntime(name string) bool {
	for _, r := range KnownRuntimes() {
		if name == r {
			return true
		}
	}
	return false
}

func clampHardwareString(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxHardwareStringLen {
		return s[:maxHardwareStringLen]
	}
	return s
}

func rowHardwareString(row map[string]any, key string) string {
	s, _ := row[key].(string)
	return strings.TrimSpace(s)
}

func rowHardwareInt(row map[string]any, key string) int {
	switch n := row[key].(type) {
	case int:
		return n
	case int64:
		return num.ClampInt64(n)
	case float64:
		return num.ClampFloat64(n)
	}
	return 0
}

// rowHardwareUint64 narrows a decoded payload number to a byte count.
//
// narrowing: GUARDED -- the bound is inline and both ends are load-bearing.
// A NEGATIVE value reads as ZERO rather than wrapping to an enormous positive:
// a machine cannot have minus four gigabytes, and the wrap would class it as
// the largest machine in the fleet and recommend it everything -- the one
// direction this file is built not to fail in. An ABSURDLY LARGE float
// saturates at the top for the same reason read the other way: the figure is
// an ORDERING, a cockpit reporting more memory than the address space can hold
// is a corrupt reading rather than a claim, and `uint64(x)` above the range is
// implementation-defined and answers with the integer indefinite value.
//
// It is not core/num's because core/num narrows to `int` and this is a `uint64`
// byte count; adding a uint64 arm there for two call sites would widen the one
// narrowing seam rather than use it.
func rowHardwareUint64(row map[string]any, key string) uint64 {
	switch n := row[key].(type) {
	case uint64:
		return n
	case int:
		if n < 0 {
			return 0
		}
		return uint64(n)
	case int64:
		if n < 0 {
			return 0
		}
		return uint64(n)
	case float64:
		if n <= 0 {
			return 0
		}
		if n >= math.MaxUint64 {
			return math.MaxUint64
		}
		return uint64(n)
	}
	return 0
}
