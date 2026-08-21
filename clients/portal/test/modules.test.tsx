// The Modules surface (memql#4191): owner/admin courtesy gating, the
// browser rendering the engine's inventory verbatim, the pack flip going
// through a ConfirmDialog that states restart-required honestly, and the
// structural secret rule -- a secret env var row never renders a value,
// because the engine never sent one and the page must not imply otherwise.

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

const INVENTORY_REPLY = {
  modulesListResult: {
    modules: [
      {
        kind: "pack",
        name: "harness",
        description: "The agent harness, as a pack.",
        state: "enabled",
        stateDetail: "",
        scope: "cluster",
      },
      {
        kind: "integration",
        name: "email",
        description: "Outbound mail.",
        state: "active",
        stateDetail: "",
        scope: "node",
      },
    ],
    reportingNodeId: "bff-test",
    reportingNodeType: "bff",
  },
};

const DETAIL_REPLY = {
  moduleDetailResult: {
    module: {
      kind: "pack",
      name: "harness",
      description: "The agent harness, as a pack.",
      state: "enabled",
      stateDetail: "",
      scope: "cluster",
    },
    envVars: [
      {
        name: "MEMQL_AI_OPENAI_API_KEY",
        description: "Vendor key.",
        secret: true,
        set: true,
        value: "",
      },
      {
        name: "MEMQL_OBSERVE_LEVEL",
        description: "Default observe level.",
        secret: false,
        set: true,
        value: "count",
      },
    ],
    reportingNodeId: "bff-test",
    reportingNodeType: "bff",
  },
};

// The hooks build ModulesClient over the connection's dispatcher, so the
// fake lives at the dispatcher boundary: reply by payload key, record sends.
function fakeDispatcher(sent: Array<Record<string, unknown>>) {
  return {
    send: vi.fn(),
    addEventListener: vi.fn(() => () => {}),
    registerStream: vi.fn(() => () => {}),
    sendAndWait: vi.fn(async (msg: Record<string, unknown>) => {
      sent.push(msg);
      if ("modulesList" in msg) return { correlateTo: "x", ...INVENTORY_REPLY };
      if ("moduleDetail" in msg) return { correlateTo: "x", ...DETAIL_REPLY };
      if ("setPackEnabled" in msg) {
        return {
          correlateTo: "x",
          setPackEnabledResult: {
            packDomain: "harness",
            priorEnabled: true,
            enabled: false,
            restartRequired: true,
          },
        };
      }
      return { correlateTo: "x" };
    }),
  };
}

function fakeConnection(role: string, sent: Array<Record<string, unknown>>): Connection {
  const query = asQueryClient({
    listConcepts: vi.fn(async () => []),
    getMyAccess: vi.fn(async () => ({
      userId: "user-1",
      primaryEmail: "op@example.test",
      clusterRole: role,
    })),
  });
  return {
    nodeId: "bff-test",
    serverVersion: "0.0.0-test",
    query,
    dispatcher: fakeDispatcher(sent),
    close: vi.fn(),
    done: vi.fn(() => new Promise<void>(() => {})),
  } as unknown as Connection;
}

function renderModules(role: string, path: string, sent: Array<Record<string, unknown>> = []) {
  const dial = vi.fn(async () => fakeConnection(role, sent)) as unknown as typeof Connection.dial;
  render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("modules tests must make no identity calls");
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

describe("the modules browser", () => {
  it("hides everything below admin and issues no inventory read", async () => {
    const sent = renderModules("reader", "/modules");
    await waitFor(() =>
      expect(screen.getByText("This is an owner and admin surface")).toBeTruthy(),
    );
    expect(sent.some((m) => "modulesList" in m)).toBe(false);
    // The rail declines to advertise the door either.
    expect(screen.queryByRole("link", { name: /Modules/ })).toBeNull();
  });

  it("groups the inventory by kind and names the answering node", async () => {
    renderModules("admin", "/modules");
    await waitFor(() => expect(screen.getByText("harness")).toBeTruthy());
    expect(screen.getByRole("heading", { name: "Packs" })).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Integrations" })).toBeTruthy();
    expect(screen.getAllByText(/bff-test/).length).toBeGreaterThan(0);
    expect(screen.getByRole("link", { name: /Modules/ })).toBeTruthy();
  });

  it("renders env vars with the secret rule intact and flips a pack through the dialog", async () => {
    const sent = renderModules("owner", "/modules/pack/harness");
    await waitFor(() => expect(screen.getByText("MEMQL_OBSERVE_LEVEL")).toBeTruthy());

    // Non-secret shows its value; the secret shows the rule, never a value.
    expect(screen.getByText("count")).toBeTruthy();
    expect(screen.getByText(/secret — the value never leaves the engine/)).toBeTruthy();
    const secretRow = screen.getByText("MEMQL_AI_OPENAI_API_KEY").closest("li");
    expect(secretRow).toBeTruthy();
    expect(secretRow!.textContent).not.toContain("count");

    // The flip: reason recorded, dialog states restart-required, wire carries all three.
    fireEvent.change(screen.getByPlaceholderText("Why the change"), {
      target: { value: "maintenance window" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Disable this pack" }));
    await waitFor(() => expect(screen.getByRole("dialog")).toBeTruthy());
    expect(screen.getByRole("dialog").textContent).toContain("restart");
    fireEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", { name: "Disable the pack" }),
    );

    await waitFor(() => expect(sent.some((m) => "setPackEnabled" in m)).toBe(true));
    const flip = sent.find((m) => "setPackEnabled" in m) as {
      setPackEnabled: { packDomain: string; enabled: boolean; reason: string };
    };
    expect(flip.setPackEnabled.packDomain).toBe("harness");
    expect(flip.setPackEnabled.enabled).toBe(false);
    expect(flip.setPackEnabled.reason).toBe("maintenance window");

    // The reply's honesty lands on screen: saved now, applies on restart.
    await waitFor(() =>
      expect(
        screen.getAllByRole("status").some((el) => (el.textContent ?? "").includes("restart")),
      ).toBe(true),
    );
  });

  it("keeps the flip owner-only as a courtesy", async () => {
    renderModules("admin", "/modules/pack/harness");
    await waitFor(() => expect(screen.getByText("MEMQL_OBSERVE_LEVEL")).toBeTruthy());
    expect(screen.queryByRole("button", { name: /Disable this pack/ })).toBeNull();
    expect(screen.getByText(/owner-only action/)).toBeTruthy();
  });
});
