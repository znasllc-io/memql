// The absorbed admin console (memql#3324), end to end against a fake cluster.
//
// WHAT THESE OWN. Two properties, and the second is the reason this file is
// worth more than a rendering check.
//
//   1. COMPOSITION -- each surface draws through view-kit's element library
//      rather than markup of its own, and the bands appear in the designed
//      order. portal_view_composition_test.go polices src/views/ and does NOT
//      reach src/admin/, so the discipline is held here deliberately instead of
//      mechanically.
//
//   2. THE AUTHORIZATION SHAPE OF THE MOVE. The server-rendered console this
//      replaces gated every read and every write behind requireAdmin. Moving
//      those screens into a browser must not move the gate with them, and two
//      things make that true: every read names a query that gates ITSELF
//      server-side, and the console issues no write at all. Both are asserted
//      below against the calls the console actually makes, because both are
//      invisible in a screenshot and both are one careless edit away.

import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import {
  Result,
  type AccessSummary,
  type Concept,
  type Connection,
  type QueryClient,
  type Role,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";

const USER = "v1:identity:user";
const AUDIT = "v1:identity:auditEvent";

// The real display cards from dsl/identity/concepts.memql -- the admin pages
// bind slots against them, so a rail that finds `role` here finds it live.
const CONCEPTS: Concept[] = [
  {
    id: USER,
    version: "v1",
    domain: "identity",
    entity: "user",
    type: "concept",
    description: "A person who can sign in",
    displayCard: {
      primary: "displayName",
      secondary: "role",
      tertiary: "primaryEmail",
      status: "active",
    },
  },
  {
    id: AUDIT,
    version: "v1",
    domain: "identity",
    entity: "auditEvent",
    type: "concept",
    description: "Append-only audit trail",
    displayCard: {
      primary: "action",
      secondary: "category",
      tertiary: "actorEmail",
      status: "outcome",
    },
  },
];

// Wire-shaped rows: payload NESTED. Result.rows() does the flatten, so
// exercising it is part of the point.
function node(concept: string, id: string, payload: Record<string, unknown>): Row {
  return { id, concept, type: "concept", createdAt: "2026-08-01T10:00:00Z", payload };
}

const PEOPLE: Row[] = [
  node(USER, "user-ada", {
    displayName: "Ada Lovelace",
    primaryEmail: "ada@example.com",
    role: "owner",
    active: true,
  }),
  node(USER, "user-grace", {
    displayName: "Grace Hopper",
    primaryEmail: "grace@example.com",
    role: "reader",
    active: true,
  }),
];

const AUDIT_EVENTS: Row[] = [
  node(AUDIT, "audit-1", {
    action: "login_succeeded",
    category: "auth",
    outcome: "success",
    actorEmail: "ada@example.com",
    occurredAt: "2026-08-08T06:00:00Z",
    sourceIP: "10.0.0.9",
  }),
  node(AUDIT, "audit-2", {
    action: "admin_auth_forbidden",
    category: "admin",
    outcome: "blocked",
    actorEmail: "grace@example.com",
    occurredAt: "2026-08-08T06:30:00Z",
    failureReason: "role_not_admin",
    sourceIP: "10.0.0.11",
  }),
];

const CONFIGURATION_EVENTS: Row[] = [
  node(AUDIT, "audit-rot", {
    action: "jwks_rotated",
    category: "configuration",
    outcome: "success",
    actorEmail: "ada@example.com",
    occurredAt: "2026-08-07T04:00:00Z",
  }),
];

const SETTINGS: Row[] = [
  node("v1:identity:clusterSettings", "cluster", {
    clusterDomain: "acme.example.com",
    registrationMode: "invite_only",
    internalDefaultRole: "writer",
    accessTokenTTLSeconds: 900,
    refreshTokenTTLSeconds: 2592000,
    brandName: "Acme",
    brandLogoDataURI: "data:image/png;base64,AAA",
    authoredAutomationsEnabled: true,
  }),
];

const TOKENS: Readonly<Record<string, Row[]>> = {
  "user-ada": [
    node("v1:identity:identity", "pat-1", {
      userId: "user-ada",
      label: "laptop",
      active: true,
      lastUsedAt: "2026-08-08T05:00:00Z",
      usableByAgents: false,
    }),
  ],
  "user-grace": [
    node("v1:identity:identity", "pat-2", {
      userId: "user-grace",
      label: "ci",
      active: false,
      usableByAgents: true,
    }),
  ],
};

const JWKS = {
  keys: [
    { kty: "OKP", alg: "EdDSA", use: "sig", crv: "Ed25519", kid: "kid-current" },
    { kty: "OKP", alg: "EdDSA", use: "sig", crv: "Ed25519", kid: "kid-previous" },
  ],
};

const IDENTITY_ORIGIN = "https://identity.example.com";

// authEnabled false so RequireAuth renders the routes without a sign-in round
// trip; identityUrl still set, because the signing-keys page reads the public
// JWKS feed off it and a blank origin is a different branch (also covered).
const CLUSTER_CONFIG = {
  identityUrl: IDENTITY_ORIGIN,
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
};

interface Harness {
  role: Role;
  // Serve the JWKS feed, or fail the fetch.
  jwks: "ok" | "fail";
  // Blank the identity origin, to exercise the "no feed to read" branch.
  noIdentityOrigin: boolean;
}

// Every executeNamed the console issued, in order. The authorization
// assertions read this rather than a rendered string.
let calls: { name: string; call: string }[] = [];

function renderAdmin(
  { role = "owner", jwks = "ok", noIdentityOrigin = false }: Partial<Harness>,
  path: string,
) {
  calls = [];

  const access: AccessSummary = {
    requestId: "r1",
    userId: "user-ada",
    primaryEmail: "ada@example.com",
    clusterRole: role,
  };

  const query = {
    listConcepts: vi.fn(async () => CONCEPTS),
    getMyAccess: vi.fn(async () => access),
    executeNamed: vi.fn(async (name: string, call: string) => {
      calls.push({ name, call });
      if (name === "searchUsers") {
        return new Result({ bundle: { nodes: PEOPLE }, meta: { cursor: "" } });
      }
      if (name === "clusterSettingsCurrent") {
        return new Result({ bundle: { nodes: SETTINGS }, meta: { cursor: "" } });
      }
      if (name === "recentAuditEvents") {
        const configuration = call.includes('"configuration"');
        return new Result({
          bundle: { nodes: configuration ? CONFIGURATION_EVENTS : AUDIT_EVENTS },
          meta: { cursor: "" },
        });
      }
      if (name === "patIdentitiesForUser") {
        const match = /userId: "([^"]+)"/.exec(call);
        const nodes = TOKENS[match?.[1] ?? ""] ?? [];
        return new Result({ bundle: { nodes }, meta: { cursor: "" } });
      }
      throw new Error(`the admin console called an unexpected query: ${name}`);
    }),
  } as unknown as QueryClient;

  const dial = vi.fn(
    async () =>
      ({
        nodeId: "bff-test",
        serverVersion: "0.0.0-test",
        query,
        close: vi.fn(),
        done: vi.fn(() => new Promise<void>(() => {})),
      }) as unknown as Connection,
  ) as unknown as typeof Connection.dial;

  globalThis.fetch = vi.fn(async () => {
    if (jwks === "fail") return { ok: false, status: 503 } as unknown as Response;
    return { ok: true, status: 200, json: async () => JWKS } as unknown as Response;
  }) as unknown as typeof globalThis.fetch;

  const result = render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider
        config={{
          ...CLUSTER_CONFIG,
          ...(noIdentityOrigin ? { identityUrl: "" } : {}),
        }}
        fetchImpl={async () => {
          throw new Error("the admin tests must make no identity API calls");
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
  return { ...result, query };
}

// Every band an admin page draws comes out of view-kit, so its markup carries a
// vk- class. Asserting on the class rather than the text is what makes this a
// composition test: text could be printed by anything.
function viewKitClasses(container: HTMLElement): Set<string> {
  const out = new Set<string>();
  for (const el of container.querySelectorAll("[class]")) {
    for (const cls of (el.getAttribute("class") ?? "").split(/\s+/)) {
      if (cls.startsWith("vk-")) out.add(cls);
    }
  }
  return out;
}

function bandTitles(): (string | null)[] {
  const main = screen.getByRole("main");
  return [...main.querySelectorAll("h2")].map((h) => h.textContent);
}

beforeEach(() => {
  calls = [];
});

describe("the admin console's authorization shape", () => {
  it("reads through queries that gate themselves, never a raw concept browse", async () => {
    renderAdmin({}, "/admin");
    await waitFor(() => expect(screen.getByText("login_succeeded")).toBeTruthy());

    // THE ASSERTION THAT MATTERS. `searchUsers` and `patIdentitiesForUser`
    // carry `requiresOwnerOrAdmin` as a top-level conjunct in their own DSL
    // filter, so the engine empties the result for a caller below the floor. A
    // generic `concept==v1:identity:user` browse would return the same rows to
    // anybody, because that concept declares no @rowAuthz tier -- so reaching
    // for the browse path is exactly how this move would lose its gate.
    expect(calls.length).toBeGreaterThan(0);
    for (const { call } of calls) {
      expect(call).not.toMatch(/concept==/);
    }
    expect(calls.some((c) => c.name === "searchUsers")).toBe(true);
  });

  it("issues no write from any of its surfaces", async () => {
    for (const path of ["/admin", "/admin/tokens", "/admin/keys", "/admin/settings"]) {
      const view = renderAdmin({}, path);
      await waitFor(() => expect(calls.length).toBeGreaterThan(0));
      // Every write the retired console performed is either refused for a
      // client-originated call or reachable without the owner/admin gate that
      // console applies at its route. A mutation appearing here would mean a
      // control shipped whose only gate is a boolean in the browser.
      for (const { call } of calls) {
        expect(call.startsWith("mutation ")).toBe(false);
      }
      view.unmount();
    }
  });

  it("offers a reader nothing, and says whose decision that is", async () => {
    renderAdmin({ role: "reader" }, "/admin");
    await waitFor(() =>
      expect(screen.getByText(/resolved your role on this connection as reader/)).toBeTruthy(),
    );
    expect(screen.getByText("This is an owner and admin surface")).toBeTruthy();
    // The courtesy costs the cluster nothing: no gated read is issued at all.
    expect(calls.some((c) => c.name === "searchUsers")).toBe(false);
  });

  it("states the role floor and the caller's own role in the eyebrow", async () => {
    renderAdmin({}, "/admin");
    await waitFor(() => expect(screen.getByText(/owner or admin/)).toBeTruthy());
    expect(screen.getByText(/you are owner/)).toBeTruthy();
  });
});

describe("the overview", () => {
  it("opens on readings, divides people by role, and rolls out the trail", async () => {
    const { container } = renderAdmin({}, "/admin");
    await waitFor(() => expect(screen.getByText("login_succeeded")).toBeTruthy());

    const classes = viewKitClasses(container);
    expect(classes.has("vk-chart-rail-seg")).toBe(true);
    expect(classes.has("vk-table")).toBe(true);

    expect(bandTitles()).toEqual(["By role", "Recent activity"]);
    expect(screen.getByText("owner 1 (50%)")).toBeTruthy();
    expect(screen.getByText("reader 1 (50%)")).toBeTruthy();

    // The four readings the retired dashboard carried, plus the one it could
    // not: the rotation itself rather than a duration off one replica's clock.
    expect(screen.getByText("People who can sign in")).toBeTruthy();
    expect(screen.getByText("invite_only")).toBeTruthy();
    expect(screen.getByText("kid-current")).toBeTruthy();
    expect(screen.getByText("2026-08-07T04:00:00Z")).toBeTruthy();
    expect(screen.getByText("by ada@example.com")).toBeTruthy();
  });

  it("filters the trail server-side, by category", async () => {
    renderAdmin({}, "/admin");
    await waitFor(() => expect(screen.getByText("login_succeeded")).toBeTruthy());

    // The unfiltered strip asks for every category, and the rotation reading
    // asks for `configuration` regardless -- a rotation the operator filtered
    // away has not stopped being the answer to "when did this key change".
    const audits = calls.filter((c) => c.name === "recentAuditEvents");
    expect(audits.some((c) => c.call === "query recentAuditEvents()")).toBe(true);
    expect(audits.some((c) => c.call.includes('category: "configuration"'))).toBe(true);
  });

  it("says where changing a person went, rather than hiding the absence", async () => {
    renderAdmin({}, "/admin");
    await waitFor(() => expect(screen.getByText("Changing a person")).toBeTruthy());
    expect(screen.getByText(/updateUser/)).toBeTruthy();
  });
});

describe("sessions and tokens", () => {
  it("lists every person's tokens, joined to the person who holds them", async () => {
    const { container } = renderAdmin({}, "/admin/tokens");
    await waitFor(() => expect(screen.getByText("ada@example.com")).toBeTruthy());

    expect(viewKitClasses(container).has("vk-table")).toBe(true);
    // The join is the point: a PAT row carries a userId, and an operator
    // responding to an incident is looking for a person.
    expect(screen.getByText("grace@example.com")).toBeTruthy();
    expect(screen.getByText("laptop")).toBeTruthy();
    expect(screen.getByText("ci")).toBeTruthy();

    // Revoked rows stay listed rather than being filtered out.
    const table = container.querySelector(".vk-table");
    expect(table).toBeTruthy();
    expect(within(table as HTMLElement).getByText("revoked")).toBeTruthy();

    // One gated read per person -- there is no cluster-wide PAT query to call.
    const fanOut = calls.filter((c) => c.name === "patIdentitiesForUser");
    expect(fanOut.length).toBe(2);
  });

  it("names the two capabilities it does not carry", async () => {
    renderAdmin({}, "/admin/tokens");
    await waitFor(() => expect(screen.getByText("Revoking a token")).toBeTruthy());
    expect(screen.getByText("Node tokens")).toBeTruthy();
    expect(screen.getByText(/nodeTokenIdentities/)).toBeTruthy();
  });
});

describe("signing keys", () => {
  it("reads the public feed and names which key is signing", async () => {
    const { container } = renderAdmin({}, "/admin/keys");
    await waitFor(() => expect(screen.getByText("kid-previous")).toBeTruthy());

    expect(viewKitClasses(container).has("vk-table")).toBe(true);
    expect(globalThis.fetch).toHaveBeenCalledWith(
      `${IDENTITY_ORIGIN}/.well-known/jwks.json`,
      expect.objectContaining({ credentials: "omit" }),
    );
    // Two keys means the overlap window is open, which is the single most
    // useful thing this page says.
    expect(screen.getByText("the overlap window is open")).toBeTruthy();
    expect(screen.getByText("signing")).toBeTruthy();
    expect(screen.getByText("accepted")).toBeTruthy();
  });

  it("explains a feed it cannot reach instead of rendering an empty table", async () => {
    renderAdmin({ jwks: "fail" }, "/admin/keys");
    await waitFor(() =>
      expect(screen.getByText(/the identity service answered 503/)).toBeTruthy(),
    );
  });

  it("explains a deployment that publishes no identity origin", async () => {
    renderAdmin({ noIdentityOrigin: true }, "/admin/keys");
    await waitFor(() =>
      expect(screen.getByText(/publishes no identity origin/)).toBeTruthy(),
    );
    expect(globalThis.fetch).not.toHaveBeenCalled();
  });

  it("says where rotation went", async () => {
    renderAdmin({}, "/admin/keys");
    await waitFor(() => expect(screen.getByText("Rotating a key")).toBeTruthy());
    expect(screen.getByText(/POST \/admin\/jwks\/rotate/)).toBeTruthy();
  });
});

describe("cluster settings", () => {
  it("transposes the settings row into one row per decision", async () => {
    const { container } = renderAdmin({}, "/admin/settings");
    await waitFor(() => expect(screen.getByText("Who may register")).toBeTruthy());

    expect(viewKitClasses(container).has("vk-table")).toBe(true);
    // Twice by design: the reading at the top and the row in the table below.
    expect(screen.getAllByText("acme.example.com").length).toBe(2);
    // Lifetimes are shown in the units an operator sets them in, and a zero
    // reads as the fallback it is rather than as "no time at all".
    expect(screen.getByText("15 minutes")).toBeTruthy();
    expect(screen.getByText("30 days")).toBeTruthy();
    // Several lifetimes are unset on this fixture, and each says so in the
    // same words -- a 0 is a fallback, not "no time at all".
    expect(screen.getAllByText("inherited from the environment").length).toBeGreaterThan(1);
    // The data-URI brand fields are reported, never printed.
    expect(screen.getByText("a logo is uploaded, no icon")).toBeTruthy();
    expect(screen.queryByText(/data:image\/png/)).toBeNull();
  });

  it("says why it does not edit", async () => {
    renderAdmin({}, "/admin/settings");
    await waitFor(() => expect(screen.getByText("Changing a setting")).toBeTruthy());
    expect(screen.getByText(/carries no role check of its own/)).toBeTruthy();
  });
});

describe("the admin sub-nav", () => {
  it("links the four surfaces and marks the current one", async () => {
    renderAdmin({}, "/admin/tokens");
    await waitFor(() => expect(screen.getByText("Sessions and tokens")).toBeTruthy());
    const nav = screen.getByRole("navigation", { name: "Administration" });
    for (const label of ["Overview", "Tokens", "Signing keys", "Settings"]) {
      expect(within(nav).getByRole("link", { name: label })).toBeTruthy();
    }
    expect(within(nav).getByRole("link", { name: "Tokens" }).getAttribute("aria-current")).toBe(
      "page",
    );
  });
});
