// The checks on the install / repair form (memql#4195), as a compact list.
//
// The wizard already KNOWS these facts before a run starts -- the installer's
// own files either load or the run refuses at its first step, and a password
// either will be asked for or not -- so it says them before Install is
// pressed rather than at the moment each one bites.
//
// PURE PROJECTION: the panel gathers the inputs (graph load, `sudo -n` probe,
// receipt read) and this module only words them, so the wording is testable
// under bare `node --test` (cmd/memql-lsp/vscodeimportrule_test.go).
//
// `PreflightItem` is the older row shape the rebuild and update checklists
// (rebuildPreflight.ts, updatePreflight.ts) still use; the install form uses
// `PreflightCheck`.

import type { ImageSource } from "../install/receipt.js";

export interface PreflightItem {
  label: string;
  /** "ok" renders quiet; "attention" renders emphasised. Never a blocker -- the run itself enforces. */
  state: "ok" | "attention";
  detail: string;
}

export interface PreflightInputs {
  action: "install" | "installGuided" | "repair";
  /** The graph document's fate: step count, or why it could not be read. */
  graph: { ok: true; steps: number; needsElevation: boolean } | { ok: false; error: string };
  /** Whether sudo would run without asking (or the process is root). */
  sudoFree: boolean;
  /**
   * Which lane set this machine's node images last (memql#4246).
   *
   * `checkout` is the only value that produces a line, because it is the only
   * one where this run CROSSES a lane. "" is no evidence -- a machine with no
   * `clusterUp` entry -- and is never read as `released`.
   */
  imageSource?: ImageSource | "";
  /** The release the cluster is returned to. Empty renders as "release". */
  releasedTag?: string;
}

/**
 * One check on the install form, as the compact list draws it: a label and
 * ONE word for its state, with a short line only where there is something to
 * act on or expect.
 *
 * WHAT THE LIST NO LONGER SAYS. "Install graph -- 16 steps, loaded. Every step
 * verifies first and skips when already satisfied", "No step needs elevation"
 * and "sudo on this machine runs without asking" were true and asked nothing
 * of anyone; a list of rows that all read OK teaches a reader to skip the one
 * that does not. So a check that has nothing to say is not drawn -- the
 * password row appears only when a password will be asked for, and the image
 * row only when this run crosses from a checkout build to a release.
 */
export interface PreflightCheck {
  label: string;
  /** The state in one word: "Ready", "Needed", "Missing", "Replaced". */
  word: string;
  tone: "ok" | "attention" | "error";
  /** One short line, when there is something to do or expect. */
  note?: string;
}

/** The checks for the install or repair form. Pure; see PreflightInputs. */
export function preflightChecks(inputs: PreflightInputs): PreflightCheck[] {
  const checks: PreflightCheck[] = [];

  checks.push(
    inputs.graph.ok
      ? { label: "Installer", word: "Ready", tone: "ok" }
      : {
          label: "Installer",
          word: "Missing",
          tone: "error",
          // The read error itself goes to the MemQL Install output; what a
          // person can do about a missing file inside the extension is this.
          note: "Reinstall the MemQL extension.",
        },
  );

  if (inputs.graph.ok && inputs.graph.needsElevation && !inputs.sudoFree) {
    checks.push({
      label: "Your password",
      word: "Needed",
      tone: "attention",
      note: "Asked once, to update the hosts file and trust a local certificate.",
    });
  }

  // THE LANE CROSSING (memql#4246): a run over a cluster on checkout-built
  // images returns it to released ones, and a developer whose own edits
  // quietly stopped running has no reason to go and look for why. Only when it
  // is actually a crossing.
  if (inputs.imageSource === "checkout") {
    const tag = (inputs.releasedTag ?? "").trim();
    checks.push({
      label: "Images",
      word: "Replaced",
      tone: "attention",
      note: `Your checkout build is replaced by ${tag === "" ? "the release" : `release ${tag}`}. Rebuild from checkout brings it back.`,
    });
  }

  return checks;
}

/** Whether the checks allow the run to start at all (the installer's own files are there). */
export function preflightBlocks(checks: readonly PreflightCheck[]): boolean {
  return checks.some((check) => check.tone === "error");
}
