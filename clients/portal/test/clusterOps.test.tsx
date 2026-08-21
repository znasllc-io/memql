// Cluster operations (memql#4193): courtesy gating, the timeline rendered
// from the graph's own deployment records, rollback staying owner-only, and
// repair (memql#4209) -- owner-only, type-to-confirm, riding the
// deploy-control wire like every other action, with the engine's audit id
// shown back and the repair record marked on the timeline.

import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { Connection} from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { asQueryClient } from "./support/queryFake";

const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
};

const DEPLOYMENT_ROWS = [
  {
    id: "v1:cluster:deployment:dep-2",
    concept: "v1:cluster:deployment",
    createdAt: "2026-08-20T21:00:00Z",
    payload: { deploymentId: "dep-2", status: "succeeded", engineVersion: "v0.19.1" },
  },
  {
    // A repair record: the same concept, marked by the engine's "repair:"
    // note prefix (component/deploycontrol/repair.go). Succeeded and not the
    // newest entry, so the only thing keeping it out of the rollback targets
    // is its kind -- it re-synced the version already in force.
    id: "v1:cluster:deployment:rep-1",
    concept: "v1:cluster:deployment",
    createdAt: "2026-08-20T10:00:00Z",
    payload: {
      deploymentId: "rep-1",
      status: "succeeded",
      engineVersion: "v0.19.0",
      notes: "repair: re-sync of ArgoCD application memql from the committed overlay (version v0.19.0), initiated by op@example.test",
    },
  },
  {
    id: "v1:cluster:deployment:dep-1",
    concept: "v1:cluster:deployment",
    createdAt: "2026-08-19T21:00:00Z",
    payload: { deploymentId: "dep-1", status: "succeeded", engineVersion: "v0.19.0" },
  },
];

function fakeConnection(role: string, sent: Array<Record<string, unknown>>): Connection {
  const query = asQueryClient({
    listConcepts: vi.fn(async () => []),
    getMyAccess: vi.fn(async () => ({
      userId: "user-1",
      primaryEmail: "op@example.test",
      clusterRole: role,
    })),
    executeNamed: vi.fn(async (_name: string, call: string) => {
      sent.push({ executeNamed: call });
      const rows = call.includes("v1:cluster:deployment,") || call.includes("v1:cluster:deployment ")
        ? DEPLOYMENT_ROWS
        : call.includes("deploymentNodeSpec")
          ? []
          : [];
      return {
        rawNodes: () => rows,
        meta: () => ({ cursor: "" }),
      };
    }),
  });
  const dispatcher = {
    send: vi.fn(),
    addEventListener: vi.fn(() => () => {}),
    registerStream: vi.fn(() => () => {}),
    sendAndWait: vi.fn(async (msg: Record<string, unknown>) => {
      sent.push(msg);
      if ("deployControl" in msg) {
        const dc = msg["deployControl"] as Record<string, unknown>;
        if ("repair" in dc) {
          // The engine's ack: ok is "accepted + kicked off", the record id is
          // what the page polls, and the success event's id comes back on
          // the action (memql#4209).
          return {
            correlateTo: "x",
            deployControlResult: {
              requestId: dc["requestId"],
              ok: true,
              action: {
                ok: true,
                message: "repair kicked off: ArgoCD application memql is re-syncing from the committed overlay",
                auditEventId: "aud-repair-1",
                details: { deploymentId: "rep-2", status: "in_progress", async: "true" },
              },
            },
          };
        }
        return {
          correlateTo: "x",
          deployControlResult: {
            getDeploymentStatus: {
              version: "v0.19.1",
              engineVersion: "v0.19.1",
              syncStatus: "Synced",
              healthStatus: "Healthy",
              components: [],
            },
          },
        };
      }
      return { correlateTo: "x" };
    }),
  };
  return {
    nodeId: "bff-test",
    serverVersion: "0.0.0-test",
    query,
    dispatcher,
    subscriptions: { subscribeGraph: vi.fn(() => () => {}) },
    close: vi.fn(),
    done: vi.fn(() => new Promise<void>(() => {})),
  } as unknown as Connection;
}

function renderOps(role: string, sent: Array<Record<string, unknown>> = []) {
  const dial = vi.fn(async () => fakeConnection(role, sent)) as unknown as typeof Connection.dial;
  render(
    <MemoryRouter initialEntries={["/cluster-ops"]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("cluster-ops tests must make no identity calls");
        }}
        storage={null}
        navigate={() => {}}
        redirectUri="https://api.example.com/portal/auth/callback"
      >
        <ClusterProvider dial={dial}>
          <AppRoutes />
        </ClusterProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
  return sent;
}

describe("cluster operations", () => {
  it("offers nothing below the operator roles", async () => {
    renderOps("reader");
    await waitFor(() => expect(screen.getByText("This is an operator surface")).toBeTruthy());
  });

  it("renders the timeline from the deployment records and keeps repair owner-only", async () => {
    renderOps("admin");
    await waitFor(() => expect(screen.getAllByText("v0.19.0").length).toBeGreaterThan(0));
    // The repair record is marked as one on the timeline.
    expect(screen.getByText("repair")).toBeTruthy();
    // Admin can ship but neither roll back nor repair: the two owner-only
    // controls never render, and the band says why instead of faking one.
    expect(screen.queryByRole("button", { name: /Roll back to this/ })).toBeNull();
    expect(screen.queryByRole("button", { name: "Repair this installation" })).toBeNull();
    expect(screen.getByText(/Owner-only: the control is offered to the cluster owner/)).toBeTruthy();
    // The gap sentence is gone with the gap.
    expect(screen.queryByText(/no repair verb/)).toBeNull();
  });

  it("owner repairs through a type-to-confirm dialog and is shown the audit id", async () => {
    const sent = renderOps("owner");
    await waitFor(() => expect(screen.getAllByText("v0.19.0").length).toBeGreaterThan(0));
    fireEvent.click(screen.getByRole("button", { name: "Repair this installation" }));
    await waitFor(() => expect(screen.getByRole("dialog")).toBeTruthy());
    const dialog = screen.getByRole("dialog");
    expect(dialog.textContent).toContain("Owner-only");
    expect(dialog.textContent).toContain("Nothing changes version");

    // Armed only by the phrase: the verb is disabled until "repair" is typed.
    const confirm = within(dialog).getByRole("button", { name: "Repair" }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true);
    fireEvent.change(within(dialog).getByPlaceholderText("repair"), { target: { value: "repai" } });
    expect(confirm.disabled).toBe(true);
    expect(sent.some((m) => "deployControl" in m && "repair" in (m["deployControl"] as Record<string, unknown>))).toBe(false);
    fireEvent.change(within(dialog).getByPlaceholderText("repair"), { target: { value: "repair" } });
    expect(confirm.disabled).toBe(false);
    fireEvent.click(confirm);

    // The wire carries the bridged repair request, and nothing else is
    // shelled or improvised on the client.
    await waitFor(() =>
      expect(
        sent.some((m) => "deployControl" in m && "repair" in (m["deployControl"] as Record<string, unknown>)),
      ).toBe(true),
    );
    // The ack and the success event's id are shown back, exactly as a
    // rollback refusal's id is. (The shell carries its own role="status"
    // element, so the outcome line is found by its text.)
    const outcome = () =>
      screen.getAllByRole("status").find((el) => el.textContent?.includes("repair kicked off"));
    await waitFor(() => expect(outcome()).toBeTruthy());
    expect(outcome()?.textContent).toContain("aud-repair-1");
  });

  it("a repair record is never offered as a rollback target", async () => {
    renderOps("owner");
    await waitFor(() => expect(screen.getAllByText("v0.19.0").length).toBeGreaterThan(0));
    // Two succeeded records sit below the newest entry (rep-1 and dep-1);
    // only the deploy record is a rollback target.
    expect(screen.getAllByRole("button", { name: "Roll back to this" })).toHaveLength(1);
  });

  it("owner rolls back through a dialog that states the gate", async () => {
    const sent = renderOps("owner");
    await waitFor(() => expect(screen.getAllByText("v0.19.0").length).toBeGreaterThan(0));
    fireEvent.click(screen.getByRole("button", { name: "Roll back to this" }));
    await waitFor(() => expect(screen.getByRole("dialog")).toBeTruthy());
    expect(screen.getByRole("dialog").textContent).toContain("Owner-only");
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Roll back" }));
    await waitFor(() =>
      expect(
        sent.some(
          (m) =>
            "deployControl" in m &&
            "rollbackDeployment" in (m["deployControl"] as Record<string, unknown>),
        ),
      ).toBe(true),
    );
  });
});
