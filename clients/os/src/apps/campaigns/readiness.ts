import type { Readiness } from "../../live/readiness";

/** Preparation works without a provider. Sending requires fresh affirmative
 * readiness from the shared cluster feed; absence is never a success. */
export function campaignSendingConfigured(
  readiness: Readiness | undefined,
): boolean {
  return (
    !!readiness?.loaded &&
    readiness.state === "live" &&
    readiness.of("email")?.state === "configured" &&
    readiness.of("campaigns")?.state === "configured"
  );
}
