import type { ActionBarTone } from "../../../kit/ActionBar";
import { runCoversApp, runIsScopedToApp, type DeploymentRow, type PackageRow } from "../packages/rows";
import type { SiteRow } from "../rows";
import { siteIsBuilt, siteStateDetail, siteStateWord } from "../words";
import { TERMINAL_RUN_STATUSES } from "./rail";

// acts.ts -- WHAT A DEPLOYABLE OFFERS, given what it is (epic memql#4937,
// design section D; DESIGN.md rule 12; the vocabulary of 2026-09-05 D1).
//
// ===========================================================================
// PURE, BECAUSE THE TABLE IS THE FEATURE
// ===========================================================================
// The whole design of the action bar is which acts a state offers and which it
// does not, so that is what gets asserted -- with no DOM, no connection and no
// render. Two negative cases carry more of it than any positive one: a DRAFT
// yields no Archive, and a system-owned row yields no acts at all.
//
// ===========================================================================
// AN ILLEGAL ACT IS ABSENT, NEVER DISABLED
// ===========================================================================
// This is the bug fix, not a preference. Before this, a draft rendered an
// ENABLED "Archive this deployable" that `validateSiteStatusTransition`
// refuses -- archiving admits `disabled` alone -- and the draft had no control
// anywhere that could reach `disabled`. The only lifecycle control a draft
// offered was one the engine rejected. Deriving the acts from the state is
// what makes that unrepresentable.
//
// ===========================================================================
// A SOURCE'S APP AND A STANDALONE ARE TWO LADDERS (2026-09-05, D2 and D4)
// ===========================================================================
// A standalone deployable's bundle is its only copy, so it climbs the D10
// ladder: Offline -> Archived (the name is kept) -> Delete (the name is
// released). A source's app is reproducible from its source, so it has ONE
// destructive act from every state, Deactivate, which releases the address
// and puts the app back on the source's off-list -- and it is never archived.
// The state words are the same on both; the acts beside them differ.

/** The act names, as a closed set. One name, one promise, everywhere. */
export type ActName =
  | "Discard"
  | "Go live"
  | "Take offline"
  | "Archive"
  | "Restore"
  | "Delete"
  | "Deactivate"
  | "Deploy"
  | "Deploy the update"
  | "Redeploy"
  | "Retry the deploy"
  | "Cancel";

export interface ActSpec {
  name: ActName;
  tone?: "primary" | "danger" | "quiet";
}

export interface BarReading {
  /** The state, in the words a person uses. */
  state: string;
  /** What that state means, in one clause. */
  detail: string;
  tone: ActionBarTone;
  /** The acts legal from this state, primary LAST. At most three. */
  acts: ActSpec[];
}

export interface ActsInput {
  site: SiteRow;
  /** The source, when the deployable came from one. */
  pkg: PackageRow | null;
  /**
   * THIS APP'S newest run, whatever its status -- not the source's
   * (memql#4953). It used to be the source's, and every branch below then
   * answered about the wrong thing: a serving deployable read Building while a
   * sibling deployed, hid its own acts, and offered a Cancel that killed the
   * other app's run. `runForApp` is what selects it.
   */
  run: DeploymentRow | null;
  /**
   * A run of the SAME SOURCE, in flight, that is not this app's.
   *
   * Present so that fixing the reading above does not lose what its being
   * wrong was accidentally providing. While any run of a source is in flight,
   * every app of that source used to read "Building" and offer no way to start
   * another -- which was a false statement AND the only thing stopping two
   * concurrent runs of one source. There is no gate for that in the engine,
   * and the roll is per-source: it rewrites one pointer and restarts the
   * cluster onto it, so two runs racing there is not a thing to let somebody
   * discover.
   *
   * So the app keeps its own state and its own words, and the acts that would
   * START a run are absent until the source is free -- absent rather than
   * disabled, which is rule 12.
   */
  siblingRun?: DeploymentRow | null;
  /** Rank >= 200. Presentation over a server-side law. */
  canWrite: boolean;
  /** True while this deployable's own delete is still tearing its domains down. */
  deleting?: boolean;
  /** The domain the teardown is releasing right now, for the progress line. */
  releasing?: string;
}

/**
 * The stages a run can still be stopped at.
 *
 * MIRRORS component/packages' CancellableStage, and the mirroring is
 * deliberate rather than lazy: the engine is the authority and refuses the ask
 * from `staging_dsl` on, so this is presentation over a server-side law -- the
 * same relationship every other gate in this app has to its guard. A CLOSED
 * list of what is allowed, so a stage added later is safe by default.
 */
const CANCELLABLE = new Set(["analyzing", "awaiting_confirm", "building"]);

/**
 * Terminal deployment statuses. A run at one of these is not running.
 *
 * READ FROM THE RAIL, never listed again here. This was a second copy, and
 * when `cancelled` arrived only this one learned about it -- so the bar called
 * a cancelled run finished while the rail beside it drew the same run as still
 * moving, at the same moment on the same row. Two answers to one question is
 * the bug; the list is not.
 */
const TERMINAL = new Set(TERMINAL_RUN_STATUSES);

export function runIsMoving(run: DeploymentRow | null): boolean {
  return run !== null && run.status !== "" && !TERMINAL.has(run.status);
}

export function runIsCancellable(run: DeploymentRow | null): boolean {
  return run !== null && CANCELLABLE.has(run.status);
}

/**
 * This app's newest run out of a source's timeline, newest first.
 *
 * The whole of memql#4953's client half is choosing correctly here rather than
 * taking `rows[0]`.
 */
export function runForApp(runs: readonly DeploymentRow[], app: string): DeploymentRow | null {
  return runs.find((run) => runCoversApp(run, app)) ?? null;
}

/**
 * A run of the same source, ACTUALLY RUNNING, that this app's page is not
 * about.
 *
 * A PARKED RUN IS NOT IN FLIGHT and does not hold the source. `runIsMoving`
 * answers "not terminal", which `awaiting_confirm` satisfies -- but a run at
 * the gate is waiting for a PERSON: nothing is executing, nothing is holding
 * the pointer, and withholding a sibling's Redeploy over it would mean one
 * unanswered gate froze every other app of the source until somebody found it.
 * The hazard this guards is two runs racing at the ROLL, which a parked run
 * has not reached and may never reach.
 */
export function siblingRunInFlight(
  runs: readonly DeploymentRow[],
  app: string,
): DeploymentRow | null {
  return (
    runs.find(
      (run) => !runCoversApp(run, app) && runIsMoving(run) && run.status !== "awaiting_confirm",
    ) ?? null
  );
}

/**
 * The acts that would start a new run, as a closed set.
 *
 * Withheld while a sibling's run is in flight. Every other act on the bar
 * changes THIS site's own state -- going live, going offline, archiving,
 * deactivating, discarding -- and none of them touches the source's pointer,
 * so none of them has to wait for a deploy of a different app to finish.
 */
const STARTS_A_RUN = new Set<ActName>(["Deploy", "Deploy the update", "Redeploy", "Retry the deploy"]);

/**
 * The line a busy source's other apps show INSTEAD of their own detail.
 *
 * It replaces rather than appends, and it is short, because `.os-actbar-detail`
 * ellipsizes: a clause explaining why a button is missing, cut off half way,
 * is worse than no clause. The state word and its dot already say the app is
 * live, so this says the one thing they do not -- what it is waiting for.
 */
function busyClause(sibling: DeploymentRow): string {
  const other = sibling.scopedTo.length === 1 ? sibling.scopedTo[0] : "another app from this source";
  return `waiting for ${other}'s deploy to finish`;
}

/**
 * Whether this deployable has a state of its own to be described BY.
 *
 * `live` and `disabled` both mean it has been on the internet -- serving, or
 * deliberately taken offline. A draft has no such answer yet, so a run at the
 * gate is the most relevant thing about it.
 */
function hasServed(site: SiteRow): boolean {
  return site.status === "live" || site.status === "disabled";
}

/**
 * Whether a parked run was started FOR this deployable.
 *
 * `scopedTo` names the apps a run is for and is EMPTY for a whole-source run
 * (and absent on every row written before the field existed). A gate about
 * this app is this page's business and speaks for it even when the app is
 * serving -- it is a redeploy somebody is being asked to confirm. A
 * whole-source gate is the SOURCE's business: it says nothing about whether
 * this app is live, and it is answered from the source's own page.
 */
function gateIsAboutThisApp(run: DeploymentRow, site: SiteRow): boolean {
  return runIsScopedToApp(run, site.packageDeployableName);
}

/** The gate, mentioned beside a state word that stays true. */
function gateClause(read: BarReading): BarReading {
  return { ...read, detail: `${read.detail} -- a deploy for this source is waiting for you` };
}

/** What the bar reads and offers. The whole of DESIGN.md rule 12 for this app. */
export function actsFor(input: ActsInput): BarReading {
  const read = holdWhileTheSourceIsBusy(reading(input), input.siblingRun ?? null);
  // The gate did not get to be the state; it still gets to be mentioned, or a
  // person on this page would have no sign that one is waiting at all.
  const parked =
    input.run !== null &&
    input.run.status === "awaiting_confirm" &&
    hasServed(input.site) &&
    !gateIsAboutThisApp(input.run, input.site);
  return parked ? gateClause(read) : read;
}

/**
 * Withhold the run-starting acts while a sibling's deploy is in flight, and
 * say so in the line that already explains the state.
 *
 * The state word and the tone are UNCHANGED: a live deployable is still live
 * while another app of its source deploys, and saying otherwise is the defect
 * this is part of fixing.
 */
function holdWhileTheSourceIsBusy(read: BarReading, sibling: DeploymentRow | null): BarReading {
  if (sibling === null) return read;
  const acts = read.acts.filter((act) => !STARTS_A_RUN.has(act.name));
  if (acts.length === read.acts.length) return read;
  return { ...read, acts, detail: busyClause(sibling) };
}

/** Whether this deployable came from a source, and so climbs the app ladder rather than the standalone one. */
function fromSource(site: SiteRow, pkg: PackageRow | null): boolean {
  return pkg !== null || site.packageId !== "";
}

function reading(input: ActsInput): BarReading {
  const { site, pkg, run, canWrite } = input;

  // A SYSTEM-OWNED ROW GETS NO ACTS AT ALL -- not disabled ones. The seeded
  // portal and OS sites are exempt from the lifecycle entirely and the server
  // refuses those writes whoever asks; the bar states what it is and offers
  // nothing, which is the courtesy on top of the guard.
  if (site.systemOwned) {
    return {
      state: "Live",
      detail: "a cluster surface -- re-seeded live at every boot, so it has no lifecycle to change",
      tone: "live",
      acts: [],
    };
  }

  // DELETING IS A STATE, because the teardown is asynchronous: the
  // reconciliation sweep takes the certificate and route away on its own
  // schedule. Dropping the row the instant the delete is asked would show
  // somebody a name they cannot reuse yet.
  if (input.deleting) {
    return {
      state: "Deleting",
      detail: siteStateDetail("Deleting", input.releasing ?? ""),
      tone: "busy",
      acts: [],
    };
  }

  // A PARKED RUN IS WAITING FOR THE PERSON, not for the cluster, so it is not
  // "in flight" in the sense the next branch means. Its two acts are the two
  // answers it is waiting for: deploy what the report describes, or stop.
  //
  // Cancel is FIRST and Deploy is the primary, because the gate exists to be
  // passed -- somebody who opened it meant to deploy. The report itself is on
  // the What-it-is stop, which `openStopFor` opens for exactly this status.
  //
  // A GATE SPEAKS ONLY FOR A DEPLOYABLE THAT HAS NOTHING TO SAY FOR ITSELF, or
  // one the gate was opened FOR. A run parked at the confirm gate is waiting
  // for a PERSON: nothing is executing, and it decides nothing about an app
  // that is already on the internet. This branch used to run before
  // `site.status`, so a deployable live at its address -- five green marks on
  // its rail -- read "Ready to deploy" and offered Cancel and Deploy.
  if (run !== null && run.status === "awaiting_confirm" && (!hasServed(site) || gateIsAboutThisApp(run, site))) {
    return {
      state: "Ready to deploy",
      detail: "this deploy is waiting for you -- the report above is what it would do",
      tone: "paused",
      acts: canWrite ? [{ name: "Cancel", tone: "danger" }, { name: "Deploy", tone: "primary" }] : [],
    };
  }

  // A RUN IN FLIGHT OWNS THE BAR. Cancel is the only act, and from the roll on
  // there is not even that -- a roll restarts the cluster onto staged MemQL,
  // and the bar says so rather than offering a control the engine refuses.
  // ...and it is not MOVING either, so it must not fall through to the branch
  // below: `runIsMoving` answers "not terminal", which a parked run satisfies,
  // and that branch would hand a live deployable "Waiting for you" and a lone
  // Cancel. Nothing is executing at a gate. Either the branch above spoke for
  // it, or the deployable's own state does.
  if (runIsMoving(run) && run !== null && run.status !== "awaiting_confirm") {
    const cancellable = runIsCancellable(run);
    return {
      state: stageWord(run.status),
      detail: cancellable
        ? "stopping is safe until the roll begins"
        : "past the point where stopping is safe -- this cluster is restarting onto the staged MemQL, and it will finish on its own",
      tone: "busy",
      acts: canWrite && cancellable ? [{ name: "Cancel", tone: "danger" }] : [],
    };
  }

  // A READER SEES THE STATE AND NO ACTS. The engine decides the writes; this
  // only declines to draw controls somebody cannot use.
  const write = canWrite;

  // A REFUSED OR FAILED LAST RUN NAMES THE ACT, whatever the site's own state
  // is. A live deployable whose last deploy broke is still serving the version
  // before it -- so the honest word is Retry, not "Deploy the update": there
  // is no update, there is an attempt that did not land.
  const lastRunBroke = run !== null && (run.status === "refused" || run.status === "failed");
  const app = fromSource(site, pkg);
  const word = siteStateWord(site);
  // THE DESTRUCTIVE ACT, by ladder. A source's app deactivates from every
  // state; a standalone climbs Offline -> Archive -> Delete, with Discard for
  // a draft that never served (D5: nobody is using a draft, so the pause that
  // lets people notice has nobody to notify).
  const off: ActSpec = app ? { name: "Deactivate", tone: "danger" } : { name: "Discard", tone: "danger" };

  switch (site.status) {
    case "live":
      return {
        state: word,
        detail: lastRunBroke
          ? "serving the version before the last attempt, which did not finish"
          : siteStateDetail(word, site.hostname),
        tone: "live",
        acts: write
          ? [{ name: "Take offline" }, { name: lastRunBroke ? "Retry the deploy" : nextDeployName(pkg), tone: "primary" }]
          : [],
      };
    case "disabled":
      return {
        state: word,
        // The engine's own distinction, kept where the state word no longer
        // carries it: a deployable taken offline on purpose answers 503, so a
        // visitor is told it is temporarily away rather than that it never
        // existed.
        detail: siteStateDetail(word, site.hostname),
        tone: "paused",
        acts: write ? [app ? { name: "Deactivate", tone: "danger" } : { name: "Archive" }, { name: "Go live", tone: "primary" }] : [],
      };
    case "archived":
      // A source's app is never archived any more (D2); a row that IS -- one
      // the old cascade left behind -- offers Deactivate, which is what frees
      // the name it is still holding. A standalone offers the fourth rung.
      return {
        state: word,
        detail: siteStateDetail(word, site.hostname),
        tone: "none",
        acts: write ? [app ? { name: "Deactivate", tone: "danger" } : { name: "Delete", tone: "danger" }, { name: "Restore", tone: "primary" }] : [],
      };
    default: {
      // DRAFT: Not deployed, or Built. Neither serves, and the forward act is
      // the one thing each is waiting for -- files, or the word.
      if (siteIsBuilt(site)) {
        return {
          state: "Built",
          detail: lastRunBroke
            ? "the last deploy did not finish; the version before it is in place, not live"
            : siteStateDetail("Built", site.hostname),
          tone: "none",
          acts: write ? [off, { name: lastRunBroke ? "Retry the deploy" : "Go live", tone: "primary" }] : [],
        };
      }
      return {
        state: "Not deployed",
        detail: lastRunBroke
          ? "the last deploy did not finish, and nothing is in place"
          : siteStateDetail("Not deployed", site.hostname),
        tone: "none",
        acts: write ? [off, { name: lastRunBroke ? "Retry the deploy" : "Deploy", tone: "primary" }] : [],
      };
    }
  }
}

/**
 * What the forward act is called on a live deployable.
 *
 * THREE DIFFERENT PROMISES, THREE DIFFERENT WORDS -- which is the whole reason
 * there is one Retry in this app now instead of six:
 *
 *   Redeploy            a source with nothing newer upstream. Deploying it
 *                       again does the same thing again, and saying "Deploy
 *                       the update" would name an update that does not exist.
 *   Deploy the update   a source the poll has seen move. There IS something
 *                       newer, and this is what fetches it.
 *   Deploy              a hand-made deployable, whose next version is a zip
 *                       somebody picks rather than anything to fetch.
 */
function nextDeployName(pkg: PackageRow | null): ActName {
  if (pkg === null) return "Deploy";
  return pkg.updateAvailable ? "Deploy the update" : "Redeploy";
}

/** A run's stage, in the reader's terms rather than the state machine's. */
export function stageWord(status: string): string {
  switch (status) {
    case "analyzing":
      return "Analyzing";
    case "awaiting_confirm":
      return "Waiting for you";
    case "building":
      return "Building";
    case "staging_dsl":
      return "Staging MemQL";
    case "rolling":
      return "Rolling";
    case "publishing":
      return "Putting files in place";
    default:
      return "Deploying";
  }
}
