import { Notice } from "../../kit";
import type { useEmailReadiness } from "./useCampaigns";

/**
 * The one thing this app says before anything else, when it applies.
 *
 * ===========================================================================
 * ONCE, AT THE TOP -- NOT A FAILURE PER ACTION
 * ===========================================================================
 * With no mail credentials the cluster's sender DEGRADES rather than failing:
 * every send returns success, a line goes in a log, and nothing is delivered.
 * A surface that only reported refusals would show a perfectly healthy app
 * that has never delivered a message -- and would tell somebody five separate
 * times, after they had written a campaign, that the same thing was missing.
 * So it is read once at the app root and said once at the top.
 *
 * SILENCE IS NOT A REFUSAL. Only an explicit `configured: "no"` or the
 * log-only `health: "degraded"` raises this. An integration that publishes no
 * self-report answers "unknown", and warning on that would put a permanent
 * banner on a healthy cluster.
 *
 * IT NAMES WHERE TO GO RATHER THAN OPENING IT. An app in this shell navigates
 * its own sections and never another app's, so the honest affordance is the
 * words -- "Settings, under Integrations" -- which is also what somebody would
 * have to be told over a phone.
 */
export function NeedsConfiguration({ email }: { email: ReturnType<typeof useEmailReadiness> }) {
  if (!email.value.needsConfiguration) return null;
  return (
    <Notice
      tone="warn"
      sentence={
        email.value.mode === "log"
          ? "This cluster is not set up to send mail, so nothing sent from here will arrive."
          : "This cluster is missing what it needs to send mail."
      }
      next="Everything here can still be written and saved. Sending is what will not work until the mail settings are filled in -- Settings, under Integrations."
      detail={email.value.detail}
    />
  );
}

