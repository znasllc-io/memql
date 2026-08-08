// The deploy-control surface (memql#3311): the memQL Deployment Console's
// nine RPCs, bridged onto MemqlService.Stream so a WebSocket client -- the
// VS Code extension, the portal -- can reach them at all.

export {
  DeployControlClient,
  DeployControlError,
  CODE_PERMISSION_DENIED,
  CODE_UNAUTHENTICATED,
  CODE_UNIMPLEMENTED,
  type ActionResult,
  type ArgoStatus,
  type ComponentDigest,
  type DeployControlCallOptions,
  type DeploymentStatus,
  type GateLeg,
  type GateResult,
  type NextVersionSuggestion,
  type RolloutStatus,
} from "./deployControl.js";
