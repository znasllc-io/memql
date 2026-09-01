package packages

// builder.go records why Deps.Builder is nil in production, because a nil
// field with no explanation reads as an oversight and this one is a boundary.
//
// # What ships
//
// The D4 FAST PATH ships and is complete: a package whose deployable already
// carries its built output (dist/index.html in the snapshot) deploys with no
// build at all -- no workbench, no network, no restart. That covers every tree
// whose CI already builds it, which is what the memql-project template
// produces, and it is the path the epic's own fixtures exercise end to end.
//
// A package that needs a build gets a TYPED REFUSAL naming the command it
// would have run and the two ways forward (commit the output, or configure a
// build surface). It is refused at the build stage, after the analysis has
// already reported the build plan at the confirm gate -- so nobody discovers
// this by watching a deploy hang.
//
// # What does not, and why it is not a shortcut
//
// D4 puts builds on the WORKBENCH: sandboxed, resource-capped, no cluster
// credentials in the environment. That is not decoration -- a package's build
// script is somebody else's code running inside this cluster, and `npm ci`
// executes whatever the tree's dependencies put in a postinstall hook.
//
// The workbench reaches that isolation through a PER-PLAN workspace, and its
// own gate refuses a call whose planId does not resolve to a readable plan
// (workspace_owner_unresolved, memql#4354) -- deliberately, because a workspace
// written under a blank actor is readable by nobody, including the operator
// answering "where did my file go". A package deploy has no Plan, and the two
// ways to give it one are both decisions this epic's spec does not make:
//
//   - createPlan lands a row in status "planning", which the planner agent
//     CLAIMS off the node-created event and decomposes with an LLM. A deploy
//     that silently spends model budget is a worse defect than a build that
//     refuses.
//   - createAdHocPlan sets status "running" and is not claimed, but requires
//     an agentId. A deploy has no agent, and writing a synthetic one would put
//     a false statement in the graph to satisfy a schema.
//
// The third option -- running the build in this process with os/exec -- is the
// one that must not be taken. It would deliver the feature by deleting the
// property D4 named: untrusted code would run in the engine pod, with the
// engine's environment, which includes its credentials.
//
// So the SEAM is here, complete and tested (the pipeline suite drives a fake
// Builder through success, failure and the bounded log tail), and binding it
// wants one small decision on the workbench side: either a plan kind the
// planner does not claim, or a workspace owner that is a user rather than a
// plan. Filed rather than guessed.
