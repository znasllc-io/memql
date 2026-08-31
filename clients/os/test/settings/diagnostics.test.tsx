import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { ReactNode } from "react";

// The connection module is replaced wholesale, so the two path exports it
// also owns have to be restated -- the OS resolves its bridge through them.
const h = vi.hoisted(() => {
  type StatusHandler = (ev: { status: string; attempt: number; error: string }) => void;
  let statusHandler: StatusHandler = () => {};
  const connection = {
    nodeId: "bff-test",
    engineVersion: "v9.9.9",
    engineCommit: "abcdef123456",
    query: {
      existingCluster: vi.fn(async () => ({ rows: () => [] })),
      deploymentsForCluster: vi.fn(async () => ({ rows: () => [] })),
      nodeSpecsForDeployment: vi.fn(async () => ({ rows: () => [] })),
      integrationStatus: vi.fn(async () => ({ rows: () => [] })),
      providerAuthStatus: vi.fn(async () => ({ rows: () => [] })),
    },
    subscriptions: null,
    onStatusChange: (fn: StatusHandler) => {
      statusHandler = fn;
      // The SDK fires synchronously on subscribe with the CURRENT reading.
      fn({ status: "connected", attempt: 0, error: "" });
      return () => {};
    },
  };
  return {
    connection,
    emit(ev: { status: string; attempt: number; error: string }) {
      statusHandler(ev);
    },
  };
});

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { SessionProvider } = await import("../../src/chrome/access");
const { OsProvider } = await import("../../src/chrome/state");
const { OS_REGISTRY } = await import("../../src/apps/registry");
const { SettingsApp } = await import("../../src/apps/settings/SettingsApp");
const { LocalDesktopStore } = await import("../../src/system/store");
const { UNKNOWN_RUNTIME_CONFIG } = await import("../../src/cluster/config");

const ACCESS = { userId: "u-1", primaryEmail: "owner@example.com", clusterRole: "owner" };

function memStorage(): Pick<Storage, "getItem" | "setItem"> {
  const data = new Map<string, string>();
  return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
}

function wrap(children: ReactNode, role = "owner") {
  return (
    <SessionProvider
      value={{
        access: { ...ACCESS, clusterRole: role },
        config: { ...UNKNOWN_RUNTIME_CONFIG, domain: "example.com" },
      }}
    >
      <OsProvider
        registry={OS_REGISTRY}
        actorRole={role}
        grid={{ cols: 12, rows: 8 }}
        store={new LocalDesktopStore(memStorage())}
      >
        {children}
      </OsProvider>
    </SessionProvider>
  );
}

function renderDiagnostics(role = "owner") {
  return render(
    wrap(<SettingsApp sectionId="diagnostics" navigate={vi.fn()} askContext={vi.fn()} />, role),
  );
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe("the connection panel (memql#4744)", () => {
  it("records the reading it opened on, and calls it a reading", () => {
    renderDiagnostics();
    const list = screen.getByRole("list", { name: "Connection transitions" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(1);
    expect(within(list).getByText(/reading when this window opened/)).toBeTruthy();
    expect(screen.getByText("none in this session")).toBeTruthy();
  });

  it("tracks a simulated disconnect and reconnect in order", () => {
    renderDiagnostics();
    act(() => h.emit({ status: "reconnecting", attempt: 1, error: "stream closed" }));
    act(() => h.emit({ status: "reconnecting", attempt: 2, error: "stream closed" }));
    act(() => h.emit({ status: "connected", attempt: 0, error: "" }));

    const rows = within(screen.getByRole("list", { name: "Connection transitions" })).getAllByRole(
      "listitem",
    );
    expect(rows).toHaveLength(4);
    expect(rows[1]!.textContent).toContain("reconnecting");
    expect(rows[1]!.textContent).toContain("attempt 1");
    expect(rows[1]!.textContent).toContain("stream closed");
    expect(rows[3]!.textContent).toContain("connected");
    // The reconnect time is now known, so the caption is no longer "none".
    expect(screen.queryByText("none in this session")).toBeNull();
  });

  it("shows the resolved endpoint, not the relative path", () => {
    renderDiagnostics();
    // Resolved the way the SDK resolves it, against this document's origin.
    // jsdom serves over http, so the scheme is the unsecured one.
    const shown = screen.getByText(/_memql\/ws/).textContent ?? "";
    expect(shown).toMatch(/^ws:\/\/[^/]+\/_memql\/ws$/);
    expect(shown).not.toBe("/_memql/ws");
  });
});

describe("the permissions self-view", () => {
  it("names each hidden surface and what it needs, for a reader", () => {
    renderDiagnostics("reader");
    const list = screen.getByRole("list", { name: "Hidden from this session" });
    const rows = within(list).getAllByRole("listitem").map((li) => li.textContent ?? "");
    expect(rows.some((r) => /Users.*requires admin.*you are reader/.test(r))).toBe(true);
    expect(rows.some((r) => /Training.*requires writer/.test(r))).toBe(true);
    // The informative case: a section gated above an app the reader CAN open.
    expect(rows.some((r) => /Settings -- Cluster.*requires admin/.test(r))).toBe(true);
  });

  it("tells an owner that nothing is hidden", () => {
    renderDiagnostics("owner");
    expect(screen.queryByRole("list", { name: "Hidden from this session" })).toBeNull();
    expect(screen.getByText(/Nothing in this shell is hidden from you/)).toBeTruthy();
  });

  it("states the presentation-gating caveat on the panel itself", () => {
    renderDiagnostics();
    expect(screen.getByText(/row admission is the\s+authority on every read/)).toBeTruthy();
  });
});

describe("copy diagnostics", () => {
  it("writes the report to the clipboard and says so", async () => {
    // Typed argument: an untyped vi.fn gives `mock.calls` an empty tuple
    // type, so indexing it is a typecheck error rather than a test failure.
    const writeText = vi.fn(async (_text: string) => {});
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    renderDiagnostics();
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Copy diagnostics" }));
    });
    expect(writeText).toHaveBeenCalledOnce();
    expect(writeText.mock.calls[0]![0]).toContain("MemQL OS -- diagnostics");
    expect(screen.getByText("Diagnostics copied.")).toBeTruthy();
    vi.unstubAllGlobals();
  });

  it("falls back IN SURFACE when the clipboard refuses -- never a toast", async () => {
    vi.stubGlobal("navigator", {
      clipboard: { writeText: vi.fn(async () => Promise.reject(new Error("denied"))) },
    });
    renderDiagnostics();
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Copy diagnostics" }));
    });
    expect(screen.getByText(/select the report below and copy it/)).toBeTruthy();
    const box = screen.getByRole("textbox", { name: "Diagnostics report" }) as HTMLTextAreaElement;
    expect(box.value).toContain("MemQL OS -- diagnostics");
    expect(box.readOnly).toBe(true);
    vi.unstubAllGlobals();
  });

  it("falls back when the browser offers no clipboard at all", async () => {
    vi.stubGlobal("navigator", {});
    renderDiagnostics();
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Copy diagnostics" }));
    });
    expect(screen.getByText(/did not offer a clipboard/)).toBeTruthy();
    vi.unstubAllGlobals();
  });

  it("says cluster facts are not admitted for a session below admin", async () => {
    vi.stubGlobal("navigator", {});
    renderDiagnostics("writer");
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Copy diagnostics" }));
    });
    const box = screen.getByRole("textbox", { name: "Diagnostics report" }) as HTMLTextAreaElement;
    expect(box.value).toContain("Cluster facts\n  not admitted");
    vi.unstubAllGlobals();
  });
});
