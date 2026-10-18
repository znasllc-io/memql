import { backgroundReadController } from "./queryPriority.js";
import { isTransportInterruption } from "./dispatcher.js";
// LiveCollection -- the one machine every live surface was hand-rolling
// (memql#4538).
//
// Read a list, subscribe, fold events by id, re-read `payload_omitted` rows,
// drop what the read refuses, re-apply the read's own scope, notice when the
// stream lost continuity, and track "live is degraded" separately from "the
// read failed". Three divergent implementations of that existed in the portal
// alone -- one of them string-sniffing the event kind -- eleven more surfaces
// had no liveness at all, and nothing survived navigation.
//
// THE LIFECYCLE IS SUBSCRIBE-THEN-SEED, and the order is load-bearing. The
// engine registers a subscription synchronously and runs a read on a
// goroutine (memql#4536, design D2), so subscribing first and reading second
// cannot miss a row: anything written during the read arrives as an event and
// folds onto the seeded answer by id -- which is why a seed KEEPS what arrives
// while it reads and replays it onto its answer (see runSeed). The reverse
// order can miss a row forever, and no amount of client care recovers it.
//
// AN ENGINE ROW CARRIES ITS VERSION, AND THAT IS THE ORDER (memql#6033). The
// store is append-only: every write lands a new version stamped `createdAt`,
// the latest version is the one with the greatest, and every read, re-read and
// graph event carries that field at the top level. Arrival order is not that
// order -- the engine delivers each event on its own goroutine, and a read can
// answer after an event it is newer than -- so by default a collection keeps
// the newer version of a row whichever arrives last (see `supersedes`).
//
// CONTINUITY IS A STREAM PROPERTY, NOT A COLLECTION PROPERTY. `seq` numbers
// every notification on the socket and `gap_before` lands on whichever
// delivery comes first after a drop, so no single collection can read either
// field correctly on its own -- see LiveStore, which watches the stream once
// and re-seeds everything when the socket loses its thread.
//
// IN-MEMORY ONLY. A full page reload starts clean, by construction: there is
// no persistence layer to be stale, and "what a refresh gives you" is a
// question with one answer (owner decision, 2026-08-25).

import type { SubscriptionManager } from "./subscriptions.js";
import type { ConnectionStatusEvent } from "./connection.js";
import { isVersionTime, versionOrder, type Event, type GraphAction, type Row } from "./types.js";

// LiveState is what a UI MUST be able to render. `degraded` is the one that
// did not exist before and matters most: rows on screen that are known to be
// behind, with a re-seed in flight. A surface that cannot say this renders a
// live-looking table over a stream it has lost.
export type LiveState = "seeding" | "live" | "degraded" | "disconnected";

export interface LiveSnapshot<T> {
  rows: T[];
  state: LiveState;
  // The last read failure, empty when the last read succeeded. Kept SEPARATE
  // from `state`: an error with rows still on screen is a different situation
  // from an empty list, and collapsing them loses the operator's context.
  error: string;
  // Bumped on every observable change. A binding compares this rather than
  // deep-equalling rows.
  version: number;
  /** A paged seed stopped at its safety limit with a cursor still outstanding. */
  truncated?: boolean;
}

export interface SeedPage<T> {
  rows: T[];
  nextCursor: string;
}

export interface LiveCollectionSpec<T = Row> {
  // The concept whose CDC events this collection folds. The server composes
  // the bus topic from it (memql#2460).
  concept: string;
  // Read one page. `cursor` is "" for the first page. A collection that pages
  // walks the cursor INSIDE the seed, so paging never tears the subscription
  // down and re-opens it.
  seed: (cursor: string, signal: AbortSignal) => Promise<SeedPage<T>>;
  // Row identity. Defaults to the wire's `id`, falling back to a nested
  // payload's id -- the two shapes a graph row arrives in.
  rowId?: (row: T) => string;
  // Re-read ONE row through the authorized read path, for an id-only
  // (`payload_omitted`) event. Without it such events are dropped: rendering
  // an id-only payload as a row yields a card whose every field is blank.
  reread?: (rowId: string, signal: AbortSignal) => Promise<T | null>;
  // Re-apply the READ's own scope to a FOLDED row.
  //
  // A subscription is scoped by concept; a read is usually scoped by more than
  // that. The two disagree the moment a caller can see rows the list
  // deliberately excludes -- the fleet page's lesson, where an owner's
  // subscription carries machines the scoped read filtered out, and folding
  // them in makes rows appear that a refresh then removes.
  //
  // IT DOES NOT TOUCH SEEDED ROWS, and that is deliberate rather than an
  // omission. The read is the authority on membership (see runSeed), and this
  // predicate is a CLIENT-SIDE MIRROR of a decision the server already made --
  // typically over state that resolves asynchronously, like the caller's own
  // user id. Applying a not-yet-resolved mirror to the seed empties the list
  // and then repopulates it, which looks exactly like a page that loaded
  // nothing.
  //
  // So: NARROW IN `seed`, which is entirely the caller's code and is where a
  // read's meaning belongs -- including a narrowing no query declares. Use
  // this to say the same thing about events.
  inScope?: (row: T) => boolean;
  // Re-read EVERY event through `reread`, not only the id-only ones.
  //
  // The default trusts a full payload, which is right for a list whose event
  // rate is high and whose rows are cheap. It is wrong for a surface that is
  // about to move to the `granted` tier: there, the id-only path is the one
  // most events will take, and having two code paths means the trusted one is
  // the one that stops being exercised and quietly rots. Making the re-read
  // the ONLY path removes the branch. Repeated events for a row are coalesced,
  // and background re-reads share a concurrency limit across the connection.
  //
  // Requires `reread`; without it this is ignored, because dropping every
  // event would be a worse answer than trusting the payload.
  rereadEveryEvent?: boolean;
  // Decide whether an arriving copy REPLACES the one held.
  //
  // Arrivals are not ordered: the engine delivers each event on its own
  // goroutine, two re-reads issued a moment apart can settle in either order,
  // and a read can answer after an event it is newer than. Taking the last
  // arrival lets an older copy roll a newer one backwards -- silently, only
  // under load, and for good when the change after it sends nothing this
  // collection folds.
  //
  // THE DEFAULT ORDERS ENGINE ROWS BY THEIR VERSION (memql#6033): the
  // `createdAt` each row and event carries at the top level, compared at every
  // digit the wire carries. An arrival older than the row held is refused; the
  // same version or a newer one replaces it. Where either side has no version
  // time to compare -- a shape that does not project `row.createdAt`, a row
  // mapped without it -- the arrival wins, as any later arrival would. Pass
  // your own to order by something else (a concept's own watermark), or to
  // take every arrival.
  //
  // Return false to keep what is held. Called with `held === undefined` for a
  // row the collection has not seen, where returning false would mean the row
  // can never arrive at all. A removal is not weighed: it always lands.
  //
  // It is also the judge when a seed replays what arrived while it read: the
  // row the read returned is what is held, so an arrival older than it is
  // refused rather than rolling it back.
  //
  // PASSING ONE TURNS THE TOMBSTONES OFF. The default also remembers the version
  // that took a row out of the list (a removal, or a copy outside the read's
  // scope) so a late OLDER copy cannot bring it back. A collection with its own
  // judge keeps no such record: it owns its rows' history, and once a row is out
  // nothing is held to compare a late copy with, so the judge sees it as a row
  // the collection has not seen (`held === undefined`) and decides whether it
  // may arrive.
  supersedes?: (incoming: T, held: T | undefined) => boolean;
  // CDC verbs. Defaults to all three.
  actions?: GraphAction[];
  // Walk every page during a seed (default true). A surface that shows one
  // bounded window sets false and pages itself.
  paged?: boolean;
  // Runaway-cursor guard for the paged walk (default 50 pages). A server that
  // keeps minting a cursor would otherwise spin forever.
  maxPages?: number;
}

const DEFAULT_MAX_PAGES = 50;

// How many removed rows a collection remembers the version of (see `gone`).
const MAX_TOMBSTONES = 1_024;

// defaultRowId reads the two shapes a graph row arrives in: flattened (a
// browse result, a CDC payload) and nested under `payload` (an authorized
// single-row read).
function defaultRowId(row: unknown): string {
  if (!row || typeof row !== "object") return "";
  const rec = row as Record<string, unknown>;
  if (typeof rec["id"] === "string") return rec["id"];
  const nested = rec["payload"];
  if (nested && typeof nested === "object" && !Array.isArray(nested)) {
    const id = (nested as Record<string, unknown>)["id"];
    if (typeof id === "string") return id;
  }
  return "";
}

// eventRowId pulls the row id out of a CDC envelope, which carries it
// flattened alongside the intrinsics and again under `payload`.
function eventRowId(event: Event): string {
  const payload = event.payload;
  if (!payload) return "";
  return defaultRowId(payload);
}

// newerOrSame is the default `supersedes`: the arrival replaces the held row
// unless both carry a version time and the arrival's is older.
function newerOrSame(incoming: unknown, held: unknown): boolean {
  if (held === undefined) return true;
  const order = versionOrder(versionTimeOf(incoming), versionTimeOf(held));
  return order === null || order >= 0;
}

// versionTimeOf reads an engine row's version time: `createdAt`, at the top
// level in every shape a row arrives in -- a shaped read that projects
// `row.createdAt`, a flattened or raw node, and a graph event's payload.
function versionTimeOf(row: unknown): unknown {
  if (!row || typeof row !== "object") return undefined;
  return (row as Record<string, unknown>)["createdAt"];
}

// DELETED is matched on the decoded event KIND, never by sniffing a string
// out of the payload. The kinds come from the proto enum; a substring test
// over a topic is what one of the replaced implementations did, and it is one
// renamed topic away from silently folding deletes as updates.
function isDeleteKind(kind: string): boolean {
  return kind === "NODE_DELETED";
}

// The bridge defaults to four active queries, with a bounded waiting queue.
// Live refreshes must leave room for page loads and user actions. One queue
// belongs to the connection's LiveStore, not each pane independently.
class RereadQueue {
  private active = 0;
  private readonly pending = new Set<() => Promise<void>>();

  enqueue(read: () => Promise<void>): () => void {
    this.pending.add(read);
    this.drain();
    return () => { this.pending.delete(read); };
  }

  private drain(): void {
    while (this.active < 2) {
      const read = this.pending.values().next().value;
      if (!read) return;
      this.pending.delete(read);
      this.active++;
      // Defer invocation so events delivered together can share one read.
      void Promise.resolve().then(read).catch(() => {}).finally(() => {
        this.active--;
        this.drain();
      });
    }
  }
}

interface PendingReread {
  abort: AbortController;
  running: boolean;
  dirty: boolean;
  cancel: () => void;
}

export class LiveCollection<T = Row> {
  private rowsById = new Map<string, T>();
  private order: string[] = [];
  private state: LiveState = "seeding";
  private errorText = "";
  private truncated = false;
  private versionValue = 0;
  private readonly listeners = new Set<() => void>();
  private cachedSnapshot: LiveSnapshot<T> | null = null;

  private unsubscribe: (() => void) | null = null;
  // What arrived while the running seed read, for it to replay onto its
  // answer: the last word per row -- its newest copy, or its removal (null) --
  // so it is bounded by the rows that changed, not by the events. Null while
  // no seed is reading.
  private arrivals: Map<string, T | null> | null = null;
  // WHAT TOOK A ROW OUT KEEPS IT OUT (memql#6033): id -> the version time of
  // the copy that took the row out of this list -- a removal that carried one,
  // or a copy outside the read's scope. Once a row is out nothing is held to
  // compare a late copy with, and the bus delivers events in no particular
  // order, so without this a late OLDER copy brings back a deleted or archived
  // row. Kept under the default ordering only (a feed's own `supersedes` owns
  // its rows' history) and ACROSS SEEDS: a read that does not return a removed
  // row says nothing about how old a late copy of it is, and this still does.
  // Only a strictly older copy is refused, so a row re-created or re-read at
  // that version or a newer one is placed. Bounded, and that is the only bound:
  // the oldest is forgotten first.
  private gone = new Map<string, string>();
  private seedAbort: AbortController | null = null;
  private seedPending = false;
  private seedRunning = false;
  private closed = false;
  private refs = 0;
  private lingerTimer: ReturnType<typeof setTimeout> | null = null;
  private readonly rereads = new Map<string, PendingReread>();

  constructor(
    private readonly spec: LiveCollectionSpec<T>,
    private readonly subscriptions: SubscriptionManager | null,
    private readonly lingerMs: number,
    private readonly rereadQueue = new RereadQueue(),
  ) {}

  get snapshot(): LiveSnapshot<T> {
    if (this.cachedSnapshot === null) {
      this.cachedSnapshot = {
        rows: this.order.flatMap((id) => {
          const row = this.rowsById.get(id);
          return row === undefined ? [] : [row];
        }),
        state: this.state,
        error: this.errorText,
        version: this.versionValue,
        truncated: this.truncated,
      };
    }
    return this.cachedSnapshot;
  }

  // subscribe is the useSyncExternalStore shape: a change notifier, not a
  // value channel. The caller re-reads `snapshot`, which is cached until
  // something actually changes -- so an unchanged store returns an
  // identity-stable object and a binding does not re-render.
  subscribe(listener: () => void): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  // retain / release are the reference count. The store hands out the same
  // collection to every caller asking for the same key, so two components
  // share one subscription and one seed -- and the last release lingers
  // rather than tearing down, which is what makes A -> B -> A navigation
  // issue zero new reads.
  retain(): void {
    this.refs++;
    if (this.lingerTimer !== null) {
      clearTimeout(this.lingerTimer);
      this.lingerTimer = null;
    }
    if (this.unsubscribe === null && !this.closed) this.start();
  }

  release(onExpire: () => void): void {
    this.refs = Math.max(0, this.refs - 1);
    if (this.refs > 0 || this.closed) return;
    this.lingerTimer = setTimeout(() => {
      this.lingerTimer = null;
      if (this.refs === 0) {
        this.close();
        onExpire();
      }
    }, this.lingerMs);
  }

  // reseed re-runs the read. It is the answer to a gap, a reconnect, and an
  // explicit refresh -- one mechanism, so there is one thing to get right.
  // Coalesced: a burst produces exactly one seed.
  reseed(): void {
    if (this.closed) return;
    if (this.seedRunning) {
      this.seedPending = true;
      // Rows on screen are known to be behind now, and saying so is the whole
      // point of the state.
      this.setState(this.rowsById.size > 0 ? "degraded" : "seeding");
      return;
    }
    void this.runSeed();
  }

  // markDisconnected is called by the store when the stream is down. Rows are
  // KEPT -- an operator staring at a table wants the last known answer, not a
  // blank -- but the state says they are no longer live.
  markDisconnected(): void {
    if (this.closed) return;
    this.seedAbort?.abort();
    this.clearRereads();
    this.setState("disconnected");
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.seedAbort?.abort();
    this.clearRereads();
    this.unsubscribe?.();
    this.unsubscribe = null;
    if (this.lingerTimer !== null) {
      clearTimeout(this.lingerTimer);
      this.lingerTimer = null;
    }
    this.listeners.clear();
  }

  // ---- internals ---------------------------------------------------------

  private start(): void {
    // SUBSCRIBE FIRST. See the module comment: registration is synchronous
    // server-side, so a row written during the seed arrives as an event and
    // folds by id. Reading first can miss it forever.
    if (this.subscriptions && typeof this.subscriptions.subscribeGraph === "function") {
      try {
        this.unsubscribe = this.subscriptions.subscribeGraph((event) => this.fold(event), {
          concept: this.spec.concept,
          actions: this.spec.actions ?? ["created", "updated", "deleted"],
          idOnly: !!(this.spec.rereadEveryEvent && this.spec.reread),
        });
      } catch (err) {
        // A refused subscribe does not break the collection -- ordinary reads
        // still work on the same connection. It breaks the PROMISE that it is
        // live, which has to be said out loud rather than inferred from rows
        // that never appear.
        this.errorText = err instanceof Error ? err.message : String(err);
        this.setState("degraded");
      }
    }
    void this.runSeed();
  }

  private async runSeed(): Promise<void> {
    this.seedRunning = true;
    this.seedPending = false;
    this.seedAbort?.abort();
    const abort = new AbortController();
    this.seedAbort = abort;
    if (this.rowsById.size === 0) this.setState("seeding");

    // KEEP WHAT ARRIVES WHILE THIS READS (memql#6033). Every arrival still
    // folds onto the rows on screen at once -- a list being re-read is still
    // live -- but those rows are about to be REPLACED by the read's answer,
    // and a page answered before an arrival is older than it: a `sent` that
    // lands between page 1 and page 2 would be overwritten by page 1's
    // `sending` until the next reseed, an insert that sorts onto a page already
    // read would never be seen, and a delete would come back. So each seed
    // keeps its own record, from before its first page is asked for until its
    // answer is installed -- the last word per row (see keep) -- and replays
    // it onto that answer.
    const arrivals = new Map<string, T | null>();
    this.arrivals = arrivals;

    const collected = new Map<string, T>();
    const seenOrder: string[] = [];
    const idOf = this.spec.rowId ?? (defaultRowId as (row: T) => string);
    const maxPages = this.spec.maxPages ?? DEFAULT_MAX_PAGES;
    const paged = this.spec.paged !== false;

    try {
      let cursor = "";
      for (let page = 0; page < maxPages; page++) {
        const result = await this.spec.seed(cursor, abort.signal);
        if (abort.signal.aborted) return;
        for (const row of result.rows) {
          const id = idOf(row);
          if (id === "") continue;
          if (!collected.has(id)) seenOrder.push(id);
          collected.set(id, row);
        }
        cursor = result.nextCursor;
        if (!paged || cursor === "") break;
      }
      if (abort.signal.aborted) return;
      // REPLACE, not merge. The read is the authority on membership: a row
      // the seed no longer returns is a row that left the set, and merging
      // would keep it on screen forever.
      this.truncated = paged && cursor !== "";
      this.rowsById = collected;
      this.order = seenOrder;
      // ...THEN REPLAY what arrived while it read, through the same rules a
      // fold follows: a removal removes, a row out of the read's scope leaves,
      // and the ordering judges an arrival against the row the read returned
      // -- an older version does not roll it back (see `supersedes`).
      this.arrivals = null;
      for (const [id, row] of arrivals) {
        if (row === null) this.drop(id);
        else this.place(id, row);
      }
      this.errorText = "";
      this.bump();
      this.setState("live");
    } catch (err) {
      if (abort.signal.aborted) return;
      if (isTransportInterruption(err)) {
        this.errorText = "";
        this.setState("disconnected");
        return;
      }
      this.errorText = err instanceof Error ? err.message : String(err);
      // Rows already on screen stay, marked degraded; with nothing to show,
      // "seeding" would be a lie about work in progress.
      this.setState(this.rowsById.size > 0 ? "degraded" : "live");
    } finally {
      // A seed that failed or was abandoned has nothing to replay onto: what
      // arrived during it already folded onto the rows on screen.
      if (this.arrivals === arrivals) this.arrivals = null;
      this.seedRunning = false;
      if (this.seedPending && !this.closed) {
        this.seedPending = false;
        void this.runSeed();
      }
    }
  }

  private fold(event: Event): void {
    if (this.closed) return;
    const id = eventRowId(event);
    if (id === "") return;

    if (isDeleteKind(event.kind)) {
      this.cancelReread(id);
      // The removal's own time, when it carries one, is what keeps an older
      // copy from bringing the row back (see `gone`).
      this.arrive(id, null, versionTimeOf(event.payload));
      return;
    }

    const reread = this.spec.reread;
    if (event.payloadOmitted || (this.spec.rereadEveryEvent && reread)) {
      // The `granted` tier cannot be decided against one row at fan-out, so
      // the engine sent the identity and left the decision to a read
      // (memql#4309). A REFUSED read drops the event silently: the caller was
      // not entitled to the row, and announcing that one changed would leak
      // exactly what the gate withheld.
      if (!reread) return;
      this.requestReread(id);
      return;
    }

    const payload = event.payload;
    if (!payload) return;
    this.arrive(id, payload as unknown as T);
  }

  private requestReread(id: string): void {
    const reread = this.spec.reread;
    if (!reread || this.closed) return;
    const pending = this.rereads.get(id);
    if (pending) {
      // A queued read has not taken its snapshot yet. An in-flight one may
      // have, so remember one follow-up to cover changes during that read.
      if (pending.running) pending.dirty = true;
      return;
    }
    const next: PendingReread = {
      abort: backgroundReadController(), running: false, dirty: false, cancel: () => {},
    };
    this.rereads.set(id, next);
    next.cancel = this.rereadQueue.enqueue(async () => {
      if (this.rereads.get(id) !== next) return;
      next.running = true;
      try {
        const row = await reread(id, next.abort.signal);
        // A delete, disconnect or teardown can invalidate even an unversioned
        // answer, and a read is allowed to settle after its signal was aborted.
        if (this.rereads.get(id) === next) this.arrive(id, row);
      } catch {
        // Preserve the authorized-read contract: a refusal is not rendered.
      } finally {
        if (this.rereads.get(id) === next) {
          this.rereads.delete(id);
          if (next.dirty) this.requestReread(id);
        }
      }
    });
  }

  private cancelReread(id: string): void {
    const pending = this.rereads.get(id);
    if (!pending) return;
    this.rereads.delete(id);
    pending.cancel();
    // Keep an in-flight slot until the read settles. Removing a row alone
    // must not free capacity while the server is still answering its read.
  }

  private clearRereads(): void {
    const pending = [...this.rereads.values()];
    this.rereads.clear();
    for (const read of pending) {
      read.cancel();
      read.abort.abort();
    }
  }

  // arrive folds one change onto the rows on screen and, while a seed reads,
  // keeps it for that seed to replay onto its answer (see runSeed). A removal
  // passes the version time it carried, if any (see `gone`).
  private arrive(id: string, row: T | null, removedAt?: unknown): void {
    if (row === null) this.entomb(id, removedAt);
    if (this.arrivals !== null) this.keep(this.arrivals, id, row);
    if (row === null ? this.drop(id) : this.place(id, row)) this.bump();
  }

  // keep records one arrival for the running seed as the last word on its
  // row: a removal replaces whatever was kept, and a row replaces a kept
  // removal or a kept row it supersedes -- judged as a fold judges, so an
  // older version arriving late does not displace a newer one already kept,
  // nor a removal it is older than (the replay would bring the row back).
  private keep(arrivals: Map<string, T | null>, id: string, row: T | null): void {
    if (row !== null && this.entombed(id, row)) return;
    const kept = arrivals.get(id);
    if (row !== null && kept !== undefined && kept !== null && !this.supersedes(row, kept)) return;
    arrivals.set(id, row);
  }

  // place puts `row` under `id` as the fold rules allow, and reports whether
  // the rows changed. It does not notify: a seed replays many at once.
  private place(id: string, row: T): boolean {
    const held = this.rowsById.get(id);
    // FIRST, WHETHER THIS COPY SAYS ANYTHING: one older than the row held says
    // nothing about it -- neither what it holds nor whether it belongs -- so it
    // is refused before its scope is read. With nothing held, the same goes
    // for a copy older than the one that took the row out.
    if (!this.supersedes(row, held)) return false;
    if (held === undefined && this.entombed(id, row)) return false;
    if (this.spec.inScope && !this.spec.inScope(row)) {
      // In scope for the SUBSCRIPTION, out of scope for the READ. Removing
      // rather than ignoring matters for the update case: a row that has just
      // left the list's scope must leave the list -- and stay out of it for
      // any copy older than this one.
      this.entomb(id, versionTimeOf(row));
      return this.drop(id);
    }
    if (held === undefined) this.order.push(id);
    this.rowsById.set(id, row);
    this.gone.delete(id);
    return true;
  }

  // entomb remembers the version time that took `id` out of the list, keeping
  // the newer where one is already kept. A removal with no time records
  // nothing: there is nothing to compare a later copy with.
  private entomb(id: string, at: unknown): void {
    if (this.spec.supersedes || !isVersionTime(at)) return;
    const had = this.gone.get(id);
    if (had !== undefined) {
      if ((versionOrder(at, had) ?? 0) < 0) return;
      // Re-inserted, so it is forgotten last: it is the newest word on the row.
      this.gone.delete(id);
    }
    this.gone.set(id, at);
    if (this.gone.size > MAX_TOMBSTONES) {
      const oldest = this.gone.keys().next().value;
      if (oldest !== undefined) this.gone.delete(oldest);
    }
  }

  // entombed: this copy is older than the one that took the row out.
  private entombed(id: string, row: T): boolean {
    if (this.spec.supersedes) return false;
    const at = this.gone.get(id);
    if (at === undefined) return false;
    const order = versionOrder(versionTimeOf(row), at);
    return order !== null && order < 0;
  }

  // supersedes is the feed's own judge, or the default: by version.
  private supersedes(incoming: T, held: T | undefined): boolean {
    return (this.spec.supersedes ?? newerOrSame)(incoming, held);
  }

  // drop removes `id`, and reports whether it was there.
  private drop(id: string): boolean {
    if (!this.rowsById.delete(id)) return false;
    this.order = this.order.filter((candidate) => candidate !== id);
    return true;
  }

  private setState(next: LiveState): void {
    if (this.state === next) return;
    this.state = next;
    this.bump();
  }

  private bump(): void {
    this.versionValue++;
    this.cachedSnapshot = null;
    for (const listener of [...this.listeners]) listener();
  }
}

// LiveValue is the single-read counterpart: one shared, connection-scoped
// answer with IN-FLIGHT DEDUPE.
//
// It exists because the portal asked the cluster who it was fourteen times per
// page load -- once per component that needed the caller's role -- and the
// only thing wrong with each of those call sites was that there were fourteen
// of them.
export class LiveValue<T> {
  private value: T | null = null;
  private state: LiveState = "seeding";
  private errorText = "";
  private versionValue = 0;
  private inFlight: Promise<void> | null = null;
  private readonly listeners = new Set<() => void>();
  private cachedSnapshot: LiveValueSnapshot<T> | null = null;
  private refs = 0;
  private closed = false;
  private lingerTimer: ReturnType<typeof setTimeout> | null = null;

  constructor(
    private readonly read: (signal: AbortSignal) => Promise<T | null>,
    private readonly lingerMs: number,
  ) {}

  get snapshot(): LiveValueSnapshot<T> {
    if (this.cachedSnapshot === null) {
      this.cachedSnapshot = {
        value: this.value,
        state: this.state,
        error: this.errorText,
        version: this.versionValue,
      };
    }
    return this.cachedSnapshot;
  }

  subscribe(listener: () => void): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  retain(): void {
    this.refs++;
    if (this.lingerTimer !== null) {
      clearTimeout(this.lingerTimer);
      this.lingerTimer = null;
    }
    if (this.versionValue === 0 && this.inFlight === null) this.refresh();
  }

  release(onExpire: () => void): void {
    this.refs = Math.max(0, this.refs - 1);
    if (this.refs > 0 || this.closed) return;
    this.lingerTimer = setTimeout(() => {
      this.lingerTimer = null;
      if (this.refs === 0) {
        this.close();
        onExpire();
      }
    }, this.lingerMs);
  }

  // refresh reads, or joins the read already in flight. The dedupe is the
  // point: N callers arriving in the same tick produce ONE round trip.
  refresh(): void {
    if (this.closed || this.inFlight !== null) return;
    const abort = new AbortController();
    this.inFlight = this.read(abort.signal)
      .then((value) => {
        if (this.closed) return;
        this.value = value;
        this.errorText = "";
        this.state = "live";
        this.bump();
      })
      .catch((err: unknown) => {
        if (this.closed) return;
        this.errorText = isTransportInterruption(err) ? "" : err instanceof Error ? err.message : String(err);
        this.state = isTransportInterruption(err) ? "disconnected" : this.value === null ? "live" : "degraded";
        this.bump();
      })
      .finally(() => {
        this.inFlight = null;
      });
  }

  markDisconnected(): void {
    if (this.closed || this.state === "disconnected") return;
    this.state = "disconnected";
    this.bump();
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    if (this.lingerTimer !== null) {
      clearTimeout(this.lingerTimer);
      this.lingerTimer = null;
    }
    this.listeners.clear();
  }

  private bump(): void {
    this.versionValue++;
    this.cachedSnapshot = null;
    for (const listener of [...this.listeners]) listener();
  }
}

export interface LiveValueSnapshot<T> {
  value: T | null;
  state: LiveState;
  error: string;
  version: number;
}

// LiveStoreHost is the slice of a Connection the store needs. A narrow
// interface rather than the class so a test -- or a consumer wrapping the
// connection -- can supply one without a socket.
export interface LiveStoreHost {
  subscriptions: SubscriptionManager | null;
  onConnectionCycle?: (handler: (cycle: number) => void) => () => void;
  onStatusChange?: (handler: (ev: ConnectionStatusEvent) => void) => () => void;
}

export interface LiveStoreOptions {
  // How long a released collection lingers before teardown (default 20s).
  // This is what makes A -> B -> A navigation free; it is also the only
  // memory the store holds after nothing is watching.
  lingerMs?: number;
}

const DEFAULT_LINGER_MS = 20_000;

// LiveStore owns every collection on ONE connection, and owns the stream's
// continuity for all of them.
//
// The continuity job cannot live in a collection: `seq` numbers every
// notification on the socket and `gap_before` lands on whichever delivery
// comes first after a drop, so a collection reading either field sees holes
// that belong to its neighbours, or misses its own. Watching once, here, and
// re-seeding EVERYTHING is both the correct reading and the honest one -- an
// overflow means the socket lost deliveries, and which ones is exactly the
// thing nobody knows.
export class LiveStore {
  private readonly collections = new Map<string, LiveCollection<never>>();
  private readonly values = new Map<string, LiveValue<never>>();
  private readonly lingerMs: number;
  private readonly teardown: Array<() => void> = [];
  private readonly gapListeners = new Set<() => void>();
  private lastSeq = 0;
  private disposed = false;
  private readonly rereadQueue = new RereadQueue();

  constructor(
    private readonly host: LiveStoreHost,
    opts: LiveStoreOptions = {},
  ) {
    this.lingerMs = opts.lingerMs ?? DEFAULT_LINGER_MS;

    const subs = host.subscriptions;
    // `typeof === "function"` rather than a bare call, for the reason
    // ClusterProvider states about `conn.subscriptions ?? null`: the host may
    // be a Connection-shaped object that is not one -- an older SDK build, or
    // a test double narrowed to the methods its subject uses. Such a host
    // simply gets no stream-wide continuity, which degrades to "re-seed only
    // on an explicit reload"; calling into undefined would take the whole
    // console down at dial time instead.
    if (subs && typeof subs.onDelivery === "function") {
      this.teardown.push(subs.onDelivery((event) => this.observeDelivery(event)));
    }
    if (host.onConnectionCycle) {
      // A reconnect IS a gap: the new stream restarts the sequence
      // (memql#4536). It fires after the SDK replayed every subscription, so
      // re-seeding here cannot race its own subscription.
      this.teardown.push(
        host.onConnectionCycle(() => {
          this.lastSeq = 0;
          this.reseedAll();
        }),
      );
    }
    if (host.onStatusChange) {
      this.teardown.push(
        host.onStatusChange((ev) => {
          if (ev.status === "connected") return;
          for (const c of this.collections.values()) c.markDisconnected();
          for (const v of this.values.values()) v.markDisconnected();
        }),
      );
    }
  }

  // collection returns the shared collection for `key`, creating it on first
  // use. RETAINED for the caller -- the returned release() is the caller's
  // only obligation.
  collection<T>(key: string, spec: LiveCollectionSpec<T>): LiveHandle<LiveCollection<T>> {
    let existing = this.collections.get(key) as LiveCollection<T> | undefined;
    if (!existing) {
      // `?? null` because the host may be a Connection-SHAPED object rather
      // than a Connection -- an older SDK, or a test double narrowed to the
      // methods its subject uses. The declared type says "a manager or null";
      // an absent field is neither, and the collection's own guard is what
      // turns that into "no liveness" rather than a call on undefined.
      existing = new LiveCollection<T>(spec, this.host.subscriptions ?? null, this.lingerMs, this.rereadQueue);
      this.collections.set(key, existing as unknown as LiveCollection<never>);
    }
    existing.retain();
    let released = false;
    return {
      value: existing,
      release: () => {
        if (released) return;
        released = true;
        existing.release(() => this.collections.delete(key));
      },
    };
  }

  // value returns the shared single-read for `key`. Same refcount contract.
  value<T>(key: string, read: (signal: AbortSignal) => Promise<T | null>): LiveHandle<LiveValue<T>> {
    let existing = this.values.get(key) as LiveValue<T> | undefined;
    if (!existing) {
      existing = new LiveValue<T>(read, this.lingerMs);
      this.values.set(key, existing as unknown as LiveValue<never>);
    }
    existing.retain();
    let released = false;
    return {
      value: existing,
      release: () => {
        if (released) return;
        released = true;
        existing.release(() => this.values.delete(key));
      },
    };
  }

  // onGap notifies a consumer that the stream lost continuity -- an overflow,
  // a non-contiguous sequence, or a reconnect (memql#4539).
  //
  // Collections re-seed themselves; this exists for a surface that folds by
  // hand for a reason the collection cannot express -- the concept browser's
  // arrivals BAND is the case: it accumulates arrivals ALONGSIDE a paged walk
  // rather than into it, because the walk's keyset cursor orders ascending and
  // splicing a row created now guarantees a duplicate when paging reaches it.
  //
  // Such a consumer must not read seq / gap_before itself. Continuity is a
  // property of the STREAM, and a handler watching one subscription sees holes
  // that belong to its neighbours and misses its own. This is the one reading.
  onGap(handler: () => void): () => void {
    this.gapListeners.add(handler);
    return () => this.gapListeners.delete(handler);
  }

  reseedAll(): void {
    for (const c of this.collections.values()) c.reseed();
    for (const v of this.values.values()) v.refresh();
    for (const notify of [...this.gapListeners]) {
      try {
        notify();
      } catch {
        // A consumer's handler is its own bug; one throwing must not stop the
        // rest of the store from recovering.
      }
    }
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    for (const fn of this.teardown) fn();
    this.teardown.length = 0;
    for (const c of this.collections.values()) c.close();
    for (const v of this.values.values()) v.close();
    this.collections.clear();
    this.values.clear();
    this.gapListeners.clear();
  }

  private observeDelivery(event: Event): void {
    if (this.disposed) return;
    let gapped = event.gapBefore;
    if (event.seq > 0) {
      // A zero seq is "this server does not number its deliveries", not "the
      // first event" -- and comparing against it would report a gap on every
      // delivery from an older node.
      if (this.lastSeq > 0 && event.seq !== this.lastSeq + 1) gapped = true;
      this.lastSeq = event.seq;
    }
    if (gapped) this.reseedAll();
  }
}

export interface LiveHandle<T> {
  value: T;
  // Drop this caller's claim. After the last release the underlying store
  // LINGERS before teardown, so a remount inside that window reuses it and
  // issues no new read.
  release: () => void;
}

// liveStoreFor returns the ONE store for a connection, creating it on first
// use. A module-level registry, keyed by the connection object, so a store
// outlives every component that mounts against it -- which is what makes
// navigation instant -- while still being scoped to the actor that connection
// resolved for. A different connection is a different actor and gets its own.
const stores = new WeakMap<object, LiveStore>();

export function liveStoreFor(host: LiveStoreHost, opts: LiveStoreOptions = {}): LiveStore {
  const existing = stores.get(host as object);
  if (existing) return existing;
  const created = new LiveStore(host, opts);
  stores.set(host as object, created);
  return created;
}

// disposeLiveStoreFor tears down a connection's store. Called when the
// connection itself is finished; a WeakMap would collect it eventually, but
// "eventually" leaves subscriptions registered on a dead stream.
export function disposeLiveStoreFor(host: LiveStoreHost): void {
  const existing = stores.get(host as object);
  if (!existing) return;
  existing.dispose();
  stores.delete(host as object);
}
