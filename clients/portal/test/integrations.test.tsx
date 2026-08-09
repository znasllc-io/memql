// The integration surface (memql#3323), end to end against a fake cluster.
//
// WHAT THIS FILE OWNS. integrations/email/status_test.go proves the SERVER
// never puts a credential in the reply. This file proves the other half, which
// a Go test cannot see: that the page never renders one even when handed a
// reply that contains one, that "configured" and "healthy" arrive on screen as
// separate facts, and that the live probe is something an operator asks for
// rather than something a page navigation triggers.
//
// The last of those is worth a test rather than a comment: a probe reaches a
// third party, so a page that probed on mount would send an operator's mail
// provider a request every time they clicked back.

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import {
  Result,
  type AccessSummary,
  type Concept,
  type Connection,
  type QueryClient,
  type Role,
} from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";

const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
};

// A recognisable value planted where a credential value would be if the server
// ever leaked one -- and, in the hostile case below, actually put there.
const PLANTED_SECRET = "PLANTED-CLIENT-SECRET-9f3c11";

function statusReply(options: { probed: boolean; hostile?: boolean }) {
  const credential: Record<string, unknown> = {
    name: "clientSecret",
    present: true,
    source: "globalSecret",
    envVar: "MEMQL_EMAIL_AZURE_CLIENT_SECRET",
    purpose: "Client-credentials secret for the app registration.",
    rotate: "make secret-set NAME=MEMQL_EMAIL_AZURE_CLIENT_SECRET VALUE=<new value> SCOPE=global",
  };
  if (options.hostile) {
    // A field the contract does not have. If a future engine change (or a
    // compromised node) starts returning one, the page must still not paint
    // it.
    credential["value"] = PLANTED_SECRET;
  }

  return {
    checkedAt: "2026-08-08T10:00:00Z",
    probed: options.probed,
    integrations: [
      {
        name: "email",
        registered: true,
        capabilities: ["sendEmail", "status"],
        configured: "no",
        health: "degraded",
        mode: "log",
        probed: options.probed,
        detail:
          "No sender is configured, so the integration is running in log-only mode: every send succeeds, writes a line to the node log, and delivers nothing.",
        settings: [
          {
            name: "senderAddress",
            value: "",
            source: "unset",
            envVar: "MEMQL_EMAIL_SENDER",
            purpose: "The mailbox mail is sent AS.",
            editable: true,
          },
          {
            name: "tenantId",
            value: "tenant-abc",
            source: "env",
            envVar: "MEMQL_EMAIL_AZURE_TENANT_ID",
            purpose: "Entra tenant the app registration lives in.",
            editable: false,
          },
        ],
        credentials: [credential],
      },
      {
        name: "knowledge",
        registered: true,
        capabilities: [],
        configured: "unknown",
        health: "unknown",
        mode: "",
        probed: false,
        detail:
          "Registered on this node. This integration publishes no configuration self-report, so whether its credentials resolved is not knowable from here.",
        settings: [],
        credentials: [],
      },
    ],
  };
}

interface Harness {
  role: Role;
  hostile: boolean;
}

function renderIntegrations({ role = "owner", hostile = false }: Partial<Harness> = {}) {
  const access: AccessSummary = {
    requestId: "r1",
    userId: "user-1",
    primaryEmail: "ada@example.com",
    clusterRole: role,
  };

  const calls: string[] = [];
  const executeNamed = vi.fn(async (name: string, call: string) => {
    calls.push(call);
    if (name === "integrationStatus") {
      const probed = /probe:\s*true/.test(call);
      return new Result({
        data: [{ integrationStatus: { id: "integrationStatus", payload: statusReply({ probed, hostile }) } }],
      });
    }
    return new Result({ bundle: { nodes: [] }, meta: { cursor: "" } });
  });

  const query = {
    listConcepts: vi.fn(async (): Promise<Concept[]> => []),
    getMyAccess: vi.fn(async () => access),
    executeNamed,
  } as unknown as QueryClient;

  const dial = vi.fn(
    async () =>
      ({
        nodeId: "bff-test",
        serverVersion: "0.0.0-test",
        query,
        dispatcher: { sendAndWait: vi.fn(async () => ({})) },
        close: vi.fn(),
        done: vi.fn(() => new Promise<void>(() => {})),
      }) as unknown as Connection,
  ) as unknown as typeof Connection.dial;

  const utils = render(
    <MemoryRouter initialEntries={["/integrations"]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("the integration tests must make no identity calls");
        }}
        storage={null}
        navigate={() => {}}
        redirectUri="https://cockpit.example.com/portal/auth/callback"
      >
        <ClusterProvider dial={dial}>
          <AppRoutes />
        </ClusterProvider>
      </AuthProvider>
    </MemoryRouter>,
  );

  return { ...utils, calls, executeNamed };
}

describe("the integrations registry", () => {
  it("lists every registered plug-in through the shared element library", async () => {
    const { container } = renderIntegrations();

    await waitFor(() => expect(screen.getAllByText("email").length).toBeGreaterThan(0));
    expect(screen.getAllByText("knowledge").length).toBeGreaterThan(0);

    // Asserting on view-kit's own class prefix rather than on the text is what
    // makes this a test about COMPOSITION: text could be printed by anything,
    // a vk- class could only have come from the element library.
    const classes = new Set<string>();
    for (const node of container.querySelectorAll("[class]")) {
      for (const cls of (node.getAttribute("class") ?? "").split(/\s+/)) {
        if (cls.startsWith("vk-")) classes.add(cls);
      }
    }
    expect(classes.size).toBeGreaterThan(0);
  });

  it("reports an integration that publishes no self-report as unknown, not as unconfigured", async () => {
    renderIntegrations();
    await waitFor(() => expect(screen.getAllByText("knowledge").length).toBeGreaterThan(0));

    // "unknown" has to reach the screen. Rendering it as "no" would tell an
    // operator that a perfectly healthy integration is broken.
    expect(screen.getAllByText("unknown").length).toBeGreaterThan(0);
  });

  it("says log-only mode delivers nothing instead of showing it as working", async () => {
    renderIntegrations();
    // Twice over: once in the registry table's detail column and once in the
    // prose summary. Both are deliberate -- the table is scannable, the
    // sentence is the one an operator actually reads.
    await waitFor(() => expect(screen.getAllByText(/log-only mode/).length).toBeGreaterThan(0));
    expect(screen.getAllByText(/delivers nothing/).length).toBeGreaterThan(0);
  });

  it("shows how to rotate a credential rather than offering a field that writes one", async () => {
    const { container } = renderIntegrations();
    await waitFor(() => expect(screen.getAllByText("clientSecret").length).toBeGreaterThan(0));

    expect(screen.getAllByText(/make secret-set/).length).toBeGreaterThan(0);
    // No input anywhere on the page could carry a secret to the server: the
    // registry page has no form at all.
    expect(container.querySelectorAll("input").length).toBe(0);
    expect(container.querySelectorAll("textarea").length).toBe(0);
  });

  it("never paints a credential value, even when the reply carries one", async () => {
    const { container } = renderIntegrations({ hostile: true });
    await waitFor(() => expect(screen.getAllByText("clientSecret").length).toBeGreaterThan(0));

    // The whole rendered document, not one cell: a leak would most plausibly
    // arrive through a field nobody thought to assert on.
    expect(container.textContent ?? "").not.toContain(PLANTED_SECRET);
  });

  it("asks for configuration on mount and for a live check only when told to", async () => {
    const { calls } = renderIntegrations();
    await waitFor(() => expect(screen.getAllByText("email").length).toBeGreaterThan(0));

    const statusCalls = calls.filter((call) => call.includes("integrationStatus"));
    expect(statusCalls.length).toBe(1);
    expect(statusCalls[0]).toContain("probe: false");

    fireEvent.click(screen.getByRole("button", { name: "Check now" }));
    await waitFor(() => {
      const probes = calls.filter((call) => /integrationStatus.*probe:\s*true/.test(call));
      expect(probes.length).toBe(1);
    });
  });

  it("declines to ask at all below the admin floor, and says why", async () => {
    const { calls } = renderIntegrations({ role: "reader" });

    await waitFor(() => expect(screen.getByText(/owner or admin role/)).toBeTruthy());
    expect(calls.filter((call) => call.includes("integrationStatus")).length).toBe(0);
  });
});
