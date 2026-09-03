package packages

import (
	"context"
	"strings"
)

// fleetbuild.go is the seam for building on a machine the person owns (epic
// memql#4900, task memql#4904).
//
// ===========================================================================
// A SEAM WITH A HOP TEST AND NO CONSUMER, AND THAT IS THE DELIVERABLE
// ===========================================================================
// The workbench is a headless Linux sandbox. It cannot run Xcode, and no
// amount of configuring it will: an iOS or macOS build needs macOS, which in
// this product means a Mac in the person's own Fleet. That target does not
// exist yet -- `targets.go` registers web and nothing else -- so this route
// ships as the mechanism plus its hop test, and the first mobile target is
// what turns it on. Making it a REGISTERED SURFACE rather than a branch in the
// build stage is what makes that a one-line change later instead of a design
// round.
//
// ===========================================================================
// WHAT IS REAL HERE AND WHAT IS DELIBERATELY REFUSED
// ===========================================================================
// REAL: the selection. Which of the owner's machines can take this build is
// the Fleet router's answer to a label requirement, under the OWNER's actor,
// with `no_worker_available` naming every machine considered and why each was
// ruled out. That is the half a first mobile target would otherwise have to
// invent, and it is the half that is wrong in interesting ways -- an exact
// label match, a machine that is online by a derived window, a policy the
// owner set.
//
// REFUSED, by name: the dispatch, when the consent a machine dispatch needs is
// absent. Running a build on somebody's laptop is a computer-use act, and the
// dispatcher's gates ask for a Plan the person approved and a standing scope
// on the agent doing it. A DEPLOY HAS NEITHER: nobody chose an agent, and
// there is no plan. Rather than mint a synthetic plan to satisfy a gate -- a
// false statement in the graph to get past a check that exists to stop exactly
// this -- the route answers `no_worker_available` with a sentence saying so.
// The first mobile target's design decides what consent for a build looks
// like; this file makes sure the question is asked rather than answered by
// accident.

// FleetDispatcher is the slice of the Fleet the build route needs.
//
// An interface declared HERE rather than an import of the worker package,
// because that package is behind the `agent` build tag: component/packages
// compiles into every node type, and a hard dependency would make the bff
// unbuildable. The agent-tagged file beside this one is what supplies a real
// one.
type FleetDispatcher interface {
	// SelectMachine answers which of the owner's machines can take a build
	// with these label requirements, or a sentence naming every machine it
	// considered and why each was ruled out.
	SelectMachine(ctx context.Context, ownerUserId string, requireLabels map[string]string) (machineId string, reason string, err error)
}

// fleetBuilder builds on a machine in the owner's Fleet.
type fleetBuilder struct {
	fleet FleetDispatcher
}

// NewFleetBuilder wires the route. Nil dispatcher yields nil, so a node with
// no Fleet keeps `Deps.FleetBuilder` nil and a target asking for the fleet is
// refused by the build stage's own "no build surface configured" message.
func NewFleetBuilder(fleet FleetDispatcher) Builder {
	if fleet == nil {
		return nil
	}
	return &fleetBuilder{fleet: fleet}
}

// FleetBuildLabels are what a build asks of a machine.
//
// Derived from the deployable's KIND rather than from an environment hint,
// because unlike the agent tool loop -- which learns what it needs from a
// refusal it already got -- the pipeline knows before it starts: an iOS build
// needs macOS because iOS builds need macOS, and that is a property of the
// target rather than a discovery.
//
// EXACTLY MATCHED, because that is how the Fleet router matches: there is no
// "any value" form, so `os=darwin` means a machine whose cockpit reported
// exactly that.
func FleetBuildLabels(kind string) map[string]string {
	switch strings.TrimSpace(kind) {
	case "ios", "macos":
		// A need for Apple tooling IS a need for macOS, spelled as the label
		// the cockpit already reports rather than as one somebody has to add
		// by hand.
		return map[string]string{"os": "darwin"}
	case "android":
		// Nothing yet: an Android build needs a JDK and an SDK, which a
		// workbench flavour could carry. When one does, this kind moves to
		// the workbench in targets.go and this arm goes with it.
		return nil
	default:
		return nil
	}
}

func (b *fleetBuilder) Build(ctx context.Context, run BuildRun, _ *SourceSnapshot, dep DeployableReport) (BuildResult, error) {
	res := BuildResult{BuiltOn: BuiltOn{Surface: SurfaceFleet}}
	labels := FleetBuildLabels(dep.Kind)

	// THE SELECTION IS REAL, and it runs first -- so a target with no machine
	// to run on says so before anything else is considered, which is the
	// answer a person can act on (pair a Mac, or wake the one you have).
	machineId, reason, err := b.fleet.SelectMachine(ctx, run.OwnerUserId, labels)
	if err != nil {
		return res, refuseScoped(CodeNoWorkerAvailable, dep.Name,
			"this cluster could not read the machines you have paired, so it cannot build %q on one: %v", dep.Name, err)
	}
	if machineId == "" {
		return res, refuseScoped(CodeNoWorkerAvailable, dep.Name,
			"deployable %q builds on one of your own machines, and none of them can take it. %s", dep.Name, reason)
	}
	res.BuiltOn.NodeId = machineId

	// AND THE DISPATCH IS REFUSED, by name, with the reason.
	//
	// This is the seam's edge, and it is stated rather than hidden: a build on
	// somebody's machine is a computer-use act, and the consent that governs
	// one is a Plan the person approved plus standing scope on the agent doing
	// it. A deploy has no agent and no plan. The available shortcuts are both
	// worse than refusing -- minting a plan nobody made would put a false
	// statement in the graph to get past a gate that exists for this exact
	// case, and bypassing the gate would run somebody else's build script on a
	// person's laptop on the strength of a manifest.
	//
	// So the mechanism above is complete and the last step waits for the
	// design round the first mobile target brings with it.
	return res, refuseScoped(CodeNoWorkerAvailable, dep.Name,
		"deployable %q would build on your machine %s, and this cluster does not yet know how to ask you for that. "+
			"Building on your own machine is a computer-use act: it needs your approval for this specific run, and a deploy "+
			"has no plan to hang that approval on. Nothing was dispatched and nothing was published. This route lands with "+
			"the first target that needs a Mac.", dep.Name, machineId)
}
