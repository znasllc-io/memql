import { act, render, renderHook } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { LiveSnapshot } from "@znasllc-io/memql-sdk-core/client";

import { LiveList, type LiveListSource } from "../../src/live/LiveList";
import { useLiveView } from "../../src/apps/fleet/liveView";

interface Row {
  id: string;
  keep: boolean;
}

function fakeSource(initial: LiveSnapshot<Row>): LiveListSource<Row> & {
  push: (s: LiveSnapshot<Row>) => void;
} {
  let snapshot = initial;
  const listeners = new Set<() => void>();
  return {
    get snapshot() {
      return snapshot;
    },
    subscribe(listener) {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    push(next) {
      snapshot = next;
      for (const l of listeners) l();
    },
  };
}

const snap = (rows: Row[], version: number): LiveSnapshot<Row> => ({
  rows,
  state: "live",
  error: "",
  version,
});

describe("useLiveView", () => {
  it("returns an IDENTITY-STABLE snapshot while upstream is unchanged", () => {
    const source = fakeSource(snap([{ id: "a", keep: true }, { id: "b", keep: false }], 1));
    const { result } = renderHook(() =>
      useLiveView<Row>(source, "keep", (rows) => rows.filter((r) => r.keep)),
    );

    // useSyncExternalStore compares getSnapshot's result with Object.is on
    // every render. A view that rebuilt the array each call would render
    // forever; this is the property that stops it.
    const first = result.current!.snapshot;
    expect(result.current!.snapshot).toBe(first);
    expect(first.rows.map((r) => r.id)).toEqual(["a"]);
  });

  it("recomputes when upstream changes, and only then", () => {
    const source = fakeSource(snap([{ id: "a", keep: true }], 1));
    const { result } = renderHook(() =>
      useLiveView<Row>(source, "keep", (rows) => rows.filter((r) => r.keep)),
    );
    const first = result.current!.snapshot;

    act(() => source.push(snap([{ id: "a", keep: true }, { id: "b", keep: true }], 2)));
    const second = result.current!.snapshot;
    expect(second).not.toBe(first);
    expect(second.rows.map((r) => r.id)).toEqual(["a", "b"]);
  });

  it("passes the feed's own state and error through unchanged", () => {
    const source = fakeSource({ rows: [], state: "degraded", error: "stream gone", version: 3 });
    const { result } = renderHook(() => useLiveView<Row>(source, "all", (rows) => [...rows]));
    // A view that narrows rows must not also claim the feed is in a
    // different condition than it is.
    expect(result.current!.snapshot.state).toBe("degraded");
    expect(result.current!.snapshot.error).toBe("stream gone");
    expect(result.current!.snapshot.version).toBe(3);
  });

  it("is null for a null source, which LiveList renders as disconnected", () => {
    const { result } = renderHook(() => useLiveView<Row>(null, "k", (rows) => [...rows]));
    expect(result.current).toBeNull();

    const view = render(
      <LiveList<Row>
        source={null}
        rowId={(r) => r.id}
        fingerprint={(r) => r.id}
        label="Rows"
        emptyText="Nothing yet."
        renderRow={(r) => <span>{r.id}</span>}
      />,
    );
    // Never a fake empty list.
    expect(view.getByText("Not connected to the cluster")).toBeTruthy();
    expect(view.queryByText("Nothing yet.")).toBeNull();
  });

  it("drives a LiveList without re-rendering it into a loop", () => {
    const source = fakeSource(snap([{ id: "a", keep: true }, { id: "b", keep: false }], 1));
    function Harness() {
      const view = useLiveView<Row>(source, "keep", (rows) => rows.filter((r) => r.keep));
      return (
        <LiveList<Row>
          source={view}
          rowId={(r) => r.id}
          fingerprint={(r) => r.id}
          label="Rows"
          emptyText="Nothing yet."
          renderRow={(r) => <span>{r.id}</span>}
        />
      );
    }
    const view = render(<Harness />);
    expect(view.getByText("a")).toBeTruthy();
    expect(view.queryByText("b")).toBeNull();
  });
});
