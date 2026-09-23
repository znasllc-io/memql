import { act, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { MeshSection } = await import("../../src/apps/cluster/mesh/MeshSection");
const { clusterNodeRow, fakeConnection, withSession } = await import("./harness");

// Cluster > Mesh (epic memql#5338), through the REAL live collection, fold,
// list and node page, over the suite's fixture connection.

type Conn = ReturnType<typeof fakeConnection>;

function mount(connection: Conn, props: Parameters<typeof MeshSection>[0] = {}) {
  h.connection = connection;
  return render(withSession(<MeshSection {...props} />));
}

async function click(el: Element) {
  await act(async () => {
    (el as HTMLElement).click();
  });
}

const minutesAgo = (m: number) => new Date(Date.now() - m * 60_000).toISOString();

const BFF = clusterNodeRow({
  id: "bff-a",
  nodeType: "bff",
  mesh: {
    links: [
      { node: "agent-a", type: "agent", via: "both" },
      { node: "identity-a", type: "identity", via: "dialed" },
      { node: "edge-a", type: "edge", via: "accepted" },
    ],
  },
});
const AGENT = clusterNodeRow({
  id: "agent-a",
  nodeType: "agent",
  mesh: { links: [{ node: "bff-a", type: "bff", via: "both" }, { node: "workbench-gone", type: "workbench", via: "dialed" }] },
});
/** The island: linked, up for an hour, and has heard nothing. */
const DEAF_EDGE = clusterNodeRow({
  id: "edge-a",
  nodeType: "edge",
  mesh: { since: minutesAgo(60), links: [{ node: "bff-a", type: "bff", via: "dialed" }], heard: 0, lastHeardAt: undefined },
});
const IDENTITY = clusterNodeRow({
  id: "identity-a",
  nodeType: "identity",
  mesh: { receives: false, heard: 0, links: [{ node: "bff-a", type: "bff", via: "accepted" }], lastHeardAt: undefined },
});
/** A replica on an older release: no report at all. */
const UNREPORTED = clusterNodeRow({ id: "mcp-a", nodeType: "mcp", mesh: null });

/** A fact's whole value, as a person reads it: the figure and its unit are
 *  separate spans, which is the kit's markup and not something to assert on. */
function fact(label: string): string {
  const dt = screen.getAllByText(label).find((el) => el.tagName === "DT");
  return dt?.nextElementSibling?.textContent ?? "";
}

/** The sentence above the list, as a person reads it: a node it names is a
 *  button inside the sentence, so the words are split across elements.
 *  Waits for the Delivery breakdown, which is drawn only once the feed is
 *  live -- the sentence arrives with it. */
async function verdict(): Promise<string> {
  await screen.findByRole("region", { name: "Delivery" });
  return document.querySelector(".os-cluster-mesh-verdict")?.textContent ?? "";
}

function strip(row: Record<string, unknown>) {
  // An absent lastHeardAt must be ABSENT, not the string "undefined".
  const mesh = row.mesh as Record<string, unknown> | undefined;
  if (mesh && mesh.lastHeardAt === undefined) delete mesh.lastHeardAt;
  return row;
}

beforeEach(() => {
  for (const r of [BFF, AGENT, DEAF_EDGE, IDENTITY, UNREPORTED]) strip(r);
});

describe("the list", () => {
  it("groups the running nodes by type and says each one's state in words", async () => {
    mount(fakeConnection({ clusterNodes: [BFF, AGENT, DEAF_EDGE, IDENTITY, UNREPORTED] }));
    const list = await screen.findByRole("list", { name: "Running nodes" });
    expect(within(list).getByText("BFF")).toBeTruthy();
    expect(within(list).getByText("Agent")).toBeTruthy();
    expect(within(list).getByText("Edge")).toBeTruthy();
    expect(within(list).getByText("Identity")).toBeTruthy();

    const row = (id: string) => within(list).getByRole("button", { name: new RegExp(`^Open ${id},`) });
    expect(row("bff-a").textContent).toContain("Hearing");
    expect(row("edge-a").textContent).toContain("Not hearing");
    // Identity's zero is the design, not an island.
    expect(row("identity-a").textContent).toContain("Sends only");
    expect(row("identity-a").textContent).not.toContain("Not hearing");
    expect(row("mcp-a").textContent).toContain("Not reported");
  });

  it("names the node that needs a look, first, in the sentence above the list", async () => {
    mount(fakeConnection({ clusterNodes: [BFF, AGENT, DEAF_EDGE, IDENTITY] }));
    expect(await verdict()).toBe("edge-a is not hearing the cluster.");
  });

  it("opens a node the sentence names, straight from the sentence", async () => {
    mount(fakeConnection({ clusterNodes: [BFF, AGENT, DEAF_EDGE, IDENTITY] }));
    await verdict();
    // Named exactly: the row's own name goes on to say the node's state.
    await click(screen.getByRole("button", { name: "Open edge-a" }));
    expect(screen.getByText(/This node has heard nothing in the 60 minutes since it started/)).toBeTruthy();
  });

  it("says nothing needs a look only when nothing does", async () => {
    mount(fakeConnection({ clusterNodes: [BFF, AGENT, IDENTITY] }));
    expect(await screen.findByText("Every node that takes events is hearing the cluster.")).toBeTruthy();
  });

  it("leaves out stopped and silent rows, and counts them rather than hiding them", async () => {
    const stopped = clusterNodeRow({ id: "bff-old", health: "stopped" });
    const silent = clusterNodeRow({ id: "agent-old", nodeType: "agent", lastSeen: minutesAgo(40) });
    mount(fakeConnection({ clusterNodes: [BFF, stopped, silent] }));
    const list = await screen.findByRole("list", { name: "Running nodes" });
    expect(within(list).queryByText("bff-old")).toBeNull();
    expect(within(list).queryByText("agent-old")).toBeNull();
    expect(await screen.findByText(/2 nodes have stopped or not written a row for five minutes/)).toBeTruthy();
  });

  it("follows the live feed: a deaf node that starts hearing turns over without a reload", async () => {
    const connection = fakeConnection({ clusterNodes: [BFF, DEAF_EDGE] });
    mount(connection);
    expect(await verdict()).toBe("edge-a is not hearing the cluster.");
    await act(async () => {
      connection.subscriptions.emit(
        "v1:cluster:node",
        clusterNodeRow({
          id: "edge-a",
          nodeType: "edge",
          mesh: { since: minutesAgo(61), links: [{ node: "bff-a", type: "bff", via: "dialed" }], heard: 12 },
        }),
      );
    });
    expect(await screen.findByText("Every node that takes events is hearing the cluster.")).toBeTruthy();
  });

  it("says when no node has written its row", async () => {
    mount(fakeConnection({ clusterNodes: [] }));
    expect(await screen.findByText("No node has written its row in the last five minutes.")).toBeTruthy();
  });
});

describe("a node's page", () => {
  it("opens from its row, dates its report, and splits its links by who opened each stream", async () => {
    mount(fakeConnection({ clusterNodes: [BFF, AGENT, DEAF_EDGE, IDENTITY] }));
    const list = await screen.findByRole("list", { name: "Running nodes" });
    await click(within(list).getByRole("button", { name: /^Open bff-a,/ }));

    expect(screen.getByText("This node hears the cluster.")).toBeTruthy();
    expect(screen.getByText(/Report written/)).toBeTruthy();
    expect(fact("Heard")).toBe("1,204 events");
    expect(fact("Copies dropped")).toBe("0 copies");

    const opened = screen.getByText("Streams it opened").parentElement as HTMLElement;
    const accepted = screen.getByText("Streams opened to it").parentElement as HTMLElement;
    // Linked both ways: on BOTH sides, because two streams exist.
    expect(within(opened).getByRole("button", { name: /^Open agent-a,/ })).toBeTruthy();
    expect(within(accepted).getByRole("button", { name: /^Open agent-a,/ })).toBeTruthy();
    expect(within(opened).getByRole("button", { name: /^Open identity-a,/ })).toBeTruthy();
    expect(within(accepted).getByRole("button", { name: /^Open edge-a, Edge, not hearing/ })).toBeTruthy();
  });

  it("walks from peer to peer and comes back to the list", async () => {
    mount(fakeConnection({ clusterNodes: [BFF, AGENT, DEAF_EDGE] }));
    const list = await screen.findByRole("list", { name: "Running nodes" });
    await click(within(list).getByRole("button", { name: /^Open bff-a,/ }));
    await click(screen.getAllByRole("button", { name: /^Open edge-a,/ })[0] as Element);
    expect(screen.getByText(/This node has heard nothing in the 60 minutes since it started/)).toBeTruthy();
    await click(screen.getByRole("button", { name: /^Open bff-a,/ }));
    expect(screen.getByText("This node hears the cluster.")).toBeTruthy();
  });

  it("draws a peer the page does not list as text, not as a control that opens nothing", async () => {
    mount(fakeConnection({ clusterNodes: [BFF, AGENT] }));
    const list = await screen.findByRole("list", { name: "Running nodes" });
    await click(within(list).getByRole("button", { name: /^Open agent-a,/ }));
    expect(screen.getByText("workbench-gone")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /workbench-gone/ })).toBeNull();
    expect(screen.getByText("Workbench, not listed")).toBeTruthy();
  });

  it("shows a node that has not reported as absent figures, never zeros", async () => {
    mount(fakeConnection({ clusterNodes: [BFF, UNREPORTED] }));
    const list = await screen.findByRole("list", { name: "Running nodes" });
    await click(within(list).getByRole("button", { name: /^Open mcp-a,/ }));
    expect(screen.getByText(/has not reported what it hears/)).toBeTruthy();
    expect(screen.getByText("This node has not reported its links.")).toBeTruthy();
    // ABSENT, the kit's em dash -- a zero here would say "looked, and heard
    // nothing", which is what a deaf node reports.
    expect(fact("Heard")).toBe("\u2014");
    expect(fact("Copies dropped")).toBe("\u2014");
    expect(fact("Last heard")).toBe("\u2014");
    // The row was written, but not by a report -- so the date is the row's.
    expect(screen.getByText(/^Row last written/)).toBeTruthy();
    expect(screen.queryByText(/Report written/)).toBeNull();
  });

  it("calls identity's figures what it sends, since it hears nothing by design", async () => {
    mount(fakeConnection({ clusterNodes: [BFF, IDENTITY] }));
    const list = await screen.findByRole("list", { name: "Running nodes" });
    await click(within(list).getByRole("button", { name: /^Open identity-a,/ }));
    expect(screen.getByText("What it sends")).toBeTruthy();
    expect(screen.queryByText("What it hears")).toBeNull();
    expect(screen.queryByText("Heard")).toBeNull();
    expect(fact("Put on the mesh")).toBe("7 events");
  });

  it("opens straight onto a node named by the window's intent, and consumes it", async () => {
    const consume = vi.fn();
    mount(fakeConnection({ clusterNodes: [BFF, DEAF_EDGE] }), {
      intent: { id: "intent-1", payload: { nodeId: "edge-a" } },
      consumeIntent: consume,
    });
    expect(await screen.findByText(/This node has heard nothing/)).toBeTruthy();
    expect(consume).toHaveBeenCalledWith("intent-1");
  });
});
