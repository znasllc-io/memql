import { useId, useEffect, useState } from "react";
import { Button, Caption, Field, Input, Notice } from "../../../kit";
import { useOsConnection } from "../../../live/connection";
import { formatStorageBytes, type StorageUsage } from "./transfers";

export function StorageAllowance({ userId = "", editable = false }: { userId?: string; editable?: boolean }) {
  const ids=useId();
  const connection = useOsConnection();
  const [usage, setUsage] = useState<StorageUsage | null>(null);
  const [error, setError] = useState("");
  const [saveError, setSaveError] = useState("");
  const [editing, setEditing] = useState(false);
  const [gigabytes, setGigabytes] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    if (!connection) return;
    const abort = new AbortController(); let timer: ReturnType<typeof setTimeout>;
    const read = async () => {
      try {
        const r = await connection.query.libraryBackupUsage({ ...(userId ? { userId } : {}) }, { signal: abort.signal });
        const next = r.rows()[0] as unknown as StorageUsage | undefined;
        if (!next || !Number.isFinite(next.available)) throw new Error("The cluster returned no storage allowance.");
        if (!abort.signal.aborted) { setUsage(next); setError(""); }
      } catch (e) { if (!abort.signal.aborted) setError(e instanceof Error ? e.message : String(e)); }
      if (!abort.signal.aborted) timer = setTimeout(() => void read(), 15_000);
    };
    void read(); return () => { abort.abort(); clearTimeout(timer); };
  }, [connection, userId]);
  const bytes = Math.round(Number(gigabytes) * 1_000_000_000);
  const valid = gigabytes.trim() !== "" && Number.isFinite(bytes) && bytes >= 0 && bytes <= 5_000_000_000_000;
  const save = async () => {
    if (!connection || !valid || busy) return;
    setBusy(true); setSaveError("");
    try { const r = await connection.query.libraryBackupSetUserLimit({ userId, bytes }); setUsage(r.rows()[0] as unknown as StorageUsage); setEditing(false); }
    catch (e) { setSaveError(e instanceof Error ? e.message : String(e)); } finally { setBusy(false); }
  };
  return <div aria-label="Storage allowance">
    {usage ? <Caption>{formatStorageBytes(usage.used)} stored · {formatStorageBytes(usage.reserved)} reserved · {formatStorageBytes(usage.available)} available{usage.exempt ? " in shared cluster storage" : ` of ${formatStorageBytes(usage.limit)}`}</Caption> : <Caption>Checking storage…</Caption>}
    {usage && <Caption>Cluster: {formatStorageBytes(Math.max(0, usage.clusterLimit - usage.clusterUsed - usage.clusterReserved))} available of 5 TB. Files, versions, the Bin and unfinished transfers count.</Caption>}
    {editable && usage && !usage.exempt && !editing && <Button onClick={() => { setGigabytes(String(usage.limit / 1_000_000_000)); setEditing(true); }}>Change allowance</Button>}
    {editing && <>
      <Field label="Storage allowance in GB"><Input id={`${ids}-limit`} label="Storage allowance in GB" value={gigabytes} onChange={setGigabytes} onEnter={() => void save()} /></Field>
      <Caption>From 0 to 5,000 GB. Lowering this keeps existing files and stops new reservations that exceed the allowance.</Caption>
      <Button onClick={() => setEditing(false)}>Cancel</Button>{valid && <Button tone="primary" busy={busy} onClick={() => void save()}>Save allowance</Button>}
    </>}
    {error && <Notice tone="error" sentence="Storage could not be checked." detail={error} next={usage ? "The displayed allowance is from the last successful check." : undefined} />}
    {saveError && <Notice tone="error" sentence="The allowance was not changed." detail={saveError} />}
  </div>;
}
