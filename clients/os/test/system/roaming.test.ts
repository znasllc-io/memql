import { beforeEach, describe, expect, it, vi } from "vitest";

import {
  canonicalDocument,
  GraphDesktopStore,
  type DesktopGateway,
  type StoredDesktop,
} from "../../src/system/graphStore";
import {
  LocalDesktopStore,
  type DesktopDocument,
  type DesktopStoreEvent,
} from "../../src/system/store";

// GraphDesktopStore against a fake gateway (epic memql#4746).
//
// The gateway is the seam ON PURPOSE: every decision worth testing here --
// what is written and when, what is adopted, what is refused -- is the
// store's, and a test that had to stand up a connection to reach them would
// be testing the SDK. SdkDesktopGateway holds nothing but three calls.

function doc(overrides: Partial<DesktopDocument> = {}): DesktopDocument {
  return {
    version: 1,
    desks: [{ id: "desk-1", createdBy: "user" }],
    activeDeskId: "desk-1",
    surfaces: { "desk-1": { items: {}, positions: {} } },
    dock: { pinned: ["settings"] },
    themePack: "graphite",
    ...overrides,
  };
}

class FakeGateway implements DesktopGateway {
  stored: StoredDesktop | null = null;
  readonly writes: Array<{ revision: number; document: DesktopDocument }> = [];
  reads = 0;
  failRead = false;
  failWrite = false;
  private listener: (() => void) | null = null;

  async read(): Promise<StoredDesktop | null> {
    this.reads += 1;
    if (this.failRead) throw new Error("unreachable");
    return this.stored;
  }

  async write(input: { revision: number; document: DesktopDocument }): Promise<void> {
    if (this.failWrite) throw new Error("refused");
    this.writes.push(input);
    this.stored = { revision: input.revision, document: input.document };
  }

  watch(onChange: () => void): () => void {
    this.listener = onChange;
    return () => {
      this.listener = null;
    };
  }

  /** Another machine saved. */
  arrive(revision: number, document: unknown): void {
    this.stored = { revision, document };
    this.listener?.();
  }
}

/** Let the store's reads, writes and zero-delay debounce settle. */
async function settle(): Promise<void> {
  for (let i = 0; i < 8; i += 1) await new Promise((r) => setTimeout(r, 0));
}

function build(gateway: FakeGateway) {
  const events: DesktopStoreEvent[] = [];
  const local = new LocalDesktopStore();
  const store = new GraphDesktopStore(local, gateway, 0);
  const stop = store.subscribe((e) => events.push(e));
  return { store, local, events, stop };
}

beforeEach(() => {
  localStorage.clear();
});

describe("GraphDesktopStore: nothing is written until the read resolves", () => {
  it("a seed saved before the row resolves never reaches the cluster", async () => {
    // THE FAILURE THIS PREVENTS: a browser signing in for the first time has
    // no local document, so the shell seeds one and saves it immediately. If
    // that save were sent, it would overwrite the desktop this person built on
    // their other machine with an empty one, before they saw anything.
    const gateway = new FakeGateway();
    gateway.stored = { revision: 4, document: doc({ themePack: "midnight" }) };
    const { store, events } = build(gateway);

    store.save(doc({ themePack: "graphite" })); // the seed
    await settle();

    expect(gateway.writes).toEqual([]);
    expect(events).toEqual([
      { kind: "document", document: expect.objectContaining({ themePack: "midnight" }), origin: "hydrate" },
    ]);
  });

  it("with no row, the buffered document IS the first-sign-in upload", async () => {
    const gateway = new FakeGateway(); // no stored row
    const { store } = build(gateway);

    store.save(doc({ themePack: "sunrise" }));
    await settle();

    expect(gateway.writes).toHaveLength(1);
    expect(gateway.writes[0]?.revision).toBe(1);
    expect(gateway.writes[0]?.document.themePack).toBe("sunrise");
  });

  it("uploads once, not once per save", async () => {
    const gateway = new FakeGateway();
    const { store } = build(gateway);

    store.save(doc({ themePack: "a" }));
    store.save(doc({ themePack: "b" }));
    store.save(doc({ themePack: "c" }));
    await settle();

    // The debounce coalesces: one write, carrying the newest.
    expect(gateway.writes).toHaveLength(1);
    expect(gateway.writes[0]?.document.themePack).toBe("c");
  });

  it("an unreachable cluster leaves local working and writes nothing", async () => {
    const gateway = new FakeGateway();
    gateway.failRead = true;
    const { store, local, events } = build(gateway);

    store.save(doc({ themePack: "offline" }));
    await settle();

    expect(gateway.writes).toEqual([]);
    expect(events).toEqual([]);
    expect(local.load()?.themePack).toBe("offline");
  });
});

describe("GraphDesktopStore: last writer wins on a stamped revision", () => {
  it("stamps held + 1", async () => {
    const gateway = new FakeGateway();
    gateway.stored = { revision: 9, document: doc() };
    const { store } = build(gateway);
    await settle();

    store.save(doc({ themePack: "next" }));
    await settle();

    expect(gateway.writes[0]?.revision).toBe(10);
  });

  it("our own echo is not written back, and is not reported", async () => {
    // Without this the shell would save the document it just adopted, which
    // would produce another event on every machine, forever.
    const gateway = new FakeGateway();
    const { store, events } = build(gateway);
    store.save(doc({ themePack: "mine" }));
    await settle();
    expect(gateway.writes).toHaveLength(1);

    gateway.arrive(1, doc({ themePack: "mine" })); // our own save, echoed
    await settle();
    store.save(doc({ themePack: "mine" }));
    await settle();

    expect(gateway.writes).toHaveLength(1);
    expect(events.filter((e) => e.kind === "document")).toHaveLength(0);
  });

  it("another machine's document is adopted and reported as remote", async () => {
    const gateway = new FakeGateway();
    const { store, events } = build(gateway);
    store.save(doc({ themePack: "mine" }));
    await settle();

    gateway.arrive(2, doc({ themePack: "theirs" }));
    await settle();

    expect(events).toEqual([
      { kind: "document", document: expect.objectContaining({ themePack: "theirs" }), origin: "remote" },
    ]);
  });

  it("a row older than the one held is ignored", async () => {
    const gateway = new FakeGateway();
    gateway.stored = { revision: 5, document: doc({ themePack: "current" }) };
    const { events } = build(gateway);
    await settle();

    gateway.arrive(3, doc({ themePack: "stale-arrival" }));
    await settle();

    expect(events).toHaveLength(1);
    expect(events[0]).toMatchObject({ origin: "hydrate" });
  });
});

describe("GraphDesktopStore: the active desk rides along but never causes a save", () => {
  it("a desk switch alone writes nothing", async () => {
    // The ping-pong this closes: the shell KEEPS the desk it is showing when
    // it adopts a document, so the document it rebuilds differs from the row
    // in exactly that field. Compared whole, each machine would save the
    // other's arrival straight back, forever.
    const gateway = new FakeGateway();
    const two = doc({
      desks: [
        { id: "desk-1", createdBy: "user" },
        { id: "desk-2", createdBy: "user" },
      ],
      surfaces: { "desk-1": { items: {}, positions: {} }, "desk-2": { items: {}, positions: {} } },
    });
    const { store } = build(gateway);
    store.save(two);
    await settle();
    expect(gateway.writes).toHaveLength(1);

    store.save({ ...two, activeDeskId: "desk-2" });
    await settle();

    expect(gateway.writes).toHaveLength(1);
  });

  it("but it is carried by a save that happens for another reason", async () => {
    const gateway = new FakeGateway();
    const two = doc({
      desks: [
        { id: "desk-1", createdBy: "user" },
        { id: "desk-2", createdBy: "user" },
      ],
      surfaces: { "desk-1": { items: {}, positions: {} }, "desk-2": { items: {}, positions: {} } },
    });
    const { store } = build(gateway);
    store.save(two);
    await settle();

    store.save({ ...two, activeDeskId: "desk-2", themePack: "midnight" });
    await settle();

    expect(gateway.writes).toHaveLength(2);
    expect(gateway.writes[1]?.document.activeDeskId).toBe("desk-2");
  });
});

describe("GraphDesktopStore: a document this bundle cannot read", () => {
  it("stops writing rather than overwriting it, and says so", async () => {
    const gateway = new FakeGateway();
    // A document from a newer app: sanitizeDocument rejects an unknown version.
    gateway.stored = { revision: 12, document: { version: 2, desks: [{ id: "d" }] } };
    const { store, local, events } = build(gateway);
    await settle();

    store.save(doc({ themePack: "downgrade" }));
    await settle();

    expect(events).toEqual([{ kind: "stale" }]);
    expect(gateway.writes).toEqual([]);
    // Local still works: this machine keeps its desktop, it just stops
    // publishing it.
    expect(local.load()?.themePack).toBe("downgrade");
  });
});

describe("GraphDesktopStore: writes", () => {
  it("a refused write is not retried on a timer", async () => {
    const gateway = new FakeGateway();
    gateway.failWrite = true;
    const { store } = build(gateway);

    store.save(doc({ themePack: "refused" }));
    await settle();
    await settle();

    expect(gateway.writes).toEqual([]);
  });

  it("flush sends a debounced save immediately", async () => {
    const gateway = new FakeGateway();
    const local = new LocalDesktopStore();
    const store = new GraphDesktopStore(local, gateway, 60_000);
    store.subscribe(() => {});
    await settle();

    store.save(doc({ themePack: "closing" }));
    expect(gateway.writes).toEqual([]);
    store.flush();
    await settle();

    expect(gateway.writes).toHaveLength(1);
  });

  it("an in-flight upload state is not published to the other machine", async () => {
    // sanitizeDocument turns "uploading" into "failed", and the store sends
    // the sanitized form -- an upload is a fact about THIS browser's network,
    // and on another machine it would be a spinner nothing there can finish.
    const gateway = new FakeGateway();
    const { store } = build(gateway);
    store.save(
      doc({
        surfaces: {
          "desk-1": {
            items: {
              "item-1": {
                kind: "file",
                id: "item-1",
                artifactId: "a",
                title: "t",
                fileKind: "document",
                source: "uploaded",
                uploadState: "uploading",
              },
            },
            positions: { "item-1": { col: 0, row: 0 } },
          },
        },
      }),
    );
    await settle();

    const written = gateway.writes[0]?.document.surfaces["desk-1"]?.items["item-1"];
    expect(written).toMatchObject({ uploadState: "failed" });
  });
});

describe("canonicalDocument", () => {
  it("ignores key order, so a round-tripped document compares equal", () => {
    // JSONB does not preserve key order and neither does the engine's value
    // rendering. A plain JSON.stringify comparison would call two identical
    // desktops different and write on every save.
    expect(canonicalDocument({ b: 1, a: [2, { d: 3, c: 4 }] })).toBe(
      canonicalDocument({ a: [2, { c: 4, d: 3 }], b: 1 }),
    );
  });

  it("still tells different documents apart", () => {
    expect(canonicalDocument({ a: 1 })).not.toBe(canonicalDocument({ a: 2 }));
  });

  it("does not confuse a listener's failure for a delivery", async () => {
    const gateway = new FakeGateway();
    gateway.stored = { revision: 1, document: doc() };
    const store = new GraphDesktopStore(new LocalDesktopStore(), gateway, 0);
    const second = vi.fn();
    store.subscribe(() => {
      throw new Error("this listener is broken");
    });
    store.subscribe(second);
    await settle();
    expect(second).toHaveBeenCalledTimes(1);
  });
});
