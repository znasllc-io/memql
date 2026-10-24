import { act, renderHook, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Connection, Result } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { useFileDestination } from "../../src/apps/files/useFileDestination";
import { artifactRow, click, fakeConnection, renderFiles, rowsResult } from "./harness";

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(done => { resolve = done; });
  return { promise, resolve };
}

beforeEach(() => { h.connection = null; });
afterEach(() => { vi.useRealTimers(); });

describe("explicit file destinations", () => {
  it("opens an owner-checked artifact while the whole Library seed remains pending", async () => {
    const connection = fakeConnection();
    const original = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
    const lookup = vi.spyOn(connection.query, "executeNamed").mockImplementation((name, call, opts) => {
      if (name === "libraryArtifactsByLens") return new Promise(() => {});
      if (name === "libraryArtifactById") return Promise.resolve(rowsResult([artifactRow({ id: "log-index", title: "build.log" })]));
      return original(name, call, opts);
    });
    h.connection = connection;
    const consumed = vi.fn();
    const view = await renderFiles({ intent: { id: "open-log", payload: { artifactId: "log-index" } }, consumeIntent: consumed });
    expect(within(screen.getByRole("region", { name: "File details" })).getByRole("heading", { name: "build.log" })).toBeTruthy();
    expect(lookup.mock.calls.some(([name, call]) => name === "libraryArtifactById" && call === 'query libraryArtifactById(artifactId: "log-index")')).toBe(true);
    expect(consumed).toHaveBeenCalledExactlyOnceWith("open-log");
    expect(connection.subscriptions.activeCount("v1:library:artifact")).toBe(1);
    await act(async () => view.setIntent(undefined));
    expect(within(screen.getByRole("region", { name: "File details" })).getByRole("heading", { name: "build.log" })).toBeTruthy();
    await click(screen.getByRole("button", { name: "Back to Library" }));
    expect(screen.queryByRole("region", { name: "File details" })).toBeNull();
  });

  it("keeps an unauthorized or absent file unavailable and retries the same bounded owner lookup", async () => {
    const connection = fakeConnection();
    const original = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
    let available = false;
    vi.spyOn(connection.query, "executeNamed").mockImplementation((name, call, opts) => name === "libraryArtifactForFile"
      ? Promise.resolve(rowsResult(available ? [artifactRow({ id: "index", sourceConceptRef: "v1:library:file:target", title: "reply.txt" })] : []))
      : original(name, call, opts));
    h.connection = connection;
    const consumed = vi.fn();
    await renderFiles({ intent: { id: "open-file", payload: { fileId: "target" } }, consumeIntent: consumed });
    expect(screen.getByText("This file is unavailable.")).toBeTruthy();
    expect(consumed).not.toHaveBeenCalled();
    expect(screen.queryByRole("region", { name: "File details" })).toBeNull();
    available = true;
    await click(screen.getByRole("button", { name: "Try again" }));
    expect(within(screen.getByRole("region", { name: "File details" })).getByRole("heading", { name: "reply.txt" })).toBeTruthy();
    expect(consumed).toHaveBeenCalledExactlyOnceWith("open-file");
    const updated = artifactRow({ id: "index", sourceConceptRef: "v1:library:file:target", title: "renamed.txt" });
    await act(async () => connection.subscriptions.emit("v1:library:artifact", updated, "NODE_UPDATED"));
    expect(within(screen.getByRole("region", { name: "File details" })).getByRole("heading", { name: "renamed.txt" })).toBeTruthy();
    await act(async () => connection.subscriptions.emit("v1:library:artifact", updated, "NODE_DELETED"));
    expect(screen.queryByRole("region", { name: "File details" })).toBeNull();
    expect(screen.queryByText("reply.txt")).toBeNull();
    expect(screen.queryByText("renamed.txt")).toBeNull();
  });

  it.each(["direct", "already indexed"])("keeps the live feed authoritative after a %s destination is updated and deleted", async mode => {
    const first = artifactRow({ id: "target", title: "original.log" });
    const connection = fakeConnection({ artifacts: mode === "already indexed" ? [first] : [] });
    const original = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
    const lookup = vi.spyOn(connection.query, "executeNamed").mockImplementation((name, call, opts) => name === "libraryArtifactById"
      ? Promise.resolve(rowsResult([first])) : original(name, call, opts));
    h.connection = connection;
    const intent = { id: "open-live", payload: { artifactId: "target" } };
    const view = await renderFiles(mode === "already indexed" ? {} : { intent });
    if (mode === "already indexed") {
      await screen.findByRole("button", { name: /original\.log/ });
      await act(async () => view.setIntent(intent));
      expect(lookup.mock.calls.filter(([name]) => name === "libraryArtifactById")).toHaveLength(0);
    }
    expect(within(screen.getByRole("region", { name: "File details" })).getByRole("heading", { name: "original.log" })).toBeTruthy();
    const updated = artifactRow({ id: "target", title: "renamed.log" });
    await act(async () => connection.subscriptions.emit("v1:library:artifact", updated, "NODE_UPDATED"));
    expect(within(screen.getByRole("region", { name: "File details" })).getByRole("heading", { name: "renamed.log" })).toBeTruthy();
    await act(async () => connection.subscriptions.emit("v1:library:artifact", updated, "NODE_DELETED"));
    expect(screen.queryByRole("region", { name: "File details" })).toBeNull();
    expect(screen.queryByText("original.log")).toBeNull();
    expect(screen.queryByText("renamed.log")).toBeNull();
  });

  it("shows an interrupted lookup with a working retry", async () => {
    const refuse = { libraryArtifactById: "Connection interrupted" };
    const connection = fakeConnection({ refuse, artifacts: [] });
    h.connection = connection;
    await renderFiles({ intent: { id: "retry-file", payload: { artifactId: "target" } } });
    expect(screen.getByText("This file could not be opened.")).toBeTruthy();
    delete (refuse as Record<string, string>).libraryArtifactById;
    await click(screen.getByRole("button", { name: "Try again" }));
    expect(screen.getByText("This file is unavailable.")).toBeTruthy();
  });

  it("ignores a superseded reply and aborts its read", async () => {
    const first = deferred<Result>();
    const connection = fakeConnection();
    const original = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
    let signal: AbortSignal | undefined;
    vi.spyOn(connection.query, "executeNamed").mockImplementation((name, call, opts) => {
      if (name !== "libraryArtifactById") return original(name, call, opts);
      if (call.includes('"first"')) { signal = opts?.signal; return first.promise; }
      return Promise.resolve(rowsResult([artifactRow({ id: "second", title: "second.log" })]));
    });
    h.connection = connection;
    const consumed = vi.fn();
    const view = await renderFiles({ intent: { id: "one", payload: { artifactId: "first" } }, consumeIntent: consumed });
    expect(screen.getByText("Opening the file")).toBeTruthy();
    await act(async () => view.setIntent({ id: "two", payload: { artifactId: "second" } }));
    expect(signal?.aborted).toBe(true);
    await act(async () => first.resolve(rowsResult([artifactRow({ id: "first", title: "first.log" })])));
    expect(within(screen.getByRole("region", { name: "File details" })).getByRole("heading", { name: "second.log" })).toBeTruthy();
    expect(screen.queryByText("first.log")).toBeNull();
    expect(consumed).toHaveBeenCalledExactlyOnceWith("two");
  });

  it("bounds a stalled read and ignores its reply after timeout or unmount", async () => {
    vi.useFakeTimers();
    const pending = deferred<Result>();
    const connection = fakeConnection();
    let signal: AbortSignal | undefined;
    vi.spyOn(connection.query, "executeNamed").mockImplementation((_name, _call, opts) => { signal = opts?.signal; return pending.promise; });
    const intent = { id: "bounded", payload: { artifactId: "target" } };
    const { result, unmount } = renderHook(() => useFileDestination(connection as unknown as Connection, intent, []));
    await act(async () => { vi.advanceTimersByTime(20_000); });
    expect(signal?.aborted).toBe(true);
    expect(result.current.destination?.status).toBe("error");
    await act(async () => pending.resolve(rowsResult([artifactRow({ id: "target" })])));
    expect(result.current.destination?.status).toBe("error");
    act(() => result.current.retry());
    const retrySignal = signal;
    unmount();
    expect(retrySignal?.aborted).toBe(true);
  });
});
