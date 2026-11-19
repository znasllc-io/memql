import { useMemo } from "react";
import { Concepts, getRowByConceptAndId, rowNumber, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useSessionIfPresent } from "../../chrome/access";
import { flatten } from "../../kit/rows";
import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import { holds } from "../../system/roles";
import type { FileShare, SharePerson } from "./sharing";

// FILE SHARING'S THREE READS (the engine's library sharing, Nov 2026).
//
//   the directory   who a file can go to; read once per session, no events
//                   (it is a computed answer over people, not a concept's rows)
//   a file's shares v1:library:share, owned by the SHARER, so a share, a
//                   renewal and a stop reach the sharer's stream as events
//   shared with me  v1:library:shareNotice, owned by the RECIPIENT, so a file
//                   shared with them appears while they look
//
// The OS only asks: the engine re-checks the part, the organization and the
// share itself on every act and every download.

/** The part that lets a person share their own files (dsl/rbac/seeds.memql). */
export const SHARE_PART = "app:files/share";

/** Whether this session may share files, recomputed when the effective set changes. */
export function useCanShareFiles(): boolean {
  const epoch = useSessionIfPresent()?.accessEpoch ?? 0;
  // `epoch` is the reactivity signal, not an input (see deployables/parts.ts).
  return useMemo(() => holds("execute", SHARE_PART), [epoch]);
}

/** The people a file can go to. Null until the read settles: unread is not "nobody". */
export function useShareDirectory(enabled: boolean): { people: SharePerson[] | null; error: string } {
  const feed = useLiveCollection<Row>(enabled ? "files:share-directory" : null, (connection) => ({
    concept: Concepts.IDENTITY_USER,
    actions: [],
    paged: false,
    seed: async (_cursor, signal) => {
      const result = await connection.query.libraryShareDirectory({}, { signal });
      return { rows: result.rows().map((row) => ({ ...row, id: rowString(row, "userId") })), nextCursor: "" };
    },
  }));
  const { rows, state, error } = feed.snapshot;
  const people = useMemo(
    () => rows.map((row) => ({ userId: rowString(row, "userId"), displayName: rowString(row, "displayName") })).filter((p) => p.userId !== ""),
    [rows],
  );
  return { people: state === "seeding" || !enabled ? null : people, error: error ?? "" };
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
