package memql

// worker_caps.go -- the ENGINE's own capability-slug bundles: the
// computer-use (worker) and workbench tool surfaces. Registered into the
// generic capability registry (capability_registry.go) from init(), through
// the same API a product pack uses for its product-specific bundles.
//
//   - computer-use-headless   -- expands into workerHost + the cross-
//                                cutting pair (workerStatus,
//                                requestComputerUseScope).
//                                Shell / fs / http on the user's machine.
//   - computer-use-embodied   -- expands into workerComputer + the same
//                                cross-cutting pair. Mouse / keyboard /
//                                screenshot on the user's machine.
//   - workbench-use           -- expands into workbenchHost.
//                                Sandboxed Linux execution per-Plan in the
//                                cluster. Universal: every agent has it
//                                by default. No scope-request tool (no
//                                blast radius to gate against) and no
//                                status tool (the workbench is a cluster
//                                service, not a remote process that can
//                                disconnect).
//
// The two computer-use slugs replaced a single legacy `computer-use`
// slug on 2026-05-17. Splitting by mode lets the headless slice be
// served by the workbench backend without dragging the embodied
// (computer-use-only) tools along. Authorization (scope grants, kill switch,
// knowledge domain) is still unified under the "computer use"
// concept because both modes act on the user's machine. workbench-use
// is a sibling, not a child -- it's the safer default for headless
// work and runs entirely inside the cluster.

// workerCrossCuttingNames are the tools shared by both computer-use
// modes: status (live availability check) and scope elevation (per-task
// approval flow). Product presentation tools belong to the product's own
// skills. In particular, canvasPublish is not shipped by the engine and must
// not be required or advertised by an engine capability.
var workerCrossCuttingNames = []string{
	"workerStatus",
	"requestComputerUseScope",
}

// WorkerHeadlessCapabilityNames is the canonical tool list served
// to agents that hold `computer-use-headless`: shell / fs / http
// on the user's machine, plus the cross-cutting pair. Future
// sandbox capability will parallel this one (same headless verbs,
// different backend); the embodied slug is the computer-use-only sibling.
//
// Keep in sync with the engine's worker tool definitions.
var WorkerHeadlessCapabilityNames = append(
	[]string{"workerHost"},
	workerCrossCuttingNames...,
)

// WorkerEmbodiedCapabilityNames is the canonical tool list served
// to agents that hold `computer-use-embodied`: mouse / keyboard /
// screenshot on the user's machine, plus the cross-cutting pair.
// Mode-exclusive -- no sandbox analogue exists since the agent
// needs a real display.
//
// Keep in sync with the workerComputer tool definition.
var WorkerEmbodiedCapabilityNames = append(
	[]string{"workerComputer"},
	workerCrossCuttingNames...,
)

// WorkbenchCapabilityNames is the canonical tool list served to
// agents that hold `workbench-use`: headless shell / fs / http
// operations against a sandboxed per-Plan Linux environment in
// the cluster. Files written through workbenchHost are promoted to the
// Library; product-specific presentation tools are separate capabilities.
//
// Universal capability -- expected to be default-on for every
// agent. No status tool (the workbench is a cluster service,
// not a process that can disconnect), no scope-request tool
// (no host blast radius to gate against; the workspace is
// torn down with the Plan).
//
// Keep in sync with the workbenchHost tool definition.
var WorkbenchCapabilityNames = []string{
	"workbenchHost",
}

func init() {
	RegisterCapabilitySlug("computer-use-headless", WorkerHeadlessCapabilityNames)
	RegisterCapabilitySlug("computer-use-embodied", WorkerEmbodiedCapabilityNames)
	RegisterCapabilitySlug("workbench-use", WorkbenchCapabilityNames)
}
