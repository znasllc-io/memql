import { rowNumber, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { boolOr, flatten, stringsOf } from "../../../kit/rows";
import { LINK_STATES, type LinkState } from "../links";

// A backup -- one folder on one machine, kept arriving in the Library -- and
// the pure readings every surface over it takes.
//
// PURE, and separate from every component, for the reason apps/fleet/rows.ts
// is: a projection asserted through render() is asserted through three layers
// that can each fail for unrelated reasons. Everything here is a function of
// rows and is unit-testable with no browser, no cluster and no React.
//
// ===========================================================================
// EVERY PROJECTION FLATTENS FIRST
// ===========================================================================
// A row reaches these functions from the SEED (a named query's result, already
// shape-flattened) and from the SUBSCRIPTION fold (a CDC envelope, which may
// carry a `payload`-wrapped form). The two paths have to produce the same
// object or a backup would render one way on load and another the moment its
// next sweep lands.

export const WATCHED_FOLDER_CONCEPT = "v1:library:watchedFolder";

/** The concept's `originState` enum, verbatim and in its order. ABSENT is not
 *  a member: "no cockpit has reported yet" is its own answer, spelled "". */
export const ORIGIN_STATES = ["ok", "missing", "unreadable", "refused_by_policy"] as const;
export type OriginState = (typeof ORIGIN_STATES)[number];

export interface BackupRow {
  id: string;
  ownerUserId: string;
  /** v1:worker:registration.id of the machine that does the watching. */
  workerId: string;
  /** Absolute path on THAT machine. The engine never interprets it. */
  localPath: string;
  /** v1:library:folder.id it files into; "" = the Library root. */
  folderId: string;
  status: "active" | "paused";
  excludeGlobs: string[];
  includeHidden: boolean;
  archived: boolean;
  /** "" when no cockpit has reported yet -- NOT "ok". */
  originState: OriginState | "";
  lastSweepAt: string;
  lastSweepError: string;
  filesSeen: number;
  bytesSeen: number;
}

export function backupFromRow(raw: Row): BackupRow {
  const row = flatten(raw);
  const status = rowString(row, "status");
  const origin = rowString(row, "originState");
  return {
    id: rowString(row, "id"),
    ownerUserId: rowString(row, "ownerUserId"),
    workerId: rowString(row, "workerId"),
    localPath: rowString(row, "localPath"),
    folderId: rowString(row, "folderId"),
    // An UNRECOGNISED status reads as active, which is the reading that
    // asserts least: a backup nobody paused is running, and rendering an
    // unknown value as paused would tell somebody their files are not being
    // copied when they are.
    status: status === "paused" ? "paused" : "active",
    excludeGlobs: stringsOf(row, "excludeGlobs"),
    includeHidden: boolOr(row, "includeHidden", false),
    archived: boolOr(row, "archived", false),
    // An unrecognised origin state reads as "" -- not reported -- for the
    // reason linkStateOf does the same: a value this build cannot name is one
    // it cannot describe, and "waiting to hear" is the honest rendering of it.
    originState: (ORIGIN_STATES as readonly string[]).includes(origin) ? (origin as OriginState) : "",
    lastSweepAt: rowString(row, "lastSweepAt"),
    lastSweepError: rowString(row, "lastSweepError"),
    filesSeen: rowNumber(row, "filesSeen") ?? 0,
    bytesSeen: rowNumber(row, "bytesSeen") ?? 0,
  };
}

/** What a create or an edit supplies. The two fields an EDIT cannot change --
 *  the machine and the path -- are the backup's identity: they are the
 *  (machine, path) key every re-push is matched on, so changing them would
 *  orphan the machine's local ledger and start the whole folder again as new
 *  files. */
export interface NewBackup {
  workerId: string;
  localPath: string;
  folderId: string;
  excludeGlobs: string[];
  includeHidden: boolean;
}

// ---------------------------------------------------------------------------
// The link, and what it is saying
// ---------------------------------------------------------------------------

/**
 * How a backup's link reads.
 *
 * ONE value, because the surface draws one thing -- the link between the
 * machine and the Library folder -- and a link cannot be two states at once.
 */
export type LinkTone = "paused" | "waiting" | "refused" | "broken" | "behind" | "settled";

/**
 * Which tone a backup's link carries.
 *
 * THE ORDER IS THE DECISION, and the top two rungs are not about severity:
 *
 *  - `paused` wins over every fault, because the person turned this off. The
 *    cockpit stops sweeping when they do, so whatever `originState` holds is
 *    the last thing seen BEFORE the pause -- and painting old news as a
 *    current alarm on something somebody deliberately stopped is the fastest
 *    way to teach them to ignore the colour. The facts line still carries the
 *    last state and when it was seen, which is the honest place for it.
 *  - `waiting` wins over the rest for the absent-is-not-a-state reason the
 *    whole epic turns on: a backup no cockpit has reported on is neither
 *    working nor broken, and claiming either would be inventing evidence.
 *
 * Below those, worst wins: a machine that refused the path, then an origin
 * that is gone, then bytes that have not arrived.
 */
export function linkToneOf(backup: BackupRow, worst: LinkState | ""): LinkTone {
  if (backup.status === "paused") return "paused";
  if (backup.originState === "") return "waiting";
  if (backup.originState === "refused_by_policy") return "refused";
  if (backup.originState === "missing" || backup.originState === "unreadable") return "broken";
  if (worst === "origin_gone") return "broken";
  if (worst === "stale") return "behind";
  return "settled";
}

/** What each tone is called, where the eye lands first. */
export const TONE_LABEL: Record<LinkTone, string> = {
  paused: "Paused",
  waiting: "Waiting for this machine",
  refused: "This machine said no",
  broken: "Needs a look",
  behind: "Catching up",
  settled: "Backed up",
};

/**
 * What each tone MEANS, in one sentence, said to the person rather than about
 * the system. Every fault sentence names the repair and where it lives,
 * because each of these is fixed somewhere other than this screen.
 */
export const TONE_SENTENCE: Record<LinkTone, string> = {
  paused: "Nothing is being copied while this is paused. Everything already here stays.",
  waiting:
    "This machine has not checked in since the backup was set up. It starts on its next sweep and reports what it finds at that path.",
  refused:
    "The machine's own policy does not allow that folder. Add the path to its policy.yaml and the next sweep picks it up.",
  broken: "Something at the other end needs attention. The details are below.",
  behind: "This machine has newer files that have not arrived here yet.",
  settled: "Everything at that path has arrived, as of the last check.",
};

/**
 * What the ORIGIN state means, said the same way. Separate from the tone
 * sentence because a paused backup still has one of these to report, and
 * because `ok` is a real thing to be able to say.
 */
/**
 * What the ORIGIN state means, said the same way, and NAMING THE REPAIR --
 * because when one of these is showing it is the only sentence on the row.
 *
 * These replace the tone sentence rather than joining it. A fault has exactly
 * one thing worth saying, and the first version of this surface said it twice:
 * a vague tone sentence ("Something at the other end needs attention") above a
 * specific one, which is the shape that teaches people to stop reading either.
 */
export const ORIGIN_SENTENCE: Record<OriginState, string> = {
  ok: "The folder was there and readable.",
  missing:
    "Nothing is at that path any more. Point this backup at where the folder went, or stop it -- everything already copied here stays either way.",
  unreadable:
    "The folder is there and that machine cannot read it, so a permission or a volume that is not mounted. Nothing new arrives until it can.",
  refused_by_policy:
    "The machine's own policy does not list that path. Add it to that machine's policy.yaml and the next sweep picks it up.",
};

/** Whether an origin state is one the person has to do something about.
 *  `ok` is not, and absent is not -- absent is "nobody has looked yet", which
 *  the waiting tone already says better than this could. */
export function isOriginFault(state: OriginState | ""): state is "missing" | "unreadable" | "refused_by_policy" {
  return state === "missing" || state === "unreadable" || state === "refused_by_policy";
}

// ---------------------------------------------------------------------------
// Which files belong to a backup
// ---------------------------------------------------------------------------

/**
 * Whether a file was pushed from THIS backup: same machine, and a path at or
 * beneath the watched folder.
 *
 * MATCHED ON (machine, path), NOT on the destination folder, and the
 * difference is the reliability of the whole badge. A person can put anything
 * they like in the Library folder a backup files into -- a browser upload, a
 * file dragged in from elsewhere -- and none of it has an origin to be stale
 * against. Rolling the folder up would put "changed on the machine" on a
 * backup because of a file that came from no machine at all.
 *
 * The separator check is what stops `/Users/a/Clients` claiming
 * `/Users/a/Clients2/report.pdf`. Both separators are accepted because the
 * path is whatever the reporting machine uses, and a Windows cockpit reports
 * backslashes.
 */
export function fileBelongsToBackup(
  file: { uploadedFromWorkerId: string; uploadedFromPath: string },
  backup: Pick<BackupRow, "workerId" | "localPath">,
): boolean {
  if (file.uploadedFromWorkerId === "" || file.uploadedFromWorkerId !== backup.workerId) return false;
  const root = backup.localPath.replace(/[/\\]+$/, "");
  if (root === "") return false;
  const path = file.uploadedFromPath;
  if (path === root) return true;
  return path.startsWith(root + "/") || path.startsWith(root + "\\");
}

/**
 * The worst link state among a backup's own files, or "" when none of them is
 * tracked.
 *
 * Shares LINK_STATES' ordering with the Files rollup rather than restating it:
 * "worst last" is a property of that array, and a second ordering here would
 * be a second thing to keep in step.
 */
export function worstFileState(states: readonly (LinkState | "")[]): LinkState | "" {
  let worst: LinkState | "" = "";
  for (const state of states) {
    if (state === "") continue;
    if (worst === "" || LINK_STATES.indexOf(state) > LINK_STATES.indexOf(worst)) worst = state;
  }
  return worst;
}

/**
 * What counts as a CHANGE to a backup -- the arrival cue's contract.
 *
 * DELIBERATELY NOT `lastSweepAt`, `filesSeen` or `bytesSeen`. A sweep touches
 * all three on a schedule for every backup forever, so naming any of them
 * would strobe this list on the sweep's own cycle -- the heartbeat rule
 * (clients/os/README.md), and the same reason the concept's own field doc
 * gives for rendering `lastSweepAt` continuously instead.
 *
 * The counts still RE-RENDER when they move: the fold runs on every snapshot
 * and only the CUE is driven by this string. A new figure appearing without a
 * flash is exactly the wanted behaviour, and the campaigns app pins the pair.
 */
export function backupFingerprint(backup: BackupRow): string {
  return [
    backup.status,
    backup.originState,
    backup.localPath,
    backup.folderId,
    backup.archived ? "archived" : "",
    backup.lastSweepError,
  ].join(" ");
}
