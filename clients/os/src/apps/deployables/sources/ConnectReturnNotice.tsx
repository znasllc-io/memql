import { Notice } from "../../../kit";
import { ProblemNotice } from "../packages/ReportView";
import { toneFor } from "../packages/refusals";
import { connectSucceeded, type ConnectReturn } from "./connectReturn";

/** A callback answers only the connection attempt; the feed owns the grant. */
export function ConnectReturnNotice({ result }: { result?: ConnectReturn | null }) {
  if (!result || connectSucceeded(result)) return null;
  if (result.reason === "installed") {
    return <Notice tone="info" sentence="GitHub installation finished. Connect your account to choose its repositories." />;
  }
  return <ProblemNotice problem={{ code: result.reason,
    message: "GitHub sent you back without completing the connection." }} tone={toneFor(result.reason)} />;
}
