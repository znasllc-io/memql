import { useCallback, useRef, useState } from "react";

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

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export interface UploadTasksApi {
  tasks: UploadTask[];
  /** One task per file, into the folder. */
  uploadFiles: (files: readonly File[], folderId: string) => void;
  /** One aggregate task for a dropped tree. `label` names the drop. */
  uploadTree: (files: readonly DroppedFile[], targetFolderId: string, label: string) => void;
}

export function useUploadTasks(
  provider: UploadProvider,
  /** Create one Library folder, answering its id -- the tree flow's port. */
  createFolder: (name: string, parentFolderId: string) => Promise<string>,
): UploadTasksApi {
  const [tasks, setTasks] = useState<UploadTask[]>([]);
  const seq = useRef(0);

  const patch = useCallback((id: string, p: Partial<UploadTask>) => {
    setTasks((ts) => ts.map((t) => (t.id === id ? { ...t, ...p } : t)));
  }, []);
  const remove = useCallback((id: string) => {
    setTasks((ts) => ts.filter((t) => t.id !== id));
  }, []);
  const settleDone = useCallback(
    (id: string) => {
      patch(id, { state: "done", error: "" });
      setTimeout(() => remove(id), DONE_LINGER_MS);
    },
    [patch, remove],
  );

  const uploadFiles = useCallback(
    (files: readonly File[], folderId: string) => {
      for (const file of files) {
        seq.current += 1;
        const id = `up-${seq.current}`;
        let handle: UploadHandle | null = null;
        const run = () => {
          patch(id, { state: "sending", error: "" });
          handle = provider.upload(file, folderId !== "" ? { folderId } : undefined);
          handle.onProgress?.((p) =>
            patch(id, {
              sentBytes: p.sentBytes,
              totalBytes: p.totalBytes,
              ...(p.resumedChunks !== undefined ? { resumedChunks: p.resumedChunks } : {}),
              ...(p.totalChunks !== undefined ? { totalChunks: p.totalChunks } : {}),
            }),
          );
          handle.done
            .then(() => settleDone(id))
            .catch((err: unknown) => patch(id, { state: "failed", error: describe(err) }));
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
    [provider, patch, remove, settleDone],
  );

  const uploadTree = useCallback(
    (files: readonly DroppedFile[], targetFolderId: string, label: string) => {
      seq.current += 1;
      const id = `up-${seq.current}`;
      const totalBytes = files.reduce((sum, f) => sum + f.file.size, 0);
      const sentByFile = new Map<string, number>();
      const handles = new Set<UploadHandle>();
      const knownFolders = new Map<string, string>();
      let aborted = false;

      const report = () => {
        let sent = 0;
        for (const bytes of sentByFile.values()) sent += bytes;
        patch(id, { sentBytes: sent });
      };

      const run = (batch: readonly DroppedFile[]) => {
        patch(id, { state: "sending", error: "", failures: [] });
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
              error: f.error,
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
    [provider, createFolder, patch, remove, settleDone],
  );

  return { tasks, uploadFiles, uploadTree };
}
