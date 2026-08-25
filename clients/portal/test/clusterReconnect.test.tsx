// The portal riding SDK-owned reconnect (memql#4537).
//
// The provider used to null query / subscriptions / clients the moment the
// stream ended, and recovery was a button. Now the SDK redials and replays,
// and the provider's job is narrower: map the SDK's transport state onto the
// console's wider vocabulary, hold the handles across a blip, and pass the
// connection-cycle signal on so stores can re-seed.
//
// What is pinned here is the part a jsdom test can actually see: consumers are
// NOT torn down by a recovered drop, the status is honest while it lasts, and
// Retry accelerates the SDK's loop rather than replacing it.

import React from "react";
import { describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import type { Connection } from "@znasllc-io/memql-sdk-core/client";

import { ClusterProvider, useCluster } from "../src/cluster/ClusterProvider";
import { asQueryClient } from "./support/queryFake";

type StatusHandler = (ev: { status: string; attempt: number; error: string }) => void;

interface Harness {
  connection: Connection;
  retryNow: ReturnType<typeof vi.fn>;
  emitStatus: StatusHandler;
  emitCycle: (cycle: number) => void;
}

function harness(): Harness {
  let statusHandler: StatusHandler = () => {};
  let cycleHandler: (cycle: number) => void = () => {};
  let status = "connected";
  const retryNow = vi.fn();

  const connection = {
    nodeId: "bff-test",
    serverVersion: "0.0.0-test",
    engineVersion: "v9.9.9",
    query: asQueryClient({}),
    subscriptions: { subscribeGraph: () => () => {} },
    close: vi.fn(),
    // A recovered drop must never resolve done(): that is the whole point of
    // supervision, and a provider that treated it as the end would blank the
    // console every time a node rolled.
    done: vi.fn(() => new Promise<void>(() => {})),
    get status() {
      return status;
    },
    retryNow,
    onStatusChange: (fn: StatusHandler) => {
      statusHandler = fn;
      fn({ status: "connected", attempt: 0, error: "" });
      return () => {};
    },
    onConnectionCycle: (fn: (cycle: number) => void) => {
      cycleHandler = fn;
      return () => {};
    },
  } as unknown as Connection;

  return {
    connection,
    retryNow,
    emitStatus: (ev) => {
      status = ev.status;
      statusHandler(ev);
    },
    emitCycle: (cycle) => cycleHandler(cycle),
  };
}

let mounts = 0;

function Probe(): React.ReactNode {
  const { status, query, subscriptions, reconnectAttempt, connectionCycle, reconnect } =
    useCluster();
  return (
    <div>
      <span data-testid="status">{status}</span>
      <span data-testid="handles">{query !== null && subscriptions !== null ? "live" : "gone"}</span>
      <span data-testid="attempt">{String(reconnectAttempt)}</span>
      <span data-testid="cycle">{String(connectionCycle)}</span>
      <span data-testid="mounts">{String(mounts)}</span>
      <button type="button" onClick={reconnect}>
        Retry
      </button>
    </div>
  );
}

function CountingProbe(): React.ReactNode {
  // Counts MOUNTS, not renders: what a torn-down consumer costs is its state,
  // and remount is how that is lost.
  const first = React.useRef(false);
  if (!first.current) {
    first.current = true;
    mounts++;
  }
  return <Probe />;
}

function renderProvider(h: Harness) {
  mounts = 0;
  return render(
    <ClusterProvider dial={vi.fn(async () => h.connection)}>
      <CountingProbe />
    </ClusterProvider>,
  );
}

describe("the portal on SDK reconnect", () => {
  it("reports reconnecting without tearing consumers down", async () => {
    const h = harness();
    renderProvider(h);
    await waitFor(() => expect(screen.getByTestId("status").textContent).toBe("connected"));

    act(() => h.emitStatus({ status: "reconnecting", attempt: 2, error: "stream closed" }));

    await waitFor(() => expect(screen.getByTestId("status").textContent).toBe("reconnecting"));
    expect(screen.getByTestId("attempt").textContent).toBe("2");
    // The handles survive. The dispatcher's identity survives a redial, so
    // nulling them would unmount every consumer's data for a blip the SDK
    // recovers from in a second -- and a surface that must not look live has
    // `status` to say so.
    expect(screen.getByTestId("handles").textContent).toBe("live");
    expect(screen.getByTestId("mounts").textContent).toBe("1");

    act(() => h.emitStatus({ status: "connected", attempt: 0, error: "" }));
    await waitFor(() => expect(screen.getByTestId("status").textContent).toBe("connected"));
    expect(screen.getByTestId("mounts").textContent).toBe("1");
  });

  it("passes the connection cycle on, which is what a store re-seeds off", async () => {
    const h = harness();
    renderProvider(h);
    await waitFor(() => expect(screen.getByTestId("cycle").textContent).toBe("0"));
    act(() => h.emitCycle(1));
    await waitFor(() => expect(screen.getByTestId("cycle").textContent).toBe("1"));
  });

  it("Retry accelerates the SDK's loop instead of redialing behind it", async () => {
    const h = harness();
    renderProvider(h);
    await waitFor(() => expect(screen.getByTestId("status").textContent).toBe("connected"));
    act(() => h.emitStatus({ status: "reconnecting", attempt: 1, error: "" }));
    await waitFor(() => expect(screen.getByTestId("status").textContent).toBe("reconnecting"));

    act(() => screen.getByRole("button", { name: "Retry" }).click());
    // A second dial here would race the SDK's own in-flight attempt and leave
    // two connections fighting over one context slot.
    expect(h.retryNow).toHaveBeenCalledTimes(1);
    expect(screen.getByTestId("mounts").textContent).toBe("1");
  });
});
