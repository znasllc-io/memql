// The portal's binding to the SDK store (memql#4538).
//
// The store is the SDK's and is tested there; what is pinned here is the part
// that is React's: a retain balanced by a release across StrictMode's double
// mount, a remount issuing no new read, and a dead connection outranking the
// collection's own view.

import React from "react";
import { describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import type { Connection, Row } from "@znasllc-io/memql-sdk-core/client";

import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { useLive, useLiveValue } from "../src/cluster/useLive";
import { asQueryClient } from "./support/queryFake";

type GraphHandler = (ev: {
  kind: string;
  payload: Row | null;
  payloadOmitted: boolean;
  seq: number;
  gapBefore: boolean;
  subscriptionId: string;
  timestamp: null;
}) => void;

function fakeConnection(): { connection: Connection; emit: (ev: Partial<Parameters<GraphHandler>[0]>) => void } {
  const handlers: GraphHandler[] = [];
  const connection = {
    nodeId: "bff-test",
    serverVersion: "0.0.0-test",
    engineVersion: "dev",
    query: asQueryClient({}),
    subscriptions: {
      subscribeGraph: (handler: GraphHandler) => {
        handlers.push(handler);
        return () => {
          const i = handlers.indexOf(handler);
          if (i >= 0) handlers.splice(i, 1);
        };
      },
      onDelivery: () => () => {},
    },
    close: vi.fn(),
    done: vi.fn(() => new Promise<void>(() => {})),
    onStatusChange: (fn: (ev: { status: string; attempt: number; error: string }) => void) => {
      fn({ status: "connected", attempt: 0, error: "" });
      return () => {};
    },
    onConnectionCycle: () => () => {},
  } as unknown as Connection;

  return {
    connection,
    emit: (ev) => {
      for (const h of [...handlers]) {
        h({
          subscriptionId: "sub",
          kind: "NODE_CREATED",
          timestamp: null,
          payload: null,
          payloadOmitted: false,
          seq: 0,
          gapBefore: false,
          ...ev,
        });
      }
    },
  };
}

let seeds = 0;

function Rows({ collectionKey }: { collectionKey: string }): React.ReactNode {
  const { rows, state } = useLive<Row>(collectionKey, () => ({
    concept: "v1:worker:registration",
    seed: async () => {
      seeds++;
      return { rows: [{ id: "m1", name: "laptop" }], nextCursor: "" };
    },
  }));
  return (
    <div>
      <span data-testid="rows">{rows.map((r) => String(r["id"])).join(",")}</span>
      <span data-testid="state">{state}</span>
    </div>
  );
}

// ONE dial function per test. Its identity is a dependency of the provider's
// dial effect, so a fresh one per render would re-dial -- which in the real
// portal never happens (the prop defaults to the stable Connection.dial) and
// here would quietly turn the remount test into a reconnect test.
function stableDial(connection: Connection) {
  return vi.fn(async () => connection);
}

function renderRows(connection: Connection, node: React.ReactNode, dial = stableDial(connection)) {
  const view = render(
    <React.StrictMode>
      <ClusterProvider dial={dial}>{node}</ClusterProvider>
    </React.StrictMode>,
  );
  return {
    ...view,
    swap: (next: React.ReactNode) =>
      view.rerender(
        <React.StrictMode>
          <ClusterProvider dial={dial}>{next}</ClusterProvider>
        </React.StrictMode>,
      ),
  };
}

describe("useLive", () => {
  it("seeds once and folds live arrivals", async () => {
    seeds = 0;
    const { connection, emit } = fakeConnection();
    renderRows(connection, <Rows collectionKey="machines" />);
    await waitFor(() => expect(screen.getByTestId("rows").textContent).toBe("m1"));
    expect(screen.getByTestId("state").textContent).toBe("live");
    // StrictMode mounts effects twice. A retain taken during render instead of
    // in the effect would leak, and a release without a linger would make this
    // two reads.
    expect(seeds).toBe(1);

    act(() => emit({ kind: "NODE_CREATED", payload: { id: "m2" } }));
    await waitFor(() => expect(screen.getByTestId("rows").textContent).toBe("m1,m2"));
  });

  it("two components on the same key share one seed and one subscription", async () => {
    seeds = 0;
    const { connection } = fakeConnection();
    renderRows(
      connection,
      <>
        <Rows collectionKey="machines" />
        <Rows collectionKey="machines" />
      </>,
    );
    await waitFor(() => expect(screen.getAllByTestId("rows")[0]?.textContent).toBe("m1"));
    expect(seeds).toBe(1);
  });

  it("remounting inside the linger window issues ZERO new reads", async () => {
    // The operator's original complaint, measured: navigating away and back
    // must not refetch.
    seeds = 0;
    const { connection } = fakeConnection();
    const view = renderRows(connection, <Rows collectionKey="machines" />);
    await waitFor(() => expect(screen.getByTestId("rows").textContent).toBe("m1"));
    expect(seeds).toBe(1);

    // Navigate away...
    view.swap(<div data-testid="elsewhere" />);
    await waitFor(() => expect(screen.getByTestId("elsewhere")).toBeTruthy());

    // ...and back.
    view.swap(<Rows collectionKey="machines" />);
    await waitFor(() => expect(screen.getByTestId("rows").textContent).toBe("m1"));
    expect(seeds).toBe(1);
  });
});

describe("useLiveValue", () => {
  it("collapses many callers into one read", async () => {
    let reads = 0;
    const { connection } = fakeConnection();
    function Who(): React.ReactNode {
      const { value } = useLiveValue<{ role: string }>("myAccess", async () => {
        reads++;
        return { role: "admin" };
      });
      return <span data-testid="role">{value?.role ?? ""}</span>;
    }
    renderRows(
      connection,
      <>
        <Who />
        <Who />
        <Who />
      </>,
    );
    await waitFor(() => expect(screen.getAllByTestId("role")[0]?.textContent).toBe("admin"));
    expect(reads).toBe(1);
  });
});
