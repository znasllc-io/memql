import { useCallback, useEffect, useRef, useState } from "react";
import type { Connection } from "@znasllc-io/memql-sdk-core/client";

import type { OsAppProps } from "../../system/registry";
import { artifactFromRow, isContentKind, type ArtifactRow } from "./rows";

const LOOKUP_TIMEOUT_MS = 20_000;
type Target = { intentId: string; artifactId: string; fileId: string };
type Destination = Target & {
  connection: Connection | null;
  status: "loading" | "ready" | "missing" | "error";
  row: ArtifactRow | null;
  error: string;
  indexed: boolean;
  resolvedArtifactId: string;
};

function matches(row: ArtifactRow, target: Target): boolean {
  return target.artifactId !== "" ? row.id === target.artifactId
    : row.kind === "file" && row.sourceConceptRef === target.fileId;
}

/** A deliberate file link is one owner-checked read, independent of Library paging. */
export function useFileDestination(connection: Connection | null, intent: OsAppProps["intent"], content: readonly ArtifactRow[]) {
  const artifactId = typeof intent?.payload.artifactId === "string" ? intent.payload.artifactId : "";
  const fileId = typeof intent?.payload.fileId === "string" ? intent.payload.fileId : "";
  const intentId = intent?.id ?? "";
  const [destination, setDestination] = useState<Destination | null>(null);
  const [attempt, setAttempt] = useState(0);
  const contentRef = useRef(content);
  contentRef.current = content;
  const pending = useRef<AbortController | null>(null);
  const dismissed = useRef("");

  useEffect(() => {
    if (!intentId || dismissed.current === intentId || (!artifactId && !fileId)) return;
    const target = { intentId, artifactId, fileId };
    const known = contentRef.current.find(row => matches(row, target));
    if (known) {
      setDestination({ ...target, connection, status: "ready", row: known, error: "", indexed: true, resolvedArtifactId: known.id });
      return;
    }
    const abort = new AbortController();
    pending.current = abort;
    setDestination({ ...target, connection, status: "loading", row: null, error: "", indexed: false, resolvedArtifactId: "" });
    const fail = (error: string) => setDestination(current => current?.intentId === intentId && current.status !== "ready"
      ? { ...current, status: "error", error } : current);
    if (!connection) {
      fail("Reconnect to the cluster and try again.");
      return () => abort.abort();
    }
    const timer = setTimeout(() => {
      abort.abort();
      fail("The file lookup took too long. Try again.");
    }, LOOKUP_TIMEOUT_MS);
    const read = artifactId
      ? connection.query.libraryArtifactById({ artifactId }, { signal: abort.signal })
      : connection.query.libraryArtifactForFile({ fileId }, { signal: abort.signal });
    void read.then(result => {
      if (abort.signal.aborted) return;
      const rows = result.rows().map(artifactFromRow).filter(row => row.id !== "" && isContentKind(row.kind));
      // The typed file query resolves its backing reference on the server;
      // clients must not parse the artifact's polymorphic reference to do so.
      const row = rows.length === 1 && (artifactId ? matches(rows[0]!, target) : rows[0]!.kind === "file") ? rows[0]! : null;
      const indexedRow = row ? contentRef.current.find(item => item.id === row.id) : undefined;
      setDestination(current => current?.intentId === intentId && current.status !== "ready"
        ? { ...current, status: row ? "ready" : "missing", row: indexedRow ?? row, indexed: Boolean(indexedRow), resolvedArtifactId: row?.id ?? "" } : current);
    }).catch(error => {
      if (!abort.signal.aborted) fail(error instanceof Error ? error.message : String(error));
    }).finally(() => clearTimeout(timer));
    return () => { clearTimeout(timer); abort.abort(); };
  }, [connection, intentId, artifactId, fileId, attempt]);

  // A freshly generated file may still be indexing. Its existing live feed
  // can resolve an honest not-yet-found answer without another subscription.
  useEffect(() => {
    setDestination(current => {
      if (!current || current.connection !== connection) return current;
      const row = content.find(item => current.resolvedArtifactId
        ? item.id === current.resolvedArtifactId : matches(item, current));
      if (row) return current.indexed && current.row === row ? current
        : { ...current, status: "ready", row, error: "", indexed: true, resolvedArtifactId: row.id };
      // Once the live feed has held this row, its later removal wins forever.
      // The earlier direct lookup must not resurrect deleted metadata.
      return current.indexed && current.row ? { ...current, row: null } : current;
    });
  }, [connection, content]);

  const clear = useCallback(() => {
    dismissed.current = intentId;
    pending.current?.abort();
    setDestination(null);
  }, [intentId]);
  const retry = useCallback(() => setAttempt(value => value + 1), []);
  return { destination: destination?.connection === connection ? destination : null, clear, retry };
}
