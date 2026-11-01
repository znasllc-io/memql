// Package pipelinesteps runs one compiled pipeline step somewhere safe (epic
// memql#5478): as a Kubernetes Job in the pipelines namespace, or, for a step
// naming a need the cluster cannot meet, on a fleet machine that offers it.
// It is the substrate behind component/pipelines' Executor; the seam decides
// WHAT runs, and this package decides WHERE and HOW.
//
// # Three halves, and which node runs each
//
// The AGENT node holds the executor the seam's driver calls. It turns a
// pl.StepRequest into a StepRun (the effective timeout, the deadline code,
// the moment its run reaches its ceiling, the environment without the
// secrets), routes a step that names a need to
// the fleet through the agent's dispatcher, and forwards every other step to
// a workbench replica over NodeService. It never talks to the Kubernetes API,
// and it is what notices a workbench replica going quiet and forwards again.
//
// The WORKBENCH node holds the runner: the only MemQL process that creates,
// watches, captures and deletes a step's Job and Secret, through
// component/deploycontrol's ClusterAPI under the engine's identity, whose Role
// reaches the pipelines namespace and nothing else. It persists the outcome on
// the Job before it replies, and every name it uses is derived (names.go), so
// whichever replica asks next finds the same Job.
//
// The STEP'S POD is not a MemQL node at all, and that is the point. When the
// step declares caches, an init container opens the step's directory of the
// cache claim; another shallow-clones the commit; named services run as native
// sidecars; and the manifest's own image runs the command through a POSIX
// wrapper that frames declared artifacts on stdout. The step sees only its
// repository's cache at its run's trust, in a tree of its own uid. Nothing in
// the pod holds a cluster credential: no ServiceAccount token, no Service
// links, no platform Secret.
// The one credential it ever sees is a short-lived clone token, which only the
// clone container can read (TestTheCloneTokenReachesOnlyTheCloneContainer).
//
// # Attempts
//
// The seam's driver retries a step that failed for a transient reason, and the
// executor is what makes each attempt safe to retry. Two things are the
// executor's, not the driver's, because only it knows whether a dispatch reached
// a machine and what the machine or the Job answered:
//
//   - every attempt that did not succeed leaves with a class
//     (pl.AttemptClass): the command did not start, asked to be run again,
//     may have run with an unknown outcome, or failed for good. The rules are
//     attempt_class.go (a Job's answer, and what both surfaces read the same)
//     and attempt_class_fleet.go (a machine's, which also needs the dispatcher's
//     own verdict). A code nobody classified is final, so an unexpected answer is
//     never retried.
//   - an attempt arrives with its place already chosen (pl.AttemptPlacement) and
//     goes exactly there: a fleet attempt never spills to the cluster inside that
//     attempt, a cluster attempt never asks a machine, and neither reaches a
//     place the step's manifest placement or the installation's policy does not
//     allow (refusePlacement). A step's first attempt has no placement and goes
//     where its manifest put it, a pool step's fleet-then-cloud preference
//     included.
//
// What the next place is, how long to wait before it, and when to stop are the
// driver's decisions (spec section 5), because only the driver holds the journal
// of the attempts so far; nothing in this package retries a step.
//
// # The pure half
//
// The configuration, the derived names, the forward payloads, the Kubernetes
// JSON types and the Job and Secret builders are functions over values, shared
// by both MemQL halves and tested without a cluster. The three shell scripts
// they install are tested by running them.
package pipelinesteps
