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
function fakeConnection(
  overrides: Partial<Connection> = {},
  // Extra stubs merged into the QUERY rather than onto the connection. The
  // rail reads composed views on every shell render, and asQueryClient
  // deliberately defaults that call to an empty list (see
  // support/queryFake.ts) -- so a test about the Custom sub-section has to
  // supply its own rows here, not through `overrides`, which lands on the
  // Connection and never reaches the client.
  queryStub: Record<string, unknown> = {},
): Connection {
  const query = asQueryClient({
    listConcepts: vi.fn(async () => CONCEPTS),
    // The shell reads the caller's access to decide what the rail offers
    // (the Modules item is owner/admin-only, memql#4191).
    getMyAccess: vi.fn(async () => ({
      userId: "user-test",
      primaryEmail: "op@example.test",
      clusterRole: "admin",
      displayName: "Ada Lovelace",
    })),
    ...queryStub,
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
  //
  // The name became "Portal header" in memql#4316, when the header stopped
  // being about the cluster and the session and became the brand.
  return screen.getByRole("banner", { name: "Portal header" });
}

function rail(): HTMLElement {
  return screen.getByRole("navigation", { name: "Portal sections" });
}

function connectionTone(): string | null {
  return document.querySelector("[data-connection-tone]")?.getAttribute("data-connection-tone") ?? null;
}

describe("the shell chrome (memql#4240, restructured in memql#4316)", () => {
  afterEach(() => {
    globalThis.localStorage?.removeItem("memql-portal-rail");
  });

  it("puts the brand in the header and the machine facts in the rail footer", async () => {
    const dial = vi.fn(async () =>
      fakeConnection({ engineVersion: "v0.19.5" }),
    ) as unknown as typeof Connection.dial;
    renderApp(dial);

    // The wordmark is the header's content now. The cluster host that used to
    // sit here was window.location.host -- the address bar, retyped.
    await waitFor(() => expect(within(header()).getByText("MemQL Portal")).toBeTruthy());

    // The machine facts, in the footer.
    await waitFor(() => expect(within(rail()).getByText("v0.19.5")).toBeTruthy());
    expect(within(rail()).getByText("bff-test")).toBeTruthy();
    expect(within(rail()).queryByText("0.0.0-test")).toBeNull();

    // ...and NONE of them in the header, nor the wire version, nor anything
    // about the person.
    for (const stray of ["0.0.0-test", "v0.19.5", "v1", "bff-test", "Connected"]) {
      expect(within(header()).queryByText(stray)).toBeNull();
    }
    expect(within(header()).queryByRole("button", { name: "Sign out" })).toBeNull();
  });

  it("renders dev when engineVersion is empty", async () => {
    const dial = vi.fn(async () => fakeConnection()) as unknown as typeof Connection.dial;
    renderApp(dial);

    await waitFor(() => expect(within(rail()).getByText("dev")).toBeTruthy());
    expect(within(rail()).queryByText("0.0.0-test")).toBeNull();
  });

  it("carries exactly one live region, and it is the footer's", async () => {
    const dial = vi.fn(async () =>
      fakeConnection({ engineVersion: "v0.19.5" }),
    ) as unknown as typeof Connection.dial;
    renderApp(dial);

    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("Connected"));
    // getByRole throws on more than one match, which is the assertion: a
    // second live region would announce every transition twice.
    expect(within(rail()).getByRole("status")).toBeTruthy();
    expect(document.querySelectorAll("[data-connection-tone]")).toHaveLength(1);
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
    // ...and no profile row, because there is no person to link to on a
    // cluster that admits every dial as the synthetic local-dev owner.
    expect(document.querySelector("[data-profile-row]")).toBeNull();
  });

  it("collapses from a borderless handle on the rail's edge, and persists it", async () => {
    const dial = vi.fn(async () =>
      fakeConnection({ engineVersion: "v0.19.5" }),
    ) as unknown as typeof Connection.dial;
    renderSignedIn(dial);

    await waitFor(() => expect(within(rail()).getByText("Ada Lovelace")).toBeTruthy());

    const collapse = within(rail()).getByRole("button", { name: "Collapse the navigation rail" });
    expect(collapse.getAttribute("aria-expanded")).toBe("true");
    expect(collapse.className).not.toMatch(/\bborder\b/);
    // A CHILD OF THE NAV, and its first: the handle straddles the rail's own
    // right border, so a sibling parked outside would belong to neither
    // landmark.
    expect(rail().firstElementChild).toBe(collapse);
    // The ONLY control matching /navigation rail/. Two would mean the old
    // brand-row chevron survived alongside the new tab.
    expect(within(rail()).getAllByRole("button", { name: /navigation rail/ })).toHaveLength(1);

    fireEvent.click(collapse);
    expect(globalThis.localStorage.getItem("memql-portal-rail")).toBe("collapsed");

    // Same spot, flipped icon and state -- a control that relocates when you
    // use it costs a person the position they just learned.
    const expand = within(rail()).getByRole("button", { name: "Expand the navigation rail" });
    expect(expand.getAttribute("aria-expanded")).toBe("false");
    expect(rail().firstElementChild).toBe(expand);

    // Collapsed, the rail keeps the dot and the version; the node id and the
    // person move into tooltips.
    expect(within(rail()).queryByText("bff-test")).toBeNull();
    expect(within(rail()).getByText("v0.19.5")).toBeTruthy();
    expect(connectionTone()).toBe("ok");
    expect(screen.getByTitle(/bff-test/)).toBeTruthy();
  });
});

// The Views group's two sub-sections (memql#4527): Built-in and Custom, each
// a disclosure inside ONE Views caption.
//
// What these own is the part a source read cannot settle: that the control is
// a real disclosure (focusable, Enter/Space, aria-expanded), that closing one
// hides ONLY its own rows, that the choice survives a reload, and that the
// COLLAPSED icon rail throws the whole structure away rather than rendering
// sub-captions an icon column has no room for.
describe("the Views sub-sections (memql#4527)", () => {
  const SECTION_KEYS = [
    "memql-portal-rail-section-built-in",
    "memql-portal-rail-section-custom",
  ];

  afterEach(() => {
    globalThis.localStorage?.removeItem("memql-portal-rail");
    for (const key of SECTION_KEYS) globalThis.localStorage?.removeItem(key);
  });

  function signedIn() {
    const dial = vi.fn(async () =>
      fakeConnection({ engineVersion: "v0.19.5" }),
    ) as unknown as typeof Connection.dial;
    return renderSignedIn(dial);
  }

  function disclosure(name: string): HTMLElement {
    return within(rail()).getByRole("button", { name });
  }

  it("opens expanded, and each disclosure governs the list it names", async () => {
    signedIn();
    await waitFor(() => expect(within(rail()).getByText("Ada Lovelace")).toBeTruthy());

    // ONE Views caption. The Custom group that used to sit beside it is gone.
    expect(within(rail()).getByRole("heading", { name: "Views", level: 2 })).toBeTruthy();
    expect(within(rail()).queryByRole("heading", { name: "Custom", level: 2 })).toBeNull();

    for (const name of ["Built-in", "Custom"]) {
      const button = disclosure(name);
      // Default expanded: a person who has never touched the control gets the
      // whole rail.
      expect(button.getAttribute("aria-expanded")).toBe("true");
      // A native <button>, which is the whole keyboard story -- focusable in
      // source order, Enter and Space activate it. A <div> with a click
      // handler would look identical here and be unreachable.
      expect(button.tagName).toBe("BUTTON");
      expect(button.getAttribute("type")).toBe("button");
      const id = button.getAttribute("aria-controls") ?? "";
      expect(document.getElementById(id)).toBeTruthy();
    }
  });

  it("closes one sub-section without touching the other, and persists it", async () => {
    signedIn();
    await waitFor(() => expect(within(rail()).getByRole("link", { name: "Users" })).toBeTruthy());
    expect(within(rail()).getByRole("link", { name: "Compose" })).toBeTruthy();

    fireEvent.click(disclosure("Built-in"));

    // Its own rows leave the accessibility tree; Custom's stay. `hidden` is
    // what makes getByRole stop finding them, which is the same reason a
    // screen reader and the Tab order stop finding them.
    expect(disclosure("Built-in").getAttribute("aria-expanded")).toBe("false");
    await waitFor(() =>
      expect(within(rail()).queryByRole("link", { name: "Users" })).toBeNull(),
    );
    expect(within(rail()).queryByRole("link", { name: "Audit" })).toBeNull();
    expect(within(rail()).getByRole("link", { name: "Compose" })).toBeTruthy();
    expect(disclosure("Custom").getAttribute("aria-expanded")).toBe("true");

    // Beside the rail's own key, one key per section -- so a half-written
    // value takes down the section it belongs to and not the other.
    expect(globalThis.localStorage.getItem("memql-portal-rail-section-built-in")).toBe(
      "collapsed",
    );
    expect(globalThis.localStorage.getItem("memql-portal-rail-section-custom")).toBeNull();

    // Re-open, and the stored value follows the control rather than sticking
    // at whatever the first click wrote.
    fireEvent.click(disclosure("Built-in"));
    await waitFor(() => expect(within(rail()).getByRole("link", { name: "Users" })).toBeTruthy());
    expect(globalThis.localStorage.getItem("memql-portal-rail-section-built-in")).toBe(
      "expanded",
    );
  });

  it("reopens closed, because the choice is read back at mount", async () => {
    globalThis.localStorage.setItem("memql-portal-rail-section-custom", "collapsed");
    signedIn();
    await waitFor(() => expect(within(rail()).getByRole("link", { name: "Users" })).toBeTruthy());
    expect(disclosure("Custom").getAttribute("aria-expanded")).toBe("false");
    expect(within(rail()).queryByRole("link", { name: "Compose" })).toBeNull();
  });

  it("lists the operator's saved views inside Custom, above Compose", async () => {
    // The regression this guards is the restructure quietly dropping the
    // composer's output: Custom used to be a top-level group with its own
    // derivation, and it is now a sub-section fed by the same useSavedViews
    // hook. A rail that lost the saved views would still look correct.
    const dial = vi.fn(async () =>
      fakeConnection(
        { engineVersion: "v0.19.5" },
        {
          composedViews: async () => ({
            rows: () => [
              { id: "sv-1", name: "Churn watch", status: "active", conceptIds: [], arrangements: [] },
              { id: "sv-2", name: "Deploy health", status: "active", conceptIds: [], arrangements: [] },
            ],
            rawNodes: () => [],
            single: () => null,
            meta: () => null,
          }),
        },
      ),
    ) as unknown as typeof Connection.dial;
    renderSignedIn(dial);

    const custom = await waitFor(() => {
      const button = disclosure("Custom");
      const list = document.getElementById(button.getAttribute("aria-controls") ?? "");
      expect(list).toBeTruthy();
      return list as HTMLElement;
    });
    await waitFor(() =>
      expect(within(custom).getByRole("link", { name: "Churn watch" })).toBeTruthy(),
    );
    expect(within(custom).getByRole("link", { name: "Deploy health" })).toBeTruthy();

    // Compose is LAST, always -- the door to making another one, after the
    // ones already made.
    const rows = [...custom.querySelectorAll("a")].map((a) => a.textContent);
    expect(rows).toEqual(["Churn watch", "Deploy health", "Compose"]);

    // And they are under Views, not a caption of their own.
    expect(within(rail()).queryByRole("heading", { name: "Custom", level: 2 })).toBeNull();
  });

  it("flattens in the collapsed icon rail, closed sub-section and all", async () => {
    // Closed in the WIDE rail, and still rendered in the icon rail: an icon
    // column has no caption to explain why four destinations vanished, so
    // hiding them there would read as a bug rather than as a fold.
    globalThis.localStorage.setItem("memql-portal-rail-section-built-in", "collapsed");
    globalThis.localStorage.setItem("memql-portal-rail", "collapsed");
    signedIn();

    await waitFor(() => expect(within(rail()).getByRole("link", { name: "Users" })).toBeTruthy());
    expect(within(rail()).getByRole("link", { name: "Compose" })).toBeTruthy();

    // No captions and no disclosure controls -- the rule the group captions
    // already follow.
    expect(within(rail()).queryByRole("heading", { name: "Views", level: 2 })).toBeNull();
    for (const name of ["Built-in", "Custom"]) {
      expect(within(rail()).queryByRole("button", { name })).toBeNull();
    }

    // The rows keep their accessible names, which is what a collapsed row has
    // instead of a label.
    expect(within(rail()).getByRole("link", { name: "Users" }).getAttribute("title")).toBe(
      "Users",
    );
  });
});

describe("the rail's profile row (memql#4317)", () => {
  afterEach(() => {
    globalThis.localStorage?.removeItem("memql-portal-rail");
  });

  it("links to /me and shows the name, the email and the role", async () => {
    const dial = vi.fn(async () =>
      fakeConnection({ engineVersion: "v0.19.5" }),
    ) as unknown as typeof Connection.dial;
    renderSignedIn(dial);

    await waitFor(() => expect(within(rail()).getByText("Ada Lovelace")).toBeTruthy());
    expect(within(rail()).getByText("op@example.test")).toBeTruthy();
    expect(within(rail()).getByText("admin")).toBeTruthy();

    const row = document.querySelector("[data-profile-row]");
    expect(row?.getAttribute("href")).toBe("/me");

    // The three facts belong to the PERSON, so none of them is in the header.
    for (const stray of ["Ada Lovelace", "op@example.test", "admin"]) {
      expect(within(header()).queryByText(stray)).toBeNull();
    }
  });

  it("shows the avatar alone when collapsed, with the facts in the tooltip", async () => {
    const dial = vi.fn(async () =>
      fakeConnection({ engineVersion: "v0.19.5" }),
    ) as unknown as typeof Connection.dial;
    renderSignedIn(dial);

    await waitFor(() => expect(within(rail()).getByText("Ada Lovelace")).toBeTruthy());
    fireEvent.click(within(rail()).getByRole("button", { name: "Collapse the navigation rail" }));

    expect(within(rail()).queryByText("Ada Lovelace")).toBeNull();
    expect(within(rail()).queryByText("op@example.test")).toBeNull();

    const row = document.querySelector("[data-profile-row]");
    // Both, and they must agree: the tooltip is what a pointer gets and the
    // accessible name is what a screen reader gets, and the second must not
    // be the poorer of the two.
    expect(row?.getAttribute("title")).toBe("Ada Lovelace · op@example.test · admin");
    expect(row?.getAttribute("aria-label")).toBe("Ada Lovelace · op@example.test · admin");
    // The initials are the first and family name, and they are hidden from
    // the accessible tree because the link already carries the name.
    const avatar = row?.querySelector("[data-avatar]");
    expect(avatar?.textContent).toBe("AL");
    expect(avatar?.getAttribute("aria-hidden")).toBe("true");
  });

  it("carries the active style while any /me facet is open", async () => {
    const dial = vi.fn(async () =>
      fakeConnection({ engineVersion: "v0.19.5" }),
    ) as unknown as typeof Connection.dial;
    renderSignedIn(dial, "/me/sessions");

    await waitFor(() => expect(within(rail()).getByText("Ada Lovelace")).toBeTruthy());
    const row = document.querySelector("[data-profile-row]");
    // The nav-row recipe's active edge, on a SUB-route: the row is the
    // person's whole surface, not just its index.
    expect(row?.className).toMatch(/border-accent/);
  });

  it("renders a skeleton rather than a half-identity while the read is in flight", async () => {
    let release!: (access: unknown) => void;
    const held = new Promise((resolve) => {
      release = resolve;
    });
    const dial = vi.fn(async () =>
      fakeConnection({
        engineVersion: "v0.19.5",
        query: asQueryClient({
          listConcepts: vi.fn(async () => CONCEPTS),
          getMyAccess: vi.fn(() => held),
        }),
      } as unknown as Partial<Connection>),
    ) as unknown as typeof Connection.dial;
    renderSignedIn(dial);

    await waitFor(() =>
      expect(document.querySelector("[data-profile-skeleton]")).toBeTruthy(),
    );
    // NOT the email with an ellipsis: a name that is about to change reads as
    // a name, and a person who glances at the wrong one has been told
    // something false about which account they are in.
    expect(within(rail()).queryByText("op@example.test")).toBeNull();

    release({
      userId: "user-test",
      primaryEmail: "op@example.test",
      clusterRole: "admin",
      displayName: "Ada Lovelace",
    });
    await waitFor(() => expect(within(rail()).getByText("Ada Lovelace")).toBeTruthy());
  });

  it("falls back to the email, and never takes the shell down, when the name is absent", async () => {
    // A node that predates display_name sends nothing. AccessSummary is a
    // WIRE shape, so the type promising a string is not the same as the
    // payload carrying one -- and this row renders on every paint of the
    // chrome, so a throw here is the whole console.
    const dial = vi.fn(async () =>
      fakeConnection({
        engineVersion: "v0.19.5",
        query: asQueryClient({
          listConcepts: vi.fn(async () => CONCEPTS),
          getMyAccess: vi.fn(async () => ({
            userId: "user-test",
            primaryEmail: "op@example.test",
            clusterRole: "admin",
          })),
        }),
      } as unknown as Partial<Connection>),
    ) as unknown as typeof Connection.dial;
    renderSignedIn(dial);

    await waitFor(() => expect(within(rail()).getByText("op@example.test")).toBeTruthy());
    expect(document.querySelector("[data-profile-row]")?.getAttribute("href")).toBe("/me");
    // The shell is still standing.
    expect(within(header()).getByText("MemQL Portal")).toBeTruthy();
  });
});
