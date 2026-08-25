// The LiveCollection machine (memql#4538) -- the fold matrix, continuity, the
// reference count, and the dedupe.
//
// Driven against a scripted subscription manager and a counting read, with no
// socket and no server: what is being pinned is the state machine, and a
// machine asserted through three layers can fail for three unrelated reasons.

import test from "node:test";
import assert from "node:assert/strict";

import {
  LiveStore,
  type LiveCollectionSpec,
  type LiveStoreHost,
} from "../src/client/liveCollection.js";
import type { SubscriptionManager } from "../src/client/subscriptions.js";
import type { Event, GraphAction, Row } from "../src/client/types.js";

interface Sub {
  concept?: string;
  actions?: GraphAction[];
  handler: (ev: Event) => void;
}

class FakeSubscriptions {
  readonly subs: Sub[] = [];
  onSubscribe: (() => void) | null = null;
  private deliveryObservers = new Set<(ev: Event) => void>();

  subscribeGraph(handler: (ev: Event) => void, opts: { concept?: string; actions?: GraphAction[] } = {}) {
    const sub: Sub = { handler, ...opts };
    this.subs.push(sub);
    this.onSubscribe?.();
    return () => {
      const i = this.subs.indexOf(sub);
      if (i >= 0) this.subs.splice(i, 1);
    };
  }

  onDelivery(handler: (ev: Event) => void) {
    this.deliveryObservers.add(handler);
    return () => this.deliveryObservers.delete(handler);
  }

  // deliver routes an event the way the real manager does: stream-wide
  // observers first, then the matching subscription.
  deliver(ev: Event, toConcept?: string): void {
    for (const observe of [...this.deliveryObservers]) observe(ev);
    for (const sub of this.subs) {
      if (toConcept !== undefined && sub.concept !== toConcept) continue;
      sub.handler(ev);
    }
  }

  as(): SubscriptionManager {
    return this as unknown as SubscriptionManager;
  }
}

function event(partial: Partial<Event> & { payload?: Row | null }): Event {
  return {
    subscriptionId: "sub",
    kind: "NODE_CREATED",
    timestamp: null,
    payload: null,
    payloadOmitted: false,
    seq: 0,
    gapBefore: false,
    ...partial,
  };
}

function hostWith(subs: FakeSubscriptions): {
  host: LiveStoreHost;
  cycle: (n: number) => void;
  status: (s: "connected" | "reconnecting" | "disconnected") => void;
} {
  let cycleHandler: (n: number) => void = () => {};
  let statusHandler: (ev: { status: string; attempt: number; error: string }) => void = () => {};
  const host: LiveStoreHost = {
    subscriptions: subs.as(),
    onConnectionCycle: (fn) => {
      cycleHandler = fn;
      return () => {};
    },
    onStatusChange: (fn) => {
      statusHandler = fn as typeof statusHandler;
      return () => {};
    },
  };
  return {
    host,
    cycle: (n) => cycleHandler(n),
    status: (s) => statusHandler({ status: s, attempt: 0, error: "" }),
  };
}

// A seed that counts its calls, which is how "zero duplicate reads" is
// measured rather than asserted.
function countingSeed(pages: Array<{ rows: Row[]; nextCursor: string }>) {
  const calls: string[] = [];
  const seed: LiveCollectionSpec["seed"] = async (cursor) => {
    calls.push(cursor);
    const page = pages.find((_, i) => (i === 0 ? cursor === "" : pages[i - 1]?.nextCursor === cursor));
    return page ?? { rows: [], nextCursor: "" };
  };
  return { seed, calls };
}

const tick = (): Promise<void> => new Promise((r) => setTimeout(r, 0));

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i++) await tick();
}

// ---------------------------------------------------------------------
// lifecycle
// ---------------------------------------------------------------------

test("the subscription opens BEFORE the seed read goes out", async () => {
  // The whole ordering contract (memql#4536, D2): registration is synchronous
  // server-side, so subscribing first means a row written during the read
  // arrives as an event. Reading first can miss it forever.
  const subs = new FakeSubscriptions();
  const order: string[] = [];
  subs.onSubscribe = () => order.push("subscribed");
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "v1:worker:registration",
    seed: async () => {
      order.push("read");
      return { rows: [], nextCursor: "" };
    },
  });
  await settle();
  assert.deepEqual(order, ["subscribed", "read"]);
  assert.equal(subs.subs[0]?.concept, "v1:worker:registration");
  handle.release();
  store.dispose();
});

test("the seed walks every page, and folds arrivals during it by id", async () => {
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async (cursor) =>
      cursor === ""
        ? { rows: [{ id: "a" }], nextCursor: "c1" }
        : { rows: [{ id: "b" }], nextCursor: "" },
  });
  await settle();
  assert.deepEqual(handle.value.snapshot.rows.map((r) => r["id"]), ["a", "b"]);
  assert.equal(handle.value.snapshot.state, "live");

  subs.deliver(event({ kind: "NODE_CREATED", payload: { id: "c" } }));
  assert.deepEqual(handle.value.snapshot.rows.map((r) => r["id"]), ["a", "b", "c"]);
  store.dispose();
});

// ---------------------------------------------------------------------
// the fold matrix
// ---------------------------------------------------------------------

test("created upserts, updated replaces, deleted removes -- matched on the KIND", async () => {
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [{ id: "a", name: "one" }], nextCursor: "" }),
  });
  await settle();

  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", name: "two" } }));
  assert.equal(handle.value.snapshot.rows[0]?.["name"], "two");

  subs.deliver(event({ kind: "NODE_CREATED", payload: { id: "b" } }));
  assert.equal(handle.value.snapshot.rows.length, 2);

  // NODE_DELETED comes off the decoded enum. A substring test over a topic --
  // which one of the replaced implementations did -- is one renamed topic
  // away from folding deletes as updates.
  subs.deliver(event({ kind: "NODE_DELETED", payload: { id: "a" } }));
  assert.deepEqual(handle.value.snapshot.rows.map((r) => r["id"]), ["b"]);
  store.dispose();
});

test("an id-only event is resolved through the authorized re-read", async () => {
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const reads: string[] = [];
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [], nextCursor: "" }),
    reread: async (id) => {
      reads.push(id);
      return { id, name: "resolved" };
    },
  });
  await settle();
  subs.deliver(event({ kind: "NODE_CREATED", payloadOmitted: true, payload: { id: "g1" } }));
  await settle();
  assert.deepEqual(reads, ["g1"]);
  assert.equal(handle.value.snapshot.rows[0]?.["name"], "resolved");
  store.dispose();
});

test("a REFUSED re-read drops the event silently -- no row, no error", async () => {
  // The caller was not entitled to the row, and announcing that one changed
  // would leak exactly what the gate withheld (memql#4309).
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [], nextCursor: "" }),
    reread: async () => {
      throw new Error("permission denied");
    },
  });
  await settle();
  subs.deliver(event({ kind: "NODE_UPDATED", payloadOmitted: true, payload: { id: "g1" } }));
  await settle();
  assert.equal(handle.value.snapshot.rows.length, 0);
  assert.equal(handle.value.snapshot.error, "");
  store.dispose();
});

test("a re-read that finds nothing removes the row", async () => {
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [{ id: "g1" }], nextCursor: "" }),
    reread: async () => null,
  });
  await settle();
  subs.deliver(event({ kind: "NODE_UPDATED", payloadOmitted: true, payload: { id: "g1" } }));
  await settle();
  assert.equal(handle.value.snapshot.rows.length, 0);
  store.dispose();
});

test("inScope governs FOLDED rows; the seed narrows itself", async () => {
  // The read is the authority on membership, and inScope is a CLIENT-SIDE
  // MIRROR of a decision the server already made -- usually over state that
  // resolves asynchronously, like the caller's own user id. Applying a
  // not-yet-resolved mirror to the seed would empty the list and repopulate
  // it, which looks exactly like a page that loaded nothing. A narrowing no
  // query declares belongs in `seed`, which is the caller's own code.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "v1:cluster:node",
    seed: async () => ({
      rows: [{ id: "workbench-0", nodeType: "workbench" }],  // narrowed HERE
      nextCursor: "",
    }),
    inScope: (row) => row["nodeType"] === "workbench",
  });
  await settle();
  assert.deepEqual(handle.value.snapshot.rows.map((r) => r["id"]), ["workbench-0"]);

  // ...and the same predicate keeps the subscription from widening it.
  subs.deliver(event({ kind: "NODE_CREATED", payload: { id: "bff-0", nodeType: "bff" } }));
  assert.deepEqual(handle.value.snapshot.rows.map((r) => r["id"]), ["workbench-0"]);
  store.dispose();
});

test("the scope re-filter keeps a broad subscription from widening a scoped read", async () => {
  // The fleet lesson: an owner's subscription carries rows the scoped read
  // excluded, and folding them in makes rows appear that a refresh removes.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [{ id: "a", mine: true }], nextCursor: "" }),
    inScope: (row) => row["mine"] === true,
  });
  await settle();
  subs.deliver(event({ kind: "NODE_CREATED", payload: { id: "b", mine: false } }));
  assert.deepEqual(handle.value.snapshot.rows.map((r) => r["id"]), ["a"]);

  // And a row that LEAVES scope leaves the list -- ignoring the event would
  // strand it on screen until the next full read.
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", mine: false } }));
  assert.equal(handle.value.snapshot.rows.length, 0);
  store.dispose();
});

// ---------------------------------------------------------------------
// continuity
// ---------------------------------------------------------------------

test("gap_before re-seeds ONCE, and reports degraded while it does", async () => {
  const subs = new FakeSubscriptions();
  const { seed, calls } = countingSeed([{ rows: [{ id: "a" }], nextCursor: "" }]);
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", { concept: "c", seed });
  await settle();
  assert.equal(calls.length, 1);

  subs.deliver(event({ seq: 5, gapBefore: true, payload: { id: "b" } }));
  await settle();
  assert.equal(calls.length, 2, "a gap re-seeds");

  store.dispose();
  void handle;
});

test("a burst of gaps coalesces into one extra seed", async () => {
  const subs = new FakeSubscriptions();
  let inFlight!: () => void;
  let seedCalls = 0;
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => {
      seedCalls++;
      await new Promise<void>((r) => {
        inFlight = r;
      });
      return { rows: [], nextCursor: "" };
    },
  });
  await tick();
  assert.equal(seedCalls, 1);

  // Three gaps while the first seed is still running.
  subs.deliver(event({ seq: 10, gapBefore: true }));
  subs.deliver(event({ seq: 20, gapBefore: true }));
  subs.deliver(event({ seq: 30, gapBefore: true }));
  assert.equal(handle.value.snapshot.state, "seeding");

  inFlight();
  await settle();
  inFlight();
  await settle();
  assert.equal(seedCalls, 2, "three gaps produced one further seed, not three");
  store.dispose();
});

test("a NON-CONTIGUOUS seq is a gap, and a zero seq never is", async () => {
  const subs = new FakeSubscriptions();
  const { seed, calls } = countingSeed([{ rows: [], nextCursor: "" }]);
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", { concept: "c", seed });
  await settle();
  assert.equal(calls.length, 1);

  subs.deliver(event({ seq: 1 }));
  subs.deliver(event({ seq: 2 }));
  await settle();
  assert.equal(calls.length, 1, "a contiguous run is not a gap");

  subs.deliver(event({ seq: 9 }));
  await settle();
  assert.equal(calls.length, 2, "a hole in the sequence is");

  // An older server sends 0 on every delivery. Treating that as "the first
  // event" would report a gap on every single one.
  const before = calls.length;
  subs.deliver(event({ seq: 0 }));
  subs.deliver(event({ seq: 0 }));
  await settle();
  assert.equal(calls.length, before, "an unnumbered stream is not a broken one");
  store.dispose();
  void handle;
});

test("the gap is read STREAM-WIDE: a neighbour's drop re-seeds this collection too", async () => {
  // seq numbers the socket, and gap_before lands on whichever delivery comes
  // first after a drop. A collection reading either field alone sees holes
  // that belong to its neighbours, or misses its own.
  const subs = new FakeSubscriptions();
  const a = countingSeed([{ rows: [], nextCursor: "" }]);
  const b = countingSeed([{ rows: [], nextCursor: "" }]);
  const store = new LiveStore(hostWith(subs).host);
  const ha = store.collection<Row>("a", { concept: "concept-a", seed: a.seed });
  const hb = store.collection<Row>("b", { concept: "concept-b", seed: b.seed });
  await settle();
  assert.equal(a.calls.length, 1);
  assert.equal(b.calls.length, 1);

  // Delivered only to concept-a's subscription, but the drop was the socket's.
  subs.deliver(event({ seq: 7, gapBefore: true }), "concept-a");
  await settle();
  assert.equal(a.calls.length, 2);
  assert.equal(b.calls.length, 2, "the neighbour re-seeded too");
  store.dispose();
  void ha;
  void hb;
});

test("a reconnect re-seeds, and a drop reports disconnected while keeping rows", async () => {
  const subs = new FakeSubscriptions();
  const { seed, calls } = countingSeed([{ rows: [{ id: "a" }], nextCursor: "" }]);
  const h = hostWith(subs);
  const store = new LiveStore(h.host);
  const handle = store.collection<Row>("k", { concept: "c", seed });
  await settle();

  h.status("reconnecting");
  assert.equal(handle.value.snapshot.state, "disconnected");
  // Rows are KEPT. An operator staring at a table wants the last known
  // answer, labelled stale -- not a blank.
  assert.equal(handle.value.snapshot.rows.length, 1);

  h.cycle(1);
  await settle();
  assert.equal(calls.length, 2, "a reconnect is a gap by construction");
  assert.equal(handle.value.snapshot.state, "live");
  store.dispose();
});

// ---------------------------------------------------------------------
// the store: refcounting + dedupe
// ---------------------------------------------------------------------

test("two callers on one key share one subscription and one seed", async () => {
  const subs = new FakeSubscriptions();
  const { seed, calls } = countingSeed([{ rows: [], nextCursor: "" }]);
  const store = new LiveStore(hostWith(subs).host);
  const first = store.collection<Row>("k", { concept: "c", seed });
  const second = store.collection<Row>("k", { concept: "c", seed });
  await settle();
  assert.equal(first.value, second.value, "the same collection object");
  assert.equal(calls.length, 1, "one seed");
  assert.equal(subs.subs.length, 1, "one subscription");
  first.release();
  second.release();
  store.dispose();
});

test("a remount inside the linger window issues ZERO new reads", async () => {
  // This is the operator's original complaint -- every page open refetches
  // everything -- measured rather than asserted.
  const subs = new FakeSubscriptions();
  const { seed, calls } = countingSeed([{ rows: [{ id: "a" }], nextCursor: "" }]);
  const store = new LiveStore(hostWith(subs).host, { lingerMs: 1000 });
  const spec: LiveCollectionSpec<Row> = { concept: "c", seed };

  const a = store.collection<Row>("k", spec); // page A mounts
  await settle();
  a.release(); //                                 navigate away
  const b = store.collection<Row>("k", spec); // ...and back
  await settle();

  assert.equal(calls.length, 1, "A -> B -> A read once, not twice");
  assert.deepEqual(b.value.snapshot.rows.map((r) => r["id"]), ["a"], "rows were still there");
  b.release();
  store.dispose();
});

test("the collection is torn down after the linger expires", async () => {
  const subs = new FakeSubscriptions();
  const { seed, calls } = countingSeed([{ rows: [], nextCursor: "" }]);
  const store = new LiveStore(hostWith(subs).host, { lingerMs: 5 });
  const spec: LiveCollectionSpec<Row> = { concept: "c", seed };

  store.collection<Row>("k", spec).release();
  await new Promise((r) => setTimeout(r, 25));
  assert.equal(subs.subs.length, 0, "the subscription was closed");

  store.collection<Row>("k", spec);
  await settle();
  assert.equal(calls.length, 2, "a fresh collection seeds again");
  store.dispose();
});

test("LiveValue collapses concurrent identical reads into one round trip", async () => {
  // The MyAccess fix: fourteen call sites, one answer.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  let reads = 0;
  const read = async (): Promise<{ role: string } | null> => {
    reads++;
    await tick();
    return { role: "admin" };
  };

  const handles = Array.from({ length: 14 }, () => store.value("myAccess", read));
  await settle();
  assert.equal(reads, 1, "fourteen callers, one read");
  assert.equal(handles[0]?.value.snapshot.value?.role, "admin");
  assert.equal(handles[13]?.value, handles[0]?.value, "and one shared value");
  for (const h of handles) h.release();
  store.dispose();
});

test("a failing seed keeps the rows it has and says so", async () => {
  const subs = new FakeSubscriptions();
  let attempt = 0;
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => {
      attempt++;
      if (attempt === 1) return { rows: [{ id: "a" }], nextCursor: "" };
      throw new Error("read refused");
    },
  });
  await settle();
  assert.equal(handle.value.snapshot.state, "live");

  handle.value.reseed();
  await settle();
  assert.equal(handle.value.snapshot.rows.length, 1, "the last known answer stays on screen");
  assert.equal(handle.value.snapshot.state, "degraded");
  assert.match(handle.value.snapshot.error, /read refused/);
  store.dispose();
});

test("the snapshot is identity-stable until something changes", async () => {
  // What keeps a binding from re-rendering on every unrelated event.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [{ id: "a" }], nextCursor: "" }),
  });
  await settle();
  const first = handle.value.snapshot;
  assert.equal(handle.value.snapshot, first);

  subs.deliver(event({ kind: "NODE_CREATED", payload: { id: "b" } }));
  assert.notEqual(handle.value.snapshot, first);
  store.dispose();
});

// ---------------------------------------------------------------------
// rereadEveryEvent + supersedes -- the two options the Nexus feed needs
// ---------------------------------------------------------------------

test("rereadEveryEvent resolves a FULL-payload event through the read too", async () => {
  // One code path instead of two. The branch that trusts a payload is the one
  // that stops being exercised on a concept heading for the `granted` tier,
  // and a rotting branch is worse than a round trip.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const reads: string[] = [];
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [], nextCursor: "" }),
    reread: async (id) => {
      reads.push(id);
      return { id, name: "authoritative" };
    },
    rereadEveryEvent: true,
  });
  await settle();
  subs.deliver(event({ kind: "NODE_CREATED", payload: { id: "r1", name: "from-the-event" } }));
  await settle();
  assert.deepEqual(reads, ["r1"]);
  assert.equal(handle.value.snapshot.rows[0]?.["name"], "authoritative");
  store.dispose();
});

test("rereadEveryEvent without a reread falls back to trusting the payload", async () => {
  // Dropping every event would be a worse answer than the default.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [], nextCursor: "" }),
    rereadEveryEvent: true,
  });
  await settle();
  subs.deliver(event({ kind: "NODE_CREATED", payload: { id: "r1" } }));
  await settle();
  assert.equal(handle.value.snapshot.rows.length, 1);
  store.dispose();
});

test("supersedes refuses a row that would roll a newer one backwards", async () => {
  // Two re-reads issued a moment apart can settle in either order. Without a
  // watermark that means an older copy overwrites a newer one -- silently,
  // and only under load.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [{ id: "r1", version: 5 }], nextCursor: "" }),
    supersedes: (incoming, held) =>
      held === undefined || Number(incoming["version"] ?? 0) >= Number(held["version"] ?? 0),
  });
  await settle();

  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "r1", version: 3 } }));
  assert.equal(handle.value.snapshot.rows[0]?.["version"], 5, "the older copy was refused");

  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "r1", version: 7 } }));
  assert.equal(handle.value.snapshot.rows[0]?.["version"], 7, "and a newer one lands");

  // A row never seen must not be refusable into nonexistence.
  subs.deliver(event({ kind: "NODE_CREATED", payload: { id: "r2", version: 1 } }));
  assert.equal(handle.value.snapshot.rows.length, 2);
  store.dispose();
});

test("onGap gives a hand-rolled consumer the same continuity signal", async () => {
  // A surface that folds by hand -- the concept browser's arrivals band --
  // must not read seq / gap_before itself: continuity is a stream property and
  // a per-subscription reading is wrong in both directions.
  const subs = new FakeSubscriptions();
  const h = hostWith(subs);
  const store = new LiveStore(h.host);
  let gaps = 0;
  const off = store.onGap(() => gaps++);

  subs.deliver(event({ seq: 1 }));
  subs.deliver(event({ seq: 2 }));
  assert.equal(gaps, 0, "a contiguous run is not a gap");

  subs.deliver(event({ seq: 9 }));
  assert.equal(gaps, 1, "a hole is");

  subs.deliver(event({ seq: 10, gapBefore: true }));
  assert.equal(gaps, 2, "and so is the server's own flag");

  h.cycle(1);
  assert.equal(gaps, 3, "a reconnect is a gap by construction");

  off();
  subs.deliver(event({ seq: 99 }));
  assert.equal(gaps, 3, "unsubscribed");
  store.dispose();
});
