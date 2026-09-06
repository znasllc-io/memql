import type { ButtonTone } from "../../kit";
import type { ActionBarTone } from "../../kit/ActionBar";
import { isPositive } from "../../cluster/figure";
import type { StoreHealth } from "./health";

// The words this app says about a store, and the acts a state offers -- both
// as PURE functions over the health report.
//
// They are pure so the one rule that is easy to get wrong can be asserted
// without a DOM: DESIGN.md rule 12, "an act that is not legal is ABSENT,
// never disabled". Deployables learned this the expensive way -- an enabled
// Archive the engine refused, on a page with no control that could reach the
// state its guard demanded -- and the fix was to compute the acts from the
// state rather than to grey one out.

/** The status chip's tone. `neutral` is "this shell has no copy for it". */
export type StatusTone = "ok" | "warn" | "danger" | "neutral";

export function statusTone(status: string): StatusTone {
  switch (status) {
    case "live":
      return "ok";
    case "backfilling":
    case "configured":
    case "paused":
      return "warn";
    case "error":
      return "danger";
    default:
      return "neutral";
  }
}

/** The status as the row shows it. A blank status is not "unknown data" --
 *  it is a report that carried no status, and it says so. */
export function statusChipWord(status: string): string {
  return status === "" ? "no status reported" : status;
}

/** One act the current state offers, named and toned. The label IS the
 *  promise, so it is what the button reads and what a test asserts on. */
export interface StoreAct {
  name: "Pause ingestion" | "Resume ingestion";
  tone: ButtonTone;
}

/** What the bar says, and what it offers, for one store. */
export interface StoreReading {
  /** The state, in the words a person uses. */
  state: string;
  /** What that state MEANS, in one clause. */
  detail: string;
  tone: ActionBarTone;
  acts: StoreAct[];
}

/**
 * The bar's whole content for one store.
 *
 * PAUSE AND RESUME ARE NEVER BOTH OFFERED, and neither is ever offered
 * disabled: a paused store gets Resume, an ingesting one gets Pause. A status
 * this shell has no copy for offers NEITHER -- an act is a claim about the
 * state it acts from, and "pause it" asserts the store is not already paused.
 * Naming the value instead is the reading that asserts less, and it is what
 * tells an operator that the shell, not the store, is behind.
 *
 * THE PAUSE DETAIL IS LOAD-BEARING. An operator pausing a store is deciding
 * whether they are about to lose events, and they are not: deliveries are
 * still staged, so a pause costs telemetry rather than data and resuming
 * needs no backfill. Left unsaid, a pause reads as a risk and does not get
 * used when it should.
 */
export function readingFor(store: StoreHealth): StoreReading {
  switch (store.status) {
    case "live":
      return {
        state: "Live",
        detail: "Webhooks are registered and applying; reconciliation repairs what they lose.",
        tone: "live",
        acts: [{ name: "Pause ingestion", tone: "quiet" }],
      };
    case "backfilling":
      return {
        state: "Backfilling",
        detail: "The initial bulk loads are running. Domains are applied parents first.",
        tone: "busy",
        acts: [{ name: "Pause ingestion", tone: "quiet" }],
      };
    case "configured":
      return {
        state: "Configured",
        detail: "Credentials are in place and nothing has been mirrored yet.",
        tone: "busy",
        acts: [{ name: "Pause ingestion", tone: "quiet" }],
      };
    case "paused":
      return {
        state: "Paused",
        detail:
          "Deliveries are still being staged, not applied -- a pause loses telemetry rather than events, and resuming needs no backfill.",
        tone: "paused",
        acts: [{ name: "Resume ingestion", tone: "primary" }],
      };
    case "error":
      return {
        state: "Error",
        detail: "The connector could not reach this store. Ingestion is still armed.",
        tone: "paused",
        acts: [{ name: "Pause ingestion", tone: "quiet" }],
      };
    default:
      return {
        state: "Unknown",
        detail:
          store.status === ""
            ? "This store's report carried no status, so nothing can be said about whether it is ingesting."
            : `This cluster reported a status this shell does not have copy for: ${store.status}.`,
        tone: "none",
        acts: [],
      };
  }
}

/** The status `setStoreStatus` is asked for by an act. */
export function statusForAct(act: StoreAct["name"]): string {
  return act === "Pause ingestion" ? "paused" : "live";
}

/**
 * Whether the store's pinned API version disagrees with the mirror's.
 *
 * A BLANK PIN IS NOT A MISMATCH. An empty `apiVersion` means the store pins
 * nothing and runs at the mirror's own version, which is the agreeing case.
 */
export function apiVersionMismatch(store: StoreHealth): boolean {
  return store.apiVersion !== "" && store.mirrorApiVersion !== "" && store.apiVersion !== store.mirrorApiVersion;
}

/** The sentence a mismatch gets, everywhere it is said. */
export function apiVersionMismatchSentence(store: StoreHealth): string {
  return (
    `This store is pinned to ${store.apiVersion} and the mirror was generated from ` +
    `${store.mirrorApiVersion}. A call is REFUSED rather than attempted: another version ` +
    `returns fields the concepts do not declare and omits fields they require.`
  );
}

export function protectedDataWord(store: StoreHealth): string {
  return store.protectedDataLevel === "" ? "none" : store.protectedDataLevel;
}

/** The phase word's tone in the per-domain table. */
export function phaseTone(phase: string): StatusTone {
  switch (phase) {
    case "error":
      return "danger";
    case "backfilling":
    case "paused":
      return "warn";
    default:
      return "neutral";
  }
}

/**
 * Whether a domain row has anything to say.
 *
 * QUIET IS A CONJUNCTION OF MEASURED ZEROES, not "no interesting fields":
 * `isPositive` is false for an ABSENT figure too, so a domain whose counters
 * were never reported would be quiet by that reading alone -- and a domain
 * nothing has measured is exactly the one somebody looking for a gap needs to
 * see. An unreported counter therefore keeps the row.
 *
 * LAG IS NOT PART OF IT. Every live mirror has some latency, so a predicate
 * requiring `lagSeconds == 0` would call nothing quiet and the preference
 * would do nothing at all. Drift, a backed-up outbox, a non-idle phase and an
 * error are incidents; lag is a continuous measure.
 */
export function domainIsQuiet(domain: StoreHealth["domains"][number]): boolean {
  if (domain.phase !== "idle" && domain.phase !== "") return false;
  if (domain.lastError !== "") return false;
  for (const figure of [domain.driftLast, domain.outboxDepth]) {
    if (figure.kind === "absent") return false;
    if (isPositive(figure)) return false;
  }
  return true;
}

/** Drift descending, then concept, so the table's first row is the worst one. */
export function byDriftDescending(
  a: StoreHealth["domains"][number],
  b: StoreHealth["domains"][number],
): number {
  const av = a.driftLast.kind === "measured" ? a.driftLast.value : -1;
  const bv = b.driftLast.kind === "measured" ? b.driftLast.value : -1;
  if (av !== bv) return bv - av;
  return a.concept.localeCompare(b.concept);
}
