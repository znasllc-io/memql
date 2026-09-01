import { useCallback, useRef, useState } from "react";

import type { UploadHandle } from "../../items/upload";
import type { AttachmentUploadProvider, AttachmentUploadResult } from "./attachmentUpload";

// The dropzone's own state: one entry per file somebody handed this window,
// from the moment it was dropped to the moment it is dismissed.
//
// ===========================================================================
// THIS IS NOT THE ANALYSIS, AND KEEPING THEM SEPARATE IS THE POINT
// ===========================================================================
// An upload has exactly two outcomes -- the bytes reached the cluster, or they
// did not -- and that question is answered by an HTTP response this browser is
// holding. Everything AFTER it (queued, running, succeeded, failed) is the
// Plan, which lives in the cluster and arrives on the live feed.
//
// Folding the two into one list would mean this browser inventing a "queued"
// row on a 201 and then reconciling it against the real one when it arrived --
// two sources for one fact, free to disagree, with the local one winning
// whenever the feed is slow. So the upload list retires an entry once its
// bytes have landed, and the plan list takes over. Nothing is inserted
// locally: the plan row arrives with the arrival cue, exactly like one raised
// by an upload from somebody's phone.
//
// ===========================================================================
// A FAILURE IS IN-SURFACE AND RETRYABLE, NEVER A TOAST
// ===========================================================================
// The refusal is usually the server's own sentence ("unsupported file type:
// application/zip") and belongs beside the file it is about, where somebody
// can read it while looking at what they did. Retry re-uses the SAME File
// object, so a retry after fixing a transient failure costs no second pick.

export type UploadPhase = "uploading" | "failed" | "landed";

export interface UploadEntry {
  /** Stable for the life of the entry, including across retries. */
  key: string;
  name: string;
  size: number;
  phase: UploadPhase;
  /** The server's own sentence. "" unless `phase` is "failed". */
  error: string;
}

export interface UploadsState {
  entries: UploadEntry[];
  /** Hand files to the cluster. A blank space id is refused in-surface
   *  rather than sent -- see the caller. */
  start: (files: readonly File[], spaceId: string) => void;
  retry: (key: string) => void;
  dismiss: (key: string) => void;
  /** Stop an upload still in flight and drop its entry. */
  cancel: (key: string) => void;
}

interface Held {
  file: File;
  spaceId: string;
  handle: UploadHandle<AttachmentUploadResult> | null;
}

let sequence = 0;
function nextKey(name: string): string {
  sequence += 1;
  return `${sequence}:${name}`;
}

export function useUploads(provider: AttachmentUploadProvider): UploadsState {
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
      patch(key, { phase: "uploading", error: "" });
      const handle = provider.upload(entry.spaceId, entry.file);
      entry.handle = handle;
      handle.done.then(
        () => {
          // LANDED, and the plan feed takes it from here. The entry stays for
          // a moment as an acknowledgement rather than vanishing on success:
          // a dropzone that empties itself is indistinguishable from one that
          // never accepted the file.
          if (!held.current.has(key)) return;
          patch(key, { phase: "landed", error: "" });
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
    (files: readonly File[], spaceId: string) => {
      const fresh: UploadEntry[] = [];
      for (const file of files) {
        const key = nextKey(file.name);
        held.current.set(key, { file, spaceId, handle: null });
        fresh.push({ key, name: file.name, size: file.size, phase: "uploading", error: "" });
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

  return { entries, start, retry, dismiss, cancel };
}
