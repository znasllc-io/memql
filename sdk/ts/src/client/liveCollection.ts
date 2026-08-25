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
// folds onto the seeded answer by id. The reverse order can miss a row
// forever, and no amount of client care recovers it.
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
import type { Event, GraphAction, Row } from "./types.js";

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
  // the ONLY path costs a round trip per event and removes the branch.
  //
  // Requires `reread`; without it this is ignored, because dropping every
  // event would be a worse answer than trusting the payload.
  rereadEveryEvent?: boolean;
  // Decide whether an arriving copy REPLACES the one held.
  //
  // The default is last-write-wins, which is correct when arrivals are
  // ordered. They are not always: two re-reads issued a moment apart can
  // settle in either order, and for a concept that carries its own version
  // watermark that means an older copy can roll a newer one backwards --
  // silently, and only under load.
  //
  // Return false to keep what is held. Called with `held === undefined` for a
  // row the collection has not seen, where returning false would mean the row
  // can never arrive at all.
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

// DELETED is matched on the decoded event KIND, never by sniffing a string
// out of the payload. The kinds come from the proto enum; a substring test
// over a topic is what one of the replaced implementations did, and it is one
// renamed topic away from silently folding deletes as updates.
function isDeleteKind(kind: string): boolean {
  return kind === "NODE_DELETED";
}

export class LiveCollection<T = Row> {
  private rowsById = new Map<string, T>();
  private order: string[] = [];
  private state: LiveState = "seeding";
  private errorText = "";
  private versionValue = 0;
  private readonly listeners = new Set<() => void>();
  private cachedSnapshot: LiveSnapshot<T> | null = null;

  private unsubscribe: (() => void) | null = null;
  private seedAbort: AbortController | null = null;
  private seedPending = false;
  private seedRunning = false;
  private closed = false;
  private refs = 0;
  private lingerTimer: ReturnType<typeof setTimeout> | null = null;

  constructor(
    private readonly spec: LiveCollectionSpec<T>,
    private readonly subscriptions: SubscriptionManager | null,
    private readonly lingerMs: number,
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
    this.setState("disconnected");
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.seedAbort?.abort();
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
      this.rowsById = collected;
      this.order = seenOrder;
      this.errorText = "";
      this.bump();
      this.setState("live");
    } catch (err) {
      if (abort.signal.aborted) return;
      this.errorText = err instanceof Error ? err.message : String(err);
      // Rows already on screen stay, marked degraded; with nothing to show,
      // "seeding" would be a lie about work in progress.
      this.setState(this.rowsById.size > 0 ? "degraded" : "live");
    } finally {
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
      this.remove(id);
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
      const abort = new AbortController();
      void reread(id, abort.signal)
        .then((row) => {
          if (this.closed) return;
          if (row === null) {
            this.remove(id);
            return;
          }
          this.upsert(id, row);
        })
        .catch(() => {
          // Refused, or the stream dropped. Neither is something to render.
        });
      return;
    }

    const payload = event.payload;
    if (!payload) return;
    this.upsert(id, payload as unknown as T);
  }

  private upsert(id: string, row: T): void {
    if (this.spec.inScope && !this.spec.inScope(row)) {
      // In scope for the SUBSCRIPTION, out of scope for the READ. Removing
      // rather than ignoring matters for the update case: a row that has just
      // left the list's scope must leave the list.
      this.remove(id);
      return;
    }
    const held = this.rowsById.get(id);
    if (this.spec.supersedes && !this.spec.supersedes(row, held)) return;
    if (held === undefined) this.order.push(id);
    this.rowsById.set(id, row);
    this.bump();
  }

  private remove(id: string): void {
    if (!this.rowsById.delete(id)) return;
    this.order = this.order.filter((candidate) => candidate !== id);
    this.bump();
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
        this.errorText = err instanceof Error ? err.message : String(err);
        this.state = this.value === null ? "live" : "degraded";
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
      existing = new LiveCollection<T>(spec, this.host.subscriptions ?? null, this.lingerMs);
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
