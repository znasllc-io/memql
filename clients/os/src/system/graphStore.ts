// The roaming desktop (epic memql#4746): the same DesktopStore, backed by
// the person's v1:os:desktop row, with local storage kept as the offline
// cache underneath it.
//
// ===========================================================================
// LOCAL IS NOT A FALLBACK -- IT IS THE FIRST FRAME
// ===========================================================================
// `load()` is synchronous because the shell paints a desktop before anything
// has been dialed, and must still paint one when nothing ever will. So local
// answers `load()`, always and immediately; the graph resolves a moment later
// and arrives through `subscribe()`. Making `load()` async would have put
// every boot -- including every offline boot -- behind a round trip.
//
// ===========================================================================
// NOTHING IS WRITTEN UNTIL THE FIRST READ RESOLVES
// ===========================================================================
// The order here is the whole correctness argument, and getting it wrong is
// silent and destructive. A browser signing in for the first time has no
// local document, so the shell SEEDS one and the seed is saved -- which, if
// that save reached the cluster, would overwrite the desktop this person has
// been building on their other machine with an empty one, in the first
// hundred milliseconds, before they saw anything.
//
// So saves are buffered until the read has answered, and what happens then
// is decided by the answer rather than by a flag:
//
//   a row exists   -> it is adopted, and the buffered document is dropped
//                     with it (the row is the truth; the shell had a guess)
//   no row exists  -> the buffered document is written, which IS the
//                     first-sign-in migration -- not a special path, just
//                     the first ordinary save, held until we knew
//
// ===========================================================================
// LAST WRITER WINS, ON A REVISION THE CLIENT STAMPS
// ===========================================================================
// A save writes `held + 1`. Two machines saving from revision 5 both write 6
// and the second one lands; that is the model this epic chose, and the
// conflict UX is a cue rather than a merge editor. What the stamp buys is
// that an arriving row can be told from one already held, and a row OLDER
// than what we hold is ignored rather than adopted.
//
// The echo test is the DOCUMENT, not the revision, and that is deliberate:
// our own save comes back as an event, and adopting it would push it into
// the shell, whose state change would save it again -- a write per machine
// per round trip, forever. Comparing documents ends that loop exactly, and
// stays right even when two machines happen to mint the same revision.
//
// Both sides of the comparison are SANITIZED and canonicalised. A document
// that has been to Postgres and back has no key order and has been through
// sanitizeDocument; comparing it against a freshly built one would differ on
// every save and produce the storm this is here to prevent.

import {
  sanitizeDocument,
  type DesktopDocument,
  type DesktopStore,
  type DesktopStoreEvent,
} from "./store";

/** One stored desktop row, as the gateway read it. */
export interface StoredDesktop {
  revision: number;
  /** Raw: sanitizeDocument has not run, and may reject it. */
  document: unknown;
}

/**
 * The cluster seam. Narrow on purpose -- three operations, no SDK types --
 * so the store's whole behaviour is testable with a fake and the real one
 * (live/desktopGateway.ts) has nothing in it but the SDK calls.
 */
export interface DesktopGateway {
  /** The caller's stored desktop, or null when they have never saved one. */
  read(): Promise<StoredDesktop | null>;
  write(input: { revision: number; document: DesktopDocument }): Promise<void>;
  /** Called when the row may have changed. Returns an unsubscribe. */
  watch(onChange: () => void): () => void;
}

export const SAVE_DEBOUNCE_MS = 1_200;

/**
 * Order-insensitive canonical form, for comparing two documents that took
 * different routes to get here. JSON.stringify alone would not do: object key
 * order survives neither JSONB nor the engine's own value rendering, so two
 * identical desktops routinely serialise differently.
 */
export function canonicalDocument(value: unknown): string {
  if (value === null || value === undefined) return "null";
  if (typeof value !== "object") return JSON.stringify(value) ?? "null";
  if (Array.isArray(value)) return "[" + value.map(canonicalDocument).join(",") + "]";
  const obj = value as Record<string, unknown>;
  return (
    "{" +
    Object.keys(obj)
      .sort()
      .map((k) => JSON.stringify(k) + ":" + canonicalDocument(obj[k]))
      .join(",") +
    "}"
  );
}

/**
 * The document's SHARED content: everything except which desk is on screen.
 *
 * THE ACTIVE DESK RIDES ALONG WITH A SAVE BUT NEVER CAUSES ONE, and without
 * that rule two machines write to each other forever. The shell keeps the
 * desk the person is looking at when it adopts a document (teleporting
 * somebody because another machine paged is hostile), so the document it
 * rebuilds afterwards differs from the row in exactly that field -- which,
 * compared whole, reads as a change worth saving. Each machine then saves the
 * other's arrival back, and the pair ping-pong a revision per round trip with
 * nothing having happened.
 *
 * Excluding it also says something true: which desk you are on is a view
 * position on one machine, not a fact about the desktop. It is still STORED,
 * because a cold sign-in on a new machine has no position of its own and
 * landing where you left off is better than landing on desk one.
 */
function sharedContent(doc: DesktopDocument): string {
  const { activeDeskId: _viewPosition, ...shared } = doc;
  return canonicalDocument(shared);
}

export class GraphDesktopStore implements DesktopStore {
  private readonly listeners = new Set<(event: DesktopStoreEvent) => void>();
  /** What we believe the row holds. Null until the first read answers. */
  private held: { revision: number; shared: string } | null = null;
  private resolved = false;
  /** The row is from a newer app than this one: stop writing to it. */
  private stale = false;
  private started = false;
  private pending: DesktopDocument | null = null;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private writing = false;

  constructor(
    private readonly local: DesktopStore,
    private readonly gateway: DesktopGateway,
    private readonly debounceMs: number = SAVE_DEBOUNCE_MS,
  ) {}

  load(): DesktopDocument | null {
    return this.local.load();
  }

  save(doc: DesktopDocument): void {
    // Local first and unconditionally. This machine's own copy is never
    // contingent on the cluster being reachable, or on the row being
    // readable, or on the write being accepted.
    this.local.save(doc);
    if (this.stale) return;

    // Send the SANITIZED document, so what is stored is what a reader will
    // get back. Chiefly this drops an in-flight `uploading` state, which is
    // a fact about this browser's network and would arrive on the other
    // machine as a spinner nothing there can ever finish.
    const clean = sanitizeDocument(doc);
    if (clean === null) return;
    if (this.held !== null && sharedContent(clean) === this.held.shared) return;

    this.pending = clean;
    this.schedule();
  }

  /** Send a debounced save now. The shell calls this as the page goes away. */
  flush(): void {
    this.clearTimer();
    void this.drain();
  }

  subscribe(listener: (event: DesktopStoreEvent) => void): () => void {
    this.listeners.add(listener);
    // The machine starts on the FIRST subscriber and never stops. React 19's
    // StrictMode mounts twice, so a teardown here would cancel the read that
    // the first mount started and re-run it -- and, worse, could restart the
    // resolve-then-write sequence above from the beginning.
    if (!this.started) {
      this.started = true;
      // The unsubscribe is dropped on purpose -- see the note below on why
      // this store has no close().
      void this.gateway.watch(() => {
        void this.refresh();
      });
      void this.refresh();
    }
    return () => {
      this.listeners.delete(listener);
    };
  }

  // THERE IS NO close(), and that is a decision rather than an omission.
  //
  // The obvious place to call one is an unmount cleanup, and under React 19's
  // StrictMode that runs on the first of the two development mounts -- so a
  // store closed there would be handed back, already closed, to the mount
  // that keeps it, and the desktop would silently stop roaming in dev only.
  //
  // Nothing needs closing anyway. This store's life is its connection's: the
  // watch is registered on that connection's subscription manager, which
  // stops when the connection does, and the only other resource is a debounce
  // timer that fires once within a second or two and whose write is refused
  // and swallowed if the socket has gone.

  private async refresh(): Promise<void> {
    let stored: StoredDesktop | null;
    try {
      stored = await this.gateway.read();
    } catch {
      // Unreachable or refused. Local is already authoritative for this
      // machine and the next change retries; a desktop is not worth an
      // error surface of its own.
      return;
    }

    if (stored === null) {
      // No row: this person has never saved a desktop anywhere. Whatever the
      // shell has -- their local document, or the seed it just made -- is the
      // one, and releasing the writer sends it.
      const first = !this.resolved;
      this.resolved = true;
      if (first) this.schedule();
      return;
    }

    const clean = sanitizeDocument(stored.document);
    if (clean === null) {
      // A stored desktop this bundle cannot read, which in practice means a
      // newer document version and an old tab. REFUSING TO WRITE is the safe
      // direction: local keeps working, and the newer desktop survives to be
      // read by the reload the cue asks for. Writing would replace it with a
      // downgrade, silently and permanently.
      this.stale = true;
      this.resolved = true;
      this.pending = null;
      this.clearTimer();
      this.emit({ kind: "stale" });
      return;
    }

    const shared = sharedContent(clean);
    if (this.held !== null && stored.revision < this.held.revision) return;
    const known = this.held !== null && shared === this.held.shared;
    const first = !this.resolved;
    this.held = { revision: stored.revision, shared };
    this.resolved = true;
    if (known) return;

    // A document we did not write supersedes one we have not sent. Keeping
    // the unsent one would write it straight back over the arriving row and
    // then adopt the arriving row anyway -- two writes and an incoherent
    // result. The window it can lose is one debounce.
    this.pending = null;
    this.clearTimer();
    this.emit({ kind: "document", document: clean, origin: first ? "hydrate" : "remote" });
  }

  private schedule(): void {
    if (!this.resolved || this.stale || this.pending === null) return;
    if (this.timer !== null) return;
    this.timer = setTimeout(() => {
      this.timer = null;
      void this.drain();
    }, this.debounceMs);
  }

  private async drain(): Promise<void> {
    if (!this.resolved || this.stale || this.writing) return;
    const doc = this.pending;
    if (doc === null) return;
    this.pending = null;
    this.writing = true;
    const revision = (this.held?.revision ?? 0) + 1;
    try {
      await this.gateway.write({ revision, document: doc });
      this.held = { revision, shared: sharedContent(doc) };
    } catch {
      // NOT re-queued on a timer. A cluster refusing this write will refuse
      // the retry too, and a writer spinning against a refusal is worse than
      // a desktop that roams on the person's next edit. Local has it.
    } finally {
      this.writing = false;
      if (this.pending !== null) this.schedule();
    }
  }

  private clearTimer(): void {
    if (this.timer === null) return;
    clearTimeout(this.timer);
    this.timer = null;
  }

  private emit(event: DesktopStoreEvent): void {
    for (const listener of [...this.listeners]) {
      try {
        listener(event);
      } catch {
        // A listener's failure is its own; it must not stop the others or
        // leave the store believing it never announced.
      }
    }
  }
}
