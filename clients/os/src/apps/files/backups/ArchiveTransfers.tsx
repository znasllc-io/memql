import { useId, useContext, useEffect, useRef, useState } from "react";
import { newShortId } from "@znasllc-io/memql-sdk-core/client";
import { Download, File } from "lucide-react";
import { Button, Caption, Field, Input, Notice, RecordList, RecordRow, Select, Refine, AutoRefresh, RecordListSkeleton, formatBytes } from "../../../kit";
import { InPlaceAsk } from "../../../kit/pressHold";
import { useOsConnection } from "../../../live/connection";
import { Wizard } from "../../../kit/Wizard";
import { machineName, type MachineRow } from "../../fleet/rows";
import { absoluteBackupPath, transferActions, transferLabel, transferLine, transferMoving, type BackupTransfer, type TransferAction } from "./transfers";
import { SPEED_HISTORY_SECONDS, useUploadSpeedHistory, type UploadTask } from "../useUploadTasks";

import { usePagedCollection } from "../../../live/useLiveCollection";
import { LiveReadVisibility } from "../../../live/useGraphRevision";
import { LoadMore } from "../../../kit/LoadMore";
import { flatten } from "../../../kit/rows";
import { useDebouncedValue } from "../../../logs/useDebouncedValue";

const actionLabel: Record<TransferAction, string> = { pause: "Pause", resume: "Resume", cancel: "Cancel", trash: "Delete", restore: "Restore", purge: "Purge" };

// THE DOWNLOADS LIST (owner's pick 1A, José: "like Steam"): every transfer is
// one row in the Backups row's look -- a file glyph, the name, one muted line
// with the status and the figures the transfer actually carries, and a thin
// bar while bytes move. Neither source reports a speed or a time left, so the
// line shows neither. This browser's own uploads (useUploadTasks) come first;
// they drop off on their own once landed (DONE_LINGER_MS). Engine transfers
// keep the engine's rule: listed until removed, ten per page.
//
// ONE BUTTON PER ROW while bytes move (Pause, or Cancel for a browser upload,
// which has no pause). A stopped row shows its legal pair -- Resume or Retry
// beside Cancel, Restore beside Purge, Download beside Delete -- because
// hiding the second action there would make Cancel, Purge or Delete
// unreachable, not merely quieter.
export function ArchiveTransfers({ machines, refresh, onDownload, uploads = [] }: { machines: readonly MachineRow[]; refresh: number; onDownload: (source: BackupTransfer) => void; uploads?: readonly UploadTask[] }) {
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
  // Starts from 0, not the mount's value: the list lives in its own tab now,
  // so a transfer started elsewhere (Backups, the download wizard) bumps the
  // counter while this is unmounted, and the next mount must re-read rather
  // than show the cached page. A counter still at 0 keeps the cached pages.
  const previousRefresh = useRef({connection,refresh:0});
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
  const machineLabel = (id: string) => { const m = machines.find(machine => machine.id === id); return m ? machineName(m) : id; };
  return <section aria-label="Transfers">
    <AutoRefresh resume={false} onRefresh={feed.reseed} enabled={visible && connection !== null} busy={feed.snapshot.state === "seeding" || feed.loadingMore} intervalMs={rows.some(row => !["complete", "bin", "cancelled", "deleted"].includes(row.state)) ? 3000 : 15000} />
    <Refine search={search} onSearch={setSearch} label="Search transfers" placeholder="Search transfers" />
    {feed.snapshot.state === "seeding" && !rows.length && !uploads.length ? <RecordListSkeleton label="Loading transfers" rows={3} /> : null}
    {rows.length === 0 && (needle || uploads.length === 0) && !error && feed.snapshot.state !== "seeding" && <Caption>{needle ? "No transfers match." : "No transfers yet."}</Caption>}
    {needle ? null : <UploadSpeed uploads={uploads} />}
    <RecordList label="Transfers">
    {needle ? null : uploads.map(task => <UploadLine key={task.id} task={task} />)}
    {rows.map(job => {
      const line = transferLine(job, machineLabel(job.workerId), formatBytes);
      const actions = transferActions(job).filter(action => !(action === "cancel" && transferMoving(job)));
      const word = (action: TransferAction) => action === "resume" && line.warn ? "Retry" : actionLabel[action];
      return <article key={job.id} className="os-backup">
      <RecordRow icon={<File size={15} aria-hidden />} name={job.name}
        secondary={<TransferSecondary {...line} />}
        state={line.backup ? "Backup" : undefined} tone="accent" stateTitle={line.backup ? `A backup from ${machineLabel(job.workerId)}: ${transferLabel(job)}` : undefined}
        dim={!transferMoving(job) && job.state !== "complete"}
        actions={<>
          {job.state === "complete" && job.kind !== "download" && <Button onClick={() => onDownload(job)}>Download</Button>}
          {actions.map(action => <Button key={action} tone={word(action) === "Retry" ? "primary" : undefined} busy={busy === job.id} onClick={() => { if (action === "purge" || action === "cancel") setConfirm(`${job.id}:${action}`); else void act(job, action); }}>{word(action)}</Button>)}
        </>} />
      {confirm.startsWith(`${job.id}:`) && <InPlaceAsk>
        <Caption>{confirm.endsWith(":purge") ? "Permanently remove this archive? It cannot be recovered afterwards." : job.kind === "sync_file" ? "Pause this folder sync and cancel this unfinished file? Resume the folder when you are ready to try again." : "Cancel this transfer and remove its unfinished copy?"}</Caption>
        <Button onClick={() => setConfirm("")}>Keep it</Button><Button tone="danger" busy={busy === job.id} onClick={() => act(job, confirm.endsWith(":purge") ? "purge" : "cancel")}>{confirm.endsWith(":purge") ? "Permanently purge" : "Cancel transfer"}</Button>
      </InPlaceAsk>}
    </article>; })}
    </RecordList>
    <LoadMore feed={feed} />
    {actionError && <Notice tone="error" sentence="The transfer was not changed." detail={actionError} />}
    {error && <Notice tone="error" sentence="Transfers could not be updated." detail={error} />}
  </section>;
}

/** The row's muted line, in the warning colour when it failed, with the thin
 *  bar under it while bytes move. */
function TransferSecondary({ text, warn, progress }: { text: string; warn: boolean; progress: number | null }) {
  return <>
    {warn ? <span className="os-transfer-failed">{text}</span> : text}
    {progress !== null ? <span className="os-progress-track os-transfer-bar" role="progressbar" aria-label="Progress" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(progress * 100)}><span className="os-progress-bar" style={{ width: `${progress * 100}%` }} /></span> : null}
  </>;
}

/** This browser's own upload, in the same row. A tree drop's per-file retries
 *  and the resume note stay on the Browse placeholder where the files land. */
export function uploadLine(task: UploadTask, bytes: (n: number) => string): { text: string; warn: boolean; progress: number | null } {
  if (task.state === "failed") return { text: `Upload failed · ${task.error}`, warn: true, progress: null };
  if (task.state === "done") return { text: `Uploaded · ${bytes(task.totalBytes)}`, warn: false, progress: null };
  // A sending upload is its bar alone: speed and time left are the graph's
  // above the list (look A, Jesús, Nov 23). A folder drop still counts files.
  const files = task.kind === "tree" ? `${task.doneFiles} of ${task.totalFiles} files` : "";
  return { text: files, warn: false, progress: task.totalBytes > 0 ? Math.min(1, task.sentBytes / task.totalBytes) : 0 };
}

/** "0:13", "1:40", "1:05:00": time left the way Steam shows it. */
export function clockLeft(seconds: number): string {
  const s = Math.max(1, Math.round(seconds));
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), r = s % 60;
  const two = (n: number) => String(n).padStart(2, "0");
  return h > 0 ? `${h}:${two(m)}:${two(r)}` : `${m}:${two(r)}`;
}

/**
 * LOOK A (Jesús, Nov 23 2026, after Steam's Downloads, José's reference): while
 * this browser uploads, one live graph of the last minute's speed, a dashed
 * line at its peak, and the speed and time left for everything still sending
 * in big numbers. Speed is this browser's own measure (useUploadTasks); a
 * transfer a machine runs reports no timing, so it is not in the graph.
 */
function UploadSpeed({ uploads }: { uploads: readonly UploadTask[] }) {
  const history = useUploadSpeedHistory();
  const sending = uploads.filter(task => task.state === "sending");
  if (sending.length === 0) return null;
  const rate = sending.reduce((sum, task) => sum + (task.bytesPerSecond ?? 0), 0);
  const remaining = sending.reduce((sum, task) => sum + Math.max(0, task.totalBytes - task.sentBytes), 0);
  // The live figure counts too: samples are a second apart, so the speed now can top the last sample.
  const peak = Math.max(0, rate, ...history);
  const n = SPEED_HISTORY_SECONDS;
  return <div className="os-transfer-speed" role="group" aria-label="Upload speed">
    <div className="os-transfer-graph">
      <svg viewBox={`0 0 ${n} 100`} preserveAspectRatio="none" aria-hidden="true">
        {history.map((value, i) => {
          const h = peak > 0 ? Math.max(1.5, (value / peak) * 88) : 0;
          return <rect key={i} x={n - history.length + i + 0.12} y={100 - h} width={0.76} height={h} />;
        })}
      </svg>
      {peak > 0 ? <span className="os-transfer-peak">peak {formatBytes(peak)}/s</span> : null}
    </div>
    <div className="os-transfer-figures">
      <span><strong>{rate > 0 ? `${formatBytes(rate)}/s` : "--"}</strong>speed</span>
      <span><strong>{rate > 0 ? clockLeft(remaining / rate) : "--"}</strong>left</span>
    </div>
  </div>;
}

function UploadLine({ task }: { task: UploadTask }) {
  const line = uploadLine(task, formatBytes);
  return <article className="os-backup">
    <RecordRow icon={<File size={15} aria-hidden />} name={task.label}
      secondary={<TransferSecondary {...line} />}
      actions={task.state === "sending" ? <Button onClick={task.abort}>Cancel</Button>
        : task.state === "failed" ? (task.retry ? <Button tone="primary" onClick={task.retry}>Retry</Button> : <Button onClick={task.dismiss}>Dismiss</Button>)
        : undefined} />
  </article>;
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
  return <Wizard icon={<Download />} title="Download backup" label="Download destination" open="destination" onOpen={() => {}} back={{ label: "Downloads", onSelect: onCancel }}
    steps={[{ id: "destination", name: "Save on a machine", sentence: "Where the copy is saved", state: "open", body: <>
      <Caption>{source.name} · {formatBytes(source.size)}</Caption>
      <Field label="Machine"><Select id={`${ids}-machine`} label="Machine" value={workerId} onChange={setWorkerId}>{machines.map(m => <option key={m.id} value={m.id}>{machineName(m)}</option>)}</Select></Field>
      <Field label="Full destination file path"><Input id={`${ids}-path`} label="Full destination file path" value={path} onChange={setPath} placeholder={`/Users/you/Downloads/${source.name}`} /></Field>
      <Caption>The destination folder must already exist and allow downloads on that machine. Existing files are never overwritten. Interrupted downloads resume from verified progress.</Caption>
    </> }]} status={{ word: busy ? "Starting download" : "Choose where to save", tone: busy ? "busy" : undefined }}
    acts={busy ? [] : [{ label: "Cancel", onAct: onCancel }, ...(valid ? [{ label: "Download", onAct: () => start(), tone: "primary" as const }] : [])]} notices={error ? <Notice tone="error" sentence="The download could not start." detail={error} /> : undefined} />;
}
