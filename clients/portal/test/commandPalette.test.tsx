// The command palette (memql#4656).
//
// It is the load-bearing half of the rail restructure: cutting seventeen rail
// rows to seven is only an improvement if nothing became unreachable. So the
// assertions divide in two -- the MATCHER's ranking, which is what makes the
// right entry first, and the palette's SOURCES, which is what makes every
// destination the rail lost still reachable.

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";
import type { Concept, Connection } from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { NO_MATCH, rank, scoreMatch } from "../src/palette/matcher";
import { asQueryClient } from "./support/queryFake";

describe("the matcher", () => {
  it("ranks a prefix above a word start above a substring above a scatter", () => {
    // The tiers, in order, over ONE query so the comparison is real.
    const prefix = scoreMatch("con", "Concepts");
    const wordStart = scoreMatch("con", "Sessions and connections");
    const substring = scoreMatch("con", "Reconcile");
    const scatter = scoreMatch("con", "Cluster operations node");
    expect(prefix).toBeGreaterThan(wordStart);
    expect(wordStart).toBeGreaterThan(substring);
    expect(substring).toBeGreaterThan(scatter);
    expect(scatter).toBeGreaterThan(NO_MATCH);
  });

  it("treats a colon as a word boundary, which is how concept ids are typed", () => {
    // Nobody types "v1:cluster:" to find a node.
    expect(scoreMatch("node", "v1:cluster:node")).toBeGreaterThan(
      scoreMatch("node", "the cluster's own sandboxed working directories"),
    );
  });

  it("says no when the letters are not there in order", () => {
    expect(scoreMatch("zzz", "Concepts")).toBe(NO_MATCH);
    expect(scoreMatch("stpecno", "Concepts")).toBe(NO_MATCH);
  });

  it("matches everything on an empty query, in source order", () => {
    const items = [{ label: "Console" }, { label: "Nexus" }, { label: "Views" }];
    expect(rank("", items).map((i) => i.label)).toEqual(["Console", "Nexus", "Views"]);
  });

  it("lets a hint match, but never above a label", () => {
    const items = [
      { label: "node", hint: "v1:cluster:node" },
      { label: "Nothing at all", hint: "node lives in here somewhere" },
    ];
    expect(rank("node", items)[0]?.label).toBe("node");
  });

  it("prefers the shorter of two equally-good hits", () => {
    expect(scoreMatch("view", "Views")).toBeGreaterThan(
      scoreMatch("view", "Views of everything this cluster holds"),
    );
  });
});

const CONCEPTS: Concept[] = [
  {
    id: "v1:cluster:node",
    version: "v1",
    domain: "cluster",
    entity: "node",
    description: "A registered node",
    type: "concept",
  },
];

function renderShell(path = "/concepts", role = "owner") {
  const dial = vi.fn(
    async () =>
      ({
        nodeId: "bff-test",
        serverVersion: "0.0.0-test",
        engineVersion: "v0.19.5",
        query: asQueryClient({
          listConcepts: vi.fn(async () => CONCEPTS),
          getMyAccess: vi.fn(async () => ({
            userId: "user-1",
            primaryEmail: "ada@example.test",
            clusterRole: role,
            displayName: "Ada Lovelace",
          })),
          composedViews: async () => ({
            rows: () => [
              { id: "sv-1", name: "Churn watch", status: "active", conceptIds: [], arrangements: [] },
            ],
            rawNodes: () => [],
            single: () => null,
            meta: () => null,
          }),
          executeNamed: vi.fn(async () => ({ rows: () => [], rawNodes: () => [], single: () => null, meta: () => null })),
        }),
        close: vi.fn(),
        done: vi.fn(() => new Promise<void>(() => {})),
      }) as unknown as Connection,
  ) as unknown as typeof Connection.dial;

  function Where(): null {
    const location = useLocation();
    (globalThis as Record<string, unknown>).__palettePath = location.pathname + location.search;
    return null;
  }

  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider
        config={{ identityUrl: "", identityApiBaseUrl: "", oauthClientId: "", authEnabled: false, domain: "" }}
        fetchImpl={async () => {
          throw new Error("the palette tests make no identity calls");
        }}
        storage={null}
        navigate={() => {}}
        redirectUri="https://api.example.test/auth/callback"
      >
        <ClusterProvider dial={dial}>
          <Where />
          <AppRoutes />
        </ClusterProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}

// The cluster is declared auth-DISABLED here, which is the configuration with
// nothing to sign in to -- so there is no profile row to wait on (the shell
// renders none for the synthetic local-dev owner). The rail's first row is the
// honest "the shell is up" signal.
function railReady(): boolean {
  return (
    screen
      .queryByRole("navigation", { name: "Portal sections" })
      ?.querySelector("ul a") !== null
  );
}

function openPalette(): void {
  fireEvent.keyDown(window, { key: "k", metaKey: true });
}

function box(): HTMLInputElement {
  return screen.getByRole("combobox", { name: /Search everywhere you can go/i });
}

function optionLabels(): (string | null)[] {
  return [...screen.getByRole("listbox").querySelectorAll('[role="option"]')].map(
    (el) => el.querySelector("span")?.textContent ?? null,
  );
}

describe("the palette", () => {
  it("opens on Cmd+K from a routed page, and closes on Escape", async () => {
    renderShell();
    await waitFor(() => expect(railReady()).toBe(true));
    expect(screen.queryByRole("listbox")).toBeNull();

    openPalette();
    await waitFor(() => expect(screen.getByRole("listbox")).toBeTruthy());

    fireEvent.keyDown(box(), { key: "Escape" });
    // The native <dialog> owns Escape; the component's own close is what the
    // dialog's onClose fires. Either way the list goes.
    await waitFor(() => expect(screen.queryByRole("listbox")).toBeNull());
  });

  it("carries everything that left the rail", async () => {
    renderShell();
    await waitFor(() => expect(railReady()).toBe(true));
    openPalette();
    await waitFor(() => expect(screen.getByRole("listbox")).toBeTruthy());

    const labels = optionLabels();
    // Destinations, the tabs that absorbed the old rows, a built-in view, the
    // caller's own composed view, a concept, and an action.
    for (const wanted of [
      "Console",
      "Fleet",
      "Machines",
      "Workbenches",
      "Signing keys",
      "Users",
      "Churn watch",
      "node",
      "New view",
    ]) {
      expect(`${wanted}: ${labels.includes(wanted)}`).toBe(`${wanted}: true`);
    }
  });

  it("offers a reader nothing an admin-only surface would refuse", async () => {
    renderShell("/concepts", "reader");
    await waitFor(() => expect(railReady()).toBe(true));
    openPalette();
    await waitFor(() => expect(screen.getByRole("listbox")).toBeTruthy());

    const labels = optionLabels();
    // The reachable positive first: the palette is populated at all.
    expect(labels).toContain("Console");
    // Cluster's only tab for a reader, and it is still findable BY ITS OWN
    // NAME -- the rail row says "Cluster" and the page says "Integrations".
    expect(labels).toContain("Cluster");
    expect(labels).toContain("Integrations");
    for (const gated of ["Signing keys", "Tokens", "Modules", "Stores", "Invite someone"]) {
      expect(`${gated}: ${labels.includes(gated)}`).toBe(`${gated}: false`);
    }
  });

  it("filters as you type and puts the best match first", async () => {
    renderShell();
    await waitFor(() => expect(railReady()).toBe(true));
    openPalette();
    await waitFor(() => expect(screen.getByRole("listbox")).toBeTruthy());

    fireEvent.change(box(), { target: { value: "workb" } });
    await waitFor(() => expect(optionLabels()[0]).toBe("Workbenches"));
  });

  it("moves with the arrows and goes with Enter", async () => {
    renderShell();
    await waitFor(() => expect(railReady()).toBe(true));
    openPalette();
    await waitFor(() => expect(screen.getByRole("listbox")).toBeTruthy());

    fireEvent.change(box(), { target: { value: "workb" } });
    await waitFor(() => expect(optionLabels()[0]).toBe("Workbenches"));
    // The selection is announced through aria-activedescendant, which is the
    // whole a11y story for a listbox driven from an input.
    await waitFor(() =>
      expect(box().getAttribute("aria-activedescendant")).toContain("tab.fleet.workbenches"),
    );

    fireEvent.keyDown(box(), { key: "Enter" });
    await waitFor(() =>
      expect((globalThis as Record<string, unknown>).__palettePath).toBe("/fleet/workbenches"),
    );
    expect(screen.queryByRole("listbox")).toBeNull();
  });

  it("takes 'Add machine' to the machines page with the form already open", async () => {
    renderShell();
    await waitFor(() => expect(railReady()).toBe(true));
    openPalette();
    await waitFor(() => expect(screen.getByRole("listbox")).toBeTruthy());

    fireEvent.change(box(), { target: { value: "add machine" } });
    await waitFor(() => expect(optionLabels()[0]).toBe("Add machine"));
    fireEvent.keyDown(box(), { key: "Enter" });

    // Not merely "somewhere you could do it": the form is open when you land.
    await waitFor(() =>
      expect((globalThis as Record<string, unknown>).__palettePath).toBe("/fleet/machines?add=1"),
    );
    await waitFor(() =>
      expect(
        within(screen.getByRole("main")).getAllByText("Add a machine").length,
      ).toBeGreaterThan(0),
    );
  });

  it("says so rather than sitting empty when nothing matches", async () => {
    renderShell();
    await waitFor(() => expect(railReady()).toBe(true));
    openPalette();
    await waitFor(() => expect(screen.getByRole("listbox")).toBeTruthy());
    fireEvent.change(box(), { target: { value: "zzzzzz" } });
    await waitFor(() => expect(screen.getByText(/Nothing matches/)).toBeTruthy());
  });
});
