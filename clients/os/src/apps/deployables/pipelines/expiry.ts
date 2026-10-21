import { useCallback, useEffect, useMemo, useState } from "react";
import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";
import { useOsConnection } from "../../../live/connection";
import { flatten } from "../../../kit/rows";
import type { RunFileRow, StepRow } from "./rows";

interface ExpiryRead {
  key: string;
  source: ReturnType<typeof useOsConnection>;
  files: ReadonlyMap<string, string>;
  loading: boolean;
  error: string;
}

const NO_FILES: ReadonlyMap<string, string> = new Map();

/** Read the native retirement receipts only for files absent from the Library.
 * A deletion event changes `files`, while AutoRefresh recovers a lost receipt
 * read. Unknown and unavailable are never presented as deliberate expiry. */
export function useExpiredRunFiles(runId: string, steps: readonly StepRow[], files: ReadonlyMap<string, RunFileRow>, ready: boolean) {
  const connection = useOsConnection();
  const missing = useMemo(() => [...new Set(steps.flatMap(s => [s.logFileId, ...s.artifactFileIds]))]
    .filter(id => id !== "" && !files.has(id)).sort(), [steps, files]);
  const selection = JSON.stringify(missing);
  const key = JSON.stringify([runId, selection]);
  const [revision, setRevision] = useState(0);
  const refresh = useCallback(() => setRevision(value => value + 1), []);
  const [read, setRead] = useState<ExpiryRead>({ key: "", source: null, files: NO_FILES, loading: false, error: "" });
  useEffect(() => {
    if (!connection || !ready || missing.length === 0) return;
    const abort = new AbortController();
    setRead(previous => ({ key, source: connection, files: previous.key === key && previous.source === connection ? previous.files : NO_FILES, loading: true, error: "" }));
    void (async () => {
      const found = new Map<string, string>();
      for (let offset = 0; offset < missing.length; offset += 128) {
        const batch = missing.slice(offset, offset + 128);
        const result = await connection.query.pipelinesExpiredArtifacts({ runId, fileIds: batch }, { signal: abort.signal });
        if (abort.signal.aborted) return;
        const row = result.rows()[0];
        const entries = row ? flatten(row)["files"] : null;
        if (!Array.isArray(entries)) throw Error("Artifact availability could not be checked.");
        for (const entry of entries) {
          if (!entry || typeof entry !== "object") throw Error("Artifact availability could not be checked.");
          const fileId = rowString(entry as Row, "fileId");
          if (!batch.includes(fileId)) throw Error("Artifact availability could not be checked.");
          found.set(fileId, rowString(entry as Row, "name"));
        }
      }
      if (!abort.signal.aborted) setRead({ key, source: connection, files: found, loading: false, error: "" });
    })().catch(() => {
      if (!abort.signal.aborted) setRead(previous => ({ key, source: connection, files: previous.key === key && previous.source === connection ? previous.files : NO_FILES,
        loading: false, error: "Artifact availability could not be checked." }));
    });
    return () => abort.abort();
  // The serialized selection prevents the same missing identities from
  // restarting a read when another file's live metadata changes.
  }, [connection, runId, key, selection, ready, revision]);
  const current = read.key === key && read.source === connection ? read : null;
  return { files: current?.files ?? NO_FILES, loading: current?.loading ?? false,
    error: current?.error ?? "", refresh,
    needsRefresh: ready && missing.length > (current?.files.size ?? 0) };
}
