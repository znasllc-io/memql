import type { ActionBarTone } from "../../../kit/ActionBar";
import type { DeploymentRow, PackageRow } from "../packages/rows";
import type { SiteRow } from "../rows";
import { TERMINAL_RUN_STATUSES } from "./rail";

// acts.ts -- WHAT A DEPLOYABLE OFFERS, given what it is (epic memql#4937,
// design section D; DESIGN.md rule 12).
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
// THE WORDS ARE THE PERSON'S, THE VALUES ARE THE ENGINE'S
// ===========================================================================
// `live` reads Published and `disabled` reads Unpublished (D6). The enum does
// not move: no migration, no new member on v1:platform:site.status. What the
// engine's own distinction bought -- 503 for a deliberate pause, 404 for an
// archive -- is preserved in `detail`, which is where a meaning goes once the
// state word stops carrying it.

/** The act names, as a closed set. One name, one promise, everywhere. */
export type ActName =
  | "Discard"
  | "Publish"
  | "Unpublish"
  | "Archive"
  | "Restore"
  | "Delete"
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
  /** The newest run of that source, whatever its status. */
  run: DeploymentRow | null;
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

/** What the bar reads and offers. The whole of DESIGN.md rule 12 for this app. */
export function actsFor(input: ActsInput): BarReading {
  const { site, pkg, run, canWrite } = input;

  // A SYSTEM-OWNED ROW GETS NO ACTS AT ALL -- not disabled ones. The seeded
  // portal and OS sites are exempt from the lifecycle entirely and the server
  // refuses those writes whoever asks; the bar states what it is and offers
  // nothing, which is the courtesy on top of the guard.
  if (site.systemOwned) {
    return {
      state: "Published",
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
      detail: input.releasing ? `releasing ${input.releasing}` : "releasing the name",
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
  if (run !== null && run.status === "awaiting_confirm") {
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
  if (runIsMoving(run) && run !== null) {
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

  switch (site.status) {
    case "live":
      return {
        state: "Published",
        detail: lastRunBroke ? "serving the version before the last attempt, which did not finish" : "serving now",
        tone: "live",
        acts: write
          ? [{ name: "Unpublish" }, { name: lastRunBroke ? "Retry the deploy" : nextDeployName(pkg), tone: "primary" }]
          : [],
      };
    case "disabled":
      return {
        state: "Unpublished",
        // The engine's own distinction, kept where the state word no longer
        // carries it: a deliberately paused site answers 503, so a visitor is
        // told it is temporarily away rather than that it never existed.
        detail: 'visitors get "temporarily unavailable", not "no such site"',
        tone: "paused",
        acts: write ? [{ name: "Archive" }, { name: "Publish", tone: "primary" }] : [],
      };
    case "archived":
      return {
        state: "Archived",
        detail: "answers nothing, and the name is still held",
        tone: "none",
        acts: write ? [{ name: "Delete", tone: "danger" }, { name: "Restore", tone: "primary" }] : [],
      };
    default: {
      // DRAFT. The dead end this epic was reported for: no primary action, no
      // way to reach `disabled`, and an Archive the server refuses. It offers
      // Discard -- a delete that skips the ceremony, because a draft resolves
      // for nobody and the pause exists to let people notice.
      const refused = run !== null && (run.status === "refused" || run.status === "failed");
      return {
        state: "Draft",
        detail: refused
          ? "the last deploy did not finish, and nothing is served"
          : "not served to anyone yet",
        tone: "none",
        acts: write
          ? [
              { name: "Discard", tone: "danger" },
              { name: refused ? "Retry the deploy" : nextPublishName(site, pkg), tone: "primary" },
            ]
          : [],
      };
    }
  }
}

/**
 * What the forward act is called on a published deployable.
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

/**
 * What the forward act is called on a draft.
 *
 * A draft that already HAS a bundle is one flip away from serving, so the act
 * is Publish. One that has never been published has to deploy something first.
 */
function nextPublishName(site: SiteRow, _pkg: PackageRow | null): ActName {
  // THE BUNDLE DECIDES, NOT WHAT PRODUCED IT. A draft already holding real
  // bytes is one status write away from serving -- that is Publish. One
  // holding the placeholder has nothing to serve yet, so the act is the thing
  // that gets it some: Deploy.
  const ref = site.bundleRef.trim();
  return ref === "" || isPlaceholder(ref) ? "Deploy" : "Publish";
}

function isPlaceholder(bundleRef: string): boolean {
  return bundleRef.trim().endsWith("/pending/");
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
      return "Publishing";
    default:
      return "Deploying";
  }
}
