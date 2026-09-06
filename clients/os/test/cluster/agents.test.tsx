import { act, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { AgentsSection } = await import("../../src/apps/cluster/agents/AgentsSection");
const { agentRow, fakeConnection, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

const AGENT_CONCEPT = "v1:agents:agent";

function mount(connection: Conn, showInactive = false) {
  h.connection = connection;
  return render(withSession(<AgentsSection showInactive={showInactive} />));
}

const ASSISTANT = agentRow({
  id: "v1:agents:agent:a1",
  name: "General Assistant",
  kind: "assistant",
  role: "Front desk",
  capabilities: { tools: ["workbenchHost"] },
  createdAt: "2026-08-01T00:00:00Z",
});

beforeEach(() => {
  h.connection = null;
});

describe("the agents list", () => {
  it("seeds from the cluster and lists what it runs", async () => {
    // THE REGRESSION GUARD for an un-retained collection: one that is
    // subscribed but never retained never seeds, so this list renders
    // "Loading from the cluster" forever with nothing logged and nothing
    // thrown. The assertion is that a row is on screen.
    const connection = fakeConnection({ activeAgents: [ASSISTANT] });
    mount(connection);
    expect(await screen.findByText("General Assistant")).toBeTruthy();
    expect(connection.query.activeAgents).toHaveBeenCalled();
    expect(connection.query.allAgents).not.toHaveBeenCalled();
  });

  it("reads a different query when inactive agents are shown", async () => {
    const connection = fakeConnection({ allAgents: [ASSISTANT] });
    mount(connection, true);
    await screen.findByText("General Assistant");
    expect(connection.query.allAgents).toHaveBeenCalled();
    expect(connection.query.activeAgents).not.toHaveBeenCalled();
  });

  it("cues a real change and stays SILENT on a re-stamp", async () => {
    // The two halves of a good arrival cue, and they pull against each other:
    // it has to fire when something happened, and it has to not fire the rest
    // of the time.
    //
    // `createdAt` IS this concept's heartbeat. An agent row is append-only
    // and the SeedMaterializer re-writes every system agent on EVERY boot, so
    // naming it in the fingerprint would ring the list every time a replica
    // restarted -- the standing-badge failure the cue exists to avoid.
    const connection = fakeConnection({ activeAgents: [ASSISTANT] });
    mount(connection);
    await screen.findByText("General Assistant");

    const rowOf = (name: string) =>
      screen.getByText(name).closest(".os-livelist-row") as HTMLElement;

    // A re-stamp: nothing but createdAt moves.
    await act(async () => {
      connection.subscriptions.emit(
        AGENT_CONCEPT,
        agentRow({
          id: "v1:agents:agent:a1",
          name: "General Assistant",
          kind: "assistant",
          role: "Front desk",
          capabilities: { tools: ["workbenchHost"] },
          createdAt: "2026-09-06T09:00:00Z",
        }),
      );
    });
    expect(rowOf("General Assistant").getAttribute("data-arrival")).toBeNull();

    // A rename: news.
    await act(async () => {
      connection.subscriptions.emit(
        AGENT_CONCEPT,
        agentRow({
          id: "v1:agents:agent:a1",
          name: "Concierge",
          kind: "assistant",
          role: "Front desk",
          capabilities: { tools: ["workbenchHost"] },
          createdAt: "2026-09-06T09:00:01Z",
        }),
      );
    });
    expect(rowOf("Concierge").getAttribute("data-arrival")).toBe("updated");
  });

  it("cues a capability change too, because that is what an agent may DO", async () => {
    const connection = fakeConnection({ activeAgents: [ASSISTANT] });
    mount(connection);
    await screen.findByText("General Assistant");

    await act(async () => {
      connection.subscriptions.emit(
        AGENT_CONCEPT,
        agentRow({
          id: "v1:agents:agent:a1",
          name: "General Assistant",
          kind: "assistant",
          role: "Front desk",
          capabilities: { tools: ["workbenchHost", "workerHost"] },
        }),
      );
    });
    const row = screen.getByText("General Assistant").closest(".os-livelist-row") as HTMLElement;
    expect(row.getAttribute("data-arrival")).toBe("updated");
  });
});

describe("standing authorizations", () => {
  it("says out loud that the list is the caller's own and nobody else's", async () => {
    // `v1:agents:agentAuthorization` declares @rowAuthz(owner="userId") and
    // the query filters userId==actor.userId, so this is self-only even for a
    // cluster owner. A panel of these under a cluster-wide heading would
    // claim to show something it cannot.
    mount(fakeConnection({ activeAgents: [ASSISTANT], agentAuthorizationsForSelf: [] }));
    expect(
      await screen.findByText(/These are YOUR grants and nobody else's/),
    ).toBeTruthy();
    expect(screen.getByText(/even a cluster owner reads only their own/)).toBeTruthy();
  });

  it("keeps 'no cap' apart from a cap of zero", async () => {
    mount(
      fakeConnection({
        activeAgents: [ASSISTANT],
        agentAuthorizationsForSelf: [
          {
            id: "v1:agents:agentAuthorization:g1",
            agentId: "v1:agents:agent:a1",
            userId: "v1:identity:user:me",
            planKind: "*",
            spaceScope: "*",
            computerUseScope: "observe",
            active: true,
          },
        ],
      }),
    );
    const grant = (await screen.findByText("v1:agents:agent:a1")).closest(
      ".os-cluster-grant",
    ) as HTMLElement;
    expect(within(grant).getByText("your default budget")).toBeTruthy();
    expect(within(grant).getByText(/computer use: observe/)).toBeTruthy();
  });
});
