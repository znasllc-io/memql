import { useCallback, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, Chip, Notice, Subhead, flatten } from "../../../kit";
import { useOsConnection } from "../../../live/connection";
import { useReading } from "../../../cluster/reading";

// The dead-letter band, for ONE connector, LOADED ONLY WHEN ASKED.
//
// ===========================================================================
// WHY THIS IS NOT PART OF THE PAGE READ
// ===========================================================================
// `outboxDeadLetters` takes ONE target, so covering a page of connectors is a
// fan-out: N reads on every open of a section most people open to look at
// lag. Each one is a bounded read of a table that is empty on a healthy
// cluster, so the ordinary case spends N round trips to render N empty
// bands.
//
// So the band is a component that MOUNTS on demand, and the read is its own
// mount effect. That is the mechanism rather than a flag: a band that is not
// on screen has not asked, and there is no way to reach the read without
// rendering the thing that shows the answer.
//
// ===========================================================================
// A DEAD LETTER IS NOT A FAILED ONE
// ===========================================================================
// These entries exhausted their attempts and are waiting for a person. They
// are never retried automatically -- that is what makes them dead rather than
// failed -- so nothing on this page happens unless somebody presses
// something, and both acts are behind a confirm. Discard is the one that
// cannot be undone: the change it carried is lost, and the mirror on the far
// side stays as it was.

export interface DeadLetterEntry {
  id: string;
  conceptId: string;
  rowRef: string;
  action: string;
  target: string;
  attempts: number;
  lastError: string;
  createdAt: string;
}

export function entryFromRow(raw: Row): DeadLetterEntry {
  const row = flatten(raw);
  return {
    id: stringOf(row, "id"),
    conceptId: stringOf(row, "conceptId"),
    rowRef: stringOf(row, "rowRef"),
    action: stringOf(row, "action"),
    target: stringOf(row, "target"),
    attempts: numberOf(row, "attempts"),
    lastError: stringOf(row, "lastError"),
    createdAt: stringOf(row, "createdAt"),
  };
}

export function DeadLetterBand({ connector }: { connector: string }) {
  const connection = useOsConnection();

  const read = useCallback(
    async (signal: AbortSignal): Promise<DeadLetterEntry[]> => {
      if (connection === null) throw new Error("not connected");
      const result = await connection.query.outboxDeadLetters({ target: connector }, { signal });
      return result.rows().map(entryFromRow);
    },
    [connection, connector],
  );

  const entries = useReading<DeadLetterEntry[]>(
    `cluster:deadletters:${connector}`,
    connection === null ? null : read,
  );

  const [confirming, setConfirming] = useState<{ id: string; act: "retry" | "discard" } | null>(null);
  const [busy, setBusy] = useState("");
  const [refusal, setRefusal] = useState("");

  const run = useCallback(
    async (id: string, act: "retry" | "discard") => {
      if (connection === null) return;
      setBusy(id);
      setRefusal("");
      try {
        if (act === "retry") {
          await connection.query.datasyncRetryOutboxEntry({ entryId: id });
        } else {
          await connection.query.datasyncDiscardOutboxEntry({
            entryId: id,
            reason: "Discarded from MemQL OS, Cluster -> Data origins.",
          });
        }
        setConfirming(null);
        entries.reread();
      } catch (err: unknown) {
        setRefusal(err instanceof Error ? err.message : String(err));
      } finally {
        setBusy("");
      }
    },
    [connection, entries],
  );

  const rows = entries.value ?? [];

  return (
    <div className="os-cluster-band">
      <Subhead>{connector} dead letters</Subhead>

      {entries.state === "failed" ? (
        <Notice
          tone="error"
          sentence={`The cluster did not answer ${connector}'s dead letters.`}
          detail={entries.error}
        />
      ) : null}
      {entries.state === "reading" && entries.value === null ? (
        <Caption>Reading {connector}'s dead letters.</Caption>
      ) : null}
      {entries.state === "read" && rows.length === 0 ? (
        <Caption>
          Nothing is waiting for a person on {connector}. This one was read just now -- it is not a
          standing figure.
        </Caption>
      ) : null}

      {refusal === "" ? null : (
        <Notice tone="error" sentence="The cluster refused, and nothing changed." detail={refusal} />
      )}

      {rows.map((entry) => (
        <div key={entry.id} className="os-cluster-dl">
          <span className="os-cluster-dl-head">
            <span className="os-cluster-dl-concept os-mono">{entry.conceptId}</span>
            <Chip tone="muted">{entry.action || "unstated"}</Chip>
            <Chip tone="muted" title="How many times the drain tried before giving up.">
              {entry.attempts} {entry.attempts === 1 ? "attempt" : "attempts"}
            </Chip>
          </span>
          <span className="os-cluster-dl-ref os-mono">{entry.rowRef}</span>
          {entry.lastError === "" ? null : (
            <span className="os-cluster-dl-error os-mono">{entry.lastError}</span>
          )}
          {confirming !== null && confirming.id === entry.id ? (
            <span className="os-cluster-confirm">
              <span className="os-cluster-confirm-text">
                {confirming.act === "retry"
                  ? "Send this entry to the drain again?"
                  : "Discard it? The change it carried is lost, and the mirror stays as it is."}
              </span>
              <Button tone="quiet" onClick={() => setConfirming(null)}>
                Cancel
              </Button>
              <Button
                tone={confirming.act === "retry" ? "primary" : "danger"}
                busy={busy === entry.id}
                busyLabel="Working"
                onClick={() => void run(entry.id, confirming.act)}
              >
                {confirming.act === "retry" ? "Retry" : "Discard"}
              </Button>
            </span>
          ) : (
            <span className="os-cluster-dl-acts">
              <Button tone="quiet" onClick={() => setConfirming({ id: entry.id, act: "retry" })}>
                Retry
              </Button>
              <Button tone="quiet" onClick={() => setConfirming({ id: entry.id, act: "discard" })}>
                Discard
              </Button>
            </span>
          )}
        </div>
      ))}

      {entries.at === null ? null : (
        <Caption>Read {entries.at.toLocaleTimeString()}.</Caption>
      )}
    </div>
  );
}

function stringOf(row: Row, key: string): string {
  const v = row[key];
  return typeof v === "string" ? v : "";
}

function numberOf(row: Row, key: string): number {
  const v = row[key];
  if (typeof v === "number" && Number.isFinite(v)) return v;
  if (typeof v === "string" && v.trim() !== "") {
    const parsed = Number(v);
    if (Number.isFinite(parsed)) return parsed;
  }
  return 0;
}
