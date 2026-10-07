import { act, fireEvent, render, screen, waitFor, cleanup } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { UpdateBrowser, type UpdateQueries } from "../../src/apps/cluster/updates/UpdatesSection";
import { releaseResult } from "./releaseFixtures";

afterEach(cleanup);
const candidateId = `sha256:${"a".repeat(64)}`, catalogDigest = `sha256:${"b".repeat(64)}`;
const components = [{ name: "engine", version: "0.25.0", repository: "example/engine", commit: "c".repeat(40) }];
function fixture() {
  const state = { fail: false, wrongDigest: false, empty: false, next: true, calls: [] as string[] };
  const query: UpdateQueries = {
    releaseSources: async () => { state.calls.push("sources"); return releaseResult({ sources: state.empty ? [] : [{ id: "publisher", publisher: "MemQL" }] }); },
    releaseDiscoverPublishedCandidates: async args => {
      state.calls.push(`page:${args.cursor ?? ""}`);
      if (state.fail) throw new Error("Publisher could not be verified");
      return releaseResult({ sourceId: "publisher", publisher: "MemQL", releases: [{ candidateId, catalogDigest, components }], ...(state.next ? { nextCursor: "older" } : {}) });
    },
    releaseReadDiscoveredCandidate: async args => {
      state.calls.push("get");
      expect(args).toEqual({ sourceId: "publisher", candidateId, catalogDigest });
      return releaseResult({ sourceId: "publisher", candidateId, catalogDigest: state.wrongDigest ? `sha256:${"f".repeat(64)}` : catalogDigest, release: { publisher: "MemQL", candidateId, components } });
    },
  };
  return { query, state };
}

describe("signed update discovery", () => {
  it("opens a publisher, rechecks an exact release, pages and restores navigation focus", async () => {
    const f = fixture();
    render(<UpdateBrowser query={f.query} connected />);
    fireEvent.click(await screen.findByRole("button", { name: "Check updates from MemQL" }));
    fireEvent.click(await screen.findByRole("button", { name: "Inspect engine 0.25.0" }));
    await screen.findByText("example/engine");
    expect(screen.queryByRole("list", { name: "Published releases" })).toBeNull();
    expect(screen.queryByRole("button", { name: /install|deploy|approve/i })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Back to MemQL" }));
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Inspect engine 0.25.0" })));
    fireEvent.click(screen.getByRole("button", { name: "Older" }));
    await waitFor(() => expect(f.state.calls).toContain("page:older"));
    fireEvent.click(screen.getByRole("button", { name: "Newer" }));
    await waitFor(() => expect(f.state.calls.filter(c => c === "page:").length).toBeGreaterThan(1));
  });
  it("refuses a changed selection and distinguishes a failed read from an empty list", async () => {
    const f = fixture(); f.state.fail = true;
    render(<UpdateBrowser query={f.query} connected />);
    fireEvent.click(await screen.findByRole("button", { name: "Check updates from MemQL" }));
    await screen.findByText("Publisher could not be verified");
    expect(screen.queryByText(/No published releases/)).toBeNull();
    f.state.fail = false;
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    f.state.wrongDigest = true;
    fireEvent.click(await screen.findByRole("button", { name: "Inspect engine 0.25.0" }));
    await screen.findByText(/This release changed/);
    expect(screen.queryByText("example/engine")).toBeNull();
  });
  it("performs no background polling and pauses hidden or disconnected views", async () => {
    vi.useFakeTimers();
    try {
      const f = fixture();
      const view = render(<UpdateBrowser query={f.query} connected />);
      await act(async () => {});
      expect(f.state.calls).toEqual(["sources"]);
      await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
      expect(f.state.calls).toEqual(["sources"]);
      view.rerender(<UpdateBrowser query={f.query} connected visible={false} />);
      await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
      expect(f.state.calls).toEqual(["sources"]);
      view.rerender(<UpdateBrowser query={f.query} connected={false} />);
      expect(screen.getByText(/Disconnected/)).toBeTruthy();
      view.rerender(<UpdateBrowser query={f.query} connected />);
      await act(async () => {});
      expect(f.state.calls).toEqual(["sources", "sources"]);
    } finally { vi.useRealTimers(); }
  });
  it("discards late reads after the connection changes", async () => {
    const a = fixture(), b = fixture(); b.state.empty = true;
    let resolve!: (r: ReturnType<typeof releaseResult>) => void;
    a.query.releaseSources = () => new Promise(r => { resolve = r; });
    const view = render(<UpdateBrowser query={a.query} connected />);
    view.rerender(<UpdateBrowser query={b.query} connected />);
    await screen.findByText("No release publishers are configured.");
    await act(async () => resolve(releaseResult({ sources: [{ id: "old", publisher: "Old publisher" }] })));
    expect(screen.queryByRole("button", { name: /Old publisher/ })).toBeNull();
  });
});
