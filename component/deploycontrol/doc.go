// Package deploycontrol is the deploy CAPABILITY BACKEND for the memQL DevOps
// deployment bundle -- NOT the orchestrator. (I11 #2225, epic #2212.)
//
// # Role after I11
//
// The single orchestrator for an engine deploy is the `deployEngineCluster`
// automation in the centralized `deployment` pack (dsl/deployment/, I10 #2224):
// it sequences clone -> build -> place -> gate -> finish, calling the pure
// decision logics, the persistence mutations, and the world-touching actions.
// This package is demoted to what those constructs RESOLVE TO -- the deploy
// capability implementations:
//
//   - Executor (executor.go) -- THE single side-effect boundary (promote.sh /
//     git / kubectl argo rollouts / kubectl JSON reads). The deploy pack drives
//     these exact effects through the IntegrationProvider layer via the exported
//     NewExecutor, so the DSL automation layer is ADDITIVE, never a drifting
//     parallel copy.
//   - driver.go / lockfile.go / semver.go (cutversion.go) / overlay.go --
//     mechanical deploy primitives (provider selection, release lockfile,
//     version arithmetic, overlay pinning) the actions/builtins resolve to.
//   - capability_result.go (ParseCapabilityResult) -- the #2221 capability-script
//     envelope parser the action executor uses to read a script's one JSON
//     result off stdout.
//   - role_gate.go / status.go -- the Go auth gates + status reads the deploy
//     RPCs and the deploy pack share.
//
// # The orchestration entrypoints are DEMOTED, not orchestrating
//
// The synchronous Go deploy LIFECYCLE that deploy.go's Deploy /
// RollbackDeployment once ran (select driver -> apply -> terminal transition)
// was retired in #2115 step 6 and stays retired under I11. Those RPCs are now
// thin ASYNC KICK-OFF shims: they validate, write a single in_progress edge
// (Deploy transitions an existing pending record; RollbackDeployment creates a
// new in_progress record carrying the historical digest + previousDeploymentId),
// and return an ack. The in_progress CDC edge is what a deploy-pack automation
// consumes to run promote + own the terminal transition -- the orchestration
// lives in the DSL, not here. So an entrypoint NEVER calls promote.sh and NEVER
// writes a terminal status; the only synchronous write is the kick-off edge.
// This invariant is guarded by TestDeployKicksOffWithoutApply +
// TestRollbackDeploymentKicksOffWithoutApply (deploy_test.go): a regression that
// re-added a synchronous deploy lifecycle to this package fails those tests.
//
// The legacy Deployment Console actions (DeployStaging / Promote / Rollback /
// RolloutAction / GetDeploymentStatus, service.go, #728) still call the Executor
// effects synchronously; retiring those gRPC surfaces in favor of the automation
// is owner-gated follow-on work (tracked off #2229), not part of I11, because
// they are consumed cross-repo by the cockpit Deploy Console + the SDK.
package deploycontrol
