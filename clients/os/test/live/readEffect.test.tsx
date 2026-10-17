import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, expect, it } from "vitest";
import { useReading } from "../../src/cluster/reading";

afterEach(cleanup);
it("coalesces invalidations, publishes completed reads, and never reuses another identity's late result", async () => {
  const pending: { key: string; signal: AbortSignal; resolve: (v: string) => void }[] = [];
  const { result, rerender, unmount } = renderHook(({ key, revision }) => useReading(key,
    signal => new Promise<string>(resolve => pending.push({ key, signal, resolve })), revision),
    { initialProps: { key: "first", revision: 0 } });
  for (let revision = 1; revision <= 100; revision++) rerender({ key: "first", revision });
  expect(pending).toHaveLength(1);
  await act(async () => pending[0]!.resolve("first value"));
  expect(result.current.value).toBe("first value");
  expect(pending).toHaveLength(2);
  rerender({ key: "second", revision: 0 });
  expect(pending[1]!.signal.aborted).toBe(true);
  expect(pending).toHaveLength(3);
  await act(async () => pending[1]!.resolve("obsolete"));
  act(() => result.current.reread());
  expect(pending).toHaveLength(3); // old finally must not clear the new read's busy flag
  await act(async () => pending[2]!.resolve("second value"));
  expect(result.current.value).toBe("second value");
  act(() => result.current.reread());
  expect(pending).toHaveLength(4);
  unmount();
  expect(pending[3]!.signal.aborted).toBe(true);
  await act(async () => pending[3]!.resolve("unmounted"));
  expect(pending).toHaveLength(4);
});
