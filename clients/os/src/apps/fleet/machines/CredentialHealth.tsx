import { Notice } from "../../../kit";
import { formatMoment } from "../../../kit/format";
import { ONLINE_WINDOW_SECONDS } from "../online";
import {
  clockOffsetParts,
  clockSkewMatters,
  credentialStanding,
  machineName,
  type MachineRow,
} from "../rows";

// What is wrong with this machine's CONNECTION, when something is (epic
// memql#5327).
//
// ===========================================================================
// IT RENDERS NOTHING WHEN THERE IS NOTHING TO SAY, AND THAT IS THE DESIGN
// ===========================================================================
// Two facts about a connection can make a machine stop working in a way its
// owner would otherwise have no explanation for:
//
//   the credential EXPIRES   -- and on that day the machine disconnects itself
//                               and cannot come back (design D4)
//   the clock DISAGREES      -- and the machine reads offline while it is
//                               connected and beating (design D6)
//
// Both are on the facts list below, always, for anybody investigating. This
// component is the other half: the moment either one is about to change what
// somebody sees, it says so ABOVE the fold, in one sentence, with the repair.
//
// The rest of the time it is absent. A permanent "credential healthy" badge
// would be the easy version and the wrong one -- it would train a reader to
// skip the place the warning appears, and it would be the fourth green thing
// on a page whose whole job is to make the one red thing findable.
//
// ===========================================================================
// NEVER AN INVENTED FACT
// ===========================================================================
// A token with no expiry says nothing here, because there is nothing coming.
// A cockpit that stamps no timestamp says nothing about its clock, because
// nobody measured it. Neither absence is dressed as health.

export function CredentialHealth({ machine, now }: { machine: MachineRow; now: Date }) {
  const standing = credentialStanding(machine, now);
  const skewed = clockSkewMatters(machine, ONLINE_WINDOW_SECONDS * 1000);
  const label = machineName(machine);

  // A revoked machine is not coming back, so neither warning leads anywhere:
  // its credential expiring is moot and its clock decides nothing. Saying so
  // would be noise on the one row whose state is already final.
  if (machine.revokedAt !== "") return null;
  if (standing !== "soon" && standing !== "expired" && !skewed) return null;

  return (
    <div className="os-fleet-credential-health">
      {standing === "expired" ? (
        <Notice
          tone="error"
          sentence={`${label}'s credential expired ${formatMoment(machine.credentialExpiresAt)}.`}
          next="It cannot reconnect. Pair it again to give it a new one -- its models, labels and history stay."
        />
      ) : null}

      {standing === "soon" ? (
        <Notice
          tone="warn"
          sentence={`${label}'s credential expires ${formatMoment(machine.credentialExpiresAt)}.`}
          next="Cockpit renews it while the machine is connected. One shut until after that date has to be paired again."
        />
      ) : null}

      {skewed ? (
        <Notice
          tone="warn"
          sentence={`${label}'s clock is ${describeSkew(machine.clockSkewMs)} the cluster's.`}
          next="Nothing here is timed by it -- the cluster uses its own clock. Timestamps the machine writes elsewhere will be out by that much."
        />
      ) : null}
    </div>
  );
}

/**
 * The skew, as a person would say it in a sentence.
 *
 * DIRECTION IS PART OF THE ANSWER, not a sign on a number: "ahead" and
 * "behind" are what somebody checking an NTP setting needs, and "-47s" makes
 * them work out which way round the subtraction went.
 *
 * The FIGURE comes from clockOffsetParts, which the facts list also reads.
 * Two renderings of one number, and only one place that decides what the
 * number is -- the terse form belongs in a list and the prose form belongs in
 * a sentence, but "212000 ms" and "3m 32s" must never be two answers.
 */
function describeSkew(ms: number): string {
  const { amount, direction } = clockOffsetParts(ms);
  // The prose form spells the unit out: "3m 32s ahead of the cluster's" reads
  // like a log line in the middle of a sentence.
  const spelled = amount
    .replace(/(\d+)m(?: (\d+)s)?/, (_, m: string, sec?: string) =>
      sec === undefined ? `${m} minutes` : `${m} minutes ${sec} seconds`,
    )
    .replace(/^(\d+)s$/, "$1 seconds")
    .replace(/^(\d+) ms$/, "$1 milliseconds");
  return direction === "ahead" ? `${spelled} ahead of` : `${spelled} behind`;
}
