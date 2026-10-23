// Package hardware is what this machine IS, reported as presence facts.
//
// The cluster cannot see any of it. The cockpit dials out from behind
// NAT and the engine reads what Register carries, so a machine that
// never says what it is has to be guessed at -- and the guess available
// is "derive it from the models advertised", which says nothing at all
// about the machine that has none. That machine is exactly the one this
// package exists for: a fresh worker, nothing pulled, about to be told
// which models it should have.
//
// Three rules run through it.
//
//   - PRESENCE FACTS ONLY. No serial numbers, no user names, no paths,
//     no hostname. This is not a style rule: the payload lands on a
//     registration row that the owner's whole cluster can read, so a
//     field added here is a field every reader of that row now has. The
//     field set is asserted by a test rather than reviewed, because
//     review is where "just this one path, for debugging" gets through.
//
//   - AN UNREADABLE FACT IS ABSENT, NEVER ZERO. Every probe may fail,
//     and a failure means "could not establish" -- which is a different
//     claim from establishing a zero. A memory size that came back
//     absent lets the engine read "this cockpit could not tell"; a
//     memory size of 0 tells it this machine has no memory, and the
//     class computed from that is wrong in the one direction that
//     matters.
//
//   - SCAN IS A PURE FUNCTION OF Probe, for the reason inference.Decide
//     is a pure function of Host: every platform question is answered by
//     a build-tagged file INTO the struct, so the rules fixture-test on
//     any CI runner rather than only on whichever one happens to match.
//     A runtime.GOOS inside Scan would put the Linux shape beyond the
//     reach of a Mac runner and the Mac shape beyond the reach of CI,
//     which is where a field that leaks a path survives to production.
package hardware

import (
	"context"
	"strings"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
)

// The backend names, mirrored from internal/worker/models where the
// probes that establish them live. Mirrored rather than aliased so the
// JSON values this package writes are readable in one file.
const (
	BackendMetal = models.BackendMetal
	BackendCUDA  = models.BackendCUDA
	BackendROCm  = models.BackendROCm
	BackendNone  = models.BackendNone
)

// GPU is the graphics device the cluster is told about.
//
// It is ABSENT ENTIRELY on a machine no probe found a card on, rather
// than present with backend "none". The two are different claims: an
// omitted gpu says "nothing was found", where a gpu with a name and no
// backend says "something is here and it is reached by nothing", which
// is not a state any machine is in.
type GPU struct {
	Name      string `json:"name"`
	VRAMBytes uint64 `json:"vramBytes"`
	Backend   string `json:"backend"`
}

// Runtime is one model runtime present on this machine, with the
// version it reported.
//
// A runtime whose version could not be read is still reported, with an
// empty version. "Present, version unknown" and "absent" send an
// operator to completely different places, and collapsing them into an
// absence is how somebody ends up reinstalling a runtime that is
// already running.
type Runtime struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Inventory is the whole payload, and the whole of what leaves this
// machine about its hardware.
type Inventory struct {
	// Chip is the marketing name the OS reports ("Apple M3 Max", "AMD
	// Ryzen 9 7950X"). What the OS says, never normalised: an operator
	// compares it against About This Mac or /proc/cpuinfo, and a name
	// this package invented is one they cannot find.
	Chip string `json:"chip"`
	// MemoryBytes is unified memory on Apple Silicon and system memory
	// elsewhere. Zero means the probe could not read it.
	MemoryBytes uint64 `json:"memoryBytes"`
	GPU         *GPU   `json:"gpu,omitempty"`
	CPUCores    int    `json:"cpuCores"`
	// OSVersion is the human version string ("macOS 15.1", "Ubuntu
	// 24.04.1 LTS"). The DISTRIBUTION rather than the kernel, because
	// the question it answers is "will this runtime install here".
	OSVersion string `json:"osVersion"`
	// DiskFreeBytes is space on the volume the runtime keeps models on
	// -- not the root volume, and not the home directory, which are the
	// two a caller would otherwise assume.
	DiskFreeBytes uint64    `json:"diskFreeBytes"`
	Runtimes      []Runtime `json:"runtimes"`
	// ReportedAt is when this scan ran. It rides the payload rather than
	// being stamped on arrival because the two differ by a heartbeat
	// interval plus a queue, and a reader deciding whether an inventory
	// is stale needs the first number.
	ReportedAt time.Time `json:"reportedAt"`
}

// Probe is everything Scan is allowed to know. Assembled by Local on a
// real machine, and by hand in the tests.
//
// Each fact-returning probe returns an error when it cannot establish
// the fact, which is not the same as establishing a bad one -- see the
// package doc's second rule. A nil func is treated as a probe that
// established nothing, so a partially-filled Probe is a valid one.
type Probe struct {
	// GOOS and GOARCH are the machine's, not the build's, for the same
	// reason inference.Host carries them as fields.
	GOOS, GOARCH string
	Chip         func() (string, error)
	MemoryBytes  func() (uint64, error)
	OSVersion    func() (string, error)
	CPUCores     func() int
	// GPUs is the models package's discrete-device probe, reused rather
	// than rewritten: two readers of this machine's graphics hardware
	// drift, and the failure is a class computed from a card the floor
	// never saw.
	GPUs     func() ([]models.GPU, error)
	Backend  func() string
	DiskFree func() uint64
	Runtimes func(context.Context) []Runtime
	Now      func() time.Time
}

// Scan answers what this machine is. Pure: same Probe, same Inventory.
func Scan(ctx context.Context, p Probe) Inventory {
	// Runtimes is initialised to an empty slice rather than left nil,
	// and that is the same distinction Heartbeat.apps_present exists for
	// one layer up: null on the wire reads as "this build does not
	// report runtimes", where [] reads as "none found". Only the second
	// is true of this build.
	inv := Inventory{Runtimes: []Runtime{}}

	if p.Now != nil {
		inv.ReportedAt = p.Now().UTC()
	}
	if p.Chip != nil {
		if v, err := p.Chip(); err == nil {
			inv.Chip = strings.TrimSpace(v)
		}
	}
	if p.MemoryBytes != nil {
		if v, err := p.MemoryBytes(); err == nil {
			inv.MemoryBytes = v
		}
	}
	if p.OSVersion != nil {
		if v, err := p.OSVersion(); err == nil {
			inv.OSVersion = strings.TrimSpace(v)
		}
	}
	if p.CPUCores != nil {
		inv.CPUCores = p.CPUCores()
	}
	if p.DiskFree != nil {
		inv.DiskFreeBytes = p.DiskFree()
	}
	if p.Runtimes != nil {
		if rts := p.Runtimes(ctx); len(rts) > 0 {
			inv.Runtimes = rts
		}
	}
	inv.GPU = scanGPU(p)
	return inv
}

// scanGPU picks the LARGEST device, which is the floor's own choice --
// the two packages must never disagree about which card this machine
// is, or a machine clears the floor on one card and is classed on
// another.
func scanGPU(p Probe) *GPU {
	backend := BackendNone
	if p.Backend != nil {
		backend = p.Backend()
	}

	// Apple Silicon has no discrete device to enumerate: the GPU is the
	// chip and its memory is the unified pool. Filling the entry from
	// those two facts is what lets one class function read one field on
	// both platforms -- the alternative is a nil GPU on every Apple
	// machine, and every Apple machine classless.
	if backend == BackendMetal {
		var mem uint64
		if p.MemoryBytes != nil {
			if v, err := p.MemoryBytes(); err == nil {
				mem = v
			}
		}
		// NO MEMORY FIGURE, NO GPU ENTRY. On Metal the entry is
		// synthesised from the chip and the unified pool, so without the
		// pool there is nothing to synthesise -- and a GPU with zero
		// VRAM would make Scanned() report true, which flips the class
		// line from the GAP sentence ("could not be read") to the
		// VERDICT sentence ("unsupported"), contradicting the Hardware
		// line directly above it. That is exactly the confusion Scanned
		// exists to prevent, and this is the one path that could commit
		// it.
		if mem == 0 {
			return nil
		}
		name := "Apple GPU"
		if p.Chip != nil {
			if v, err := p.Chip(); err == nil && strings.TrimSpace(v) != "" {
				name = strings.TrimSpace(v)
			}
		}
		return &GPU{Name: name, VRAMBytes: mem, Backend: BackendMetal}
	}

	if p.GPUs == nil {
		return nil
	}
	gpus, err := p.GPUs()
	if err != nil || len(gpus) == 0 {
		return nil
	}
	best := gpus[0]
	for _, g := range gpus {
		if g.VRAMBytes > best.VRAMBytes {
			best = g
		}
	}
	return &GPU{Name: strings.TrimSpace(best.Name), VRAMBytes: best.VRAMBytes, Backend: backend}
}

// Local scans the machine this process is running on.
func Local(ctx context.Context) Inventory { return Scan(ctx, platformProbe()) }
