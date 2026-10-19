import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ReactNode } from "react";
import { ConnectionRecoveryContext, ConnectionStatusContext, type ShellConnectionStatus } from "../../src/chrome/connection";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

import { useDomains } from "../../src/apps/training/useDomains";
import { useLogTail } from "../../src/logs/useLogTail";
import { useReleaseRead } from "../../src/apps/cluster/releases/useReleaseRead";

const interrupted = () => Object.assign(new Error("stream closed"), { reason: "transport" });
const rows = (value: Record<string, unknown>[]) => ({ rows: () => value });
afterEach(() => { cleanup(); vi.restoreAllMocks(); h.connection = null; });

describe("recovery of reads outside Ask", () => {
  it("restores an interrupted release read on page return without losing its saved value", async () => {
    const read = vi.fn().mockResolvedValueOnce("saved release").mockRejectedValueOnce(interrupted()).mockResolvedValueOnce("fresh release");
    const source = {};
    const { result } = renderHook(() => useReleaseRead("release", read, true, source, 0));
    await waitFor(() => expect(result.current.value).toBe("saved release"));
    act(() => result.current.retry());
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
    expect(result.current).toMatchObject({ value: "saved release", fresh: false, error: "" });
    act(() => {
      window.dispatchEvent(new Event("pageshow"));
      window.dispatchEvent(new Event("focus"));
      window.dispatchEvent(new Event("online"));
    });
    await waitFor(() => expect(result.current).toMatchObject({ value: "fresh release", fresh: true, error: "" }));
    expect(read).toHaveBeenCalledTimes(3);
  });

  it("still reports a real release refusal", async () => {
    const read = vi.fn().mockRejectedValue(new Error("Only the owner can read this release."));
    const source = {};
    const { result } = renderHook(() => useReleaseRead("release", read, true, source, 0));
    await waitFor(() => expect(result.current.error).toBe("Only the owner can read this release."));
  });

  it("keeps log rows and resumes the cursor after a lost read", async () => {
    const first = { id: "log-1", occurredAt: "2026-10-18T12:00:00Z", message: "first" };
    const tail = vi.fn().mockResolvedValueOnce(rows([first])).mockRejectedValueOnce(interrupted()).mockResolvedValueOnce(rows([{ id: "log-2", occurredAt: "2026-10-18T12:00:01Z", message: "next" }]));
    h.connection = { query: { logsTail: tail } };
    const { result } = renderHook(() => useLogTail({ args: {}, viewKey: "all", paused: false, pollMs: 100_000 }));
    await waitFor(() => expect(result.current.rows).toHaveLength(1));
    act(() => window.dispatchEvent(new Event("pageshow")));
    await waitFor(() => expect(tail).toHaveBeenCalledTimes(2));
    expect(result.current.error).toBe("");
    expect(result.current.rows[0]?.id).toBe("log-1");
    // A later return is a separate wake, not another event from the same burst.
    const now = vi.spyOn(Date, "now").mockReturnValue(Date.now() + 1_000);
    act(() => window.dispatchEvent(new Event("online")));
    await waitFor(() => expect(result.current.rows.map(row => row.id)).toEqual(["log-1", "log-2"]));
    expect(tail.mock.calls[2]?.[0]).toMatchObject({ afterId: "log-1", afterAt: first.occurredAt });
    now.mockRestore();
  });

  it("preserves domain names through interruption and rereads once connected", async () => {
    const domains = vi.fn().mockResolvedValue(rows([{ id: "chunk", domainId: "birds", validationStatus: "validated" }]));
    const catalog = vi.fn().mockResolvedValueOnce(rows([{ id: "birds", name: "Birds" }])).mockRejectedValueOnce(interrupted()).mockResolvedValue(rows([{ id: "birds", name: "Birds updated" }]));
    h.connection = { query: { allDocumentChunkDomains: domains, knowledgeDomainsAll: catalog } };
    let status: ShellConnectionStatus = "connected";
    const wrapper = ({ children }: { children: ReactNode }) => <ConnectionRecoveryContext.Provider value={{ managed: true, retry: () => {} }}><ConnectionStatusContext.Provider value={status}>{children}</ConnectionStatusContext.Provider></ConnectionRecoveryContext.Provider>;
    const { result, rerender } = renderHook(() => useDomains(), { wrapper });
    await waitFor(() => expect(result.current.domains.get("birds")?.name).toBe("Birds"));
    act(() => result.current.reload());
    await waitFor(() => expect(catalog).toHaveBeenCalledTimes(2));
    expect(result.current.error).toBe("");
    expect(result.current.domains.get("birds")?.name).toBe("Birds");
    status = "reconnecting"; rerender();
    act(() => window.dispatchEvent(new Event("pageshow")));
    expect(catalog).toHaveBeenCalledTimes(2);
    status = "connected"; rerender();
    await waitFor(() => expect(result.current.domains.get("birds")?.name).toBe("Birds updated"));
    expect(catalog).toHaveBeenCalledTimes(3);
  });
});
