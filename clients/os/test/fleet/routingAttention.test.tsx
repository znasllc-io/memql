import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, type Result, type Row } from "@znasllc-io/memql-sdk-core/client";

// The unseen-change marker for Fleet > Routing (routing redesign, 2026-09-28).
// Routing moved: Fleet's Policies and Machine routing and Settings' Rules and
// Decisions are now one section, so a person who knew where those were is
// told where they went. Marked where it is reached, never acknowledged by the
// section a Fleet window opens on, cleared where Routing is actually read --
// the shape of test/cluster/meshAttention.test.tsx.

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../src/live/connection")>();
  return { ...actual, useOsConnection: () => h.connection };
});

import { OS_REGISTRY } from "../../src/apps/registry";
import { resetIdsForTest } from "../../src/system/desks";
import { builtinReply } from "../deployables/harness";
import { OWNER, openFromLauncher, renderShell } from "../settings/shellHarness";

function connect() {
  const receipts: Row[] = [];
  const executeNamed = vi.fn(async (name: string, call: string): Promise<Result> => {
    if (name === "myAttentionReceipts") return builtinReply("myAttentionReceipts", receipts);
    if (name === "acknowledgeAttention") {
      const changeId = /changeId: "([^"]+)"/.exec(call)?.[1] ?? "";
      const revision = /revision: "([^"]+)"/.exec(call)?.[1] ?? "";
      receipts.push({ id: changeId + revision, changeId, revision });
    }
    return builtinReply(name, []);
  });
  const query = Object.assign(Object.create(QueryClient.prototype), { executeNamed });
  h.connection = { query, subscriptions: null, onStatusChange: () => () => {} };
  return { executeNamed };
}

const acknowledged = (stub: ReturnType<typeof connect>) =>
  stub.executeNamed.mock.calls.filter(([name, call]) => name === "acknowledgeAttention" && String(call).includes('changeId: "fleet:routing"'));

beforeEach(() => {
  h.connection = null;
  resetIdsForTest();
});

describe("Routing in the registry", () => {
  it("declares the move as one change, on the section every Fleet user can open", () => {
    const fleet = OS_REGISTRY.apps.find((a) => a.id === "fleet")!;
    expect(fleet.attentionChanges).toContainEqual({
      id: "fleet:routing",
      revision: "routing-1",
      sectionId: "routing",
      label: "Routes, rules and history, all in Routing",
    });
    expect(fleet.sections!.find((s) => s.id === "routing")?.requires).toBeUndefined();
  });
});

describe("the unseen-change marker on Routing", () => {
  function routingNavButton() {
    const nav = screen.getByRole("navigation", { name: "Fleet sections" });
    return within(nav).getByRole("button", { name: /^Routing/ });
  }

  it("marks the section, survives the window opening on Machines, and clears where Routing is read", async () => {
    const stub = connect();
    renderShell({ access: OWNER });
    openFromLauncher("Fleet");

    const button = routingNavButton();
    await waitFor(() => expect(within(button).getByRole("img", { name: "Unseen change" })).toBeTruthy());
    // Fleet opened on Machines, which is not the destination.
    expect(acknowledged(stub)).toHaveLength(0);

    fireEvent.click(button);
    await screen.findByRole("navigation", { name: "Routing views" });
    await waitFor(() => expect(acknowledged(stub)).toHaveLength(1));
    const [, call] = acknowledged(stub)[0]!;
    expect(call).toContain('revision: "routing-1"');
    await waitFor(() => expect(within(routingNavButton()).queryByRole("img", { name: "Unseen change" })).toBeNull());
  });
});
