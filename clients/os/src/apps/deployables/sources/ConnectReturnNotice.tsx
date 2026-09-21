import { Notice } from "../../../kit";
import { ProblemNotice } from "../packages/ReportView";
import { toneFor } from "../packages/refusals";
import { connectSucceeded, type ConnectReturn } from "./connectReturn";

/** The identity service's marker for "the cluster's GitHub App was registered,
 *  and the trip could not go on to connecting this person's account". Wire
 *  contract: component/identity/http/github_app_callback.go. */
export const APP_REGISTERED = "github_app_registered";

/** A callback answers only the attempt; the feed owns the grant. */
export function ConnectReturnNotice({ result }: { result?: ConnectReturn | null }) {
  if (!result || connectSucceeded(result)) return null;
  if (result.reason === "installed") {
    return <Notice tone="info" sentence="GitHub installation finished. Connect your account to choose its repositories." />;
  }
  // A REGISTRATION ORDINARILY SAYS NOTHING HERE, because it does not end here:
  // the cluster sends the owner straight on to install the new app and connect
  // their account, and what comes back is an ordinary `connected`. This marker
  // is the trip that could not go on, so the sentence names the step that is
  // left -- and the control for it is already on the surface.
  if (result.reason === APP_REGISTERED) {
    return <Notice tone="info" sentence="GitHub is set up for this cluster. Connect your account to choose its repositories." />;
  }
  // TWO TRIPS COME BACK THROUGH THIS ONE MARKER, and the sentence beneath the
  // headline says which one did not finish.
  const message = result.reason.startsWith("github_app_")
    ? "GitHub sent you back without setting this cluster up."
    : "GitHub sent you back without completing the connection.";
  return <ProblemNotice problem={{ code: result.reason, message }} tone={toneFor(result.reason)} />;
}
