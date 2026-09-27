import { act, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { ReactNode } from "react";

// Settings -> Procedures (epic memql#5408, #5412): the certification ladder's
// values, read-only.
//
// ===========================================================================
// THE HEADLINE ASSERTION: AN ABSENT ROW IS SAID, NEVER FILLED IN
// ===========================================================================
// The Go side falls back to 5, 2, 5, 2, 1 and 30 when the policy row is
// missing. A Settings page that printed those numbers for a cluster that
// published nothing would be claiming a policy nobody wrote -- the one page a
// person reads to learn why a procedure has not been promoted. So an absent
// row renders a sentence and NO digits, and a zero in the row (which the
// concept's @minimum(1) forbids) is an absence too.

const h = vi.hoisted(() => {
  const state = {
    rows: [] as unknown[],
    error: null as Error | null,
    calls: 0,
  };
  const connection = {
    nodeId: "bff-test",
    engineVersion: "v9.9.9",
    engineCommit: "abcdef",
    subscriptions: null,
    dispatcher: null,
    query: {
      ladderPolicyCurrent: vi.fn(async () => {
        state.calls += 1;
        if (state.error !== null) throw state.error;
        return { rows: () => state.rows };
      }),
    },
    onStatusChange: () => () => {},
  };
  return { connection, state };
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
const { PROCEDURES_SECTION_RESOURCE } = await import("../../src/apps/settings/ProceduresSection");
const { roleOpens } = await import("../seededAccess");

function memStorage(): Pick<Storage, "getItem" | "setItem"> {
  const data = new Map<string, string>();
  return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
}

function wrap(children: ReactNode, role = "owner") {
  return (
    <SessionProvider
      value={{
        access: { userId: "u-1", primaryEmail: "owner@example.com", role, roleName: "", rank: 0 },
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

async function renderProcedures() {
  render(wrap(<SettingsApp sectionId="procedures" navigate={vi.fn()} askContext={vi.fn()} />));
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
}

const POLICY = {
  id: "v1:authoring:ladderPolicy:primary",
  shadowMatches: 5,
  distinctBindings: 2,
  canaryMatches: 4,
  failuresToDemote: 3,
  insufficientToDemote: 1,
  retireAfterDays: 30,
};

beforeEach(() => {
  h.state.rows = [POLICY];
  h.state.error = null;
  h.state.calls = 0;
});

describe("Settings -> Procedures: the ladder's values", () => {
  it("says each value as the rule it enforces, with its unit", async () => {
    await renderProcedures();
    expect(await screen.findByText("Shadow matches before promotion is proposed")).toBeTruthy();
    expect(screen.getAllByText("5 in a row").length).toBe(1);
    expect(screen.getByText("Distinct bindings each parameter needs")).toBeTruthy();
    expect(screen.getByText("2 bindings")).toBeTruthy();
    expect(screen.getByText("Clean canary replays before it is trusted")).toBeTruthy();
    expect(screen.getByText("4 in a row")).toBeTruthy();
    expect(screen.getByText("Failed replays that demote it")).toBeTruthy();
    expect(screen.getByText("3 in a row")).toBeTruthy();
    expect(screen.getByText("Failures after passing checks that demote it")).toBeTruthy();
    expect(screen.getByText("1 replay")).toBeTruthy();
    expect(screen.getByText("Unused this long, it retires")).toBeTruthy();
    expect(screen.getByText("30 days")).toBeTruthy();
    // Why there is nothing to edit here is guidance, so it is behind the
    // information control rather than standing under the values (rule 7).
    expect(screen.getByText(/The seed writes them again on every boot/).closest("dialog")).not.toBeNull();
    expect(screen.queryByRole("textbox")).toBeNull();
  });

  it("says an absent policy row out loud and invents no numbers", async () => {
    h.state.rows = [];
    await renderProcedures();
    expect(
      await screen.findByText("This cluster has not published its ladder policy yet, so no values are shown."),
    ).toBeTruthy();
    const panel = screen.getByRole("region", { name: "Certification ladder" });
    expect(panel.textContent ?? "").not.toMatch(/\d/);
  });

  it("reads a zero as not published, because the concept forbids one", async () => {
    h.state.rows = [{ ...POLICY, canaryMatches: 0 }];
    await renderProcedures();
    await screen.findByText("Clean canary replays before it is trusted");
    // The Fact renders its own absence mark rather than "0 in a row".
    expect(screen.queryByText("0 in a row")).toBeNull();
  });

  it("shows a refusal in the server's own words", async () => {
    h.state.error = new Error("PERMISSION_DENIED: below the reader rung");
    await renderProcedures();
    expect(await screen.findByText("PERMISSION_DENIED: below the reader rung")).toBeTruthy();
  });

  it("draws the values' shape while they arrive, and paints no loading words", async () => {
    h.connection.query.ladderPolicyCurrent.mockImplementationOnce(() => new Promise<never>(() => {}));
    await renderProcedures();
    // LOADING IS THE SHAPE OF THE CONTENT (DESIGN.md): the panel holds quiet
    // shapes where the values will stand, not an empty box under its heading.
    const panel = screen.getByRole("region", { name: "Certification ladder" });
    const status = within(panel).getByRole("status");
    expect(status.getAttribute("aria-busy")).toBe("true");
    expect(status.querySelector(".os-content-skeleton-shape")).not.toBeNull();
    // ...and its words are announced, not painted.
    expect(within(status).getByText(/Loading/).className).toContain("os-sr-only");
    expect(screen.queryByText(/Asking the cluster/)).toBeNull();
    // No value is claimed while nothing has been read.
    expect(panel.textContent ?? "").not.toMatch(/\d/);
  });

  it("explains the ladder behind the one information control", async () => {
    await renderProcedures();
    expect(await screen.findByRole("button", { name: "About The certification ladder" })).toBeTruthy();
  });
});

describe("Settings -> Procedures: who may see it", () => {
  it("opens to the Levels roles and to nobody below them", () => {
    expect(roleOpens("owner", PROCEDURES_SECTION_RESOURCE)).toBe(true);
    expect(roleOpens("developer", PROCEDURES_SECTION_RESOURCE)).toBe(true);
    expect(roleOpens("admin", PROCEDURES_SECTION_RESOURCE)).toBe(true);
    expect(roleOpens("writer", PROCEDURES_SECTION_RESOURCE)).toBe(false);
    expect(roleOpens("reader", PROCEDURES_SECTION_RESOURCE)).toBe(false);
    expect(roleOpens("viewer", PROCEDURES_SECTION_RESOURCE)).toBe(false);
  });

  it("declares the same resource in the manifest as the section names", () => {
    const settings = OS_REGISTRY.apps.find((a) => a.id === "settings");
    expect(settings?.sections?.find((s) => s.id === "procedures")?.requires).toBe(
      PROCEDURES_SECTION_RESOURCE,
    );
  });
});
