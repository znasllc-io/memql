import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { ReactNode } from "react";

// Planted values, long and distinctive on purpose: the server-side sweep
// skips anything under eight characters, and a short planted value makes a
// sweep vacuous. Fixture domains are example.com only.
const PLANTED_SECRET = "PLANTED-GRAPH-CLIENT-SECRET-DO-NOT-EMIT";
const PLANTED_DSN = "postgres://memql:PLANTED-DSN-PASSWORD@db.example.com:5432/memql";
const PLANTED_TOKEN = "mql_pat_PLANTED_BEARER_DO_NOT_EMIT_00000000";

const h = vi.hoisted(() => {
  const reply = (rows: unknown[]) => ({ rows: () => rows });
  const state = {
    cluster: [] as unknown[],
    deployments: [] as unknown[],
    specs: [] as unknown[],
    mail: [] as unknown[],
    providers: [] as unknown[],
    database: [] as unknown[],
    identityProvider: [] as unknown[],
    infraError: null as Error | null,
    mailError: null as Error | null,
    providerError: null as Error | null,
    integrationCalls: 0,
  };
  const connection = {
    nodeId: "bff-test",
    engineVersion: "v9.9.9",
    engineCommit: "abcdef123456",
    subscriptions: null,
    query: {
      existingCluster: vi.fn(async () => reply(state.cluster)),
      deploymentsForCluster: vi.fn(async () => reply(state.deployments)),
      nodeSpecsForDeployment: vi.fn(async () => reply(state.specs)),
      integrationStatus: vi.fn(async () => {
        state.integrationCalls += 1;
        if (state.mailError) throw state.mailError;
        return reply(state.mail);
      }),
      providerAuthStatus: vi.fn(async () => {
        if (state.providerError) throw state.providerError;
        return reply(state.providers);
      }),
      clusterDatabase: vi.fn(async () => {
        if (state.infraError) throw state.infraError;
        return reply(state.database);
      }),
      clusterIdentityProvider: vi.fn(async () => {
        if (state.infraError) throw state.infraError;
        return reply(state.identityProvider);
      }),
    },
    onStatusChange: (fn: (ev: { status: string; attempt: number; error: string }) => void) => {
      fn({ status: "connected", attempt: 0, error: "" });
      return () => {};
    },
  };
  return { connection, state };
});

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { SessionProvider } = await import("../../src/chrome/access");
const { OsProvider } = await import("../../src/chrome/state");
const { OS_REGISTRY } = await import("../../src/apps/registry");
const { SettingsApp } = await import("../../src/apps/settings/SettingsApp");
const { LocalDesktopStore } = await import("../../src/system/store");
const { UNKNOWN_RUNTIME_CONFIG } = await import("../../src/cluster/config");

function memStorage(): Pick<Storage, "getItem" | "setItem"> {
  const data = new Map<string, string>();
  return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
}

function wrap(children: ReactNode, role: string) {
  return (
    <SessionProvider
      value={{
        access: { userId: "u-1", primaryEmail: "owner@example.com", clusterRole: role },
        config: {
          ...UNKNOWN_RUNTIME_CONFIG,
          domain: "example.com",
          identityUrl: "https://identity.example.com",
        },
      }}
    >
      <OsProvider
        registry={OS_REGISTRY}
        actorRole={role}
        grid={{ cols: 12, rows: 8 }}
        store={new LocalDesktopStore(memStorage())}
      >
        {children}
      </OsProvider>
    </SessionProvider>
  );
}

async function renderCluster(role = "owner") {
  const view = render(
    wrap(<SettingsApp sectionId="cluster" navigate={vi.fn()} askContext={vi.fn()} />, role),
  );
  // Let the seeds and the two request/reply reads settle.
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
  return view;
}

beforeEach(() => {
  h.state.cluster = [
    {
      id: "v1:cluster:cluster:c1",
      payload: { name: "dev", region: "local", status: "healthy", version: "v1.2.3", provider: "docker-local" },
    },
  ];
  h.state.deployments = [
    // `id` is not optional dressing: a LiveCollection keys rows on it, and
    // the `deploymentFull` shape projects `row.id` for exactly that reason.
    // A fixture without one is silently dropped by the fold.
    {
      id: "v1:cluster:deployment:d-1-a",
      deploymentId: "d-1",
      status: "succeeded",
      version: "2026.6.21",
      createdAt: "2026-06-21T00:00:00Z",
    },
    {
      id: "v1:cluster:deployment:d-0-a",
      deploymentId: "d-0",
      status: "superseded",
      version: "2026.5.1",
      createdAt: "2026-05-01T00:00:00Z",
    },
  ];
  h.state.specs = [
    { nodeType: "bff", version: "", replicas: 2 },
    { nodeType: "voice", version: "2026.5.1", replicas: 1 },
  ];
  h.state.mail = [
    {
      integrationStatus: {
        payload: {
          checkedAt: "2026-08-31T12:00:00Z",
          probed: false,
          integrations: [
            {
              name: "email",
              mode: "log",
              configured: "no",
              health: "degraded",
              detail: "No sender is configured, so the integration is running in log-only mode.",
              // Present on the wire and never rendered.
              credentials: [
                { name: "clientSecret", present: true, envVar: "AZURE_CLIENT_SECRET", rotate: PLANTED_SECRET },
              ],
              settings: [{ name: "dsn", value: PLANTED_DSN, source: "env" }],
            },
          ],
        },
      },
    },
  ];
  h.state.providers = [
    { id: "chat54Mini", name: "chat54Mini", vendor: "OpenAI", model: "gpt-5.4-mini", available: true, authSource: "env" },
  ];
  h.state.database = [
    {
      id: "v1:cluster:database:primary",
      payload: {
        host: "db.example.com",
        port: 15432,
        dbName: "memql",
        engine: "postgresql",
        engineVersion: "16.4",
        extensions: ["timescaledb", "vector"],
        extensionVersions: { timescaledb: "2.25.2", vector: "0.8.2" },
        sslMode: "require",
        clusterId: "v1:cluster:cluster:c1",
      },
    },
  ];
  h.state.identityProvider = [
    {
      id: "v1:cluster:identityProvider:primary",
      payload: {
        name: "MemQL Identity",
        providerType: "oidc",
        issuerUrl: "https://identity.example.com/",
        jwksUrl: "https://identity.example.com/.well-known/jwks.json",
        acceptedAudiences: ["memql-api"],
        clientIdPrefix: "abc12345",
        clusterId: "v1:cluster:cluster:c1",
      },
    },
  ];
  h.state.infraError = null;
  h.state.mailError = null;
  h.state.providerError = null;
  h.state.integrationCalls = 0;
});

describe("infrastructure facts (memql#4766)", () => {
  it("renders the database and identity-provider records", async () => {
    // These are the fields that had no writer at all until memql#4766 -- the
    // panel could not show them and said so in its own comment.
    await renderCluster();
    const section = screen.getByRole("region", { name: "Infrastructure" });
    expect(within(section).getByText("postgresql 16.4")).toBeTruthy();
    expect(within(section).getByText("timescaledb 2.25.2, vector 0.8.2")).toBeTruthy();
    expect(
      within(section).getByText("https://identity.example.com/.well-known/jwks.json"),
    ).toBeTruthy();
    expect(within(section).getByText("memql-api")).toBeTruthy();
  });

  it("shows the real port rather than a stamped 5432", async () => {
    // createDatabase used to stamp the literal 5432 while the DSN parse held
    // the true value; the fixture is on 15432 so a regression is visible.
    await renderCluster();
    const section = screen.getByRole("region", { name: "Infrastructure" });
    expect(within(section).getByText("memql on db.example.com:15432")).toBeTruthy();
  });

  it("carries no health verdict, because the status fields were removed", async () => {
    // database.status was structurally unanswerable and identityProvider.status
    // had no honest writer; both are gone. A panel line reading either would be
    // a constant dressed as a health check.
    await renderCluster();
    const section = screen.getByRole("region", { name: "Infrastructure" });
    expect(within(section).queryByText(/reachable|connected|healthy/i)).toBeNull();
    expect(within(section).getByText(/deliberately carrying no health verdict/)).toBeTruthy();
  });

  it("tells an admin the read is owner-only instead of issuing it", async () => {
    // The section admits admin; these two reads are cluster-owner only. An
    // admin should read WHY rather than see an empty panel -- and we should
    // not spend a round trip on an answer we already know.
    // mockClear FIRST: vitest isolates per FILE, not per test, and this spy is
    // module-level -- the owner tests above have already called it, so a bare
    // not.toHaveBeenCalled() fails on their history rather than on this
    // render.
    h.connection.query.clusterDatabase.mockClear();
    h.connection.query.clusterIdentityProvider.mockClear();

    await renderCluster("admin");
    const section = screen.getByRole("region", { name: "Infrastructure" });
    expect(within(section).getByText(/cluster-owner only/)).toBeTruthy();
    expect(h.connection.query.clusterDatabase).not.toHaveBeenCalled();
    expect(h.connection.query.clusterIdentityProvider).not.toHaveBeenCalled();
  });

  it("says a cluster that has not restarted yet has no record", async () => {
    // The rows are written when a bff starts. An existing cluster picks them
    // up on its next restart, so "absent" is a real and temporary state that
    // must not read as breakage.
    h.state.database = [];
    h.state.identityProvider = [];
    await renderCluster();
    const section = screen.getByRole("region", { name: "Infrastructure" });
    expect(within(section).getByText(/has not restarted since this was added/)).toBeTruthy();
  });
});

describe("cluster facts (memql#4742)", () => {
  it("renders the four groups read-only", async () => {
    await renderCluster();
    expect(screen.getByRole("region", { name: "Cluster and identity" })).toBeTruthy();
    expect(screen.getByRole("region", { name: "Versions" })).toBeTruthy();
    expect(screen.getByRole("region", { name: "Mail sender" })).toBeTruthy();
    expect(screen.getByRole("region", { name: "AI providers" })).toBeTruthy();

    // Every control on the surface is a Refresh. Nothing mutates.
    const buttons = screen.getAllByRole("button").map((b) => b.textContent);
    expect(new Set(buttons)).toEqual(new Set(["Refresh"]));
  });

  it("shows the domain and issuer the cluster served, and the live engine version", async () => {
    await renderCluster();
    expect(screen.getByText("example.com")).toBeTruthy();
    expect(screen.getByText("https://identity.example.com")).toBeTruthy();
    expect(screen.getByText("v9.9.9")).toBeTruthy();
    expect(screen.getByText("bff-test")).toBeTruthy();
  });

  it("resolves an unpinned node type to the deployment engine version", async () => {
    await renderCluster();
    // The per-node specs are a fetch CHAINED off the deployment seed --
    // the deployment has to resolve before the spec read has an id.
    const list = await waitFor(() => screen.getByRole("list", { name: "Node type versions" }));
    const rows = within(list).getAllByRole("listitem").map((li) => li.textContent ?? "");
    expect(rows.some((r) => /bff.*2026\.6\.21.*engine version/.test(r))).toBe(true);
    expect(rows.some((r) => /voice.*2026\.5\.1.*pinned/.test(r))).toBe(true);
  });

  it("renders log-only mail as degraded, never as healthy", async () => {
    await renderCluster();
    const panel = screen.getByRole("region", { name: "Mail sender" });
    expect(within(panel).getByText(/delivered to nobody/)).toBeTruthy();
    expect(within(panel).getByText("degraded")).toBeTruthy();
    expect(within(panel).queryByText("healthy")).toBeNull();
    expect(
      within(panel).getByRole("img", { name: "Mail health: degraded" }).getAttribute("data-os-dot"),
    ).toBe("unreachable");
  });

  it("carries a fetched-at stamp from the SERVER and a Refresh", async () => {
    await renderCluster();
    const panel = screen.getByRole("region", { name: "Mail sender" });
    // checkedAt is stamped in the handler; the client clock only says when
    // the reply landed here.
    expect(within(panel).getByText(/Read at 2026-08-31T12:00:00Z/)).toBeTruthy();
    expect(within(panel).getByText(/which replica answered is not knowable/)).toBeTruthy();

    expect(h.state.integrationCalls).toBe(1);
    await act(async () => {
      fireEvent.click(within(panel).getByRole("button", { name: "Refresh" }));
      await Promise.resolve();
    });
    expect(h.state.integrationCalls).toBe(2);
  });

  it("renders a server refusal in surface, in the engine's own words", async () => {
    h.state.providerError = new Error("providerAuthStatus is owner-only");
    await renderCluster("admin");
    const panel = screen.getByRole("region", { name: "AI providers" });
    // An admin may open the section and may not make this one read. Both are
    // true at once, and the panel says which happened where the panel is.
    expect(within(panel).getByText(/declined this read for admin/)).toBeTruthy();
    expect(within(panel).getByText("providerAuthStatus is owner-only")).toBeTruthy();
  });

  it("calls an empty provider list a fresh install, not a fault", async () => {
    h.state.providers = [];
    await renderCluster();
    const panel = screen.getByRole("region", { name: "AI providers" });
    expect(within(panel).getByText(/how a freshly installed\s+cluster starts/)).toBeTruthy();
  });

  it("renders no secret, token, DSN or credential value anywhere in the DOM", async () => {
    const { container } = await renderCluster();
    const dom = container.innerHTML;
    for (const planted of [PLANTED_SECRET, PLANTED_DSN, PLANTED_TOKEN]) {
      expect(dom).not.toContain(planted);
    }
    // Nor the credential SLOT MAP: which vendor slots are filled is
    // reconnaissance even with no values attached.
    expect(dom).not.toContain("AZURE_CLIENT_SECRET");
    expect(dom).not.toContain("clientSecret");

    // The negative control. Without it this asserts only that the sweep ran,
    // not that it could ever have seen anything.
    expect(dom).toContain("example.com");
    expect(dom).toContain("degraded");
  });
});
