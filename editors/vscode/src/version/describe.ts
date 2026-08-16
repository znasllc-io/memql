// A cluster's version, in words.
//
// SIX RENDER STATES, and the reason there are six rather than three is that
// "we do not know" splits three ways -- each asking the operator for something
// different, and each a distinct way of NOT being up to date:
//
//   unknown        nothing has reported a version for this cluster at all.
//                  The remedy is to connect it, or install it from the wizard.
//   unfetched      we know what the cluster runs but not what exists. Offline,
//                  or the release listing failed. Nothing is wrong with the
//                  cluster; we simply cannot say whether it is behind.
//   notComparable  the cluster reported something that is not a release -- a
//                  branch, a sha, the `0.15.0-<epoch>` build stamp. The value
//                  is real and is shown, but it cannot be placed on the
//                  release line.
//
// COLLAPSING ANY OF THEM INTO "current" IS THE FAILURE THIS EPIC EXISTS TO
// PREVENT, and a test walks every state asserting none of them claims to be up
// to date. It is the same discipline `compare.ts` applies one layer down: when
// in doubt, say so, because "I cannot tell" sends an operator to look and "up
// to date" does not.
//
// Shared by the Clusters surfaces (memql#3995) and the Deployments surfaces
// (memql#3996) so the two cannot disagree about what a state means or what it
// is called. The alternative -- each surface deciding for itself -- is how one
// of them ends up rendering an unfetched listing as current.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import { compareVersions } from "./compare.js";
import { latestRelease, type ReleaseListing } from "./releaseCache.js";

export type VersionRenderState =
  /** Nothing has reported a version for this cluster. */
  | "unknown"
  /** Known, and it is the newest release. */
  | "current"
  /** Known, and a newer release exists. The only state that offers an upgrade. */
  | "behind"
  /** Known, and newer than anything published. A locally built cluster. */
  | "ahead"
  /** Known, but not a release -- a branch, a sha, a build stamp. */
  | "notComparable"
  /** Known, but the newest release is not. Offline, or the listing failed. */
  | "unfetched";

export interface VersionDescription {
  state: VersionRenderState;
  /**
   * Compact, for a tree-row description beside an endpoint. Empty ONLY when
   * nothing is recorded -- a row that appended "unknown" to every cluster on a
   * fresh install would be noise, and the connection page says the word where
   * there is room to explain it.
   */
  short: string;
  /** A full sentence for a tooltip or a page fact. Never empty. */
  sentence: string;
  /** The newest known release, when one is known. */
  latest?: string;
  /**
   * True ONLY in the `behind` state. It gates a button that moves a cluster,
   * so every other state must leave it off rather than offering an action
   * nothing supports.
   */
  upgradeAvailable: boolean;
}

export interface DescribeVersionInput {
  /** The cluster's recorded version (memql#3990). */
  recorded: string | undefined;
  /** The release listing, or undefined when nothing has been fetched. */
  listing: ReleaseListing | undefined;
}

// The failure is disclosed even when a STALE listing let the comparison
// succeed: serve-stale-on-failure exists so a known-newer release does not
// vanish when the network does, and an operator reading "v0.18.0 available"
// should still know the listing is not fresh.
function staleness(listing: ReleaseListing | undefined): string {
  const error = listing?.error;
  return error === undefined || error === "" ? "" : ` The release listing is not current: ${error}`;
}

export function describeVersion(input: DescribeVersionInput): VersionDescription {
  const recorded = (input.recorded ?? "").trim();
  if (recorded === "") {
    return {
      state: "unknown",
      short: "",
      sentence:
        "This cluster's release is unknown -- nothing has reported one yet. Connecting to it, or installing it from this editor, records one.",
      upgradeAvailable: false,
    };
  }

  const latest = latestRelease(input.listing);
  const stale = staleness(input.listing);

  if (latest === undefined) {
    return {
      state: "unfetched",
      short: recorded,
      sentence:
        `This cluster records ${recorded}. Whether a newer release exists is not known` +
        (stale === "" ? " -- the release listing has not been fetched." : `.${stale}`),
      upgradeAvailable: false,
    };
  }

  switch (compareVersions(recorded, latest)) {
    case "behind":
      return {
        state: "behind",
        short: `${recorded} - ${latest} available`,
        sentence: `This cluster records ${recorded}, and ${latest} has been released.${stale}`,
        latest,
        upgradeAvailable: true,
      };
    case "current":
      return {
        state: "current",
        short: recorded,
        sentence: `This cluster records ${recorded}, which is the newest release.${stale}`,
        latest,
        upgradeAvailable: false,
      };
    case "ahead":
      return {
        state: "ahead",
        short: recorded,
        // Not an error and not phrased as one: this is what a locally built
        // cluster looks like, and offering it an "upgrade" to an older release
        // would be wrong.
        sentence: `This cluster records ${recorded}, which is ahead of the latest published release (${latest}).${stale}`,
        latest,
        upgradeAvailable: false,
      };
    default:
      return {
        state: "notComparable",
        // Shown rather than hidden: it is what the cluster actually said, and
        // an operator who sees `main` learns more from it than from a blank.
        short: recorded,
        sentence: `This cluster reports ${recorded}, which does not name a release, so it cannot be compared with the published releases.`,
        latest,
        upgradeAvailable: false,
      };
  }
}
