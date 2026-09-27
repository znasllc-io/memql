import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, type Result, type Row } from "@znasllc-io/memql-sdk-core/client";

// The unseen-change marker for learned procedures (epic memql#5408, #5412),
// through the whole shell: marked where it is reached, never acknowledged by
// an ancestor, cleared where Automations -- the list that now holds them -- is
// actually read. The shape of test/cluster/meshAttention.test.tsx.

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

beforeEach(() => {
  h.connection = null;
  resetIdsForTest();
});

describe("the learned-procedures marker in the registry", () => {
  it("declares the learned-procedures change on Nexus, destined for Automations", () => {
    const nexus = OS_REGISTRY.apps.find((a) => a.id === "nexus")!;
    // Found by its id rather than pinned as the whole list: Nexus declares
    // other changes beside it (epic memql#5414's re-run and feedback marker),
    // each with its own destination and its own test.
    expect(nexus.attentionChanges?.find((c) => c.id === "nexus:procedures")).toEqual(
      { id: "nexus:procedures", revision: "procedures-1", sectionId: "automations", label: "Learned procedures" },
    );
    // A section every Nexus reader reaches, empty cluster included: it
    // carries no requirement of its own.
    expect(nexus.sections!.find((s) => s.id === "automations")?.requires).toBeUndefined();
  });
});

describe("the marker on Automations", () => {
  function automationsNavButton() {
    const nav = screen.getByRole("navigation", { name: "Nexus sections" });
    return within(nav).getByRole("button", { name: /^Automations/ });
  }

  it("marks the section, survives the window opening on Goals, and clears where Automations is read", async () => {
    const stub = connect();
    renderShell({ access: OWNER });
    openFromLauncher("Nexus");

    const button = automationsNavButton();
    await waitFor(() => expect(within(button).getByRole("img", { name: "Unseen change" })).toBeTruthy());
    // Nexus opened on Goals: an ANCESTOR view of the change, which must
    // acknowledge nothing.
    expect(stub.executeNamed.mock.calls.some(([name]) => name === "acknowledgeAttention")).toBe(false);

    fireEvent.click(button);
    await screen.findByRole("heading", { name: "Automations" });
    await waitFor(() =>
      expect(stub.executeNamed.mock.calls.filter(([name]) => name === "acknowledgeAttention")).toHaveLength(1),
    );
    const [, call] = stub.executeNamed.mock.calls.find(([name]) => name === "acknowledgeAttention")!;
    expect(call).toContain('changeId: "nexus:procedures"');
    expect(call).toContain('revision: "procedures-1"');
    await waitFor(() =>
      expect(within(automationsNavButton()).queryByRole("img", { name: "Unseen change" })).toBeNull(),
    );
  });
});
