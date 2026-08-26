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
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import {
  Result,
  type AccessSummary,
  type Concept,
  type Connection,
  type Role,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { asQueryClient } from "./support/queryFake";

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
    displayCard: { primary: "version", secondary: "provider", tertiary: "deploymentId", status: "status" },
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
      deploymentId: "deploy-1", version: "2026.8.1", provider: "azure",
      status: "succeeded", updatedAt: "2026-08-08T07:00:00Z",
    }),
    row(DEPLOYMENT, "deploy-2", {
      deploymentId: "deploy-2", version: "2026.8.2", provider: "azure",
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
  domain: "",
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
    // The session behind this connection. Always a string on the wire
    // (the server fills it from the verified claims, empty for a
    // credential with no session); a fixture has none.
    sessionId: "",
    displayName: "Ops Person",
  };

  const query = asQueryClient({
    listConcepts: vi.fn(async () => concepts),
    getMyAccess: vi.fn(async () => access),
    executeNamed: vi.fn(async (_name: string, call: string) => {
      const match = /concept==([^,)\s]+)/.exec(call);
      const id = match?.[1] ?? "";
      return new Result({ bundle: { nodes: ROWS[id] ?? [] }, meta: { cursor: "" } });
    }),
  });

  const dispatcher = {
    sendAndWait: vi.fn(async () => {
      if (deployFails) throw new Error("stream closed");
      // `ok: true` is load-bearing, not fixture noise: the SDK client treats a
      // reply without it as a malformed one and throws, deliberately, so that a
      // protojson-omitted zero value cannot read as success.
      return {
        deployControlResult: { ok: true, errorCode: 0, deploymentStatus: DEPLOYMENT_STATUS },
      };
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
        redirectUri="https://api.example.com/portal/auth/callback"
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

// WHERE THE VIEWS LIVE IN THE CHROME (memql#4655, decision D1).
//
// They were rail rows: five built-in ones under a Built-in sub-section, the
// caller's composed ones under a Custom sub-section, both inside a Views
// caption, with the composer's door as the last Custom row. That shape did
// not scale -- a saved view was a permanent rail row, so forty views meant a
// rail forty rows longer -- and it asked the reader to learn a
// built-in-versus-composed distinction that is provenance rather than
// category.
//
// One rail row now, opening a gallery that lists both. What is asserted here
// is that the rows really left, that the gallery really carries them, and
// that no URL moved on the way -- rail placement is not URL shape, which is
// why this restructure needed no redirects.
describe("the Views destination", () => {
  function railOf(): HTMLElement {
    return screen.getByRole("navigation", { name: "Portal sections" });
  }

  it("is one rail row, and no view is a row of its own", async () => {
    renderView({}, "/views/users");
    const nav = railOf();
    await waitFor(() =>
      expect(within(nav).getByRole("link", { name: "Views" }).getAttribute("href")).toBe("/views"),
    );

    // No captions at all, and none of the rows the captions used to hold.
    expect(within(nav).queryAllByRole("heading", { level: 2 })).toHaveLength(0);
    for (const gone of ["Users", "Accounts", "Deployments", "Audit", "Agents", "Compose"]) {
      expect(within(nav).queryByRole("link", { name: gone })).toBeNull();
    }
    // ...including the disclosures that governed them.
    for (const name of ["Built-in", "Custom"]) {
      expect(within(nav).queryByRole("button", { name })).toBeNull();
    }
  });

  it("lights its row for a view, and for the composer that makes one", async () => {
    renderView({}, "/views/users");
    await waitFor(() => expect(screen.getByText("Ada Lovelace")).toBeTruthy());
    expect(
      within(railOf()).getByRole("link", { name: "Views" }).getAttribute("aria-current"),
    ).toBe("page");
  });

  it("lists the five built-in views as gallery cards, at their own URLs", async () => {
    renderView({}, "/views");
    const main = within(await waitFor(() => screen.getByRole("main")));
    for (const [label, href] of [
      ["Users", "/views/users"],
      ["Accounts", "/views/accounts"],
      ["Agents", "/views/agents"],
      ["Deployments", "/views/deployments"],
      ["Audit", "/views/audit"],
    ] as const) {
      const card = await waitFor(() => main.getByRole("link", { name: new RegExp(`^${label}`) }));
      // Agents is a card here rather than a row under Nexus, and its address
      // is the one it always had.
      expect(card.getAttribute("href")).toBe(href);
    }
  });

  it("absorbs the composer's rail row as the gallery's own action", async () => {
    renderView({}, "/views");
    // "New view" names the OUTCOME, which is what a gallery of views is
    // choosing among -- the rail said Compose because it sat beside the thing
    // the composer produces.
    await waitFor(() => expect(screen.getByRole("button", { name: "New view" })).toBeTruthy());
  });
});

describe("the Users view", () => {
  it("opens on a count, divides by role, and rolls out as a table", async () => {
    const { container } = renderView({}, "/views/users");
    await waitFor(() => expect(screen.getByText("Ada Lovelace")).toBeTruthy());

    // THE CONCEPT ID LEFT THE HEADER (memql#4657). What is above the title is
    // the area, in words; the id is in the Views guide under Technical
    // details, which is where the people who query rows will look.
    expect(screen.queryByText(USER)).toBeNull();

    const classes = viewKitClasses(container);
    expect(classes.has("vk-stat")).toBe(true);
    expect(classes.has("vk-chart-rail-seg")).toBe(true);
    expect(classes.has("vk-table")).toBe(true);

    // Band order is the grammar: reading, then shape, then roll. "Invited" is
    // an administrative addendum (memql#4272) and sits after the population --
    // the operator came to look at users, not at who is on their way in.
    //
    // It moved one place, from the end of the PAGE to the end of the users
    // SECTION, when the view became data (epic memql#4661): sessions are now a
    // second section over a second concept rather than a band of the first, and
    // an addendum about users belongs with the users. The intent the original
    // order expressed -- not at the top -- is what is asserted here.
    expect(bandTitles()).toEqual(["By role", "Everyone", "Invited", "Open sessions"]);

    // The rail divides on role, which is what this view designed for -- not
    // on the concept's declared status slot, which would be active/inactive.
    expect(screen.getByText("owner 1 (50%)")).toBeTruthy();
    expect(screen.getByText("reader 1 (50%)")).toBeTruthy();
  });

  it("shows the row count and never a summed revocation epoch", async () => {
    renderView({}, "/views/users");
    await waitFor(() => expect(screen.getByText("user rows")).toBeTruthy());
    // The stat strip declines the measure slot: the only number on a user row
    // is revocationEpoch, and its total is a true, meaningless figure.
    expect(screen.queryByText(/revocationEpoch/)).toBeNull();
  });

  it("opens a row's detail in a dialog, from the URL", async () => {
    renderView({}, "/views/users/rows/user-1");
    await waitFor(() => expect(screen.getByRole("dialog")).toBeTruthy());
    // The dialog preserves the wire's nesting, exactly as the concept
    // browser's pane does -- it is literally the same RowDetail.
    await waitFor(() => expect(within(screen.getByRole("dialog")).getByText("payload")).toBeTruthy());
    expect(screen.getByRole("dialog").className).toContain("max-w-2xl");
    expect(screen.queryByRole("complementary")).toBeNull();
  });
});

// The addresses these two views used to have (memql#4526). people -> users and
// customers -> accounts, tail included.
//
// These are worth their own tests rather than a glance at the route table for
// one specific reason, spelled out in RetiredViewRedirect's header: React
// Router scores a static segment above a dynamic one but PENALISES a splat, so
// the obvious `views/people/*` spelling loses to `views/:viewId/rows/:rowId`
// and a bookmarked ROW lands on "No such view" while the diff looks correct.
// The row cases below are the ones that would catch it.
describe("the retired view slugs", () => {
  it("sends /views/people to the Users view", async () => {
    renderView({}, "/views/people");
    await waitFor(() => expect(screen.getByText("Ada Lovelace")).toBeTruthy());
    // Landed on the Users view, asserted by its own heading rather than by
    // the concept id the header no longer carries.
    expect(within(screen.getByRole("main")).getByRole("heading", { name: "Users" })).toBeTruthy();
    expect(screen.queryByText(/has no view called/)).toBeNull();
  });

  it("sends /views/customers to the Accounts view", async () => {
    renderView({}, "/views/customers");
    await waitFor(() => expect(screen.getByText("Northwind Trading")).toBeTruthy());
    expect(within(screen.getByRole("main")).getByRole("heading", { name: "Accounts" })).toBeTruthy();
    expect(screen.queryByText(/has no view called/)).toBeNull();
  });

  it("carries the row segment, so a deep bookmark still opens its row", async () => {
    renderView({}, "/views/people/rows/user-1");
    await waitFor(() => expect(screen.getByRole("dialog")).toBeTruthy());
    await waitFor(() =>
      expect(within(screen.getByRole("dialog")).getByText("payload")).toBeTruthy(),
    );
  });

  it("carries it on the accounts slug too", async () => {
    renderView({}, "/views/customers/rows/account-1");
    await waitFor(() => expect(screen.getByRole("dialog")).toBeTruthy());
    await waitFor(() =>
      expect(within(screen.getByRole("dialog")).getByText("payload")).toBeTruthy(),
    );
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

  it("opens RowDetail in a dialog on row select and does not stack an aside", async () => {
    const { container } = renderView({}, "/views/agents");
    await waitFor(() => expect(screen.getByText("Sofia")).toBeTruthy());
    const row = container.querySelector('[data-row-id="agent-1"]');
    expect(row).toBeTruthy();
    fireEvent.click(row!);
    await waitFor(() => expect(screen.getByRole("dialog")).toBeTruthy());
    const dialog = screen.getByRole("dialog");
    await waitFor(() => expect(within(dialog).getByText("payload")).toBeTruthy());
    expect(dialog.className).toContain("max-w-2xl");
    expect(screen.queryByRole("complementary")).toBeNull();
    expect(screen.queryByRole("heading", { name: "Row detail" })).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByText("Sofia")).toBeTruthy();
    expect(screen.queryByRole("complementary")).toBeNull();
  });
});

describe("the Accounts view", () => {
  it("divides on the lifecycle the concept itself declares", async () => {
    renderView({}, "/views/accounts");
    await waitFor(() => expect(screen.getByText("Northwind Trading")).toBeTruthy());
    // No binding names `status` in the view: the concept declares it as its
    // display-card status slot and the rail prefers whatever that is.
    expect(screen.getByText("active 1 (50%)")).toBeTruthy();
    expect(screen.getByText("archived 1 (50%)")).toBeTruthy();
  });

  it("says which concept is missing rather than showing an empty ledger", async () => {
    renderView({ without: ACCOUNT }, "/views/accounts");
    await waitFor(() =>
      expect(screen.getByText(/publishes no concept called/)).toBeTruthy(),
    );
    // TWICE, and both are wanted: the page header's eyebrow still names the
    // concept the page is about, and the statement names the one that is
    // missing. Before the views became data (epic memql#4661) this state
    // replaced the whole page, header included, so a person landed on a page
    // that had stopped saying what it was.
    expect(screen.getAllByText(ACCOUNT).length).toBeGreaterThan(0);
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

  it("carries no routine mechanics -- those moved to v1:identity:authActivity", async () => {
    // memql#4328. The Trail is a generic concept walk over auditEvent with no
    // filter of any kind, so it shows whatever that concept contains. Four
    // actions moved off it -- refresh-token rotations, the blocked ones,
    // grace-window accepts and PAT-authenticated requests -- because they are
    // two orders of magnitude more numerous than the decisions and they made
    // the trail unreadable.
    //
    // Asserted against the FIXTURE as well as the render: the fixture is what
    // a reader takes as the shape of an audit row, and a mechanic reappearing
    // in it is how the split would quietly come undone.
    const mechanics = [
      "session_refreshed",
      "session_refresh_blocked",
      "grace_window_accept",
      "pat_auth_accepted",
    ];
    for (const r of ROWS[AUDIT]!) {
      const action = (r.payload as { action?: string }).action ?? "";
      expect(mechanics).not.toContain(action);
    }

    const { container } = renderView({}, "/views/audit");
    await waitFor(() => expect(screen.getByText("role_changed")).toBeTruthy());
    for (const action of mechanics) {
      expect(container.textContent).not.toContain(action);
    }
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
    await waitFor(() => expect(screen.getByText(/Could not read this deployment/)).toBeTruthy());
  });

  it("keeps deploy selection on history click and opens the row dialog from View", async () => {
    const { container } = renderView({ role: "owner" }, "/views/deployments");
    await waitFor(() => expect(screen.getByText("2026.8.1")).toBeTruthy());
    const entry = container.querySelector('.vk-timeline-entry[data-row-id="deploy-1"]');
    expect(entry).toBeTruthy();
    fireEvent.click(entry!);
    await waitFor(() =>
      expect(container.querySelector('.vk-timeline-entry[data-row-id="deploy-1"][data-selected="true"]')).toBeTruthy(),
    );
    expect(screen.getByRole("button", { name: "Deploy the selected version" }).hasAttribute("disabled")).toBe(false);
    expect(screen.queryByRole("dialog")).toBeNull();

    const view = container.querySelector('[data-vk-row-action="view"][data-vk-action-row-id="deploy-1"]');
    expect(view).toBeTruthy();
    fireEvent.click(view!);
    await waitFor(() => expect(screen.getByRole("dialog")).toBeTruthy());
    expect(screen.getByRole("dialog").className).toContain("max-w-2xl");
    expect(container.querySelector('.vk-timeline-entry[data-row-id="deploy-1"][data-selected="true"]')).toBeTruthy();
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByRole("button", { name: "Deploy the selected version" }).hasAttribute("disabled")).toBe(false);
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
      const { container } = renderView({}, "/views/users");
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
      const { container } = renderView({}, "/views/users");
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
