// What a pull-and-rebuild will pull and build, and what needs attention first
// (memql#4578).
//
// PURE: the panel gathers the facts, this states them. It is the rebuild check
// with the pull in front of it, and it REUSES that check rather than restating
// it -- Docker, the folder, the lane crossing are the same facts about the same
// second step, and two copies would drift into two accounts of one run.
//
// WHAT THIS HALF IS FOR. A pull is the one act on the page that can be stopped
// by something the operator is holding: their own uncommitted work, their own
// commits, a merge half-done. The facts say where the branch stands and the
// notices say what will stop it, BEFORE the click -- the alternative is a
// ten-minute run stopping on its first step for a reason they could have been
// told immediately. A merge in progress or no branch at all is BLOCKING: the
// page then offers no Pull and rebuild, only the sentence that says why.
//
// WHAT IT CANNOT SAY, and says so rather than guessing. The counts come from
// `readUpdateState`, which does not fetch, so "how far behind" is EXACT when
// the remote's commit already happens to be local and UNKNOWN otherwise.
// Unknown reads "New commits available", never "Up to date".
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #4578 #4577 #4246

import type { UpdateState } from "../install/updateState.js";
import {
  buildNotices,
  type CheckFact,
  type CheckNotice,
  type RebuildCheck,
  type RebuildPreflightInputs,
} from "./rebuildPreflight.js";

/** Which answer the run gives when the checkout has commits the remote does not. */
export type UpdateStrategy = "fastForward" | "merge";

export interface UpdatePreflightInputs extends RebuildPreflightInputs {
  /** Absent when git could not read the checkout -- which is its own notice. */
  update?: UpdateState;
}

/** Everything the pull-and-rebuild screen states. */
export function updateCheck(i: UpdatePreflightInputs): RebuildCheck {
  const facts: CheckFact[] = [{ label: "Source", value: i.checkoutDir, mono: true }];
  const notices: CheckNotice[] = [];
  const u = i.update;

  if (u === undefined) {
    notices.push({ tone: "warn", line: "Couldn't read the folder's git status.", blocking: false });
  } else {
    if (u.branch !== "") facts.push({ label: "Branch", value: `${u.branch} from ${u.remote}`, mono: true });
    const pull = pullFact(u);
    if (pull !== "") facts.push({ label: "To pull", value: pull });
    if (u.ahead !== undefined && u.ahead > 0) {
      facts.push({ label: "Your commits", value: `${u.ahead} not on ${u.remote}` });
    }
    if (u.dirtyCount > 0) facts.push({ label: "Uncommitted", value: `${u.dirtyCount} file${u.dirtyCount === 1 ? "" : "s"}` });

    if (u.inProgress !== "") {
      notices.push({
        tone: "error",
        line: `${capitalise(u.inProgress)} is in progress in this folder.`,
        next: "Finish or undo it first.",
        blocking: true,
      });
    }
    if (u.branch === "") {
      notices.push({
        tone: "error",
        line: "This folder isn't on a branch.",
        next: "Check out a branch first.",
        blocking: true,
      });
    }
    if (u.remoteError !== "") {
      notices.push({ tone: "warn", line: `Couldn't reach ${u.remote}.`, next: "The run tries again.", blocking: false });
    }
    if (u.shallow) {
      notices.push({ tone: "info", line: "The first pull downloads the full history, which takes a minute.", blocking: false });
    }
  }

  notices.push(...buildNotices(i));
  return { facts, notices, blocked: notices.some((n) => n.blocking) };
}

/**
 * How far behind, stated exactly or not at all: `4 new commits`, `Up to date`,
 * `New commits available` when the count is not knowable without fetching,
 * and "" when the remote could not be read (a notice says so instead).
 */
function pullFact(u: UpdateState): string {
  if (u.remoteError !== "") return "";
  if (u.behind === undefined) return "New commits available";
  if (u.behind === 0) return "Up to date";
  return `${u.behind} new commit${u.behind === 1 ? "" : "s"}`;
}

function capitalise(s: string): string {
  return s.length === 0 ? s : s[0]!.toUpperCase() + s.slice(1);
}
