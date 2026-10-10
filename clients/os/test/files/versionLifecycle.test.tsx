import { act, renderHook } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import type { Result } from "@znasllc-io/memql-sdk-core/client";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { useFileVersions } from "../../src/apps/files/actions/versions";
import { fileRow, rowsResult } from "./harness";

it("aborts superseded reads and ignores a late response from the previous file", async () => {
  const pending: Array<{ signal: AbortSignal; resolve: (result: Result) => void }> = [];
  h.connection = { query: {
    libraryFileById: vi.fn((_args, options: { signal: AbortSignal }) => new Promise<Result>(resolve => pending.push({ signal: options.signal, resolve }))),
    libraryFileVersionsForFile: vi.fn(async () => rowsResult([])),
  } };
  const hook = renderHook(({ id, revision }) => useFileVersions(id, revision), { initialProps: { id: "first", revision: "1" } });
  await act(async () => { pending[0]!.resolve(rowsResult([fileRow({ id: "first", versionNumber: 1 })])); });
  expect(hook.result.current.head?.id).toBe("first");
  hook.rerender({ id: "first", revision: "2" });
  hook.rerender({ id: "second", revision: "3" });
  expect(pending[1]!.signal.aborted).toBe(true);
  await act(async () => { pending[2]!.resolve(rowsResult([fileRow({ id: "second", versionNumber: 3 })])); });
  await act(async () => { pending[1]!.resolve(rowsResult([fileRow({ id: "first", versionNumber: 99 })])); });
  expect(hook.result.current.head?.id).toBe("second");
  expect(hook.result.current.history.entries[0]?.versionNumber).toBe(3);
  hook.unmount();
  expect(pending[2]!.signal.aborted).toBe(true);
});
