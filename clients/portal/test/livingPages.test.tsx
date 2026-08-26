// Living pages: override rows, resolution order, versions (epic memql#4661,
// tasks memql#4668 and memql#4669).
//
// FOUR CLAIMS, and each of them fails silently if it is not asserted:
//
//   1. RESOLUTION ORDER. Override, else seed, NEVER a model. A render path
//      that reached a provider would cost money on every page view and change
//      a page under somebody mid-read -- and it would look exactly like a
//      working page while doing it.
//
//   2. PER-USER SCOPE (spec D4). One person's regeneration must never repaint
//      another's console. That property is one filter conjunct plus one
//      @serverSet field; a test with two actors is what says it is real
//      rather than intended.
//
//   3. THE VERSION WALK. Original / v1 / v2, with revert expressed as an
//      APPEND -- a rollback that destroyed later versions would make browsing
//      your own history the gesture that ends it.
//
//   4. NOTHING IS WRITTEN ON A FAILURE. A page that half-regenerated would be
//      in a state nobody chose.

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import {
  Result,
  type AccessSummary,
  type Connection,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { asQueryClient } from "./support/queryFake";

const USER = "v1:identity:user";
const SESSION = "v1:identity:authSession";
const VIEW = "v1:portalviews:view";

const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
  domain: "",
};

function row(concept: string, id: string, payload: Record<string, unknown>, createdAt: string): Row {
  return { id, concept, type: "concept", createdBy: "system", createdAt, payload } as unknown as Row;
}

const USERS: Row[] = [
  row(USER, "user-1", { email: "ada@example.com", role: "owner", active: true }, "2026-08-01T00:00:00Z"),
  row(USER, "user-2", { email: "grace@example.com", role: "reader", active: true }, "2026-08-02T00:00:00Z"),
];

// An override that is deliberately NOT the seed: a gallery whose only element
// is a row list. If the page renders this, resolution preferred the override;
// if it renders a table and a proportion rail, it preferred the seed.
function overrideRow(
  id: string,
  ownerUserId: string,
  createdAt: string,
  title: string,
): Row {
  return row(
    VIEW,
    id,
    {
      ownerUserId,
      name: "views.users",
      kind: "override",
      targetPageId: "views.users",
      conceptIds: [USER],
      arrangements: [
        {
          conceptId: USER,
          layout: "gallery",
          elements: [{ element: "rowList", band: "roll", title }],
        },
      ],
      origin: "suggested",
      status: "active",
      updatedAt: createdAt,
    },
    createdAt,
  );
}

interface Harness {
  // The override versions this cluster holds for the signed-in actor, newest
  // first. Empty means the person has never regenerated the page.
  versions?: Row[];
  // Whose rows the cluster returns. The engine gates pageOverride on
  // ownerUserId==actor.userId; the fake enforces the same rule so a test can
  // hand it another person's row and watch it not arrive.
  actorUserId?: string;
  suggest?: "propose" | "refuse";
}

function renderUsers(harness: Harness = {}) {
  const actorUserId = harness.actorUserId ?? "user-1";
  const calls: { name: string; call: string }[] = [];
  const suggestCalls: { domain: string; payload: Record<string, unknown> }[] = [];

  const access: AccessSummary = {
    requestId: "r1",
    userId: actorUserId,
    primaryEmail: "ada@example.com",
    clusterRole: "owner",
    sessionId: "",
    displayName: "Ops Person",
  };

  const executeNamed = vi.fn(async (name: string, call: string) => {
    calls.push({ name, call });

    if (name.startsWith("pageOverride")) {
      // OWNERSHIP, enforced by the fake exactly as the engine's filter does.
      const mine = (harness.versions ?? []).filter(
        (v) => (v as unknown as { payload: { ownerUserId: string } }).payload.ownerUserId === actorUserId,
      );
      // The asOf walk: each step asks for versions strictly older than the
      // instant it names. A plain read returns the newest.
      const at = /asOf\([^,]+, "([^"]*)"\)/.exec(call)?.[1] ?? "";
      const eligible = at === "" ? mine : mine.filter((v) => v.createdAt < at);
      const newest = eligible[0];
      return new Result({
        bundle: { nodes: newest ? [newest] : [] },
        meta: { cursor: "" },
      });
    }

    if (name === "writePageOverride") {
      return new Result({ bundle: { nodes: [] }, meta: { cursor: "" } });
    }

    const browse = /concept==([^,)\s]+)/.exec(call);
    if (browse) {
      const id = browse[1];
      return new Result({
        bundle: { nodes: id === USER ? USERS : [] },
        meta: { cursor: "" },
      });
    }
    return new Result({ bundle: { nodes: [] }, meta: { cursor: "" } });
  });

  const query = asQueryClient({
    listConcepts: vi.fn(async () => [
      { id: USER, version: "v1", domain: "identity", entity: "user", type: "concept", description: "", displayCard: { primary: "email", secondary: "role", status: "active" } },
      { id: SESSION, version: "v1", domain: "identity", entity: "authSession", type: "concept", description: "" },
      { id: VIEW, version: "v1", domain: "portalviews", entity: "view", type: "concept", description: "" },
    ]),
    getMyAccess: vi.fn(async () => access),
    executeNamed,
  });

  const dispatcher = {
    sendAndWait: vi.fn(async (msg: Record<string, unknown>) => {
      const suggest = msg["aiSuggest"] as { domain: string; payload: Record<string, unknown> } | undefined;
      if (suggest === undefined) return {};
      suggestCalls.push(suggest);
      if (harness.suggest === "propose") {
        return {
          aiSuggestResult: {
            domain: suggest.domain,
            result: {
              reasoning: "Cards read better here.",
              layout: "gallery",
              elements: [{ element: "rowList", band: "roll", title: "Everyone, as cards" }],
            },
          },
        };
      }
      throw new Error("suggestions are not available on this cluster");
    }),
  };

  const connection = {
    query,
    dispatcher,
    subscriptions: { subscribeGraph: vi.fn(() => () => {}) },
    close: vi.fn(),
    done: vi.fn(() => new Promise<void>(() => {})),
  } as unknown as Connection;

  const dial = vi.fn(async () => connection) as unknown as typeof Connection.dial;

  render(
    <MemoryRouter initialEntries={["/views/users"]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("these tests make no identity calls");
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

  return { calls, suggestCalls };
}

describe("resolution order", () => {
  it("renders the seed when the caller has never regenerated the page", async () => {
    renderUsers();
    // The seed's roll band is a TABLE captioned "Everyone".
    await waitFor(() => expect(screen.getByText("Everyone")).toBeTruthy());
    expect(screen.queryByText("Regenerated by me")).toBeNull();
  });

  it("prefers the caller's override over the seed", async () => {
    renderUsers({ versions: [overrideRow("ov-1", "user-1", "2026-08-10T00:00:00Z", "Regenerated by me")] });
    await waitFor(() => expect(screen.getByText("Regenerated by me")).toBeTruthy());
    // ...and the seed's own caption is gone, which is what "prefers" means.
    expect(screen.queryByText("Everyone")).toBeNull();
  });

  it("never asks a model at render", async () => {
    // Spec D3, and the assertion is the whole point of the decision: a render
    // that reached a provider would cost money on every page view and change
    // a page under somebody mid-read. The suggester here THROWS, so a render
    // path that called it would also fail loudly -- but the count is what
    // proves nothing called it at all.
    const { suggestCalls } = renderUsers({ suggest: "refuse" });
    await waitFor(() => expect(screen.getByText("Everyone")).toBeTruthy());
    expect(suggestCalls).toEqual([]);
  });
});

describe("per-user scope", () => {
  it("does not show one person's regeneration to another", async () => {
    // The property spec D4 names, with two actors. The override below belongs
    // to user-2; the signed-in actor is user-1.
    renderUsers({
      actorUserId: "user-1",
      versions: [overrideRow("ov-2", "user-2", "2026-08-10T00:00:00Z", "Grace's arrangement")],
    });
    await waitFor(() => expect(screen.getByText("Everyone")).toBeTruthy());
    expect(screen.queryByText("Grace's arrangement")).toBeNull();
  });

  it("scopes the read by the ACTOR rather than by an argument", async () => {
    // There is no userId argument to get wrong: the query gates on
    // actor.userId server-side, so the call carries only the page id. A call
    // that named a user would be a call somebody could point at another one.
    const { calls } = renderUsers({
      versions: [overrideRow("ov-1", "user-1", "2026-08-10T00:00:00Z", "Mine")],
    });
    await waitFor(() => expect(screen.getByText("Mine")).toBeTruthy());
    const override = calls.find((c) => c.name.startsWith("pageOverride"));
    expect(override).toBeTruthy();
    expect(override!.call).toContain("views.users");
    expect(override!.call).not.toContain("user-1");
  });
});

describe("the version strip", () => {
  const twoVersions = [
    overrideRow("ov-1", "user-1", "2026-08-12T00:00:00Z", "The newest"),
    overrideRow("ov-1", "user-1", "2026-08-10T00:00:00Z", "The older one"),
  ];

  it("walks the history and opens on the newest version", async () => {
    renderUsers({ versions: twoVersions });
    await waitFor(() => expect(screen.getByText("The newest")).toBeTruthy());

    const strip = screen.getByRole("group", { name: "Page versions" });
    // Original is always there and always first -- it is the seed, it has no
    // row, and it costs nothing to keep.
    expect(strip.textContent).toContain("Original");
    expect(strip.textContent).toContain("v1");
    expect(strip.textContent).toContain("v2");
    // A person opening a page they regenerated sees what they regenerated it
    // to, not the Original with their work one click away.
    expect(screen.getByRole("button", { name: "v2" }).getAttribute("aria-current")).toBe("true");
  });

  it("previews an older version without writing anything", async () => {
    // A strip that saved on click would make browsing your own history the
    // gesture that ends it.
    const { calls } = renderUsers({ versions: twoVersions });
    await waitFor(() => expect(screen.getByText("The newest")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "v1" }));
    await waitFor(() => expect(screen.getByText("The older one")).toBeTruthy());
    expect(calls.some((c) => c.name === "writePageOverride")).toBe(false);
  });

  it("offers 'Use this version' only while previewing", async () => {
    renderUsers({ versions: twoVersions });
    await waitFor(() => expect(screen.getByText("The newest")).toBeTruthy());
    // Re-writing the newest as the newest is a write that changes nothing and
    // adds a version, so the control is ABSENT rather than disabled.
    expect(screen.queryByRole("button", { name: "Use this version" })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "v1" }));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Use this version" })).toBeTruthy(),
    );
  });

  it("reverts by APPENDING the chosen version rather than destroying later ones", async () => {
    const { calls } = renderUsers({ versions: twoVersions });
    await waitFor(() => expect(screen.getByText("The newest")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "v1" }));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Use this version" })).toBeTruthy(),
    );
    fireEvent.click(screen.getByRole("button", { name: "Use this version" }));

    await waitFor(() =>
      expect(calls.some((c) => c.name === "writePageOverride")).toBe(true),
    );
    const write = calls.find((c) => c.name === "writePageOverride")!;
    // An append onto the SAME row id: that is what makes the version list a
    // history rather than a pile of unrelated rows.
    expect(write.call).toContain('viewId: "ov-1"');
    expect(write.call).toContain('targetPageId: "views.users"');
    // Nothing deletes, archives or rolls back. There is one verb.
    expect(calls.some((c) => /archive|delete|rollback/i.test(c.name))).toBe(false);
  });

  it("does not render at all when there is only the Original", async () => {
    // A strip offering one choice is chrome that explains nothing.
    renderUsers();
    await waitFor(() => expect(screen.getByText("Everyone")).toBeTruthy());
    expect(screen.queryByRole("group", { name: "Page versions" })).toBeNull();
  });
});

describe("regenerating", () => {
  it("writes the repaired result as a new version", async () => {
    const { calls, suggestCalls } = renderUsers({ suggest: "propose" });
    await waitFor(() => expect(screen.getByText("Everyone")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Regenerate" }));
    await waitFor(() => expect(screen.getByPlaceholderText(/more visual/)).toBeTruthy());
    fireEvent.change(screen.getByPlaceholderText(/more visual/), {
      target: { value: "more visual" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Go" }));

    await waitFor(() => expect(suggestCalls.length).toBeGreaterThan(0));
    expect(suggestCalls[0]!.domain).toBe("viewArrangement");
    // The hint, the layout vocabulary and the current layout are what this
    // task added to the request; without them the reshaped prompt renders
    // sections that say nothing.
    expect(suggestCalls[0]!.payload["hint"]).toBe("more visual");
    expect(suggestCalls[0]!.payload["layouts"]).toBeTruthy();
    expect(suggestCalls[0]!.payload["currentLayout"]).toBeTruthy();

    await waitFor(() => expect(calls.some((c) => c.name === "writePageOverride")).toBe(true));
    const write = calls.find((c) => c.name === "writePageOverride")!;
    // The REPAIRED arrangement is what is stored -- the reply named one
    // element and the page's required entries put the population back.
    expect(write.call).toContain("rowList");
    expect(write.call).toContain('origin: "suggested"');
  });

  it("writes nothing and says so when the suggester refuses", async () => {
    const { calls } = renderUsers({ suggest: "refuse" });
    await waitFor(() => expect(screen.getByText("Everyone")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Regenerate" }));
    await waitFor(() => expect(screen.getByPlaceholderText(/more visual/)).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Go" }));

    await waitFor(() =>
      expect(screen.getByText(/not available on this cluster/)).toBeTruthy(),
    );
    expect(screen.getByText(/The page below is unchanged/)).toBeTruthy();
    expect(calls.some((c) => c.name === "writePageOverride")).toBe(false);
    // ...and the page is still the page.
    expect(screen.getByText("Everyone")).toBeTruthy();
  });
});
