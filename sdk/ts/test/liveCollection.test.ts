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

// A walk whose LATER pages wait for the test. What the stream delivers while
// the walk is between pages is what a seed has to keep: the pages it already
// read are older than it, and the page it is waiting on may be too.
function gatedWalk(first: Row[], ...later: Row[][]) {
  let release: () => void = () => {};
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  let waiting = false;
  const seed: LiveCollectionSpec["seed"] = async (cursor) => {
    if (cursor === "") return { rows: first, nextCursor: later.length > 0 ? "page-1" : "" };
    waiting = true;
    await gate;
    const at = Number(cursor.slice("page-".length));
    return { rows: later[at - 1] ?? [], nextCursor: at < later.length ? `page-${at + 1}` : "" };
  };
  return { seed, release: () => release(), waiting: () => waiting };
}

const byId = (rows: readonly Row[]): Record<string, Row> =>
  Object.fromEntries(rows.map((row) => [String(row["id"]), row]));

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
// what arrives while a seed reads (memql#6033)
// ---------------------------------------------------------------------

test("an update, an insert and a delete that arrive between two pages all survive the seed", async () => {
  // A campaign's `sent` lands between page 1 and page 2. Page 1 was read
  // before it, so the walk's copy is the older one -- and installing the walk
  // over what arrived is how a sent campaign went back to Sending until the
  // next reseed.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const walk = gatedWalk(
    [{ id: "a", status: "sending" }, { id: "d", status: "draft" }],
    [{ id: "b", status: "draft" }],
  );
  const handle = store.collection<Row>("k", { concept: "c", seed: walk.seed });
  await settle();
  assert.equal(walk.waiting(), true, "the walk is waiting on its second page");

  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", status: "sent" } }));
  subs.deliver(event({ kind: "NODE_CREATED", payload: { id: "c", status: "draft" } }));
  subs.deliver(event({ kind: "NODE_DELETED", payload: { id: "d" } }));

  walk.release();
  await settle();
  const rows = byId(handle.value.snapshot.rows);
  assert.deepEqual(Object.keys(rows).sort(), ["a", "b", "c"], "the insert stays and the delete stays deleted");
  assert.equal(rows["a"]?.["status"], "sent", "the update is not rolled back by the page read before it");
  assert.equal(handle.value.snapshot.state, "live");
  store.dispose();
});

test("an event older than the row the walk read does not roll it back; a newer one wins", async () => {
  // The two halves of one rule. `b` changes to 5 before the page that reads it
  // at 7 is asked for: the walk is newer, and replaying the event over it
  // would roll it back. `a` changes to 2 after the page that read it at 1: the
  // event is newer, and it wins. The concept's watermark is the judge -- the
  // same `supersedes` every fold already asks.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const walk = gatedWalk([{ id: "a", version: 1 }], [{ id: "b", version: 7 }]);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: walk.seed,
    supersedes: (incoming, held) =>
      held === undefined || Number(incoming["version"] ?? 0) >= Number(held["version"] ?? 0),
  });
  await settle();

  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "b", version: 5 } }));
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", version: 2 } }));
  walk.release();
  await settle();

  const rows = byId(handle.value.snapshot.rows);
  assert.equal(rows["b"]?.["version"], 7, "the older event did not regress what the walk read");
  assert.equal(rows["a"]?.["version"], 2, "the newer event is not overwritten by the older page");
  store.dispose();
});

test("an id-only event whose re-read lands while a seed reads survives the seed", async () => {
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const walk = gatedWalk([{ id: "a" }], [{ id: "b" }]);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: walk.seed,
    reread: async (id) => ({ id, name: "resolved" }),
  });
  await settle();

  subs.deliver(event({ kind: "NODE_CREATED", payloadOmitted: true, payload: { id: "g1" } }));
  await settle(); // the re-read lands; the walk is still waiting
  walk.release();
  await settle();

  assert.equal(byId(handle.value.snapshot.rows)["g1"]?.["name"], "resolved");
  store.dispose();
});

test("a reseed keeps folding arrivals onto the rows on screen, and keeps them when it lands", async () => {
  // Keeping arrivals for the seed's answer must not freeze the screen while
  // it reads: the operator watching a degraded list still sees what changes.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  let release: () => void = () => {};
  let reads = 0;
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => {
      reads++;
      if (reads > 1) await new Promise<void>((resolve) => { release = resolve; });
      return { rows: [{ id: "a", status: "scheduled" }], nextCursor: "" };
    },
  });
  await settle();

  handle.value.reseed();
  await settle();
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", status: "sending" } }));
  assert.equal(byId(handle.value.snapshot.rows)["a"]?.["status"], "sending", "shown while the reseed reads");

  release();
  await settle();
  assert.equal(reads, 2);
  assert.equal(byId(handle.value.snapshot.rows)["a"]?.["status"], "sending", "and kept when it lands");
  store.dispose();
});

test("a seed keeps one arrival per row, however many events arrive while it reads", async () => {
  // What a seed keeps is bounded by the rows that changed, not by the events:
  // a busy row announcing every write while a slow walk reads must not grow
  // the list it replays without limit. The last word per row is all a replay
  // needs -- the newest version, or the removal.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const walk = gatedWalk([{ id: "a", n: 0 }], [{ id: "b", n: 0 }]);
  const handle = store.collection<Row>("k", { concept: "c", seed: walk.seed });
  await settle();

  for (let n = 1; n <= 500; n++) {
    subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", n } }));
    subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "b", n } }));
  }
  const kept = (handle.value as unknown as { arrivals: { size: number } | null }).arrivals;
  assert.equal(kept?.size, 2, "one kept arrival per row, not a thousand");

  walk.release();
  await settle();
  const rows = byId(handle.value.snapshot.rows);
  assert.equal(rows["a"]?.["n"], 500);
  assert.equal(rows["b"]?.["n"], 500, "the page read before them does not roll the last one back");
  store.dispose();
});

// ---------------------------------------------------------------------
// the version each engine row carries (memql#6033)
// ---------------------------------------------------------------------
//
// An engine row's version time is its `createdAt`: every write lands a new
// version stamped with it, and every read, re-read and graph event carries it
// at the top level. The bus delivers events in no particular order and a read
// can answer after an event it is newer than, so with no `supersedes` of its
// own a collection orders rows by it.

// One microsecond apart -- the precision a row keeps its version time at.
const T1 = "2026-10-12T14:47:19.325157Z";
const T2 = "2026-10-12T14:47:19.325158Z";

test("a seed keeps the row the read returned when an OLDER version lands while it reads", async () => {
  // The probe: the read returns `sent`; the `sending` written before it lands
  // while the walk is still reading, and used to replace the read's row --
  // until the next event, and for good when the change after it sends none
  // this list folds. No OS feed passes `supersedes`; none needs to.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const walk = gatedWalk([{ id: "a", status: "sent", createdAt: T2 }], [{ id: "b", status: "draft", createdAt: T1 }]);
  const handle = store.collection<Row>("k", { concept: "c", seed: walk.seed });
  await settle();

  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", status: "sending", createdAt: T1 } }));
  walk.release();
  await settle();
  assert.equal(byId(handle.value.snapshot.rows)["a"]?.["status"], "sent");
  store.dispose();
});

test("a NEWER version that lands while a seed reads wins over the read", async () => {
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const walk = gatedWalk([{ id: "a", status: "sending", createdAt: T1 }], [{ id: "b", status: "draft", createdAt: T1 }]);
  const handle = store.collection<Row>("k", { concept: "c", seed: walk.seed });
  await settle();

  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", status: "sent", createdAt: T2 } }));
  walk.release();
  await settle();
  assert.equal(byId(handle.value.snapshot.rows)["a"]?.["status"], "sent");
  store.dispose();
});

test("two versions that land out of order while a seed reads leave the newer one", async () => {
  // What a seed keeps for a row is judged as a fold judges: the late `sending`
  // does not displace the `sent` kept before it, and the replay installs `sent`
  // over the older row the read returned.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const walk = gatedWalk([{ id: "a", status: "scheduled", createdAt: "2026-10-12T14:47:18Z" }], [{ id: "b", status: "draft", createdAt: T1 }]);
  const handle = store.collection<Row>("k", { concept: "c", seed: walk.seed });
  await settle();

  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", status: "sent", createdAt: T2 } }));
  subs.deliver(event({ kind: "NODE_CREATED", payload: { id: "a", status: "sending", createdAt: T1 } }));
  walk.release();
  await settle();
  assert.equal(byId(handle.value.snapshot.rows)["a"]?.["status"], "sent");
  store.dispose();
});

test("an older version delivered after a newer one is refused, outside a seed too", async () => {
  // The bus delivers each event in its own goroutine: `sending` and `sent`
  // can reach the client in either order.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [{ id: "a", status: "scheduled", createdAt: "2026-10-12T14:47:18Z" }], nextCursor: "" }),
  });
  await settle();

  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", status: "sent", createdAt: T2 } }));
  subs.deliver(event({ kind: "NODE_CREATED", payload: { id: "a", status: "sending", createdAt: T1 } }));
  assert.equal(byId(handle.value.snapshot.rows)["a"]?.["status"], "sent");

  // The same version announced twice -- .created and .updated of one write --
  // is not older than itself.
  subs.deliver(event({ kind: "NODE_CREATED", payload: { id: "a", status: "sent", note: "again", createdAt: T2 } }));
  assert.equal(byId(handle.value.snapshot.rows)["a"]?.["note"], "again");
  store.dispose();
});

test("versions compare at every digit the wire carries, not as strings", async () => {
  // Go trims trailing zeros, so the LATER of these is the longer string that
  // sorts first: ".3251Z" > ".325101Z" as text, and earlier as an instant.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [{ id: "a", v: "held", createdAt: "2026-10-12T14:47:19.3251Z" }], nextCursor: "" }),
  });
  await settle();

  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", v: "older", createdAt: "2026-10-12T14:47:19.325099Z" } }));
  assert.equal(byId(handle.value.snapshot.rows)["a"]?.["v"], "held");
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", v: "newer", createdAt: "2026-10-12T14:47:19.325101Z" } }));
  assert.equal(byId(handle.value.snapshot.rows)["a"]?.["v"], "newer");
  store.dispose();
});

test("a row whose version is unknown is taken as it arrives", async () => {
  // Nothing to compare is not a reason to refuse: an arrival with no version
  // time, or one onto a row read without one, wins as any later arrival would.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [{ id: "a", v: "read", createdAt: T2 }, { id: "b", v: "read" }], nextCursor: "" }),
  });
  await settle();

  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", v: "no version" } }));
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "b", v: "versioned", createdAt: T1 } }));
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", v: "not a time", createdAt: "yesterday" } }));
  const rows = byId(handle.value.snapshot.rows);
  assert.equal(rows["a"]?.["v"], "not a time");
  assert.equal(rows["b"]?.["v"], "versioned");
  store.dispose();
});

test("a delete removes the row whatever version the read returned", async () => {
  // A removal is not a version to weigh: it lands, during a read and after.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const walk = gatedWalk(
    [{ id: "a", status: "sent", createdAt: T2 }, { id: "b", status: "draft", createdAt: T2 }],
    [{ id: "c", status: "draft", createdAt: T1 }],
  );
  const handle = store.collection<Row>("k", { concept: "c", seed: walk.seed });
  await settle();

  subs.deliver(event({ kind: "NODE_DELETED", payload: { id: "a", createdAt: T1 } }));
  walk.release();
  await settle();
  assert.deepEqual(Object.keys(byId(handle.value.snapshot.rows)).sort(), ["b", "c"], "deleted during the read, and stays deleted");

  subs.deliver(event({ kind: "NODE_DELETED", payload: { id: "b" } }));
  assert.deepEqual(handle.value.snapshot.rows.map((row) => row["id"]), ["c"], "and after it");
  store.dispose();
});

test("an older copy out of the read's scope does not take a newer row out of the list", async () => {
  // A copy the collection refuses as older says nothing about the row --
  // neither what it holds nor whether it belongs. Judging its scope first
  // would let a late `archived` from before a restore remove the restored row.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [{ id: "a", status: "sent", createdAt: T2 }], nextCursor: "" }),
    inScope: (row) => row["status"] !== "archived",
  });
  await settle();

  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", status: "archived", createdAt: T1 } }));
  assert.equal(byId(handle.value.snapshot.rows)["a"]?.["status"], "sent", "the older archived copy is refused");
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", status: "archived", createdAt: "2026-10-12T14:47:20Z" } }));
  assert.deepEqual(handle.value.snapshot.rows, [], "a newer one leaves the scope, and the list");
  store.dispose();
});

test("a feed's own supersedes still decides, over the version", async () => {
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [{ id: "a", status: "sent", createdAt: T2 }], nextCursor: "" }),
    // A feed whose rows carry their own watermark -- or that wants every
    // arrival, whatever its time -- says so.
    supersedes: () => true,
  });
  await settle();

  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", status: "sending", createdAt: T1 } }));
  assert.equal(byId(handle.value.snapshot.rows)["a"]?.["status"], "sending");
  store.dispose();
});

// ---------------------------------------------------------------------
// what took a row out keeps it out (memql#6033)
// ---------------------------------------------------------------------
//
// Once a row is out of the list nothing is held to compare a late copy with,
// so the version that took it out is kept instead: an OLDER copy arriving
// after it does not bring the row back.

const T3 = "2026-10-12T14:47:19.325159Z";
const T4 = "2026-10-12T14:47:19.32516Z";

test("a row a newer copy took out of the read's scope is not brought back by a late older one", async () => {
  // `archived` (T3) lands before the `sent` written ahead of it (T2): the
  // archived campaign must not come back as sent.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [{ id: "a", status: "sending", createdAt: T1 }], nextCursor: "" }),
    inScope: (row) => row["status"] !== "archived",
  });
  await settle();

  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", status: "archived", createdAt: T3 } }));
  assert.deepEqual(handle.value.snapshot.rows, [], "out of scope, out of the list");
  subs.deliver(event({ kind: "NODE_CREATED", payload: { id: "a", status: "sent", createdAt: T2 } }));
  assert.deepEqual(handle.value.snapshot.rows, [], "the older copy does not bring it back");
  store.dispose();
});

test("a deleted row is not brought back by a late older copy; a removal with no time keeps nothing", async () => {
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [{ id: "a", v: "read", createdAt: T1 }, { id: "b", v: "read", createdAt: T1 }], nextCursor: "" }),
  });
  await settle();

  subs.deliver(event({ kind: "NODE_DELETED", payload: { id: "a", createdAt: T3 } }));
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", v: "late", createdAt: T2 } }));
  assert.equal(byId(handle.value.snapshot.rows)["a"], undefined, "the copy older than the removal stays out");
  // A stale removal arriving after it does not lower the bar: the newer time is kept.
  subs.deliver(event({ kind: "NODE_DELETED", payload: { id: "a", createdAt: T1 } }));
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", v: "late", createdAt: T2 } }));
  assert.equal(byId(handle.value.snapshot.rows)["a"], undefined);

  // A removal that says nothing of when leaves nothing to compare with: what
  // arrives next is taken, as it always was.
  subs.deliver(event({ kind: "NODE_DELETED", payload: { id: "b" } }));
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "b", v: "back", createdAt: T1 } }));
  assert.equal(byId(handle.value.snapshot.rows)["b"]?.["v"], "back");
  store.dispose();
});

test("a copy newer than the one that took a row out brings it back", async () => {
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [{ id: "a", status: "sending", createdAt: T1 }], nextCursor: "" }),
    inScope: (row) => row["status"] !== "archived",
  });
  await settle();

  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", status: "archived", createdAt: T2 } }));
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", status: "sent", createdAt: T3 } }));
  assert.equal(byId(handle.value.snapshot.rows)["a"]?.["status"], "sent", "restored, and back in the list");
  const gone = (handle.value as unknown as { gone: Map<string, string> }).gone;
  assert.equal(gone.has("a"), false, "and nothing is kept for a row the list holds");
  // Back in the list, the row it holds is the judge again.
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", status: "sending", createdAt: T1 } }));
  assert.equal(byId(handle.value.snapshot.rows)["a"]?.["status"], "sent");
  store.dispose();
});

test("a seed's replay does not bring back a row removed while it read", async () => {
  // The read returned `a` (T1); while it read, `a` was deleted (T3) and an
  // older copy of it (T2) arrived late. What the seed keeps for `a` is the
  // removal, not the copy that came after it.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const walk = gatedWalk([{ id: "a", v: "read", createdAt: T1 }], [{ id: "b", v: "read", createdAt: T1 }]);
  const handle = store.collection<Row>("k", { concept: "c", seed: walk.seed });
  await settle();

  subs.deliver(event({ kind: "NODE_DELETED", payload: { id: "a", createdAt: T3 } }));
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", v: "late", createdAt: T2 } }));
  walk.release();
  await settle();
  assert.deepEqual(Object.keys(byId(handle.value.snapshot.rows)), ["b"]);

  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", v: "newer", createdAt: T4 } }));
  assert.equal(byId(handle.value.snapshot.rows)["a"]?.["v"], "newer", "a newer copy still lands");
  store.dispose();
});

test("a feed's own supersedes keeps no tombstones", async () => {
  // It owns its rows' history: what it accepts is accepted, removal or not.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [{ id: "a", status: "sending", createdAt: T1 }], nextCursor: "" }),
    inScope: (row) => row["status"] !== "archived",
    supersedes: () => true,
  });
  await settle();

  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", status: "archived", createdAt: T3 } }));
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", status: "sent", createdAt: T2 } }));
  assert.equal(byId(handle.value.snapshot.rows)["a"]?.["status"], "sent");
  subs.deliver(event({ kind: "NODE_DELETED", payload: { id: "a", createdAt: T4 } }));
  assert.equal((handle.value as unknown as { gone: Map<string, string> }).gone.size, 0, "nothing is kept for the removal");
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", status: "sending", createdAt: T1 } }));
  assert.equal(byId(handle.value.snapshot.rows)["a"]?.["status"], "sending");
  store.dispose();
});

test("what took a row out keeps it out across a reseed; a copy at that version or newer is placed", async () => {
  // A reseed's read does not return a removed row, so it says nothing about
  // how old a late copy of it is. The version that took the row out still does.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  let reads = 0;
  let release: () => void = () => {};
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => {
      reads++;
      if (reads === 2) await new Promise<void>((resolve) => { release = resolve; });
      const rows: Row[] = [{ id: "c", v: "read", createdAt: T1 }];
      if (reads === 1) rows.push({ id: "a", v: "read", createdAt: T1 }, { id: "b", v: "read", createdAt: T1 });
      return { rows, nextCursor: "" };
    },
  });
  await settle();

  subs.deliver(event({ kind: "NODE_DELETED", payload: { id: "a", createdAt: T3 } }));
  subs.deliver(event({ kind: "NODE_DELETED", payload: { id: "b", createdAt: T3 } }));
  handle.value.reseed();
  await settle();
  assert.equal(reads, 2, "the reseed is reading");

  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", v: "late, during", createdAt: T2 } }));
  assert.equal(byId(handle.value.snapshot.rows)["a"], undefined, "an older copy during the reseed stays out");
  release();
  await settle();
  assert.deepEqual(Object.keys(byId(handle.value.snapshot.rows)), ["c"], "and the reseed's answer does not bring it back");

  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", v: "late, after", createdAt: T2 } }));
  assert.equal(byId(handle.value.snapshot.rows)["a"], undefined, "nor after it");

  // A refusal needs a STRICTLY older version: the same one, or a newer one, is placed.
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", v: "at the removal's version", createdAt: T3 } }));
  subs.deliver(event({ kind: "NODE_CREATED", payload: { id: "b", v: "re-created", createdAt: T4 } }));
  const rows = byId(handle.value.snapshot.rows);
  assert.equal(rows["a"]?.["v"], "at the removal's version");
  assert.equal(rows["b"]?.["v"], "re-created");
  store.dispose();
});

test("the tombstones are bounded: the oldest is forgotten first", async () => {
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", { concept: "c", seed: async () => ({ rows: [], nextCursor: "" }) });
  await settle();

  for (let n = 0; n < 1_100; n++) subs.deliver(event({ kind: "NODE_DELETED", payload: { id: `r${n}`, createdAt: T3 } }));
  const gone = (handle.value as unknown as { gone: Map<string, string> }).gone;
  assert.equal(gone.size, 1_024);
  assert.equal(gone.has("r0"), false, "the first removed is the first forgotten");
  assert.equal(gone.has("r1099"), true);
  store.dispose();
});

test("a row removed again is remembered as the newest word on it, and forgotten last", async () => {
  // The cap forgets the oldest word first. A row removed AGAIN, at a newer
  // version, has had a new word said about it: it moves to the back of the line
  // rather than keeping the place of its first removal.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", { concept: "c", seed: async () => ({ rows: [], nextCursor: "" }) });
  await settle();
  const gone = (handle.value as unknown as { gone: Map<string, string> }).gone;

  for (let n = 0; n < 1_024; n++) subs.deliver(event({ kind: "NODE_DELETED", payload: { id: `r${n}`, createdAt: T3 } }));
  assert.equal(gone.size, 1_024);
  subs.deliver(event({ kind: "NODE_DELETED", payload: { id: "r0", createdAt: T4 } }));
  assert.equal(gone.get("r0"), T4, "the newer removal is the one kept");
  // One more row removed forces one to be forgotten: the oldest word, which is r1's now.
  subs.deliver(event({ kind: "NODE_DELETED", payload: { id: "extra", createdAt: T3 } }));
  assert.equal(gone.size, 1_024);
  assert.equal(gone.has("r0"), true, "r0 was said about last, so it stays");
  assert.equal(gone.has("r1"), false, "r1 had the oldest word, so it goes");

  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "r0", v: "late", createdAt: T3 } }));
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "r1", v: "late", createdAt: T2 } }));
  const rows = byId(handle.value.snapshot.rows);
  assert.equal(rows["r0"], undefined, "a copy older than r0's removal is still refused");
  assert.equal(rows["r1"]?.["v"], "late", "r1's is not: nothing is held to compare it with");
  store.dispose();
});

test("a removal whose time cannot be read records nothing", async () => {
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [{ id: "a", v: "read", createdAt: T1 }, { id: "b", v: "read", createdAt: T1 }], nextCursor: "" }),
  });
  await settle();
  const gone = (handle.value as unknown as { gone: Map<string, string> }).gone;

  for (const [id, createdAt] of [["a", "yesterday"], ["b", 42]] as const) {
    subs.deliver(event({ kind: "NODE_DELETED", payload: { id, createdAt } as Row }));
  }
  assert.equal(gone.size, 0, "a time nothing can order by is no time to remember");
  assert.deepEqual(handle.value.snapshot.rows, [], "the removal itself still lands");
  // So the next copy is taken as it arrives, as with any removal that said nothing of when.
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", v: "back", createdAt: T1 } }));
  assert.equal(byId(handle.value.snapshot.rows)["a"]?.["v"], "back");
  store.dispose();
});

test("a removal with no time that arrives while a tombstone exists leaves the tombstone", async () => {
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [{ id: "a", v: "read", createdAt: T1 }], nextCursor: "" }),
  });
  await settle();
  const gone = (handle.value as unknown as { gone: Map<string, string> }).gone;

  subs.deliver(event({ kind: "NODE_DELETED", payload: { id: "a", createdAt: T3 } }));
  assert.equal(gone.get("a"), T3);
  // The row is already out; a second removal that says nothing of when says
  // nothing about what the first one did.
  subs.deliver(event({ kind: "NODE_DELETED", payload: { id: "a" } }));
  assert.equal(gone.get("a"), T3, "the version that took it out is still kept");
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", v: "late", createdAt: T2 } }));
  assert.equal(byId(handle.value.snapshot.rows)["a"], undefined, "so a late older copy is still refused");
  store.dispose();
});

test("a copy with no version time is placed against a tombstone, which it then clears", async () => {
  // Nothing says it is older than the removal, so it is taken as it arrives --
  // the rule everywhere else for a copy with no version to compare.
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [{ id: "a", v: "read", createdAt: T1 }], nextCursor: "" }),
  });
  await settle();
  const gone = (handle.value as unknown as { gone: Map<string, string> }).gone;

  subs.deliver(event({ kind: "NODE_DELETED", payload: { id: "a", createdAt: T3 } }));
  assert.equal(gone.get("a"), T3);
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "a", v: "no time" } }));
  assert.equal(byId(handle.value.snapshot.rows)["a"]?.["v"], "no time");
  assert.equal(gone.has("a"), false, "and the row it brought back needs no tombstone");
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

test("event re-reads share a connection-wide bound and coalesce queued row updates", async () => {
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  let active = 0;
  let peak = 0;
  const reads: string[] = [];
  const handles = ["a", "b"].map((concept) => store.collection<Row>(concept, {
    concept,
    seed: async () => ({ rows: [], nextCursor: "" }),
    rereadEveryEvent: true,
    reread: async (id) => {
      reads.push(`${concept}:${id}`);
      peak = Math.max(peak, ++active);
      await gate;
      active--;
      return { id };
    },
  }));
  await settle();
  try {
    // Two live panes, well over the bridge's 64 waiting-query capacity.
    // Deliver repeats before a queued read starts: one read sees them all.
    for (let repeat = 0; repeat < 3; repeat++) {
      for (let id = 0; id < 100; id++) {
        for (const concept of ["a", "b"]) {
          subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: String(id) } }), concept);
        }
      }
    }
    await settle();
    assert.equal(active, 2, "background reads leave capacity for foreground page loads");
    release();
    await settle();
    assert.equal(peak, 2, "the limit is shared, not multiplied by the number of collections");
    assert.equal(reads.length, 200, "queued updates to the same row collapse into one read");
    assert.deepEqual(handles.map((h) => h.value.snapshot.rows.length), [100, 100]);
  } finally {
    release();
    store.dispose();
  }
});

test("updates during an in-flight re-read request one fresh answer after it settles", async () => {
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const answers: Array<(row: Row) => void> = [];
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [], nextCursor: "" }),
    rereadEveryEvent: true,
    reread: () => new Promise<Row>((resolve) => { answers.push(resolve); }),
  });
  await settle();
  subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "r1" } }));
  await settle();
  for (let i = 0; i < 100; i++) {
    subs.deliver(event({ kind: "NODE_UPDATED", payload: { id: "r1" } }));
  }
  assert.equal(answers.length, 1, "only one read of the row may be in flight");
  answers[0]!({ id: "r1", name: "earlier" });
  await settle();
  assert.equal(answers.length, 2, "do not lose updates that happened after the first read began");
  answers[1]!({ id: "r1", name: "latest" });
  await settle();
  assert.equal(answers.length, 2);
  assert.equal(handle.value.snapshot.rows[0]?.["name"], "latest");
  store.dispose();
});

test("deletes discard queued re-reads and prevent in-flight answers from restoring rows", async () => {
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const answers: Array<{ id: string; resolve: (row: Row) => void }> = [];
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [{ id: "a" }, { id: "b" }, { id: "c" }], nextCursor: "" }),
    reread: (id) => new Promise<Row>((resolve) => { answers.push({ id, resolve }); }),
  });
  await settle();
  for (const id of ["a", "b", "c"]) {
    subs.deliver(event({ kind: "NODE_UPDATED", payloadOmitted: true, payload: { id } }));
  }
  await settle();
  assert.deepEqual(answers.map((a) => a.id), ["a", "b"]);
  for (const id of ["a", "c"]) {
    subs.deliver(event({ kind: "NODE_DELETED", payload: { id } }));
  }
  // These unversioned results have no timestamp for the tombstone to weigh.
  for (const answer of answers) answer.resolve({ id: answer.id });
  await settle();
  assert.deepEqual(handle.value.snapshot.rows.map((r) => r["id"]), ["b"]);
  assert.equal(answers.length, 2, "the deleted queued row was never queried");
  subs.deliver(event({ kind: "NODE_CREATED", payloadOmitted: true, payload: { id: "a" } }));
  await settle();
  answers[2]!.resolve({ id: "a", name: "recreated" });
  await settle();
  assert.equal(byId(handle.value.snapshot.rows)["a"]?.["name"], "recreated");
  store.dispose();
});

for (const transition of ["disconnect", "close"] as const) {
  test(`${transition} discards queued re-reads and ignores late answers`, async () => {
    const subs = new FakeSubscriptions();
    const connection = hostWith(subs);
    const store = new LiveStore(connection.host);
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const signals: AbortSignal[] = [];
    const handle = store.collection<Row>("k", {
      concept: "c",
      seed: async () => ({ rows: [{ id: "seed" }], nextCursor: "" }),
      reread: async (id, signal) => {
        signals.push(signal);
        // A consumer may ignore cancellation; its result still must not land.
        await gate;
        return { id };
      },
    });
    await settle();
    for (let id = 0; id < 100; id++) {
      subs.deliver(event({ payloadOmitted: true, payload: { id: String(id) } }));
    }
    await settle();
    if (transition === "disconnect") connection.status("reconnecting");
    else handle.value.close();
    assert.equal(signals.length, 2);
    assert.ok(signals.every((signal) => signal.aborted));
    release();
    await settle();
    assert.equal(signals.length, 2, "queued reads cannot leak into a later connection cycle");
    assert.deepEqual(handle.value.snapshot.rows, [{ id: "seed" }]);
    if (transition === "disconnect") {
      connection.cycle(2);
      await settle();
      subs.deliver(event({ payloadOmitted: true, payload: { id: "new" } }));
      await settle();
      assert.ok(byId(handle.value.snapshot.rows)["new"], "the new cycle can read normally");
    }
    store.dispose();
  });
}

test("a refused background read releases capacity for the next authorized row", async () => {
  const subs = new FakeSubscriptions();
  const store = new LiveStore(hostWith(subs).host);
  const handle = store.collection<Row>("k", {
    concept: "c",
    seed: async () => ({ rows: [], nextCursor: "" }),
    reread: async (id) => {
      if (id !== "allowed") throw new Error("permission denied");
      return { id };
    },
  });
  for (const id of ["denied1", "denied2", "allowed"]) {
    subs.deliver(event({ payloadOmitted: true, payload: { id } }));
  }
  await settle();
  assert.deepEqual(handle.value.snapshot.rows, [{ id: "allowed" }]);
  assert.equal(handle.value.snapshot.error, "");
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


test("a capped paged seed reports incomplete membership and a complete reseed clears it", async () => {
  const store = new LiveStore(hostWith(new FakeSubscriptions()).host);
  let complete = false;
  const handle = store.collection<Row>("cap", {
    concept: "c", maxPages: 2,
    seed: async (cursor) => ({ rows: [{ id: cursor || "first" }], nextCursor: complete ? "" : cursor + "next" }),
  });
  await settle();
  assert.equal(handle.value.snapshot.rows.length, 2);
  assert.equal(handle.value.snapshot.truncated, true);
  complete = true;
  handle.value.reseed();
  await settle();
  assert.equal(handle.value.snapshot.truncated, false);
  store.dispose();
});
