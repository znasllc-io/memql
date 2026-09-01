import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../kit/rows";

// The origin-link states, and the folder rollup over them (epic memql#4783).
//
// Pure, so the rollup can be checked on fixtures with no cluster and no
// browser -- which matters more here than usual, because the interesting cases
// are all about ABSENCE and an absent value renders as nothing at all.

/** The states the engine's `linkState` enum can hold, worst LAST -- the order
 *  is the rollup's own comparison and is not alphabetical by accident. */
export const LINK_STATES = ["synced", "stale", "origin_gone"] as const;
export type LinkState = (typeof LINK_STATES)[number];

/** What each state is called on screen. Short, because it sits in a chip
 *  beside a filename. */
export const LINK_LABEL: Record<LinkState, string> = {
  synced: "in sync",
  stale: "changed on the machine",
  origin_gone: "gone from the machine",
};

/** What each state MEANS, for the title attribute and the inspector. */
export const LINK_SENTENCE: Record<LinkState, string> = {
  synced: "Matched the file on that machine when it was last checked.",
  stale: "That machine has a newer copy of this file that has not arrived here yet.",
  origin_gone:
    "It is no longer at that path on that machine. This copy is untouched -- a deletion at the origin flags the copy here, it never removes it.",
};

/**
 * The link state of a file row, or "" when it has none.
 *
 * ABSENT IS NOT A STATE, and this is the one thing a caller gets wrong. A
 * browser upload has no origin to link to; every file stored before the field
 * existed has no member at all. Both are "we are not tracking this", which is
 * a different answer from "we are tracking it and it is fine" -- and a fold
 * that read absence as `synced` would put a green in-sync badge on every file
 * in the Library.
 */
export function linkStateOf(raw: Row): LinkState | "" {
  const value = rowString(flatten(raw), "linkState");
  return (LINK_STATES as readonly string[]).includes(value) ? (value as LinkState) : "";
}

/**
 * The worst state among a set -- what a folder's badge says.
 *
 * A folder is not a file and has no link of its own, so a rollup is the only
 * honest thing it can show: "something in here needs looking at". It reports
 * the WORST rather than a count or a mixture, because the reason to put a mark
 * on a folder is to make somebody open it, and a folder holding one missing
 * file and forty synced ones needs opening exactly as much as one holding
 * forty missing files.
 *
 * A folder with NO tracked files at all answers "" and renders nothing. That
 * is most folders, and a badge on all of them would be noise that made the few
 * that matter invisible.
 */
export function rollupLinkState(states: readonly (LinkState | "")[]): LinkState | "" {
  let worst: LinkState | "" = "";
  for (const state of states) {
    if (state === "") continue;
    if (worst === "" || LINK_STATES.indexOf(state) > LINK_STATES.indexOf(worst)) {
      worst = state;
    }
  }
  return worst;
}

/**
 * Roll every folder up, including through its ancestors.
 *
 * A file three levels down flags every folder above it, because somebody
 * looking at the top of a tree has to be able to see that there is something
 * to find in it -- a badge that stopped at the immediate parent would make the
 * root of a deep tree look clean.
 *
 * `parentOf` returns "" at a root. A cycle is impossible in the folder model
 * and is guarded anyway: a corrupt parent chain would otherwise hang the
 * render rather than draw a slightly wrong badge.
 */
export function foldFolderLinkStates(
  files: readonly { folderId: string; state: LinkState | "" }[],
  parentOf: (folderId: string) => string,
): Map<string, LinkState> {
  const out = new Map<string, LinkState>();
  for (const file of files) {
    if (file.state === "") continue;
    let folderId = file.folderId;
    const seen = new Set<string>();
    while (folderId !== "" && !seen.has(folderId)) {
      seen.add(folderId);
      const current = out.get(folderId);
      const next = rollupLinkState([current ?? "", file.state]);
      if (next !== "") out.set(folderId, next);
      folderId = parentOf(folderId);
    }
  }
  return out;
}
