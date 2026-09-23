import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, type Result, type Row } from "@znasllc-io/memql-sdk-core/client";

// The unseen-change marker for Cluster > Mesh (epic memql#5338), through the
// whole shell: marked where it is reached, never acknowledged by an ancestor,
// cleared where Mesh is actually read. The shape of test/settings/
// language.test.tsx's marker cases, which is the precedent this follows.

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../src/live/connection")>();
  return { ...actual, useOsConnection: () => h.connection };
});

import { OS_REGISTRY } from "../../src/apps/registry";
import { resetIdsForTest } from "../../src/system/desks";
import { builtinReply } from "../deployables/harness";
import { appTileName } from "../appTile";
import { OWNER, READER, openFromLauncher, renderShell } from "../settings/shellHarness";

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

describe("Mesh in the registry", () => {
  it("sits after Modules with no floor of its own, and declares its change", () => {
    const cluster = OS_REGISTRY.apps.find((a) => a.id === "cluster")!;
    const ids = cluster.sections!.map((s) => s.id);
    expect(ids.indexOf("mesh")).toBe(ids.indexOf("modules") + 1);
    expect(cluster.sections!.find((s) => s.id === "mesh")).toEqual({ id: "mesh", name: "Mesh" });
    expect(cluster.attentionChanges).toContainEqual({
      id: "cluster:mesh",
      revision: "mesh-1",
      sectionId: "mesh",
      label: "See what every node hears",
    });
  });
});

describe("the unseen-change marker on Mesh", () => {
  function meshNavButton() {
    const nav = screen.getByRole("navigation", { name: "Cluster sections" });
    return within(nav).getByRole("button", { name: /^Mesh/ });
  }

  it("marks the section, survives the window opening on Readiness, and clears where Mesh is read", async () => {
    const stub = connect();
    renderShell({ access: OWNER });
    openFromLauncher("Cluster");

    const button = meshNavButton();
    await waitFor(() => expect(within(button).getByRole("img", { name: "Unseen change" })).toBeTruthy());
    // Cluster opened on Readiness: an ANCESTOR view of the change, which must
    // acknowledge nothing.
    expect(stub.executeNamed.mock.calls.some(([name]) => name === "acknowledgeAttention")).toBe(false);

    fireEvent.click(button);
    await screen.findByText("What each node hears of the events the cluster broadcasts.");
    await waitFor(() =>
      expect(stub.executeNamed.mock.calls.filter(([name]) => name === "acknowledgeAttention")).toHaveLength(1),
    );
    const [, call] = stub.executeNamed.mock.calls.find(([name]) => name === "acknowledgeAttention")!;
    expect(call).toContain('changeId: "cluster:mesh"');
    expect(call).toContain('revision: "mesh-1"');
    await waitFor(() => expect(within(meshNavButton()).queryByRole("img", { name: "Unseen change" })).toBeNull());
  });

  it("is not offered to somebody the Cluster app is not open to", () => {
    connect();
    renderShell({ access: READER });
    fireEvent.click(screen.getByRole("button", { name: "Launcher" }));
    const launcher = screen.getByRole("dialog", { name: "Launcher" });
    expect(within(launcher).queryByRole("button", { name: appTileName("Cluster") })).toBeNull();
  });
});
