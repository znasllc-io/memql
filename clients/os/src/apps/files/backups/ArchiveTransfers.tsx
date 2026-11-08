import { useId, useContext, useEffect, useRef, useState } from "react";
import { newShortId } from "@znasllc-io/memql-sdk-core/client";
import { Download } from "lucide-react";
import { Button, Caption, Field, Input, Notice, RecordList, RecordRow, Select, Subhead, Refine, AutoRefresh, RecordListSkeleton, formatBytes } from "../../../kit";
import { InPlaceAsk } from "../../../kit/pressHold";
import { useOsConnection } from "../../../live/connection";
import { Wizard } from "../../../kit/Wizard";
import { machineName, type MachineRow } from "../../fleet/rows";
import { absoluteBackupPath, transferActions, transferLabel, type BackupTransfer, type TransferAction } from "./transfers";

import { usePagedCollection } from "../../../live/useLiveCollection";
import { LiveReadVisibility } from "../../../live/useGraphRevision";
import { LoadMore } from "../../../kit/LoadMore";
import { flatten } from "../../../kit/rows";
import { useDebouncedValue } from "../../../logs/useDebouncedValue";

const actionLabel: Record<TransferAction, string> = { pause: "Pause", resume: "Resume", cancel: "Cancel transfer", trash: "Move to Bin", restore: "Restore", purge: "Purge" };
export function ArchiveTransfers({ machines, refresh, onDownload }: { machines: readonly MachineRow[]; refresh: number; onDownload: (source: BackupTransfer) => void }) {
  const connection = useOsConnection();
  const visible = useContext(LiveReadVisibility);
  const [search, setSearch] = useState("");
  const needle = useDebouncedValue(search.trim(),250);
  const feed = usePagedCollection<BackupTransfer>(`files:transfers:${needle}`, conn => ({
    concept: "integration:fileBackups:transfer", actions: [],
    seed: async (cursor, signal) => {
      const result = await conn.query.libraryBackupList({ after: cursor || undefined, scope: "all", search: needle || undefined, limit: 10 }, { signal });
      const first = result.rows()[0];
      const page = first ? flatten(first) : null;
      if (!page || !Array.isArray(page.transfers)) throw new Error("The cluster returned no transfer list.");
      return { rows: page.transfers as BackupTransfer[], nextCursor: typeof page.nextCursor === "string" ? page.nextCursor : "" };
    },
  }));
  const rows = feed.snapshot.rows;
  const error = feed.snapshot.error;
  const previousRefresh = useRef({connection,refresh});
  useEffect(() => { const previous = previousRefresh.current; previousRefresh.current = {connection,refresh}; if(previous.connection === connection && previous.refresh !== refresh) feed.reseed(); }, [connection, refresh, feed.reseed]);
  const [busy, setBusy] = useState("");
  const [confirm, setConfirm] = useState("");
  const [actionError, setActionError] = useState("");
  const act = async (job: BackupTransfer, action: TransferAction) => {
    if (!connection || busy) return;
    setBusy(job.id); setActionError("");
    try {
      if (action === "cancel" && job.kind === "sync_file" && job.metadata.watchId) {
        await connection.query.setLibraryWatchedFolderStatus({ watchId: job.metadata.watchId, status: "paused" });
      }
      const r = await connection.query.libraryBackupControl({ id: job.id, action }); if (!r.rows()[0]) throw new Error("The transfer update returned no result."); setConfirm(""); feed.reseed(); }
    catch (e) { setActionError(e instanceof Error ? e.message : String(e)); } finally { setBusy(""); }
  };
  return <section aria-label="Backup transfers and downloads">
    <Subhead>Transfers and downloads</Subhead>
    <AutoRefresh resume={false} onRefresh={feed.reseed} enabled={visible && connection !== null} busy={feed.snapshot.state === "seeding" || feed.loadingMore} intervalMs={rows.some(row => !["complete", "bin", "cancelled", "deleted"].includes(row.state)) ? 3000 : 15000} />
    <Refine search={search} onSearch={setSearch} label="Search transfers" placeholder="Search transfers" />
    {feed.snapshot.state === "seeding" && !rows.length ? <RecordListSkeleton label="Loading transfers" rows={3} /> : null}
    {rows.length === 0 && !error && feed.snapshot.state !== "seeding" && <Caption>{needle ? "No transfers match." : "No transfers yet."}</Caption>}
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
    <LoadMore feed={feed} />
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
