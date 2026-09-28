import { act, render, screen, cleanup } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ReactNode } from "react";

const h = vi.hoisted(() => {
  const reply = (rows: unknown[]) => ({ rows: () => rows });
  return {
    connection: {
      nodeId: "bff-test",
      engineVersion: "v9.9.9",
      engineCommit: "abcdef",
      subscriptions: null,
      dispatcher: null,
      onStatusChange: () => () => {},
      query: {
        providerAuthStatus: vi.fn(async () => reply([])),
        integrationStatus: vi.fn(async () => reply([])),
        providersReload: vi.fn(async () => reply([])),
        // The section reads inferenceStatus for the vendors' own state.
        // Nothing in this file asserts on it; it is here so the section can
        // render at all.
        inferenceStatus: vi.fn(async () => reply([{}])),
      },
    } as unknown,
  };
});

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { SessionProvider } from "../../src/chrome/access";
import { OsProvider } from "../../src/chrome/state";
import { OS_REGISTRY } from "../../src/apps/registry";
import { UNKNOWN_RUNTIME_CONFIG } from "../../src/cluster/config";
import { SettingsApp } from "../../src/apps/settings/SettingsApp";

// ARRIVING AT A VENDOR (epic memql#5106). The first-run wizard hands Settings
// a `{ vendor }` intent; this is the receiving half of that contract.
//
// jsdom implements neither `scrollIntoView` nor layout, so what is asserted is
// the part that is real in every browser AND in a test: focus lands inside the
// named vendor's panel, and the intent is consumed by id exactly once.
//
// THE PANEL IS NOW OPENED ON DEMAND (epic memql#5153), which splits the act in
// two and makes the ABSENCE of a panel the assertion for every case that names
// no vendor. The section opens the panel in one effect and reveals-and-consumes
// in a second, precisely because on the tick the intent arrives the region is
// not in the DOM yet and `findRegion` would answer null -- a silent no-op that
// reads exactly like a broken intent. So `[data-os-vendor]` existing at all is
// what "a vendor was revealed" means here, and the earlier form of these
// assertions -- `panel?.contains(activeElement)` being `false` -- could no
// longer distinguish "not revealed" from "no panel", answering `undefined` to
// both.

afterEach(cleanup);

function wrap(children: ReactNode, role = "owner") {
  return (
    <SessionProvider
      value={{
        access: { userId: "u-1", primaryEmail: "owner@example.com", role: role, roleName: "", rank: 0 },
        config: { ...UNKNOWN_RUNTIME_CONFIG, domain: "example.com" },
        ladderLoaded: true,
      }}
    >
      <OsProvider registry={OS_REGISTRY} actorRole={role} grid={{ cols: 12, rows: 8 }} layout="desktop">
        {children}
      </OsProvider>
    </SessionProvider>
  );
}

async function open(payload: Record<string, unknown> | null) {
  const consume = vi.fn();
  const view = render(
    wrap(
      <SettingsApp
        sectionId="providers"
        navigate={vi.fn()}
        askContext={vi.fn()}
        intent={payload === null ? undefined : { id: "intent-7", payload }}
        consumeIntent={consume}
      />,
    ),
  );
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
  return { view, consume };
}

describe("Settings, opened at a vendor", () => {
  it("puts the cursor inside the Anthropic panel and consumes the intent by id", async () => {
    const { consume } = await open({ vendor: "anthropic" });
    const panel = document.querySelector('[data-os-vendor="anthropic"]');
    expect(panel).not.toBeNull();
    expect(panel?.contains(document.activeElement)).toBe(true);
    expect(consume).toHaveBeenCalledExactlyOnceWith("intent-7");
  });

  it("puts the cursor inside the OpenAI panel for the other vendor", async () => {
    await open({ vendor: "openai" });
    expect(document.querySelector('[data-os-vendor="openai"]')?.contains(document.activeElement)).toBe(true);
  });

  it("does nothing, and consumes nothing, when the intent names no vendor", async () => {
    const { consume } = await open({ somethingElse: true });
    expect(consume).not.toHaveBeenCalled();
    // A vendor nobody named must not be revealed on a guess -- and with the
    // form behind its row, not revealing one means not opening one at all.
    expect(document.querySelector("[data-os-vendor]")).toBeNull();
  });

  it("renders the section unchanged when the shell hands it no intent at all", async () => {
    const { consume } = await open(null);
    expect(consume).not.toHaveBeenCalled();
    // RE-POINTED TITLE. "AI providers" became "Doors" (epic memql#5153) and
    // then "Vendors" (routing redesign, 2026-09-28); what this asserts is
    // unchanged -- the section renders its standing self when the shell asks
    // it for nothing in particular.
    expect(screen.getByRole("heading", { name: "Vendors" })).toBeTruthy();
    expect(document.querySelector("[data-os-vendor]")).toBeNull();
  });

  it("ignores a vendor this build has no panel for, rather than throwing", async () => {
    // COMPARED rather than interpolated: a payload is a string another
    // surface wrote, and a selector built out of one could become a
    // different selector. This value would match the Anthropic panel if it
    // were pasted into a query.
    const { consume } = await open({ vendor: 'x"] , [data-os-vendor="anthropic' });
    expect(document.querySelector("[data-os-vendor]")).toBeNull();
    // Consumed even so: an instruction nobody can act on must not be
    // delivered again forever.
    expect(consume).toHaveBeenCalledExactlyOnceWith("intent-7");
  });
});
