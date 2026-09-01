import { useCallback, useEffect, useRef, useState } from "react";

import { useOsConnection } from "../../../live/connection";
import {
  fileHeadFromRow,
  fileVersionFromRow,
  foldVersions,
  type FileHead,
  type VersionHistory,
} from "../versions";

// The version history read (epic memql#4806, design D11).
//
// ===========================================================================
// ON DEMAND, AND THE PANEL SAYS WHEN IT LOOKED
// ===========================================================================
// This is deliberately NOT a live collection, and the routing table is the
// reason rather than an oversight. The Files app's live feeds exist for rows
// written on one replica and read on another -- the folder tree, the artifact
// index -- and both carry broadcast routing rules that say so
// (component/node/routing.go). `v1:library:fileVersion` carries none: a
// version row is written by the bff that took the upload and read by the panel
// that asked for it, and that panel already re-reads the moment a version
// lands.
//
// So the honest surface is a read with a timestamp on it and a way to read
// again -- not a feed this app does not have, and not a caption implying one.
//
// TWO READS, ONE ANSWER. The head is the newest version and lives on the file
// row; the superseded ones are their own rows. Both are needed to draw one
// stack, so both are fetched here and folded in one place -- a panel that
// fetched them separately could render a head from one moment against a
// history from another.

export interface VersionHistoryState {
  history: VersionHistory;
  /** The head row itself, which the download action needs for its size. */
  head: FileHead | null;
  loading: boolean;
  /** The server's own sentence, verbatim. "" when the last read worked. */
  error: string;
  /** When this answer was read. null before the first one lands. */
  readAt: Date | null;
  /** Read again -- the panel's refresh, and what a landed upload calls. */
  refresh: () => void;
}

const EMPTY: VersionHistory = { entries: [], total: 0, shown: 0, truncated: false };

/**
 * Read one file's head and its superseded versions.
 *
 * `fileId` blank (a non-file artifact, or a row whose source ref has not
 * resolved) reads nothing and answers an empty history with no error: there is
 * no failure here, only nothing to ask about.
 */
export function useFileVersions(fileId: string): VersionHistoryState {
  const connection = useOsConnection();
  const [history, setHistory] = useState<VersionHistory>(EMPTY);
  const [head, setHead] = useState<FileHead | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [readAt, setReadAt] = useState<Date | null>(null);
  const [nonce, setNonce] = useState(0);

  // The in-flight read's own identity. A refresh or a change of file while one
  // is running must not let the older answer win: `latest` is compared on
  // arrival and a stale answer is dropped rather than rendered.
  const latest = useRef(0);

  const refresh = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    const query = connection?.query ?? null;
    if (query === null || fileId.trim() === "") {
      setHistory(EMPTY);
      setHead(null);
      setLoading(false);
      return;
    }
    const run = (latest.current += 1);
    let cancelled = false;
    setLoading(true);
    setError("");

    void (async () => {
      try {
        const [headResult, versionResult] = await Promise.all([
          query.libraryFileById({ fileId }),
          query.libraryFileVersionsForFile({ fileId }),
        ]);
        if (cancelled || latest.current !== run) return;
        const headRow = headResult.rows()[0] ?? null;
        const resolved = headRow === null ? null : fileHeadFromRow(headRow);
        setHead(resolved);
        setHistory(foldVersions(resolved, versionResult.rows().map(fileVersionFromRow)));
        setReadAt(new Date());
      } catch (err: unknown) {
        if (cancelled || latest.current !== run) return;
        setError(err instanceof Error ? err.message : String(err));
        // The previous answer is KEPT rather than blanked. A failed refresh
        // means the panel could not look again, not that the history vanished
        // -- and the caption already says when what is on screen was read.
      } finally {
        if (!cancelled && latest.current === run) setLoading(false);
      }
    })();

    return () => {
      cancelled = true;
    };
  }, [connection, fileId, nonce]);

  return { history, head, loading, error, readAt, refresh };
}
