// The portal as the reference consumer of the SDK store (memql#4539).
//
// Three claims, each MEASURED rather than asserted:
//
//   1. No surface hand-rolls a graph fold any more, except two named ones
//      whose reasons are on record here.
//   2. Landing on a page issues ONE MyAccessMsg, however many components
//      want the caller's role.
//   3. A machine's write is visible even when its own echo is dropped -- the
//      failure the fleet page's "the subscription carries the write back"
//      model had no answer for.

import { readFileSync, readdirSync, statSync } from "node:fs";
import { join } from "node:path";
import React from "react";
import { describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import type {
  Connection,
  ConnectionStatusEvent,
  Event,
  Row,
} from "@znasllc-io/memql-sdk-core/client";

import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { useMyAccess } from "../src/cluster/useMyAccess";
import { asQueryClient } from "./support/queryFake";

// ---------------------------------------------------------------------
// 1. the fold gate
// ---------------------------------------------------------------------

// The only files permitted to call subscriptions.subscribeGraph directly.
// Everything else goes through the SDK's LiveCollection, which is what makes
// the fold rules -- delete-by-enum, the authorized re-read, the scope
// re-filter, re-seed on a gap -- one implementation instead of eleven.
const HAND_ROLLED_ALLOWLIST: Record<string, string> = {
  "src/cluster/useConceptRows.ts":
    "the concept browser's arrivals BAND, which accumulates alongside a paged " +
    "walk rather than into it: the keyset cursor orders ascending, so folding " +
    "a row created now guarantees a duplicate when paging reaches it. It takes " +
    "its continuity from the store via useLiveContinuity.",
};

// stripComments removes // line comments and /* */ blocks. Crude -- it does
// not understand a `//` inside a string literal -- which is fine for a gate
// looking for a method call: a false NEGATIVE would need the call itself to
// sit inside a string, and that would not be a call.
function stripComments(source: string): string {
  return source.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/[^\n]*/g, "");
}

function sourceFiles(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) {
      sourceFiles(full, out);
    } else if (/\.tsx?$/.test(entry)) {
      out.push(full);
    }
  }
  return out;
}

describe("the fold lives in the SDK", () => {
  it("has no hand-rolled graph subscription outside the named exceptions", () => {
    const offenders: string[] = [];
    let scanned = 0;
    for (const file of sourceFiles("src")) {
      scanned++;
      // Comments STRIPPED before the scan. ClusterProvider names the method
      // in prose while explaining why it guards against a missing one, and a
      // gate that cannot tell a mention from a call teaches people to reword
      // comments rather than to change code.
      if (!/\.subscribeGraph\s*\(/.test(stripComments(readFileSync(file, "utf8")))) continue;
      const rel = file.replace(/\\/g, "/");
      if (HAND_ROLLED_ALLOWLIST[rel] === undefined) offenders.push(rel);
    }
    // The scanner reports its own coverage: a pass has to be a claim about the
    // code, not about the glob.
    expect(scanned).toBeGreaterThan(100);
    expect(offenders).toEqual([]);
  });

  it("keeps every allowlist entry pointing at a file that still subscribes", () => {
    // A stale exemption is how an allowlist stops meaning anything.
    for (const [file, reason] of Object.entries(HAND_ROLLED_ALLOWLIST)) {
      expect(reason.length).toBeGreaterThan(20);
      expect(/\.subscribeGraph\s*\(/.test(stripComments(readFileSync(file, "utf8")))).toBe(true);
    }
  });
});

// ---------------------------------------------------------------------
// 2. one MyAccess per connection
// ---------------------------------------------------------------------

function Who({ id }: { id: string }): React.ReactNode {
  const { access } = useMyAccess();
  return <span data-testid={id}>{access?.clusterRole ?? ""}</span>;
}

describe("MyAccess", () => {
  it("is read ONCE per connection, however many components ask", async () => {
    // Fifteen modules call useMyAccess and several mount on every page, so
    // landing anywhere used to issue fourteen identical round trips. The only
    // thing wrong with each of those call sites was that there were fourteen.
    let reads = 0;
    const conn = {
      nodeId: "bff-test",
      serverVersion: "0.0.0-test",
      engineVersion: "dev",
      query: asQueryClient({
        getMyAccess: vi.fn(async () => {
          reads++;
          return { userId: "user-1", primaryEmail: "op@example.test", clusterRole: "admin" };
        }),
      }),
      subscriptions: { subscribeGraph: () => () => {}, onDelivery: () => () => {} },
      close: vi.fn(),
      done: vi.fn(() => new Promise<void>(() => {})),
      onStatusChange: (fn: (ev: { status: string; attempt: number; error: string }) => void) => {
        fn({ status: "connected", attempt: 0, error: "" });
        return () => {};
      },
      onConnectionCycle: () => () => {},
    } as unknown as Connection;

    render(
      <ClusterProvider dial={vi.fn(async () => conn)}>
        {Array.from({ length: 14 }, (_, i) => (
          <Who key={i} id={`who-${i}`} />
        ))}
      </ClusterProvider>,
    );

    await waitFor(() => expect(screen.getByTestId("who-13").textContent).toBe("admin"));
    expect(reads).toBe(1);
  });
});

// ---------------------------------------------------------------------
// 3. the write's echo survives a gap
// ---------------------------------------------------------------------

describe("the fleet page's write-echo model", () => {
  it("makes a write visible even when its own event is dropped", async () => {
    // useMachines does not refetch after a write: the subscription carries the
    // new value back. That had one hole -- a dropped event made the operator's
    // own write look ignored, permanently, with the page still rendering live.
    // The collection re-seeds on any gap, so a lost echo costs a re-read.
    const { LiveStore } = await import("@znasllc-io/memql-sdk-core/client");

    let name = "before-the-write";
    let seeds = 0;
    let deliver: ((ev: Event) => void) | null = null;
    let observe: ((ev: Event) => void) | null = null;

    const store = new LiveStore({
      subscriptions: {
        subscribeGraph: (handler: (ev: Event) => void) => {
          deliver = handler;
          return () => {};
        },
        onDelivery: (handler: (ev: Event) => void) => {
          observe = handler;
          return () => {};
        },
      } as never,
    });

    const handle = store.collection<Row>("machines", {
      concept: "v1:worker:registration",
      paged: false,
      seed: async () => {
        seeds++;
        return { rows: [{ id: "wk-1", displayName: name }], nextCursor: "" };
      },
    });
    await new Promise((r) => setTimeout(r, 5));
    expect(seeds).toBe(1);
    expect(handle.value.snapshot.rows[0]?.["displayName"]).toBe("before-the-write");

    // The write lands server-side; its echo is the event that never arrives.
    name = "after-the-write";

    // Some LATER delivery on the stream carries the drop flag. Note it is not
    // this subscription's event -- which is the point: continuity is a stream
    // property, and the store reads it once for everything.
    observe!({
      subscriptionId: "someone-else",
      kind: "NODE_UPDATED",
      timestamp: null,
      payload: { id: "unrelated" },
      payloadOmitted: false,
      seq: 7,
      gapBefore: true,
    });

    await waitFor(() =>
      expect(handle.value.snapshot.rows[0]?.["displayName"]).toBe("after-the-write"),
    );
    expect(seeds).toBe(2);
    expect(deliver).not.toBeNull();
    store.dispose();
  });
});

// ---------------------------------------------------------------------
// 4. disconnect honesty
// ---------------------------------------------------------------------

describe("a store-backed surface on a dead stream", () => {
  it("keeps its rows and stops claiming they are live", async () => {
    // An operator staring at a table wants the last known answer LABELLED
    // stale -- not a blank, and not a live-looking table over a dead socket.
    const { LiveStore } = await import("@znasllc-io/memql-sdk-core/client");
    let status: (ev: ConnectionStatusEvent) => void = () => {};
    const store = new LiveStore({
      subscriptions: { subscribeGraph: () => () => {}, onDelivery: () => () => {} } as never,
      onStatusChange: (fn) => {
        status = fn;
        return () => {};
      },
    });
    const handle = store.collection<Row>("k", {
      concept: "c",
      paged: false,
      seed: async () => ({ rows: [{ id: "a" }], nextCursor: "" }),
    });
    await waitFor(() => expect(handle.value.snapshot.state).toBe("live"));

    act(() => status({ status: "reconnecting", attempt: 1, error: "stream closed" }));
    expect(handle.value.snapshot.state).toBe("disconnected");
    expect(handle.value.snapshot.rows).toHaveLength(1);
    store.dispose();
  });
});
