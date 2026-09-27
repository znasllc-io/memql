import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, type Result, type Row } from "@znasllc-io/memql-sdk-core/client";

// The unseen-change marker for reuse labels (epic memql#5414, D24), through
// the whole shell: marked on Automations, never acknowledged by the window
// opening on another section, and cleared where Automations is read -- the
// list is where the labels and the Reuse question are.

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../src/live/connection")>();
  return { ...actual, useOsConnection: () => h.connection };
});

import { OS_REGISTRY } from "../../src/apps/registry";
import { resetIdsForTest } from "../../src/system/desks";
import { builtinReply } from "../deployables/harness";
import { OWNER, openFromLauncher, renderShell } from "../settings/shellHarness";

function connect(receipts: Row[] = []) {
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

function automationsNavButton() {
  const nav = screen.getByRole("navigation", { name: "Nexus sections" });
  return within(nav).getByRole("button", { name: /^Automations/ });
}

const reuseAcknowledgements = (stub: ReturnType<typeof connect>) =>
  stub.executeNamed.mock.calls.filter(
    ([name, call]) => name === "acknowledgeAttention" && String(call).includes('changeId: "nexus:reuse"'),
  );

describe("the reuse-labels change, in the registry", () => {
  it("is declared once on Nexus, destined for Automations", () => {
    const nexus = OS_REGISTRY.apps.find((a) => a.id === "nexus")!;
    expect(nexus.attentionChanges?.filter((c) => c.id === "nexus:reuse")).toEqual([
      { id: "nexus:reuse", revision: "reuse-1", sectionId: "automations", label: "Reuse labels" },
    ]);
  });
});

describe("the reuse-labels marker", () => {
  it("marks Automations, is not acknowledged by the window opening elsewhere, and clears where Automations is read", async () => {
    // The learned-procedures change is already seen, so the only mark left
    // on Automations is this one.
    const stub = connect([{ id: "p", changeId: "nexus:procedures", revision: "procedures-1" }]);
    renderShell({ access: OWNER });
    openFromLauncher("Nexus");

    await waitFor(() => expect(within(automationsNavButton()).getByRole("img", { name: "Unseen change" })).toBeTruthy());
    expect(reuseAcknowledgements(stub)).toHaveLength(0);

    fireEvent.click(automationsNavButton());
    await screen.findByRole("heading", { name: "Automations" });
    await waitFor(() => expect(reuseAcknowledgements(stub)).toHaveLength(1));
    expect(String(reuseAcknowledgements(stub)[0]![1])).toContain('revision: "reuse-1"');
    await waitFor(() =>
      expect(within(automationsNavButton()).queryByRole("img", { name: "Unseen change" })).toBeNull(),
    );
  });
});
