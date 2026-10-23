package models

// The GPU seam, exported for internal/worker/hardware.
//
// It exists so there is exactly ONE reader of this machine's graphics
// hardware. The floor and the hardware inventory ask different questions
// of the same devices -- "does the largest card clear 8 GB" and "what is
// the largest card called" -- and a second probe written for the second
// question would drift from the first. The failure that produces is
// quiet and confusing: a machine whose class was computed from a card
// the floor never saw, advertising a recommended set it cannot hold.
//
// The platform files behind these are the floor's own (floor_darwin.go,
// floor_linux.go, floor_other.go); nothing new probes anything here.

// DetectGPUs returns the discrete GPUs this machine has, largest first
// being NOT guaranteed -- callers pick. An empty slice with no error is
// the ordinary answer across most of a fleet and is not a fault.
//
// On macOS it returns nothing at all, and that absence is correct
// rather than missing: Apple Silicon has no discrete device to
// enumerate, its GPU is the chip and its memory is the unified pool.
// The hardware package fills that in from the chip and the memory size,
// which is the only place both facts are in hand.
func DetectGPUs() ([]GPU, error) { return platformGPUs() }

// GPUBackend names how a GPU on this machine is reached: BackendMetal,
// BackendCUDA, BackendROCm, or BackendNone when no probe vouched for
// one. It is a fact about the MACHINE rather than about a device, which
// is why it is separate from DetectGPUs: a machine with no card still
// has an honest answer here, and it is "none".
func GPUBackend() string { return platformGPUBackend() }

// The backend names, mirrored by internal/worker/hardware. Defined here
// because this is where the probes that establish them live.
const (
	BackendMetal = "metal"
	BackendCUDA  = "cuda"
	BackendROCm  = "rocm"
	BackendNone  = "none"
)
