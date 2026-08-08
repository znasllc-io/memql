// The five predefined views (memql#3319), end to end against a fake cluster.
//
// WHAT THESE OWN. portal_view_composition_test.go (repo root) proves the view
// modules CANNOT hand-render a row -- it reads source, so it covers branches
// no fixture reaches. This file proves the other half, which a source scan
// cannot: that each view actually composes the elements it claims to, that the
// bands appear in the designed order, and that the deployments view shows an
// operator only the actions their role can take.
//
// The element renderers themselves are view-kit's business and are tested
// there. What is asserted here is the COMPOSITION -- that a role rail exists
// on People and a timeline on Audit, and that both arrived through view-kit's
// own markup contract (the vk- classes) rather than something this repo drew.

import { describe, expect, it, vi } from "vitest";
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
const AGENT = "v1:agents:agent";
const ACCOUNT = "v1:identity:account";
const DEPLOYMENT = "v1:cluster:deployment";
const AUDIT = "v1:identity:auditEvent";
const SESSION = "v1:identity:authSession";
const GRANT = "v1:agents:agentAuthorization";

// The display cards are the REAL ones from dsl/, because that is what the
// views compose against: a rail that finds the status slot here finds it in
// the cluster too.
const CONCEPTS: Concept[] = [
  {
    id: USER, version: "v1", domain: "identity", entity: "user", type: "concept",
    description: "A person who can sign in",
    displayCard: { primary: "displayName", secondary: "role", tertiary: "primaryEmail", status: "active" },
  },
  {
    id: AGENT, version: "v1", domain: "agents", entity: "agent", type: "concept",
    description: "An AI assistant template",
    displayCard: { primary: "name", secondary: "role", tertiary: "ownerUserId", status: "active" },
  },
  {
    id: ACCOUNT, version: "v1", domain: "identity", entity: "account", type: "concept",
    description: "A customer the operator manages",
    displayCard: { primary: "name", secondary: "description", tertiary: "primaryContactEmail", status: "status" },
  },
  {
    id: DEPLOYMENT, version: "v1", domain: "cluster", entity: "deployment", type: "concept",
    description: "A persisted deployment record",
    displayCard: { primary: "version", secondary: "environment", tertiary: "deploymentId", status: "status" },
  },
  {
    id: AUDIT, version: "v1", domain: "identity", entity: "auditEvent", type: "concept",
    description: "Append-only audit trail",
    displayCard: { primary: "action", secondary: "category", tertiary: "actorEmail", status: "outcome" },
  },
  {
    id: SESSION, version: "v1", domain: "identity", entity: "authSession", type: "concept",
    description: "A per-token session record",
    displayCard: { primary: "subject", secondary: "clientLabel", tertiary: "lastActivityAt", status: "source" },
  },
  {
    id: GRANT, version: "v1", domain: "agents", entity: "agentAuthorization", type: "concept",
    description: "A standing authorization",
    displayCard: { primary: "action", secondary: "agentId", tertiary: "userId", status: "active" },
  },
];

// Wire-shaped rows: payload NESTED, alongside the intrinsics. The flatten that
// merges them is src/viewkit/rows.ts, and exercising it is part of the point.
function row(concept: string, id: string, payload: Record<string, unknown>): Row {
  return {
    id,
    concept,
    type: "concept",
    createdBy: "system",
    createdAt: "2026-08-08T10:00:00Z",
    payload,
  };
}

const ROWS: Readonly<Record<string, Row[]>> = {
  [USER]: [
    row(USER, "user-1", {
      displayName: "Ada Lovelace", primaryEmail: "ada@example.com",
      role: "owner", active: true, lastSeenAt: "2026-08-08T09:00:00Z",
    }),
    row(USER, "user-2", {
      displayName: "Grace Hopper", primaryEmail: "grace@example.com",
      role: "reader", active: true, lastSeenAt: "2026-08-07T09:00:00Z",
    }),
  ],
  [AGENT]: [
    row(AGENT, "agent-1", { name: "Sofia", kind: "assistant", role: "assistant", active: true }),
    row(AGENT, "agent-2", { name: "Faye", kind: "specialist", role: "specialist", active: true }),
  ],
  [ACCOUNT]: [
    row(ACCOUNT, "account-1", {
      name: "Northwind Trading", status: "active",
      primaryContactEmail: "ops@northwind.example", updatedAt: "2026-08-08T08:00:00Z",
    }),
    row(ACCOUNT, "account-2", {
      name: "Contoso Bakery", status: "archived",
      primaryContactEmail: "hi@contoso.example", updatedAt: "2026-07-01T08:00:00Z",
    }),
  ],
  [DEPLOYMENT]: [
    row(DEPLOYMENT, "deploy-1", {
      deploymentId: "deploy-1", version: "2026.8.1", environment: "staging",
      status: "succeeded", updatedAt: "2026-08-08T07:00:00Z",
    }),
    row(DEPLOYMENT, "deploy-2", {
      deploymentId: "deploy-2", version: "2026.8.2", environment: "staging",
      status: "failed", updatedAt: "2026-08-08T08:30:00Z",
    }),
  ],
  [AUDIT]: [
    row(AUDIT, "audit-1", {
      action: "login_succeeded", category: "auth", outcome: "success",
      actorEmail: "ada@example.com", occurredAt: "2026-08-08T06:00:00Z",
    }),
    row(AUDIT, "audit-2", {
      action: "role_changed", category: "admin", outcome: "failure",
      actorEmail: "grace@example.com", occurredAt: "2026-08-08T06:30:00Z",
    }),
  ],
  [SESSION]: [
    row(SESSION, "session-1", {
      subject: "user-1", clientLabel: "Firefox on Linux", source: "bff_exchange",
      lastActivityAt: "2026-08-08T09:30:00Z",
    }),
  ],
  [GRANT]: [
    row(GRANT, "grant-1", {
      action: "createSpecialist", agentId: "agent-1", userId: "user-1",
      planKind: "userGoal", active: true,
    }),
  ],
};

const DEPLOYMENT_STATUS = {
  env: "staging",
  version: "2026.8.2",
  engineVersion: "2026.8.2",
  gate: "pass",
  components: [{ name: "bff", digest: "sha256:abc", repo: "acrmemql.azurecr.io/memql" }],
  argocd: { syncStatus: "Synced", healthStatus: "Healthy", outOfSync: false },
  rollouts: [],
  gateResult: {
    result: "pass",
    ranAt: "2026-08-08T08:00:00Z",
    legs: [
      { name: "smoke", passed: true, detail: "12 checks" },
      { name: "migrations", passed: false, detail: "one pending" },
    ],
  },
};

const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
};

interface Harness {
  role: Role;
  // Reject every deploy-console call, to exercise the read-failure branch.
  deployFails?: boolean;
  // Drop a concept from the registry, to exercise the missing-concept branch.
  without?: string;
}

function renderView({ role = "owner", deployFails, without }: Partial<Harness>, path: string) {
  const concepts = CONCEPTS.filter((c) => c.id !== without);
  const access: AccessSummary = {
    requestId: "r1",
    userId: "user-1",
    primaryEmail: "ada@example.com",
    clusterRole: role,
  };

  const query = {
    listConcepts: vi.fn(async () => concepts),
    getMyAccess: vi.fn(async () => access),
    executeNamed: vi.fn(async (_name: string, call: string) => {
      const match = /concept==([^,)\s]+)/.exec(call);
      const id = match?.[1] ?? "";
      return new Result({ bundle: { nodes: ROWS[id] ?? [] }, meta: { cursor: "" } });
    }),
  } as unknown as QueryClient;

  const dispatcher = {
    sendAndWait: vi.fn(async () => {
      if (deployFails) throw new Error("stream closed");
      return { deployControlResult: { deploymentStatus: DEPLOYMENT_STATUS } };
    }),
  };

  const dial = vi.fn(
    async () =>
      ({
        nodeId: "bff-test",
        serverVersion: "0.0.0-test",
        query,
        dispatcher,
        close: vi.fn(),
        done: vi.fn(() => new Promise<void>(() => {})),
      }) as unknown as Connection,
  ) as unknown as typeof Connection.dial;

  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("the view tests must make no identity calls");
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
}

// Everything a view draws comes out of view-kit, so every band's markup
// carries a vk- class. Asserting on the class rather than on the text is what
// makes these tests about COMPOSITION: text could be printed by anything.
function viewKitClasses(container: HTMLElement): Set<string> {
  const out = new Set<string>();
  // getAttribute, not `.className`: an SVG element's className is an
  // SVGAnimatedString, and stringifying it yields "[object SVGAnimatedString]"
  // -- which would silently report every chart mark as unclassed.
  for (const node of container.querySelectorAll("[class]")) {
    for (const cls of (node.getAttribute("class") ?? "").split(/\s+/)) {
      if (cls.startsWith("vk-")) out.add(cls);
    }
  }
  return out;
}

// bandTitles reads the band labels of the view itself. Scoped to <main>: the
// nav rail's group headings are h2s too, and counting them would make the
// grammar assertion depend on the shell.
function bandTitles(): (string | null)[] {
  const main = screen.getByRole("main");
  return [...main.querySelectorAll("h2")].map((h) => h.textContent);
}

describe("the nav rail", () => {
  it("groups the designed surfaces apart from the concept registry", async () => {
    renderView({}, "/views/people");
    await waitFor(() => expect(screen.getByText("Operate")).toBeTruthy());
    expect(screen.getByText("Explore")).toBeTruthy();
    const nav = screen.getByRole("navigation", { name: "Portal sections" });
    for (const label of ["People", "Agents", "Customers", "Deployments", "Audit"]) {
      expect(within(nav).getByRole("link", { name: label })).toBeTruthy();
    }
    expect(within(nav).getByRole("link", { name: "Concepts" })).toBeTruthy();
  });
});

describe("the People view", () => {
  it("opens on a count, divides by role, and rolls out as a table", async () => {
    const { container } = renderView({}, "/views/people");
    await waitFor(() => expect(screen.getByText("Ada Lovelace")).toBeTruthy());

    // The eyebrow names the concept the page is about -- the one thing that
    // tells an operator which rows these are.
    expect(screen.getByText(USER)).toBeTruthy();

    const classes = viewKitClasses(container);
    expect(classes.has("vk-stat")).toBe(true);
    expect(classes.has("vk-chart-rail-seg")).toBe(true);
    expect(classes.has("vk-table")).toBe(true);

    // Band order is the grammar: reading, then shape, then roll.
    expect(bandTitles()).toEqual(["By role", "Everyone", "Open sessions"]);

    // The rail divides on role, which is what this view designed for -- not
    // on the concept's declared status slot, which would be active/inactive.
    expect(screen.getByText("owner 1 (50%)")).toBeTruthy();
    expect(screen.getByText("reader 1 (50%)")).toBeTruthy();
  });

  it("shows the row count and never a summed revocation epoch", async () => {
    renderView({}, "/views/people");
    await waitFor(() => expect(screen.getByText("user rows")).toBeTruthy());
    // The stat strip declines the measure slot: the only number on a user row
    // is revocationEpoch, and its total is a true, meaningless figure.
    expect(screen.queryByText(/revocationEpoch/)).toBeNull();
  });

  it("opens a row's detail beside the population, from the URL", async () => {
    renderView({}, "/views/people/rows/user-1");
    await waitFor(() => expect(screen.getByText("Row detail")).toBeTruthy());
    // The detail pane preserves the wire's nesting, exactly as the concept
    // browser's does -- it is literally the same component.
    await waitFor(() => expect(screen.getByText("payload")).toBeTruthy());
  });
});

describe("the Agents view", () => {
  it("composes a fleet table and a board of standing grants", async () => {
    const { container } = renderView({}, "/views/agents");
    await waitFor(() => expect(screen.getByText("Sofia")).toBeTruthy());
    // The grants are a SECOND population on their own walk, so they settle
    // independently of the fleet.
    await waitFor(() => expect(screen.getByText("createSpecialist")).toBeTruthy());

    const classes = viewKitClasses(container);
    expect(classes.has("vk-table")).toBe(true);
    expect(classes.has("vk-board")).toBe(true);

    // The rail divides the fleet by kind.
    expect(screen.getByText("assistant 1 (50%)")).toBeTruthy();
  });
});

describe("the Customers view", () => {
  it("divides on the lifecycle the concept itself declares", async () => {
    renderView({}, "/views/customers");
    await waitFor(() => expect(screen.getByText("Northwind Trading")).toBeTruthy());
    // No binding names `status` in the view: the concept declares it as its
    // display-card status slot and the rail prefers whatever that is.
    expect(screen.getByText("active 1 (50%)")).toBeTruthy();
    expect(screen.getByText("archived 1 (50%)")).toBeTruthy();
  });

  it("says which concept is missing rather than showing an empty ledger", async () => {
    renderView({ without: ACCOUNT }, "/views/customers");
    await waitFor(() =>
      expect(screen.getByText(/publishes no concept called/)).toBeTruthy(),
    );
    expect(screen.getByText(ACCOUNT)).toBeTruthy();
  });
});

describe("the Audit view", () => {
  it("rolls out as a dated timeline with an outcome badge", async () => {
    const { container } = renderView({}, "/views/audit");
    await waitFor(() => expect(screen.getByText("role_changed")).toBeTruthy());

    const classes = viewKitClasses(container);
    expect(classes.has("vk-timeline")).toBe(true);
    expect(classes.has("vk-row-status")).toBe(true);

    // Two shape bands, because an audit trail divides two ways that matter.
    expect(bandTitles()).toEqual(["By outcome", "By category", "The trail"]);

    // The badge carries the raw value for the stylesheet, which is what the
    // portal's status colours key on.
    const badge = container.querySelector('.vk-row-status[data-status="failure"]');
    expect(badge).toBeTruthy();
  });
});

describe("the Deployments view", () => {
  it("shows an owner every action, including the one only they have", async () => {
    renderView({ role: "owner" }, "/views/deployments");
    // The version appears twice on purpose -- as the live reading and as the
    // newest entry in the history -- so this anchors on the reading's size.
    await waitFor(() => expect(screen.getAllByText("2026.8.2").length).toBe(2));
    expect(screen.getByRole("button", { name: "Cut a patch version" })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Roll back/ })).toBeTruthy();
  });

  it("shows a developer cut and deploy, and no rollback", async () => {
    renderView({ role: "developer" }, "/views/deployments");
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Cut a patch version" })).toBeTruthy(),
    );
    // Rollback is owner-only server-side; offering it and then failing the
    // call would waste the operator's time and teach them nothing.
    expect(screen.queryByRole("button", { name: /Roll back/ })).toBeNull();
    // A developer cannot read live state either, so the page says so instead
    // of leaving an empty panel.
    expect(screen.getByText(/needs the admin or owner role/)).toBeTruthy();
  });

  it("shows a reader the history and no actions at all", async () => {
    renderView({ role: "reader" }, "/views/deployments");
    await waitFor(() => expect(screen.getByText("2026.8.1")).toBeTruthy());
    expect(screen.queryByRole("button", { name: /Cut a/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /Roll back/ })).toBeNull();
    expect(screen.getByText(/needs the admin or owner role/)).toBeTruthy();
  });

  it("renders the deploy gate through the checklist element", async () => {
    const { container } = renderView({ role: "owner" }, "/views/deployments");
    await waitFor(() => expect(screen.getByText("smoke")).toBeTruthy());
    // A gate leg is a named thing that passed or did not, which is what the
    // checklist renders -- adapted into a row set rather than drawn by hand.
    expect(viewKitClasses(container).has("vk-checklist")).toBe(true);
    const failing = container.querySelector('[data-vk-done="false"]');
    expect(failing?.textContent).toContain("migrations");
  });

  it("says the read failed rather than showing a stale-looking blank", async () => {
    renderView({ role: "owner", deployFails: true }, "/views/deployments");
    await waitFor(() => expect(screen.getByText(/Could not read staging/)).toBeTruthy());
  });
});

describe("light and dark", () => {
  // The token layer resolves everything else in CSS, but a chart palette
  // cannot be theme-neutral: view-kit ships one validated set per theme and
  // picks with prefers-color-scheme, which is the wrong answer for an operator
  // who chose dark on a light OS. The portal stamps the RESOLVED theme onto
  // every element it renders, and this is the evidence that it does.
  it("stamps the operator's chosen theme onto the chart palette", async () => {
    document.documentElement.setAttribute("data-theme", "dark");
    try {
      const { container } = renderView({}, "/views/people");
      await waitFor(() => expect(screen.getByText("Ada Lovelace")).toBeTruthy());
      const figure = container.querySelector(".vk-chart-figure");
      expect(figure?.getAttribute("data-vk-theme")).toBe("dark");
    } finally {
      document.documentElement.removeAttribute("data-theme");
    }
  });

  it("stamps light when that is what is on screen", async () => {
    document.documentElement.setAttribute("data-theme", "light");
    try {
      const { container } = renderView({}, "/views/people");
      await waitFor(() => expect(screen.getByText("Ada Lovelace")).toBeTruthy());
      const figure = container.querySelector(".vk-chart-figure");
      expect(figure?.getAttribute("data-vk-theme")).toBe("light");
    } finally {
      document.documentElement.removeAttribute("data-theme");
    }
  });
});

describe("an unknown view id", () => {
  it("says so instead of rendering an empty frame", async () => {
    renderView({}, "/views/nonesuch");
    await waitFor(() => expect(screen.getByText("No such view")).toBeTruthy());
  });
});
