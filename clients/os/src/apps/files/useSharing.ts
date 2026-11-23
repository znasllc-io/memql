import { useEffect, useMemo, useState } from "react";
import { Concepts, getRowByConceptAndId, renderMemQLValue, rowNumber, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useSessionIfPresent } from "../../chrome/access";
import { useOsConnection } from "../../live/connection";
import { flatten } from "../../kit/rows";
import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import { holds } from "../../system/roles";

// FILE SHARING'S FOUR READS (the engine's library sharing, Nov 2026).
//
//   the directory   who a file can go to; read once per session, no events
//                   (it is a computed answer over people, not a concept's rows)
//   a file's shares v1:library:share, owned by the SHARER, so a share, a
//                   renewal and a stop reach the sharer's stream as events
//   sharing         the same rows across ALL the sharer's files: the Sharing
//                   place (Jesús, Nov 19: "Shared" opens into "Shared with me"
//                   and "Sharing"), live on the same events
//   shared with me  v1:library:shareNotice, owned by the RECIPIENT, so a file
//                   shared with them appears while they look
//
// The OS only asks: the engine re-checks the part, the organization and the
// share itself on every act and every download.

/** Someone a file can go to, as the directory names them. */
export interface SharePerson {
  userId: string;
  displayName: string;
}

/** A group a file can go to: everyone active in it but the sharer. */
export interface ShareGroup {
  groupId: string;
  displayName: string;
  /** Its active people, the sharer included when they are in it. */
  members: number;
}

/** One active share of one file: with a person, or with a group. */
export interface FileShare {
  shareId: string;
  recipientUserId: string;
  /** Set on a group share, which names no one person. */
  recipientGroupId: string;
  recipientDisplayName: string;
  days: number;
  expiresAt: string;
}

/** The part that lets a person share their own files (dsl/rbac/seeds.memql). */
export const SHARE_PART = "app:files/share";

/** Whether this session may share files, recomputed when the effective set changes. */
export function useCanShareFiles(): boolean {
  const epoch = useSessionIfPresent()?.accessEpoch ?? 0;
  // `epoch` is the reactivity signal, not an input (see deployables/parts.ts).
  return useMemo(() => holds("execute", SHARE_PART), [epoch]);
}

/** The people and groups a file can go to. Null until the read settles: unread is not "nobody". */
export function useShareDirectory(enabled: boolean): { people: SharePerson[] | null; groups: ShareGroup[]; error: string } {
  const feed = useLiveCollection<Row>(enabled ? "files:share-directory" : null, (connection) => ({
    concept: Concepts.IDENTITY_USER,
    actions: [],
    paged: false,
    seed: async (_cursor, signal) => {
      const result = await connection.query.libraryShareDirectory({}, { signal });
      // A GROUP WITH NOBODY BUT YOU IS NOT OFFERED: libraryShareFile refuses it
      // ("nobody but you is in that group yet"). `members` counts every active
      // person, you included, so 0 is nobody and 1 may be only you; for those
      // the builtin, asked with the group, answers who a share would reach.
      const reach = new Map(await Promise.all(result.rows()
        .filter((row) => rowString(row, "groupId") !== "" && rowNumber(row, "members") === 1)
        .map(async (row) => [rowString(row, "groupId"), (await connection.query.libraryShareDirectory({ groupId: rowString(row, "groupId") }, { signal })).rows().length] as const)));
      const reaches = (row: Row) => {
        const group = rowString(row, "groupId");
        return group === "" || (rowNumber(row, "members") > 0 && reach.get(group) !== 0);
      };
      // A group's row is keyed apart from a person's, so the two can never
      // share an id in the collection.
      return {
        rows: result.rows().filter(reaches).map((row) => ({ ...row, id: rowString(row, "userId") || `group:${rowString(row, "groupId")}` })),
        nextCursor: "",
      };
    },
  }));
  const { rows, state, error } = feed.snapshot;
  const people = useMemo(
    () => rows.map((row) => ({ userId: rowString(row, "userId"), displayName: rowString(row, "displayName") })).filter((p) => p.userId !== ""),
    [rows],
  );
  const groups = useMemo(
    () => rows.map((row) => ({ groupId: rowString(row, "groupId"), displayName: rowString(row, "displayName"), members: rowNumber(row, "members") }))
      .filter((g) => g.groupId !== ""),
    [rows],
  );
  return { people: state === "seeding" || !enabled ? null : people, groups, error: error ?? "" };
}

/** One file's active shares, live. "" reads nothing. */
export function useFileShares(artifactId: string): FileShare[] {
  const feed = useLiveCollection<Row>(artifactId ? `files:shares:${artifactId}` : null, (connection) => ({
    concept: Concepts.LIBRARY_SHARE,
    seed: async (cursor, signal) => {
      const result = await connection.query.librarySharesForArtifact({ artifactId }, { signal, ...(cursor ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    // A stop is an update that sets revokedAt: it leaves the list on its own event.
    inScope: (raw) => {
      const row = flatten(raw);
      return rowString(row, "artifactId") === artifactId && rowString(row, "revokedAt") === "";
    },
    reread: async (id, signal) => ((await getRowByConceptAndId(connection.query, Concepts.LIBRARY_SHARE, id, { signal })) as Row) ?? null,
  }));
  return useMemo(
    () =>
      feed.snapshot.rows.map(flatten).map((row) => ({
        shareId: rowString(row, "id"),
        recipientUserId: rowString(row, "recipientUserId"),
        recipientGroupId: rowString(row, "recipientGroupId"),
        recipientDisplayName: rowString(row, "recipientDisplayName"),
        days: rowNumber(row, "days"),
        expiresAt: rowString(row, "expiresAt"),
      })),
    [feed.snapshot.rows],
  );
}

/** A file somebody shared with the caller, as its notice names it. */
export interface SharedNotice {
  id: string;
  shareId: string;
  artifactId: string;
  sharerDisplayName: string;
  title: string;
  mimeType: string;
  format: string;
  size: number;
  expiresAt: string;
  endedAt: string;
  createdAt: string;
}

export function noticeFromRow(raw: Row): SharedNotice {
  const row = flatten(raw);
  return {
    id: rowString(row, "id"),
    shareId: rowString(row, "shareId"),
    artifactId: rowString(row, "artifactId"),
    sharerDisplayName: rowString(row, "sharerDisplayName"),
    title: rowString(row, "title"),
    mimeType: rowString(row, "mimeType"),
    format: rowString(row, "format"),
    size: rowNumber(row, "size"),
    expiresAt: rowString(row, "expiresAt"),
    endedAt: rowString(row, "endedAt"),
    createdAt: rowString(row, "createdAt"),
  };
}

/** Whether a notice still stands: not ended, and not past its time. */
export function noticeStands(notice: SharedNotice, now = Date.now()): boolean {
  return notice.endedAt === "" && Date.parse(notice.expiresAt) > now;
}

/** The files shared with the caller, live, while the Shared with me place is open. */
export function useSharedWithMe(enabled: boolean): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>(enabled ? "files:shared-with-me" : null, (connection) => ({
    concept: Concepts.LIBRARY_SHARE_NOTICE,
    seed: async (cursor, signal) => {
      const result = await connection.query.librarySharedWithMe({}, { signal, ...(cursor ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    inScope: (row) => noticeStands(noticeFromRow(row)),
    reread: async (id, signal) => ((await getRowByConceptAndId(connection.query, Concepts.LIBRARY_SHARE_NOTICE, id, { signal })) as Row) ?? null,
  }));
}

/** One of the caller's own active shares, as the Sharing place lists it. */
export interface OutgoingShare {
  id: string;
  artifactId: string;
  /** The file's name when it was shared; "" on a share made before shares carried it. */
  title: string;
  recipientUserId: string;
  /** Set on a group share; the row then names the group, with a group icon. */
  recipientGroupId: string;
  recipientDisplayName: string;
  expiresAt: string;
  revokedAt: string;
  createdAt: string;
  /** Who shared it: the oversight read names them; the caller's own feed leaves these "". */
  ownerUserId: string;
  ownerDisplayName: string;
}

export function outgoingFromRow(raw: Row): OutgoingShare {
  const row = flatten(raw);
  return {
    id: rowString(row, "id"),
    artifactId: rowString(row, "artifactId"),
    title: rowString(row, "title"),
    recipientUserId: rowString(row, "recipientUserId"),
    recipientGroupId: rowString(row, "recipientGroupId"),
    recipientDisplayName: rowString(row, "recipientDisplayName"),
    expiresAt: rowString(row, "expiresAt"),
    revokedAt: rowString(row, "revokedAt"),
    createdAt: rowString(row, "createdAt"),
    ownerUserId: rowString(row, "ownerUserId"),
    ownerDisplayName: rowString(row, "ownerDisplayName"),
  };
}

/** Whether a share still stands: not stopped, and not past its time. */
export function shareStands(share: Pick<OutgoingShare, "revokedAt" | "expiresAt">, now = Date.now()): boolean {
  return share.revokedAt === "" && Date.parse(share.expiresAt) > now;
}

/**
 * Every file the caller is sharing, live, while the Sharing place is open.
 *
 * Through `executeNamed` rather than a generated method: librarySharing is
 * newer than the SDK build a checkout may be linked against, and the call is
 * the same string either way (the shell's path for a construct the bundled SDK
 * does not know yet, as in settings/useLanguage.ts).
 */
export function useSharing(enabled: boolean): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>(enabled ? "files:sharing" : null, (connection) => ({
    concept: Concepts.LIBRARY_SHARE,
    seed: async (cursor, signal) => {
      const result = await connection.query.executeNamed("librarySharing", "query librarySharing()", { signal, ...(cursor ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    // A stop is an update that sets revokedAt, and a renewal one that moves
    // expiresAt: each lands on the row's own event.
    inScope: (row) => shareStands(outgoingFromRow(row)),
    reread: async (id, signal) => ((await getRowByConceptAndId(connection.query, Concepts.LIBRARY_SHARE, id, { signal })) as Row) ?? null,
  }));
}

/**
 * How long a share has left, in words: hours under a day ("18 hours left",
 * rounded up, so 90 minutes reads "2 hours left"), and days above it rounded
 * to the NEAREST day, so sharing for 7 days reads "7 days left" (Jesús, Nov 20:
 * "It should show the number that you're picking").
 *
 * Why nearest and not up: the engine ends a 7-day share exactly 7 x 24 hours
 * after it was made (integrations/library/share.go, shareEnd), and the
 * reading's clock (kit/useNow, one tick a minute) is up to a minute behind,
 * as a cluster's clock can be ahead of this browser's. Either puts the end a
 * little MORE than 7 days away, which rounding up read as "8 days left". Down
 * would read "6 days left" a second after sharing. Nearest stays on 7 until
 * 6.5 days remain, then 6, and so on; every place that counts a share's time
 * (the share view's cards, Files > Sharing) reads it from here.
 */
export function timeLeftLabel(expiresAt: string, now: Date): string {
  const ms = Date.parse(expiresAt) - now.getTime();
  if (!Number.isFinite(ms) || ms <= 0) return "Ended";
  const hours = Math.ceil(ms / 3_600_000);
  if (hours < 24) return hours === 1 ? "1 hour left" : `${hours} hours left`;
  const days = Math.max(1, Math.round(ms / 86_400_000));
  return days === 1 ? "1 day left" : `${days} days left`;
}

/**
 * The names of the files behind shares made before shares carried one (Nov 20
 * 2026), read once each from the caller's own Library -- the sharer owns every
 * file they share, so their own by-id read answers. Shares with a title never
 * reach here, so this costs nothing once those older shares have run out.
 */
export function useShareTitles(shares: readonly OutgoingShare[]): ReadonlyMap<string, string> {
  const connection = useOsConnection();
  const [titles, setTitles] = useState<ReadonlyMap<string, string>>(new Map());
  const missing = [...new Set(shares.filter((share) => share.title === "" && share.artifactId !== "").map((share) => share.artifactId))]
    .filter((id) => !titles.has(id)).sort();
  const key = missing.join(",");
  useEffect(() => {
    const query = connection?.query ?? null;
    if (query === null || key === "") return;
    const abort = new AbortController();
    void Promise.all(key.split(",").map(async (artifactId) => {
      try {
        const row = (await query.libraryArtifactById({ artifactId }, { signal: abort.signal })).rows()[0];
        return [artifactId, row ? rowString(flatten(row), "title") : ""] as const;
      } catch {
        return [artifactId, ""] as const;
      }
    })).then((found) => {
      if (abort.signal.aborted) return;
      setTitles((prev) => new Map([...prev, ...found.filter(([, title]) => title !== "")]));
    });
    return () => abort.abort();
  }, [connection, key]);
  return titles;
}

/** When a share ends: one of the drop-down's lengths, or a picked day. */
export type ShareEnd = { days: number } | { until: Date };

/**
 * The libraryShareFile call for one person or group, rendered here because
 * its group and date arguments are newer than the SDK build a checkout may be
 * linked against (useSharing's executeNamed note).
 */
export function shareFileCall(artifactId: string, to: { userId: string } | { groupId: string }, end: ShareEnd): string {
  const args = [
    `artifactId: ${renderMemQLValue(artifactId)}`,
    "userId" in to ? `recipientUserId: ${renderMemQLValue(to.userId)}` : `recipientGroupId: ${renderMemQLValue(to.groupId)}`,
    "days" in end ? `days: ${renderMemQLValue(end.days)}` : `until: ${renderMemQLValue(end.until.toISOString())}`,
  ];
  return `builtin libraryShareFile(${args.join(", ")})`;
}

/**
 * Every active share the caller may see and stop -- other people's too, by
 * rank (librarySharesOverseen, item 13 of the Nov 19 2026 call): the Sharing
 * page's "Shared by" drop-down when it is not on "Me". Other people's shares
 * send no events to the caller's stream, so this is read, not live, and
 * refreshed on the shared AutoRefresh cadence and after a Stop.
 */
export function useSharesOverseen(enabled: boolean): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>(enabled ? "files:shares-overseen" : null, (connection) => ({
    concept: Concepts.LIBRARY_SHARE,
    actions: [],
    paged: false,
    seed: async (_cursor, signal) => {
      const result = await connection.query.executeNamed("librarySharesOverseen", "builtin librarySharesOverseen()", { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
  }));
}

/** The other people in an oversight read, by id, for "Shared by": name order, the caller left out. */
export function overseenSharers(rows: readonly Row[]): Array<[string, string]> {
  const people = new Map<string, string>();
  for (const raw of rows) {
    const row = flatten(raw);
    const share = outgoingFromRow(raw);
    if (row["mine"] === true || share.ownerUserId === "" || !shareStands(share)) continue;
    people.set(share.ownerUserId, share.ownerDisplayName || "Unnamed person");
  }
  return [...people.entries()].sort((a, b) => a[1].localeCompare(b[1]) || a[0].localeCompare(b[0]));
}
