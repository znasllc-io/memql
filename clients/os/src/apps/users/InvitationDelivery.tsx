import type { UserInvitationResult } from "@znasllc-io/memql-sdk-core/identityadmin";
import { CopyValue, Notice } from "../../kit";

/** The plaintext link is returned once; retain it even when delivery fails. */
export function InvitationDelivery({ result }: { result: UserInvitationResult }) {
  return <Notice tone={result.emailSent ? "info" : "warn"}
    sentence={result.emailSent ? "The invitation is on its way." : result.emailError === "" ? "The invitation exists, and no mail is wired on this cluster." : "The invitation exists, and the email did not go out."}
    next="This link is shown once. The cluster kept only its hash, so it cannot be retrieved again."
    detail={result.emailError || undefined}>
    <CopyValue value={result.url} label="Invitation link" />
  </Notice>;
}
