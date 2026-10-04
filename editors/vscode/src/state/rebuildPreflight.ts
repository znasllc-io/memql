// What a rebuild from the checkout will build, and what needs attention first
// (memql#4246).
//
// PURE: the panel gathers the facts, this states them. It used to be a seven
// row "Before it runs" checklist, most of it "OK" rows that asked nothing of
// anybody -- Docker answers, here is the path you already know, a first build
// takes minutes. Those rows buried the two that matter, so the shape is now:
//
//   FACTS, which say what will be built: the folder and the commit in it.
//   NOTICES, only for what is not fine, one sentence each. A notice that makes
//   the rebuild pointless is BLOCKING, and the page then offers no Rebuild at
//   all -- an act whose only outcome is a failure is absent, never disabled --
//   but the fix instead (start Docker and check again; repair the install).
//
// THE LANE CROSSING IS STILL SAID BEFORE IT HAPPENS. A rebuild moves a cluster
// off released images and onto ones built from a working tree, and nothing
// else announces that; so the first rebuild of a released cluster says so, and
// its mirror is said by every act that crosses back (state/imageLane.ts).
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #4246 #4195

import type { CheckoutState } from "../install/checkoutState.js";
import type { ImageSource } from "../install/receipt.js";
import { checkoutSkew, shortCommit } from "../version/checkoutSkew.js";

export interface RebuildPreflightInputs {
  dockerReachable: boolean;
  checkoutDir: string;
  /** Both files `k3d.dev` itself gates on: a Dockerfile and the local overlay. */
  checkoutIsMemql: boolean;
  /** Absent when git could not read the checkout -- which is its own notice. */
  state?: CheckoutState;
  /** Which lane set the images last. "" is no evidence, never "released". */
  imageSource: ImageSource | "";
  /** The release the cluster would return to. Kept for the record; not said. */
  releasedTag: string;
  /**
   * The commit THIS EXTENSION was packaged from, and whether that build carried
   * uncommitted edits (memql#5076). Both absent for an unpackaged extension.
   */
  extensionCommit?: string;
  extensionDirty?: boolean;
}

/** One fact about what will be built. */
export interface CheckFact {
  label: string;
  value: string;
  /** The editor's monospace: a path, a commit. */
  mono?: boolean;
}

/** The fix a notice offers, when the page can do it. */
export type CheckFix = "checkAgain" | "repair";

/** One thing that is not fine, in one sentence. */
export interface CheckNotice {
  tone: "info" | "warn" | "error";
  line: string;
  /** What to do about it, when that is not a button. */
  next?: string;
  /** The rebuild (or pull) cannot work while this holds: the page withholds the act. */
  blocking: boolean;
  fix?: CheckFix;
}

export interface RebuildCheck {
  facts: CheckFact[];
  notices: CheckNotice[];
  /** Whether any notice is blocking. */
  blocked: boolean;
}

/**
 * The commit a checkout is on, as a fact: `main @ 3f2a9c1 · 3 uncommitted`.
 *
 * A tag or branch names the ref; a detached checkout has none and says only
 * the commit. A clean tree says nothing about uncommitted files.
 */
export function checkoutCommitFact(state: CheckoutState): string {
  const commit = shortCommit(state.commit);
  const ref = state.ref.kind === "detached" || state.ref.name === "" ? commit : `${state.ref.name} @ ${commit}`;
  return state.dirtyCount === 0 ? ref : `${ref} · ${state.dirtyCount} uncommitted`;
}

/** The notices every build from the checkout shares, rebuild and pull alike. */
export function buildNotices(i: RebuildPreflightInputs): CheckNotice[] {
  const notices: CheckNotice[] = [];
  if (!i.dockerReachable) {
    notices.push({
      tone: "error",
      line: "Docker isn't running.",
      next: "Start Docker Desktop, then check again.",
      blocking: true,
      fix: "checkAgain",
    });
  }
  if (!i.checkoutIsMemql) {
    // THE SAME TWO FILES `k3d.dev` GATES ON, so this never passes a folder the
    // run would then refuse -- the one thing a check before a run must not do.
    notices.push({
      tone: "error",
      line: "This folder isn't a MemQL checkout.",
      next: "Repair the install to download it again.",
      blocking: true,
      fix: "repair",
    });
  }
  if (i.state === undefined) {
    notices.push({ tone: "warn", line: "Couldn't read the folder's git status.", blocking: false });
  } else if (i.state.deployDirty) {
    // Manifests do not ride a rebuild, only images do: an edit under deploy/
    // is the one change a developer expects to see and will not.
    notices.push({ tone: "warn", line: "Changes under deploy/ aren't applied. Only code is rebuilt.", blocking: false });
  }
  if (i.imageSource !== "checkout") {
    notices.push({
      tone: "info",
      line: "The cluster switches to your own build.",
      next: "Changing version or repairing returns it to released images.",
      blocking: false,
    });
  }
  // A FACT ONLY WHEN IT MATTERS. The extension and the checkout on different
  // commits is the NORMAL state for a from-source install; it is said here,
  // where the consequence lands (the checkout's scripts run), and nowhere else.
  if (
    checkoutSkew({
      extensionCommit: i.extensionCommit,
      extensionDirty: i.extensionDirty,
      checkoutCommit: i.state?.commit,
    }).state === "diverged"
  ) {
    notices.push({
      tone: "info",
      line: "This extension is from a different commit than your checkout.",
      next: "The build uses the checkout's own scripts.",
      blocking: false,
    });
  }
  return notices;
}

/** Everything the rebuild screen states. */
export function rebuildCheck(i: RebuildPreflightInputs): RebuildCheck {
  const facts: CheckFact[] = [{ label: "Source", value: i.checkoutDir, mono: true }];
  if (i.state !== undefined) facts.push({ label: "Commit", value: checkoutCommitFact(i.state), mono: true });
  const notices = buildNotices(i);
  return { facts, notices, blocked: notices.some((n) => n.blocking) };
}
