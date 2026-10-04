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
// the environment without the secrets), routes a step that names a need to
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
// The STEP'S POD is not a MemQL node at all, and that is the point. An init
// container shallow-clones the commit, named services run as native sidecars,
// and the manifest's own image runs the command through a POSIX wrapper that
// frames declared artifacts on stdout. Nothing in the pod holds a cluster
// credential: no ServiceAccount token, no Service links, no platform Secret.
// The one credential it ever sees is a short-lived clone token, which only the
// clone container can read (TestTheCloneTokenReachesOnlyTheCloneContainer).
//
// # The pure half
//
// The configuration, the derived names, the forward payloads, the Kubernetes
// JSON types and the Job and Secret builders are functions over values, shared
// by both MemQL halves and tested without a cluster. The two shell scripts
// they install are tested by running them.
package pipelinesteps
