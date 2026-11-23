import { useCallback, useSyncExternalStore } from "react";

import type { UploadHandle, UploadProvider } from "../../items/upload";
import type { DroppedFile } from "../../items/folderDrop";
import { uploadDroppedTree } from "./uploadTree";

// The transfer surface's state (design D3): one task per landing file, one
// AGGREGATE task per dropped folder. A task is a PLACEHOLDER -- on success
// it yields to the live row arriving on the feed (held one beat so the two
// visibly hand over); on failure it stays, with the server's sentence
// verbatim and a retry that rides the provider's own resume.

export interface UploadTaskFailure {
  name: string;
  error: string;
  retry: () => void;
}

export interface UploadTask {
  id: string;
  label: string;
  kind: "file" | "tree";
  sentBytes: number;
  totalBytes: number;
  resumedChunks?: number;
  totalChunks?: number;
  doneFiles: number;
  totalFiles: number;
  state: "sending" | "failed" | "done";
  /** How fast it is going, smoothed over its progress reports; absent until two reports exist. */
  bytesPerSecond?: number;
  /** The refusal, verbatim. "" while sending and on success. */
  error: string;
  /** Per-file refusals of a tree drop; the aggregate lists exactly these. */
  failures: UploadTaskFailure[];
  /** Whole-task retry (single files). Tree failures retry per file. */
  retry: (() => void) | null;
  abort: () => void;
  dismiss: () => void;
}

const DONE_LINGER_MS = 1200;

/**
 * WHY AN UPLOAD FAILED, IN A FEW PLAIN WORDS (Jesús, Nov 23 2026: "super short,
 * just to let the user know why it failed and nothing else"). The engine's full
 * sentence goes to the logs (console.warn is captured into Logs, logs/capture.ts);
 * the row shows only this. An unrecognised cause says nothing beyond
 * "Upload failed".
 */
export function failureReason(message: string): string {
  if (/storage limit/i.test(message)) return "Storage is full";
  if (/too large|exceeds the|maximum (file )?size/i.test(message)) return "File is too large";
  if (/sign(ed)? ?in|unauthori[sz]ed|\b401\b|token expired/i.test(message)) return "Signed out";
  if (/network|failed to fetch|load failed|connection|offline|timed? ?out/i.test(message)) return "Connection lost";
  return "";
}

function describe(err: unknown, label: string): string {
  const message = err instanceof Error ? err.message : String(err);
  console.warn(`Upload failed: ${label}: ${message}`);
  return failureReason(message);
}

export interface UploadTasksApi {
  tasks: UploadTask[];
  /** One task per file, into the folder. */
  uploadFiles: (files: readonly File[], folderId: string) => void;
  /** One aggregate task for a dropped tree. `label` names the drop. */
  uploadTree: (files: readonly DroppedFile[], targetFolderId: string, label: string) => void;
}

// ONE LIST FOR THE PAGE (José, Nov 20 2026: a file still uploading on the
// desk, right-clicked, "should be able to take you to the downloads"). The
// desk's uploads (trackDeskUpload, Desktop.tsx) and the Files app's join the
// same list, so Files > Downloads shows both, whenever the window opens.
let tasksNow: UploadTask[] = [];
const listeners = new Set<() => void>();
let seq = 0;

function setTasks(next: (ts: UploadTask[]) => UploadTask[]): void {
  tasksNow = next(tasksNow);
  for (const listener of listeners) listener();
}
function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}
function patch(id: string, p: Partial<UploadTask>): void {
  setTasks((ts) => ts.map((t) => (t.id === id ? { ...t, ...p } : t)));
}
function remove(id: string): void {
  samples.delete(id);
  setTasks((ts) => ts.filter((t) => t.id !== id));
}

// SPEED (José, Nov 20 2026: Downloads shows "the status of the download and
// the speed"). Measured here, from this browser's own progress reports: the
// bytes sent since the last report over the time since it, smoothed so one
// slow chunk does not swing the figure. A retry starts the measure again.
const samples = new Map<string, { at: number; bytes: number; rate?: number }>();
const SMOOTHING = 0.3;
function progress(id: string, sentBytes: number, rest: Partial<UploadTask> = {}): void {
  const now = Date.now();
  const last = samples.get(id);
  let rate = last?.rate;
  if (last && now > last.at && sentBytes >= last.bytes) {
    const instant = ((sentBytes - last.bytes) * 1000) / (now - last.at);
    rate = rate === undefined ? instant : rate * (1 - SMOOTHING) + instant * SMOOTHING;
  }
  samples.set(id, { at: now, bytes: sentBytes, ...(rate !== undefined ? { rate } : {}) });
  patch(id, { sentBytes, ...rest, ...(rate !== undefined ? { bytesPerSecond: rate } : {}) });
}
function restart(id: string, p: Partial<UploadTask>): void {
  samples.delete(id);
  patch(id, { ...p, bytesPerSecond: undefined });
  startGraph();
}
function settleDone(id: string): void {
  patch(id, { state: "done", error: "" });
  setTimeout(() => remove(id), DONE_LINGER_MS);
}
function nextId(): string {
  seq += 1;
  return `up-${seq}`;
}

// THE GRAPH (Jesús, Nov 23 2026, picked look A: Steam's live speed graph on
// top of Downloads). One sample a second of every sending upload's speed
// added together, the last minute kept. The ticker runs only while something
// sends. When nothing does, the graph stays frozen as the last upload left it,
// and the next upload carries it on rather than starting over (Jesús, same
// day). It lives as long as the page.
export const SPEED_HISTORY_SECONDS = 60;
let historyNow: readonly number[] = [];
let ticker: ReturnType<typeof setInterval> | null = null;
function tick(): void {
  const sending = tasksNow.filter((t) => t.state === "sending");
  if (sending.length === 0) {
    if (ticker) clearInterval(ticker);
    ticker = null;
    return;
  }
  historyNow = [...historyNow, sending.reduce((sum, t) => sum + (t.bytesPerSecond ?? 0), 0)].slice(-SPEED_HISTORY_SECONDS);
  for (const listener of listeners) listener();
}
function startGraph(): void {
  if (ticker) return;
  ticker = setInterval(tick, 1000);
}

/** The last minute of upload speed, one sample a second, newest last. */
export function useUploadSpeedHistory(): readonly number[] {
  return useSyncExternalStore(subscribe, () => historyNow);
}

/** Tests start from an empty list: it outlives a render. */
export function resetUploadTasksForTest(): void {
  tasksNow = [];
  seq = 0;
  samples.clear();
  if (ticker) clearInterval(ticker);
  ticker = null;
  historyNow = [];
}

/**
 * A file dropped on the desk, on the same list. `key` is its desk icon, so a
 * retry replaces the icon's old line; `retry` goes back through the desk,
 * which owns the icon (null where there is no icon: a drop onto a folder).
 */
export function trackDeskUpload(key: string, file: File, handle: UploadHandle, retry: (() => void) | null): void {
  const id = `desk:${key}`;
  setTasks((ts) => [
    {
      id,
      label: file.name,
      kind: "file",
      sentBytes: 0,
      totalBytes: file.size,
      doneFiles: 0,
      totalFiles: 1,
      state: "sending",
      error: "",
      failures: [],
      retry,
      abort: () => handle.abort(),
      dismiss: () => remove(id),
    },
    ...ts.filter((t) => t.id !== id),
  ]);
  samples.delete(id);
  startGraph();
  handle.onProgress?.((p) => progress(id, p.sentBytes, { totalBytes: p.totalBytes }));
  handle.done
    .then(() => settleDone(id))
    .catch((err: unknown) => patch(id, { state: "failed", error: describe(err, file.name) }));
}

export function useUploadTasks(
  provider: UploadProvider,
  /** Create one Library folder, answering its id -- the tree flow's port. */
  createFolder: (name: string, parentFolderId: string) => Promise<string>,
): UploadTasksApi {
  const tasks = useSyncExternalStore(subscribe, () => tasksNow);

  const uploadFiles = useCallback(
    (files: readonly File[], folderId: string) => {
      for (const file of files) {
        const id = nextId();
        let handle: UploadHandle | null = null;
        const run = () => {
          restart(id, { state: "sending", error: "" });
          handle = provider.upload(file, folderId !== "" ? { folderId } : undefined);
          handle.onProgress?.((p) =>
            progress(id, p.sentBytes, {
              totalBytes: p.totalBytes,
              ...(p.resumedChunks !== undefined ? { resumedChunks: p.resumedChunks } : {}),
              ...(p.totalChunks !== undefined ? { totalChunks: p.totalChunks } : {}),
            }),
          );
          handle.done
            .then(() => settleDone(id))
            .catch((err: unknown) => patch(id, { state: "failed", error: describe(err, file.name) }));
        };
        setTasks((ts) => [
          {
            id,
            label: file.name,
            kind: "file",
            sentBytes: 0,
            totalBytes: file.size,
            doneFiles: 0,
            totalFiles: 1,
            state: "sending",
            error: "",
            failures: [],
            // RETRY IS RE-RUN: the provider's resume ledger already knows the
            // session, so only the missing chunks travel again.
            retry: run,
            abort: () => handle?.abort(),
            dismiss: () => remove(id),
          },
          ...ts,
        ]);
        run();
      }
    },
    [provider],
  );

  const uploadTree = useCallback(
    (files: readonly DroppedFile[], targetFolderId: string, label: string) => {
      const id = nextId();
      const totalBytes = files.reduce((sum, f) => sum + f.file.size, 0);
      const sentByFile = new Map<string, number>();
      const handles = new Set<UploadHandle>();
      const knownFolders = new Map<string, string>();
      let aborted = false;

      const report = () => {
        let sent = 0;
        for (const bytes of sentByFile.values()) sent += bytes;
        progress(id, sent);
      };

      const run = (batch: readonly DroppedFile[]) => {
        restart(id, { state: "sending", error: "", failures: [] });
        void uploadDroppedTree(batch, targetFolderId, {
          createFolder,
          knownFolders,
          concurrency: 3,
          uploadFile: async (file, folderId) => {
            if (aborted) throw new Error("upload aborted");
            const handle = provider.upload(file, folderId !== "" ? { folderId } : undefined);
            handles.add(handle);
            handle.onProgress?.((p) => {
              sentByFile.set(file.name, p.sentBytes);
              report();
            });
            try {
              await handle.done;
              sentByFile.set(file.name, file.size);
              report();
            } finally {
              handles.delete(handle);
            }
          },
          onFileSettled: () => {
            setTasks((ts) =>
              ts.map((t) => (t.id === id ? { ...t, doneFiles: t.doneFiles + 1 } : t)),
            );
          },
        }).then((result) => {
          for (const [key, folderId] of result.folderIdByPath) knownFolders.set(key, folderId);
          if (result.failures.length === 0) {
            settleDone(id);
            return;
          }
          patch(id, {
            state: "failed",
            error: `${result.failures.length} of ${batch.length} did not land.`,
            failures: result.failures.map((f) => ({
              name: [...f.dirPath, f.file.name].join("/"),
              error: describe(f.error, [...f.dirPath, f.file.name].join("/")),
              // The retry re-runs ONLY this file, into folders the first run
              // already resolved -- no duplicate siblings.
              retry: () => run([{ file: f.file, dirPath: f.dirPath }]),
            })),
          });
        });
      };

      setTasks((ts) => [
        {
          id,
          label,
          kind: "tree",
          sentBytes: 0,
          totalBytes,
          doneFiles: 0,
          totalFiles: files.length,
          state: "sending",
          error: "",
          failures: [],
          retry: null,
          abort: () => {
            aborted = true;
            for (const handle of handles) handle.abort();
          },
          dismiss: () => remove(id),
        },
        ...ts,
      ]);
      run(files);
    },
    [provider, createFolder],
  );

  return { tasks, uploadFiles, uploadTree };
}
