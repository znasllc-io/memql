// Cluster operations, ON THE DEPLOYMENTS VIEW (memql#4193, memql#4209,
// memql#4264): courtesy gating, rollback staying owner-only, and repair --
// owner-only, type-to-confirm, riding the deploy-control wire like every other
// action, with the engine's audit id shown back.
//
// These used to render /cluster-ops. That page is gone: it carried the same
// four verbs as this view's Ship band and the two DISAGREED about how
// dangerous they are -- the view fired deploy and roll back on a single click,
// the page confirmed every one. An operator's protection depended on which
// door they walked through. The careful set won, and it lives here now.
//
// The assertions that moved with it are the ones about SAFETY, not layout:
// what a reader is offered, what an admin is offered, that repair is armed
// only by typing the word, and that a repair record is never a rollback
// target.

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
  domain: "",
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
    // The VIEW resolves its concept through the registry (the retired page did
    // not), so the deployment concept has to be here or the view renders its
    // honest "this cluster publishes no such concept" state instead.
    listConcepts: vi.fn(async () => [
      {
        id: "v1:cluster:deployment",
        version: "v1",
        domain: "cluster",
        entity: "deployment",
        type: "concept",
        description: "An append-only deployment record",
        displayCard: {
          primary: "engineVersion",
          secondary: "deploymentId",
          tertiary: "notes",
          status: "status",
        },
      },
    ]),
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

function renderOps(
  role: string,
  sent: Array<Record<string, unknown>> = [],
  at = "/views/deployments",
) {
  const dial = vi.fn(async () => fakeConnection(role, sent)) as unknown as typeof Connection.dial;
  render(
    <MemoryRouter initialEntries={[at]}>
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

describe("cluster operations, on the Deployments view", () => {
  it("offers a reader none of the verbs", async () => {
    renderOps("reader");
    // Scoped to the page HEADING. The nav rail carries the word too, and
      // since the page header renders before its rows land (epic memql#4661)
      // a bare getByText now sees both -- which is a query that was always
      // ambiguous and happened to resolve on the rail first.
      await waitFor(() =>
        expect(screen.getByRole("heading", { level: 1, name: "Deployments" })).toBeTruthy(),
      );
    // Absent, not present-and-refused. The gate is the cluster's; hiding the
    // control is the courtesy half (src/deploy/useDeployConsole.ts).
    for (const verb of [
      "Cut a patch version",
      "Deploy the selected version",
      "Roll back to the selected version",
      "Repair this installation",
    ]) {
      expect(screen.queryByRole("button", { name: verb })).toBeNull();
    }
  });

  it("lets an admin ship but never roll back or repair", async () => {
    renderOps("admin");
    await waitFor(() => expect(screen.getByRole("button", { name: "Cut a patch version" })).toBeTruthy());
    // The two owner-only verbs do not render at all.
    expect(screen.queryByRole("button", { name: "Roll back to the selected version" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Repair this installation" })).toBeNull();
  });

  it("arms repair only by typing the word, and shows the audit id back", async () => {
    const sent = renderOps("owner");
    await waitFor(() => expect(screen.getByRole("button", { name: "Repair this installation" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Repair this installation" }));
    await waitFor(() => expect(screen.getByRole("dialog")).toBeTruthy());

    const dialog = screen.getByRole("dialog");
    expect(dialog.textContent).toContain("Owner-only");
    expect(dialog.textContent).toContain("Nothing changes version");

    const confirm = within(dialog).getByRole("button", { name: "Repair" }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true);
    fireEvent.change(within(dialog).getByPlaceholderText("repair"), { target: { value: "repai" } });
    expect(confirm.disabled).toBe(true);
    // Nothing has reached the wire on a near-miss.
    expect(
      sent.some((m) => "deployControl" in m && "repair" in (m["deployControl"] as Record<string, unknown>)),
    ).toBe(false);

    fireEvent.change(within(dialog).getByPlaceholderText("repair"), { target: { value: "repair" } });
    expect(confirm.disabled).toBe(false);
    fireEvent.click(confirm);

    // The bridged request, and nothing shelled or improvised on the client.
    await waitFor(() =>
      expect(
        sent.some((m) => "deployControl" in m && "repair" in (m["deployControl"] as Record<string, unknown>)),
      ).toBe(true),
    );
    const outcome = () =>
      screen.getAllByRole("status").find((el) => el.textContent?.includes("repair kicked off"));
    await waitFor(() => expect(outcome()).toBeTruthy());
    expect(outcome()?.textContent).toContain("aud-repair-1");
  });

  // THE SAFETY PROPERTY THAT HAD TO SURVIVE THE MERGE (memql#4264).
  //
  // The retired timeline enforced it per row; this view selects a row and acts
  // on the selection, so the same rule has to gate the button instead. A repair
  // pins no version -- it re-converges the cluster onto what is already
  // committed -- so "roll back to this repair" names nothing to roll to.
  it("never offers a repair record as a rollback target", async () => {
    renderOps("owner", [], "/views/deployments/rows/v1:cluster:deployment:rep-1");
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Roll back to the selected version" })).toBeTruthy(),
    );
    const button = screen.getByRole("button", {
      name: "Roll back to the selected version",
    }) as HTMLButtonElement;
    expect(button.disabled).toBe(true);
  });

  it("rolls back to a real deployment through a dialog that states the gate", async () => {
    const sent = renderOps("owner", [], "/views/deployments/rows/v1:cluster:deployment:dep-1");
    const button = () =>
      screen.getByRole("button", { name: "Roll back to the selected version" }) as HTMLButtonElement;
    await waitFor(() => expect(button().disabled).toBe(false));

    fireEvent.click(button());
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
