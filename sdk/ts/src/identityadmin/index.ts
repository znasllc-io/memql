// The identity-administration surface (memql#3324): the owner/admin-gated
// writes the server-rendered /admin/* console owned, bridged onto
// MemqlService.Stream so a browser can reach them.

export {
  IdentityAdminClient,
  IdentityAdminError,
  CODE_INVALID_ARGUMENT,
  CODE_NOT_FOUND,
  CODE_PERMISSION_DENIED,
  CODE_UNAUTHENTICATED,
  CODE_UNIMPLEMENTED,
  type AdminWriteResult,
  type ClusterSettingsEdit,
  type EnrolmentLinkResult,
  type IdentityAdminCallOptions,
  type UserProfileEdit,
} from "./identityAdmin.js";
