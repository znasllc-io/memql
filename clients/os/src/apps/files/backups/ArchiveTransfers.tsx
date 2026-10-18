import { useId, useEffect, useState } from "react";
import { newShortId } from "@znasllc-io/memql-sdk-core/client";
import { Download } from "lucide-react";
import { Button, Caption, Field, Input, Notice, RecordList, RecordRow, Select, Subhead, formatBytes } from "../../../kit";
import { InPlaceAsk } from "../../../kit/pressHold";
import { useOsConnection } from "../../../live/connection";
import { Wizard } from "../../../kit/Wizard";
import { machineName, type MachineRow } from "../../fleet/rows";
import { absoluteBackupPath, transferActions, transferLabel, type BackupTransfer, type TransferAction } from "./transfers";

const actionLabel: Record<TransferAction, string> = { pause: "Pause", resume: "Resume", cancel: "Cancel transfer", trash: "Move to Bin", restore: "Restore", purge: "Purge" };
export function ArchiveTransfers({ machines, refresh, onDownload }: { machines: readonly MachineRow[]; refresh: number; onDownload: (source: BackupTransfer) => void }) {
  const connection = useOsConnection();
  const [rows, setRows] = useState<BackupTransfer[]>([]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const [confirm, setConfirm] = useState("");
  const [actionError, setActionError] = useState("");
  const [pages, setPages] = useState(1);
  const [more, setMore] = useState(false);
  const [revision, setRevision] = useState(0);
  useEffect(() => {
    if (!connection) return;
    const abort = new AbortController(); let timer: ReturnType<typeof setTimeout>;
    const read = async () => {
      try {
        let cursor = ""; const next: BackupTransfer[] = [];
        for (let p = 0; p < pages; p++) {
          const r = await connection.query.libraryBackupList({ after: cursor, scope: "all" }, { signal: abort.signal });
          const page = r.rows()[0] as unknown as { transfers: BackupTransfer[]; nextCursor: string };
          if (!page || !Array.isArray(page.transfers)) throw new Error("The cluster returned no transfer list.");
          next.push(...page.transfers); cursor = page.nextCursor; if (!cursor) break;
        }
        if (!abort.signal.aborted) { setRows(next); setMore(!!cursor); setError(""); }
      } catch (e) { if (!abort.signal.aborted) setError(e instanceof Error ? e.message : String(e)); }
      if (!abort.signal.aborted) timer = setTimeout(() => void read(), 3_000);
    };
    void read(); return () => { abort.abort(); clearTimeout(timer); };
  }, [connection, refresh, revision, pages]);
  const act = async (job: BackupTransfer, action: TransferAction) => {
    if (!connection || busy) return;
    setBusy(job.id); setActionError("");
    try {
      if (action === "cancel" && job.kind === "sync_file" && job.metadata.watchId) {
        await connection.query.setLibraryWatchedFolderStatus({ watchId: job.metadata.watchId, status: "paused" });
      }
      const r = await connection.query.libraryBackupControl({ id: job.id, action }); const updated = r.rows()[0] as unknown as BackupTransfer; setRows(previous => previous.map(row => row.id === job.id ? updated : row).filter(row => !["cancelled", "deleted"].includes(row.state))); setConfirm(""); setRevision(n => n + 1); }
    catch (e) { setActionError(e instanceof Error ? e.message : String(e)); } finally { setBusy(""); }
  };
  return <section aria-label="Backup transfers and downloads">
    <Subhead>Transfers and downloads</Subhead>
    {rows.length === 0 && !error && <Caption>No transfers yet.</Caption>}
    <RecordList>
    {rows.map(job => <article key={job.id}>
      <RecordRow name={job.name} secondary={`${machines.find(m => m.id === job.workerId) ? machineName(machines.find(m => m.id === job.workerId)!) : job.workerId} · ${job.path}`} state={transferLabel(job)}
        actions={<>
          {job.state === "complete" && job.kind !== "download" && <Button onClick={() => onDownload(job)}>Download</Button>}
          {transferActions(job).map(action => <Button key={action} busy={busy === job.id} onClick={() => { if (action === "purge" || action === "cancel") setConfirm(`${job.id}:${action}`); else void act(job, action); }}>{actionLabel[action]}</Button>)}
        </>}>
        <Caption>{formatBytes(job.committedBytes)} of {formatBytes(job.size)}{job.kind === "download" ? " saved on the machine" : " stored"}</Caption>
      </RecordRow>
      {job.failure && <Notice tone="warn" sentence="Progress is kept." detail={job.failure} />}
      {confirm.startsWith(`${job.id}:`) && <InPlaceAsk>
        <Caption>{confirm.endsWith(":purge") ? "Permanently remove this archive? It cannot be recovered afterwards." : job.kind === "sync_file" ? "Pause this folder sync and cancel this unfinished file? Resume the folder when you are ready to try again." : "Cancel this transfer and remove its unfinished copy?"}</Caption>
        <Button onClick={() => setConfirm("")}>Keep it</Button><Button tone="danger" busy={busy === job.id} onClick={() => void act(job, confirm.endsWith(":purge") ? "purge" : "cancel")}>{confirm.endsWith(":purge") ? "Permanently purge" : "Cancel transfer"}</Button>
      </InPlaceAsk>}
    </article>)}
    </RecordList>
    {more && <Button onClick={() => setPages(n => n + 1)}>Show more</Button>}
    {actionError && <Notice tone="error" sentence="The transfer was not changed." detail={actionError} />}
    {error && <Notice tone="error" sentence="Transfers could not be updated." detail={error} />}
  </section>;
}

export function DownloadSetup({ source, machines, onCancel, onStarted }: { source: BackupTransfer; machines: readonly MachineRow[]; onCancel: () => void; onStarted: () => void }) {
  const ids=useId();
  const connection = useOsConnection(); const [id] = useState(newShortId);
  const [workerId, setWorkerId] = useState(source.workerId); const [path, setPath] = useState("");
  const [busy, setBusy] = useState(false); const [error, setError] = useState("");
  const valid = machines.some(m => m.id === workerId) && absoluteBackupPath(path.trim());
  const start = async () => {
    if (!connection || !valid || busy) return; setBusy(true); setError("");
    try { await connection.query.libraryBackupCreate({ id, workerId, path: path.trim(), kind: "download", metadata: { sourceTransferId: source.id } }); onStarted(); }
    catch (e) { setError(e instanceof Error ? e.message : String(e)); } finally { setBusy(false); }
  };
  return <Wizard icon={<Download />} title="Download backup" label="Download destination" open="destination" onOpen={() => {}} back={{ label: "Backups", onSelect: onCancel }}
    steps={[{ id: "destination", name: "Save on a machine", sentence: "Where the copy is saved", state: "open", body: <>
      <Caption>{source.name} · {formatBytes(source.size)}</Caption>
      <Field label="Machine"><Select id={`${ids}-machine`} label="Machine" value={workerId} onChange={setWorkerId}>{machines.map(m => <option key={m.id} value={m.id}>{machineName(m)}</option>)}</Select></Field>
      <Field label="Full destination file path"><Input id={`${ids}-path`} label="Full destination file path" value={path} onChange={setPath} placeholder={`/Users/you/Downloads/${source.name}`} /></Field>
      <Caption>The destination folder must already exist and allow downloads on that machine. Existing files are never overwritten. Interrupted downloads resume from verified progress.</Caption>
    </> }]} status={{ word: busy ? "Starting download" : "Choose where to save", tone: busy ? "busy" : undefined }}
    acts={busy ? [] : [{ label: "Cancel", onAct: onCancel }, ...(valid ? [{ label: "Download", onAct: () => void start(), tone: "primary" as const }] : [])]} notices={error ? <Notice tone="error" sentence="The download could not start." detail={error} /> : undefined} />;
}
