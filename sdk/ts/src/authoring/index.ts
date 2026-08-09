// The authoring surface (memql#2128 / C1): Gate-1 bundle validation and
// stream-scoped session-define -- the substrate that lets a client run a
// .memql buffer that has never been saved or deployed.

export {
  AuthoringClient,
  failedDiagnostics,
  type AuthoringCallOptions,
  type AuthoringConstruct,
  type AuthoringDiagnostic,
  type SessionDefineBundleResult,
  type ValidateBundleResult,
} from "./authoring.js";
