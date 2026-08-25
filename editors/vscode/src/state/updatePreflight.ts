// The "Before it runs" list for an update-and-rebuild (memql#4578).
//
// PURE: the panel gathers the facts, this states them. It is the rebuild
// checklist with an update in front of it, and it REUSES that checklist rather
// than restating it -- the Docker line, the node list, the lane crossing and
// the duration are the same facts about the same second step, and two copies
// would drift into two different accounts of one run.
//
// WHAT THIS HALF IS FOR. An update is the one verb on this page that can be
// stopped by something the operator is holding: their own uncommitted work.
// Every line below exists to answer "will this apply, and if not, why", BEFORE
// the click -- because the alternative is a developer watching a ten-minute
// run stop on its first step for a reason they could have been told about
// immediately.
//
// WHAT IT CANNOT SAY, and says so rather than guessing. The counts come from
// `readUpdateState`, which does not fetch (its header says why), so "how far
// behind" is EXACT when the remote's commit already happens to be local and
// UNKNOWN otherwise. Rendering unknown as "up to date" would be the confident
// wrong answer this whole area exists to avoid; rendering it as a number
// computed against a stale remote-tracking ref would be the same thing wearing
// a better disguise.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #4578 #4577 #4246

import type { UpdateState } from "../install/updateState.js";
import type { PreflightItem } from "./preflight.js";
import { rebuildPreflightItems, type RebuildPreflightInputs } from "./rebuildPreflight.js";

/** Which answer the run gives when the checkout has commits the remote does not. */
export type UpdateStrategy = "fastForward" | "merge";

export interface UpdatePreflightInputs extends RebuildPreflightInputs {
  /** Absent when git could not read the checkout -- which is its own line. */
  update?: UpdateState;
  strategy: UpdateStrategy;
}

/**
 * Whether the checklist has found something that makes the run pointless.
 *
 * TWO CASES ONLY, and both are things the SCRIPT refuses before it fetches: an
 * operation already under way, and no branch to update to. Everything else --
 * uncommitted work, a divergence, an unreachable remote -- is a MAYBE that only
 * the run can settle, and disabling Start on a maybe would withhold a button
 * whose outcome is very often success.
 */
export function updateIsBlocked(update: UpdateState | undefined): boolean {
  if (update === undefined) return false;
  return update.inProgress !== "" || update.branch === "";
}

export function updatePreflightItems(i: UpdatePreflightInputs): PreflightItem[] {
  const items: PreflightItem[] = [];
  const u = i.update;

  if (u === undefined) {
    items.push({
      label: "Your source",
      state: "attention",
      detail: "git could not read the checkout, so there is no way to say what an update would do.",
    });
    return [...items, ...rebuildPreflightItems(i)];
  }

  // FIRST, because during one of these every other line is about a state that
  // is not the one the operator thinks they are in.
  if (u.inProgress !== "") {
    items.push({
      label: "Unfinished work",
      state: "attention",
      detail:
        `${capitalise(u.inProgress)} is already under way in ${i.checkoutDir}. ` +
        "Finish it or undo it first -- nothing here will touch the checkout until you do.",
    });
  }

  items.push(
    u.branch === ""
      ? {
          label: "Branch",
          state: "attention",
          // Reachable, and not a fault: a release install checks out a tag,
          // and a repair reconciles onto an exact commit.
          detail:
            "This checkout is not on a branch -- its files are at one specific commit -- so there " +
            "is nothing to bring it up to date with. Check out a branch in it first.",
        }
      : {
          label: "Branch",
          state: "ok",
          detail: `${u.branch}, from ${u.remote}.`,
        },
  );

  items.push(distanceItem(u));

  // WORDED AS A MAYBE, because it is one. The overlap is decided against what
  // the fetch brings back, which has not happened yet.
  items.push(
    u.dirtyCount === 0
      ? {
          label: "Your changes",
          state: "ok",
          detail: "Nothing uncommitted -- the update applies cleanly or does nothing.",
        }
      : {
          label: "Your changes",
          state: "ok",
          detail:
            `${u.dirtyCount} uncommitted file${u.dirtyCount === 1 ? "" : "s"}. ` +
            "They come along with the update. If any of them are files the update also changes, " +
            "it stops and names them, leaving everything as it is.",
        },
  );

  items.push(
    i.strategy === "merge"
      ? {
          label: "Your own commits",
          state: "ok",
          detail:
            "Combined with the latest. That needs a settled starting point, so commit or set " +
            "aside uncommitted work first; if the two cannot be combined you resolve it here and " +
            "run this again.",
        }
      : {
          label: "Your own commits",
          state: "ok",
          detail:
            "If this checkout has commits the branch does not, the update stops and says so " +
            "rather than combining them. Choose to combine them below if that is what you want.",
        },
  );

  if (u.shallow) {
    // Named because it is the slowest thing the run does and it happens once.
    items.push({
      label: "History",
      state: "ok",
      detail:
        "This checkout was cloned without its history. The first update fetches the rest, which " +
        "takes a minute; later ones do not.",
    });
  }

  return [...items, ...rebuildPreflightItems(i)];
}

/**
 * How far behind, stated exactly or not at all.
 *
 * The four answers are genuinely different and each sends the operator
 * somewhere else, which is why they are not collapsed into two.
 */
function distanceItem(u: UpdateState): PreflightItem {
  if (u.remoteError !== "") {
    return {
      label: "Latest",
      state: "attention",
      detail: `Could not reach ${u.remote}: ${u.remoteError}. The run will try again and report it.`,
    };
  }
  if (u.ahead === undefined || u.behind === undefined) {
    return {
      label: "Latest",
      state: "ok",
      detail: `${u.remote}/${u.branch} has moved. How far is not known until the update fetches it.`,
    };
  }
  if (u.behind === 0 && u.ahead === 0) {
    return {
      label: "Latest",
      state: "ok",
      detail: "Already at the tip. This rebuilds the checkout as it stands.",
    };
  }
  const behind =
    u.behind === 0 ? "" : `${String(u.behind)} new commit${u.behind === 1 ? "" : "s"} to apply`;
  const ahead =
    u.ahead === 0
      ? ""
      : `${String(u.ahead)} commit${u.ahead === 1 ? "" : "s"} here that ${u.remote}/${u.branch} does not have`;
  return {
    label: "Latest",
    state: "ok",
    detail: [behind, ahead].filter((s) => s !== "").join("; ") + ".",
  };
}

function capitalise(s: string): string {
  return s.length === 0 ? s : s[0].toUpperCase() + s.slice(1);
}
