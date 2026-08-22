// The app shell + routing + connection state, driven against a fake dial.
//
// The fake is the SDK's own Connection.dial signature, so this exercises the
// real ClusterProvider state machine (connecting -> connected, the
// error branch, the query handoff) without a server. What it deliberately
// does NOT do is fake the QueryClient's protocol -- listConcepts is stubbed
// at the method boundary, because the wire is sdk/ts's business and is tested
// there.

import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { Concept, Connection} from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { asQueryClient } from "./support/queryFake";

const CONCEPTS: Concept[] = [
  {
    id: "v1:cluster:node",
    version: "v1",
    domain: "cluster",
    entity: "node",
    description: "A registered node in the cluster",
    type: "concept",
    displayCard: { primary: "name" },
  },
];

// fakeConnection builds the narrow slice of Connection the provider touches.
// Cast at the boundary rather than implementing the whole class: widening the
// fake every time an unrelated method is added to Connection would make this
// test a maintenance tax with no extra coverage.
function fakeConnection(overrides: Partial<Connection> = {}): Connection {
  const query = asQueryClient({
    listConcepts: vi.fn(async () => CONCEPTS),
    // The shell reads the caller's access to decide what the rail offers
    // (the Modules item is owner/admin-only, memql#4191).
    getMyAccess: vi.fn(async () => ({
      userId: "user-test",
      primaryEmail: "op@example.test",
      clusterRole: "admin",
    })),
  });
  return {
    nodeId: "bff-test",
    serverVersion: "0.0.0-test",
    query,
    close: vi.fn(),
    done: vi.fn(() => new Promise<void>(() => {})), // never settles
    ...overrides,
  } as unknown as Connection;
}

// The routes now sit behind RequireAuth (memql#3315), so a shell test needs a
// session to exist. It is supplied by declaring the cluster auth-disabled --
// the one configuration in which there is genuinely nothing to sign in to, so
// the shell renders with no credential machinery involved at all. That keeps
// these tests about the shell; the sign-in flow itself is authFlow.test.tsx's
// subject.
const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
  domain: "",
};

function renderApp(dial: typeof Connection.dial, path = "/concepts") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("the shell tests must make no identity calls");
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
}

describe("the portal shell", () => {
  it("reports connected and lists the cluster's concepts", async () => {
    const dial = vi.fn(async () => fakeConnection()) as unknown as typeof Connection.dial;
    renderApp(dial);

    await waitFor(() => expect(screen.getByText("MemQL Portal")).toBeTruthy());
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("Connected"));
    await waitFor(() => expect(screen.getByText("v1:cluster:node")).toBeTruthy());
  });

  it("surfaces a dial failure instead of showing an empty page", async () => {
    const dial = vi.fn(async () => {
      throw new Error("websocket closed before open: code=1006");
    }) as unknown as typeof Connection.dial;
    renderApp(dial);

    await waitFor(() =>
      expect(screen.getByRole("status").textContent).toBe("Connection failed"),
    );
    expect(screen.getByText(/websocket closed before open/)).toBeTruthy();
    // A page that cannot reach the cluster must say so rather than rendering
    // an empty concept list that reads as "this cluster has no concepts".
    expect(screen.getByText(/Not connected to a cluster/)).toBeTruthy();
  });

  it("routes an unknown path to the client-side 404 (the SPA fallback's other half)", async () => {
    const dial = vi.fn(async () => fakeConnection()) as unknown as typeof Connection.dial;
    renderApp(dial, "/nope");
    await waitFor(() => expect(screen.getByText("Not found")).toBeTruthy());
  });

  it("redirects the index to /concepts", async () => {
    const dial = vi.fn(async () => fakeConnection()) as unknown as typeof Connection.dial;
    renderApp(dial, "/");
    await waitFor(() => expect(screen.getByText("Concepts")).toBeTruthy());
  });
});

const AUTH_ENABLED_CLUSTER = {
  identityUrl: "https://identity.example.com",
  identityApiBaseUrl: "https://identity.example.com",
  oauthClientId: "portal",
  authEnabled: true,
  domain: "example.com",
};

function renderSignedIn(dial: typeof Connection.dial, path = "/concepts") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider
        config={AUTH_ENABLED_CLUSTER}
        fetchImpl={async () =>
          ({
            ok: true,
            status: 200,
            json: async () => ({ access_token: "AT-1", expires_in: 900 }),
          }) as unknown as Response
        }
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
}

function header(): HTMLElement {
  // The chrome header by ROLE, not by its text. A page's own <header> nests
  // inside <main>, which strips the implicit banner role, so exactly one
  // element in the document carries it.
  //
  // It used to be found by getByText("Cluster") -- which stopped being unique
  // when the rail gained a "Cluster" group (memql#4264). Two elements can
  // legitimately carry the same word; a query that depends on a word being
  // unique in the whole document is the fragile half of that.
  return screen.getByRole("banner", { name: "Cluster and session" });
}

function rail(): HTMLElement {
  return screen.getByRole("navigation", { name: "Portal sections" });
}

function connectionTone(): string | null {
  return document.querySelector("[data-connection-tone]")?.getAttribute("data-connection-tone") ?? null;
}

describe("the rail profile (memql#4240)", () => {
  afterEach(() => {
    globalThis.localStorage?.removeItem("memql-portal-rail");
  });

  it("shows engineVersion and the peer in the rail, not the wire version in the header", async () => {
    const dial = vi.fn(async () =>
      fakeConnection({ engineVersion: "v0.19.5" }),
    ) as unknown as typeof Connection.dial;
    renderApp(dial);

    await waitFor(() => expect(within(rail()).getByText("v0.19.5")).toBeTruthy());
    expect(within(rail()).getByText("bff-test")).toBeTruthy();
    expect(within(rail()).queryByText("0.0.0-test")).toBeNull();
    expect(within(header()).queryByText("0.0.0-test")).toBeNull();
    expect(within(header()).queryByText("v1")).toBeNull();
    expect(within(header()).queryByText("bff-test")).toBeNull();
    expect(within(header()).queryByText("Connected")).toBeNull();
  });

  it("renders dev when engineVersion is empty", async () => {
    const dial = vi.fn(async () => fakeConnection()) as unknown as typeof Connection.dial;
    renderApp(dial);

    await waitFor(() => expect(within(rail()).getByText("dev")).toBeTruthy());
    expect(within(rail()).queryByText("0.0.0-test")).toBeNull();
  });

  it("is green only when connected; connecting and error are red", async () => {
    let release!: (conn: Connection) => void;
    const held = new Promise<Connection>((resolve) => {
      release = resolve;
    });
    const dial = vi.fn(async () => held) as unknown as typeof Connection.dial;
    renderApp(dial);

    await waitFor(() => expect(screen.getByText("MemQL Portal")).toBeTruthy());
    expect(screen.getByRole("status").textContent).toBe("Connecting");
    expect(connectionTone()).toBe("danger");

    release(fakeConnection({ engineVersion: "v0.19.5" }));
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("Connected"));
    expect(connectionTone()).toBe("ok");
  });

  it("offers Retry on error and closed, and Retry redials", async () => {
    const dial = vi.fn(async () => {
      throw new Error("websocket closed before open: code=1006");
    });
    renderApp(dial as unknown as typeof Connection.dial);

    await waitFor(() => expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy());
    expect(connectionTone()).toBe("danger");
    const before = dial.mock.calls.length;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(dial.mock.calls.length).toBeGreaterThan(before));
  });

  it("offers Retry after the stream closes", async () => {
    let settle!: () => void;
    const dial = vi.fn(async () =>
      fakeConnection({
        engineVersion: "v0.19.5",
        done: vi.fn(() => new Promise<void>((resolve) => { settle = resolve; })),
      }),
    ) as unknown as typeof Connection.dial;
    renderApp(dial);

    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("Connected"));
    settle();
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("Disconnected"));
    expect(connectionTone()).toBe("danger");
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });

  it("keeps Authentication disabled in the rail, not the header", async () => {
    const dial = vi.fn(async () => fakeConnection()) as unknown as typeof Connection.dial;
    renderApp(dial);

    await waitFor(() => expect(screen.getByText("Authentication disabled")).toBeTruthy());
    expect(within(rail()).getByText("Authentication disabled")).toBeTruthy();
    expect(within(header()).queryByText("Authentication disabled")).toBeNull();
  });

  it("puts email, role, and Sign out in the rail footer, not the header", async () => {
    const dial = vi.fn(async () =>
      fakeConnection({ engineVersion: "v0.19.5" }),
    ) as unknown as typeof Connection.dial;
    renderSignedIn(dial);

    await waitFor(() => expect(within(rail()).getByText("op@example.test")).toBeTruthy());
    expect(within(rail()).getByText("admin")).toBeTruthy();
    expect(within(rail()).getByRole("button", { name: "Sign out" })).toBeTruthy();
    expect(within(header()).queryByText("op@example.test")).toBeNull();
    expect(within(header()).queryByText("admin")).toBeNull();
    expect(within(header()).queryByRole("button", { name: "Sign out" })).toBeNull();
  });

  it("collapses from a quiet brand-row control and persists the preference", async () => {
    const dial = vi.fn(async () =>
      fakeConnection({ engineVersion: "v0.19.5" }),
    ) as unknown as typeof Connection.dial;
    renderSignedIn(dial);

    await waitFor(() => expect(within(rail()).getByText("op@example.test")).toBeTruthy());
    const collapse = within(rail()).getByRole("button", { name: "Collapse the navigation rail" });
    expect(collapse.getAttribute("aria-expanded")).toBe("true");
    expect(collapse.className).not.toMatch(/\bborder\b/);
    expect(rail().firstElementChild?.contains(collapse)).toBe(true);
    expect(within(rail()).getAllByRole("button", { name: /navigation rail/ })).toHaveLength(1);

    fireEvent.click(collapse);
    expect(globalThis.localStorage.getItem("memql-portal-rail")).toBe("collapsed");
    expect(within(rail()).getByRole("button", { name: "Expand the navigation rail" })).toBeTruthy();
    expect(within(rail()).queryByText("v0.19.5")).toBeNull();
    expect(within(rail()).queryByText("bff-test")).toBeNull();
    expect(within(rail()).queryByText("op@example.test")).toBeNull();
    expect(within(rail()).queryByText("admin")).toBeNull();
    expect(within(rail()).getByRole("button", { name: "Sign out" })).toBeTruthy();
    expect(connectionTone()).toBe("ok");
    expect(screen.getByTitle(/bff-test/)).toBeTruthy();
    expect(screen.getByTitle(/v0.19.5/)).toBeTruthy();
    expect(screen.getByTitle(/op@example.test/)).toBeTruthy();
  });
});
