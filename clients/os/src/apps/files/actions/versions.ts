import { useCallback, useEffect, useRef, useState } from "react";

import { useOsConnection } from "../../../live/connection";
import {
  fileHeadFromRow,
  fileVersionFromRow,
  foldVersions,
  type FileHead,
  type VersionHistory,
} from "../versions";

// The app's retained file feed triggers this read whenever the head changes.
// Superseded versions do not broadcast; read them with the head as one answer.
// A visible-panel recovery interval and focus/online wake handle missed events
// and failed reads without requiring a manual refresh.

export interface VersionHistoryState {
  history: VersionHistory;
  /** The head row itself, which the download action needs for its size. */
  head: FileHead | null;
  loading: boolean;
  /** The server's own sentence, verbatim. "" when the last read worked. */
  error: string;
  /** When this answer was read. null before the first one lands. */
  readAt: Date | null;
  /** Invalidate after an upload, or an automatic recovery check. */
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
export function useFileVersions(fileId: string, headRevision = ""): VersionHistoryState {
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
  const scope = useRef({ connection, fileId });

  const refresh = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    if (scope.current.connection !== connection || scope.current.fileId !== fileId) {
      scope.current = { connection, fileId };
      setHistory(EMPTY);
      setHead(null);
      setReadAt(null);
      setError("");
    }
    const query = connection?.query ?? null;
    if (query === null || fileId.trim() === "") {
      setHistory(EMPTY);
      setHead(null);
      setLoading(false);
      setReadAt(null);
      setError("");
      return;
    }
    const run = (latest.current += 1);
    let cancelled = false;
    const controller = new AbortController();
    setLoading(true);
    setError("");

    void (async () => {
      try {
        const [headResult, versionResult] = await Promise.all([
          query.libraryFileById({ fileId }, { signal: controller.signal }),
          query.libraryFileVersionsForFile({ fileId }, { signal: controller.signal }),
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
      controller.abort();
    };
  }, [connection, fileId, headRevision, nonce]);

  return { history, head, loading, error, readAt, refresh };
}
