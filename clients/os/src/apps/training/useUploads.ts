import { useCallback, useRef, useState } from "react";

import type { UploadHandle, UploadProvider, UploadResult } from "../../items/upload";

// The dropzone's own state: one entry per file somebody handed this window,
// from the moment it was dropped to the moment its row appears in the list.
//
// ===========================================================================
// THIS IS NOT THE ANALYSIS, AND KEEPING THEM SEPARATE IS THE POINT
// ===========================================================================
// An upload has exactly two outcomes -- the bytes reached the cluster, or they
// did not -- and that question is answered by an HTTP response this browser is
// holding. Everything AFTER it (reading, ready, failed) is the cluster's, and
// arrives on the file feed.
//
// Folding the two into one list would mean this browser inventing a row on a
// 201 and then reconciling it against the real one when it arrived -- two
// sources for one fact, free to disagree, with the local one winning whenever
// the feed is slow. So the upload list retires an entry once the file row it
// produced has ARRIVED on the feed, which is a stronger handover than the old
// one: the entry does not vanish on a 201 and leave a gap until the
// subscription catches up.
//
// ===========================================================================
// THE PROVIDER IS THE LIBRARY'S, AND THAT IS WHERE THE GAINS COME FROM
// ===========================================================================
// `EdgeUploadProvider` is the one upload path in the OS (the desk's drops,
// the Files browse, drop-onto-window all ride it), so this surface inherits
// what it already does: byte progress, a chunked resumable session past
// 32 MiB, per-chunk retry, and re-drop-to-resume. The space attachment
// provider this replaced reported no progress at all and capped at 25 MB.

export type UploadPhase = "uploading" | "failed" | "landed";

export interface UploadEntry {
  /** Stable for the life of the entry, including across retries. */
  key: string;
  name: string;
  size: number;
  phase: UploadPhase;
  /** Bytes the cluster has taken, for the progress bar. */
  sentBytes: number;
  /** Chunks the cluster already held when a resume started. Absent on a
   *  fresh upload, which is the ordinary case. */
  resumedChunks: number;
  /** The file row this upload produced, once the cluster named it. "" until
   *  then, and "" forever for a reply that named none. */
  fileId: string;
  /** The server's own sentence. "" unless `phase` is "failed". */
  error: string;
}

export interface UploadsState {
  entries: UploadEntry[];
  start: (files: readonly File[]) => void;
  retry: (key: string) => void;
  dismiss: (key: string) => void;
  /** Stop an upload still in flight and drop its entry. */
  cancel: (key: string) => void;
  /** Retire every landed entry whose file row has arrived on the feed. The
   *  caller passes the ids it can see. */
  settle: (arrivedFileIds: ReadonlySet<string>) => void;
}

interface Held {
  file: File;
  handle: UploadHandle<UploadResult> | null;
}

let sequence = 0;
function nextKey(name: string): string {
  sequence += 1;
  return `${sequence}:${name}`;
}

export function useUploads(provider: UploadProvider): UploadsState {
  const [entries, setEntries] = useState<UploadEntry[]>([]);
  // The Files and their in-flight handles, OUTSIDE React state: a File is not
  // a rendering input and an AbortController in state would be compared by
  // identity on every render for nothing.
  const held = useRef(new Map<string, Held>());

  const patch = useCallback((key: string, next: Partial<UploadEntry>) => {
    setEntries((list) => list.map((e) => (e.key === key ? { ...e, ...next } : e)));
  }, []);

  const run = useCallback(
    (key: string) => {
      const entry = held.current.get(key);
      if (entry === undefined) return;
      patch(key, { phase: "uploading", error: "", sentBytes: 0 });
      const handle = provider.upload(entry.file);
      entry.handle = handle;
      handle.onProgress?.((progress) => {
        if (!held.current.has(key)) return;
        patch(key, {
          sentBytes: progress.sentBytes,
          resumedChunks: progress.resumedChunks ?? 0,
        });
      });
      handle.done.then(
        (result) => {
          // LANDED, and the file feed takes it from here. The entry stays as
          // an acknowledgement rather than vanishing on success: a dropzone
          // that empties itself is indistinguishable from one that never
          // accepted the file. `settle` retires it once its row is visible.
          if (!held.current.has(key)) return;
          patch(key, {
            phase: "landed",
            error: "",
            sentBytes: entry.file.size,
            // The upload route answers with the ARTIFACT id and the FILE id;
            // this surface is keyed on files, so it holds the file id. A
            // reply that named none leaves this "" and the entry is retired
            // on the clock the caller chooses rather than by matching -- see
            // `settle`.
            fileId: fileIdOf(result),
          });
        },
        (err: unknown) => {
          if (!held.current.has(key)) return;
          const message = err instanceof Error ? err.message : String(err);
          // An abort is not a failure -- it is what `cancel` asked for, and
          // the entry is already gone. Nothing to report.
          if (message.toLowerCase().includes("abort")) return;
          patch(key, { phase: "failed", error: message });
        },
      );
    },
    [patch, provider],
  );

  const start = useCallback(
    (files: readonly File[]) => {
      const fresh: UploadEntry[] = [];
      for (const file of files) {
        const key = nextKey(file.name);
        held.current.set(key, { file, handle: null });
        fresh.push({
          key,
          name: file.name,
          size: file.size,
          phase: "uploading",
          sentBytes: 0,
          resumedChunks: 0,
          fileId: "",
          error: "",
        });
      }
      // Appended, not prepended: the list reads in the order they were handed
      // over, which is the order somebody dropped them in.
      setEntries((list) => [...list, ...fresh]);
      for (const entry of fresh) run(entry.key);
    },
    [run],
  );

  const retry = useCallback((key: string) => run(key), [run]);

  const dismiss = useCallback((key: string) => {
    held.current.delete(key);
    setEntries((list) => list.filter((e) => e.key !== key));
  }, []);

  const cancel = useCallback((key: string) => {
    held.current.get(key)?.handle?.abort();
    held.current.delete(key);
    setEntries((list) => list.filter((e) => e.key !== key));
  }, []);

  const settle = useCallback((arrivedFileIds: ReadonlySet<string>) => {
    setEntries((list) => {
      const keep = list.filter(
        (e) => !(e.phase === "landed" && e.fileId !== "" && arrivedFileIds.has(e.fileId)),
      );
      if (keep.length === list.length) return list;
      for (const gone of list) {
        if (!keep.includes(gone)) held.current.delete(gone.key);
      }
      return keep;
    });
  }, []);

  return { entries, start, retry, dismiss, cancel, settle };
}

/** The file id off an upload reply, tolerating a provider that names none --
 *  the in-memory stand-in used by tests is one. */
function fileIdOf(result: UploadResult): string {
  const raw = (result as unknown as { fileId?: unknown }).fileId;
  return typeof raw === "string" ? raw.trim() : "";
}
