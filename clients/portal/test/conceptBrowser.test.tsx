// The concept browser end to end (memql#3316): registry -> concept -> rows ->
// row, plus paging, live updates and deep links.
//
// Driven against a fake QueryClient/SubscriptionManager rather than a server.
// The wire is sdk/ts's business and is tested there; what these tests own is
// the browser's own behaviour -- which URL shows what, what the paging
// controls do, and what a CDC event does to a list someone is reading.
//
// The paging INVARIANTS (no gaps, no duplicates, a reset cursor refused) are
// pinned at the state machine in rowWalk.test.ts, where they can be asserted
// over a whole walk without three layers of React in the way. What is asserted
// here is that this page is wired to that machine correctly.

import { describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import {
  Result,
  type Concept,
  type Connection,
  type Event,
  type QueryClient,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";

const NODE = "v1:cluster:node";
const SPACE = "v1:cognition:space";
const AUDIT = "v1:identity:auditEvent";

const CONCEPTS: Concept[] = [
  {
    id: NODE,
    version: "v1",
    domain: "cluster",
    entity: "node",
    description: "A registered node in the cluster",
    type: "concept",
    displayCard: { primary: "name", status: "state" },
  },
  {
    id: SPACE,
    version: "v1",
    domain: "cognition",
    entity: "space",
    description: "A conversation space",
    type: "concept",
    displayCard: { primary: "title", secondary: "kind" },
  },
  {
    // No display card at all -- view-kit's documented inference has to carry
    // it, which is the property that makes a brand-new concept render.
    id: AUDIT,
    version: "v1",
    domain: "identity",
    entity: "auditEvent",
    description: "Append-only audit trail",
    type: "concept",
  },
];

const NODE_SCHEMA = {
  type: "object",
  required: ["name"],
  properties: {
    name: { type: "string", description: "The node's stable name" },
    state: { type: "string" },
    secretToken: { type: "string", "x-secret": true },
  },
};

// nodeRow builds a wire-shaped row: payload NESTED under `payload`, alongside
// the intrinsics. That nesting is what the detail pane must preserve.
function nodeRow(n: number): Row {
  return {
    id: `node-${String(n).padStart(3, "0")}`,
    concept: NODE,
    type: "concept",
    createdBy: "system",
    createdAt: `2026-08-08T10:00:${String(n % 60).padStart(2, "0")}Z`,
    schema: NODE_SCHEMA,
    payload: { name: `pod-${n}`, state: n % 2 === 0 ? "healthy" : "draining" },
    provenance: { kind: "mutation", name: "registerNode" },
  };
}

const PAGE_SIZE = 3;
const TOTAL_NODE_ROWS = 7;

interface Harness {
  query: QueryClient;
  // The subscription manager the ClusterProvider hands to the browse pane.
  // Narrowed to the one method it calls; widening the double every time an
  // unrelated method lands on SubscriptionManager would be a maintenance tax
  // with no extra coverage.
  subscriptions: unknown;
  // Every cursor the browse query was called with, in order. The paging
  // assertions read this: "" then each cursor the previous page returned is
  // the shape of a walk with no page fetched twice and none skipped.
  browseCalls: string[];
  // Fire a CDC event at whatever handler the pane subscribed with.
  emit: (event: Event) => void;
}

// setSearch drives the registry's search box. fireEvent.change rather than a
// per-character type: the field is a controlled input whose value is the URL,
// and what is under test is the filtering, not React's onChange plumbing.
function setSearch(element: HTMLElement, value: string): void {
  fireEvent.change(element, { target: { value } });
}

function harness(
  overrides: {
    subscribeThrows?: Error;
    // Cursors whose FIRST browse attempt fails. A second attempt at the same
    // cursor succeeds, which is what makes "retry resumes the walk" testable.
    failOnce?: string[];
  } = {},
): Harness {
  const browseCalls: string[] = [];
  const failed = new Set<string>();
  let handler: ((event: Event) => void) | null = null;

  const query = {
    listConcepts: vi.fn(async () => CONCEPTS),
    getMyAccess: vi.fn(async () => null),
    executeNamed: vi.fn(
      async (name: string, _call: string, opts: { cursor?: string } = {}) => {
        if (name === "conceptRow") {
          // `_call` is `concept==X && id==Y`; the fake only needs the id.
          const match = /id==(\S+)/.exec(_call);
          const id = match?.[1] ?? "";
          const index = Number(id.replace("node-", ""));
          if (Number.isNaN(index) || index >= TOTAL_NODE_ROWS) {
            return new Result({ bundle: { nodes: [] } });
          }
          return new Result({ bundle: { nodes: [nodeRow(index)] } });
        }

        // conceptBrowse. The cursor is opaque to the client, so the fake mints
        // one exactly as the engine does: a token that means "resume here",
        // and "" on the last page.
        const cursor = opts.cursor ?? "";
        browseCalls.push(cursor);
        if ((overrides.failOnce ?? []).includes(cursor) && !failed.has(cursor)) {
          failed.add(cursor);
          throw new Error(`conceptBrowse: stream closed at cursor ${cursor || "(first)"}`);
        }
        if (_call.includes(SPACE) || _call.includes(AUDIT)) {
          return new Result({ bundle: { nodes: [] }, meta: { cursor: "" } });
        }
        const start = cursor === "" ? 0 : Number(cursor.replace("after-", ""));
        const nodes: Row[] = [];
        for (let i = start; i < Math.min(start + PAGE_SIZE, TOTAL_NODE_ROWS); i++) {
          nodes.push(nodeRow(i));
        }
        const next = start + PAGE_SIZE;
        return new Result({
          bundle: { nodes },
          meta: { cursor: next < TOTAL_NODE_ROWS ? `after-${next}` : "" },
        });
      },
    ),
  } as unknown as QueryClient;

  const subscriptions = {
    subscribeGraph: (fn: (event: Event) => void) => {
      if (overrides.subscribeThrows) throw overrides.subscribeThrows;
      handler = fn;
      return () => {
        handler = null;
      };
    },
  };

  return {
    query,
    subscriptions,
    browseCalls,
    // Wrapped in act(): a CDC event arrives from outside React's event
    // system, exactly as it does in the browser, and the state update it
    // causes has to be flushed before the assertion reads the DOM.
    emit: (event: Event) => {
      act(() => {
        handler?.(event);
      });
    },
  };
}

const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
};

function renderBrowser(h: Harness, path: string) {
  const dial = vi.fn(
    async () =>
      ({
        nodeId: "bff-test",
        serverVersion: "0.0.0-test",
        query: h.query,
        subscriptions: h.subscriptions,
        close: vi.fn(),
        done: vi.fn(() => new Promise<void>(() => {})),
      }) as unknown as Connection,
  ) as unknown as typeof Connection.dial;

  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("the browser tests must make no identity calls");
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

describe("the concept registry page", () => {
  it("lists every concept, grouped by domain", async () => {
    renderBrowser(harness(), "/concepts");
    await waitFor(() => expect(screen.getByText(NODE)).toBeTruthy());
    expect(screen.getByText(SPACE)).toBeTruthy();
    expect(screen.getByText(AUDIT)).toBeTruthy();
    // Domain headings, so the registry is navigable without a search.
    expect(screen.getByRole("heading", { name: /cluster/i })).toBeTruthy();
    expect(screen.getByRole("heading", { name: /identity/i })).toBeTruthy();
  });

  it("searches across ids, entities and descriptions", async () => {
    renderBrowser(harness(), "/concepts");
    await waitFor(() => expect(screen.getByText(NODE)).toBeTruthy());

    const search = screen.getByRole("searchbox");
    // "conversation" appears only in a DESCRIPTION -- the thing an operator
    // remembers when they have forgotten the id.
    setSearch(search, "conversation");
    await waitFor(() => expect(screen.queryByText(NODE)).toBeNull());
    expect(screen.getByText(SPACE)).toBeTruthy();

    setSearch(search, "auditevent");
    await waitFor(() => expect(screen.getByText(AUDIT)).toBeTruthy());
    expect(screen.queryByText(SPACE)).toBeNull();
  });

  it("filters to a domain, and says so in the URL so the filter is a link", async () => {
    renderBrowser(harness(), "/concepts?domain=identity");
    await waitFor(() => expect(screen.getByText(AUDIT)).toBeTruthy());
    expect(screen.queryByText(NODE)).toBeNull();

    // Clicking the active chip clears it.
    fireEvent.click(screen.getByRole("button", { name: /^identity/ }));
    await waitFor(() => expect(screen.getByText(NODE)).toBeTruthy());
  });

  it("explains an empty result instead of showing a blank list", async () => {
    renderBrowser(harness(), "/concepts");
    await waitFor(() => expect(screen.getByText(NODE)).toBeTruthy());
    setSearch(screen.getByRole("searchbox"), "nothing-matches-this");
    await waitFor(() => expect(screen.getByText(/No concept matches/)).toBeTruthy());
  });
});

describe("one concept's rows", () => {
  it("pages a concept with an explicit Load more, appending without duplicates", async () => {
    const h = harness();
    renderBrowser(h, `/concepts/${NODE}`);

    await waitFor(() => expect(screen.getByText("pod-0")).toBeTruthy());
    expect(screen.getAllByText(/^pod-\d$/)).toHaveLength(PAGE_SIZE);
    expect(screen.getByText(/3 rows loaded, more available/)).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    await waitFor(() => expect(screen.getAllByText(/^pod-\d$/)).toHaveLength(6));

    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    await waitFor(() =>
      expect(screen.getByText(/All 7 rows loaded/)).toBeTruthy(),
    );

    // Every row exactly once, in order, across the whole walk.
    const shown = screen.getAllByText(/^pod-\d$/).map((el) => el.textContent);
    expect(shown).toEqual(["pod-0", "pod-1", "pod-2", "pod-3", "pod-4", "pod-5", "pod-6"]);
    expect(new Set(shown).size).toBe(TOTAL_NODE_ROWS);

    // One request per page, each carrying the cursor the previous page
    // returned -- no page fetched twice, none skipped.
    expect(h.browseCalls).toEqual(["", "after-3", "after-6"]);

    // Exhausted means the control is gone, not disabled-looking.
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
  });

  it("says a concept is empty rather than looking like it is still loading", async () => {
    renderBrowser(harness(), `/concepts/${SPACE}`);
    await waitFor(() => expect(screen.getByText(/This concept has no rows/)).toBeTruthy());
  });

  it("opens a row at its own address, preserving the wire's nesting", async () => {
    renderBrowser(harness(), `/concepts/${NODE}`);
    await waitFor(() => expect(screen.getByText("pod-1")).toBeTruthy());

    fireEvent.click(screen.getByText("pod-1"));

    await waitFor(() => expect(screen.getByText("Row detail")).toBeTruthy());
    // The row id appears as the pane's subject.
    expect(screen.getAllByText("node-001").length).toBeGreaterThan(0);

    // NESTING PRESERVED: the payload is shown as a `payload` group and the
    // intrinsics stay distinct. Flattening would drop exactly these.
    expect(screen.getByText("payload")).toBeTruthy();
    expect(screen.getByText("createdBy")).toBeTruthy();
    expect(screen.getByText("provenance")).toBeTruthy();
    expect(screen.getByText("system")).toBeTruthy();
  });

  it("resolves a pasted row link with no prior navigation", async () => {
    renderBrowser(harness(), `/concepts/${NODE}/rows/node-002`);
    await waitFor(() => expect(screen.getByText("Row detail")).toBeTruthy());
    await waitFor(() => expect(screen.getByText("registerNode")).toBeTruthy());
  });

  it("says a row is gone rather than rendering an empty pane", async () => {
    renderBrowser(harness(), `/concepts/${NODE}/rows/node-999`);
    await waitFor(() =>
      expect(screen.getByText(/no row with that id under this concept/)).toBeTruthy(),
    );
  });

  it("names a concept this cluster does not declare", async () => {
    renderBrowser(harness(), "/concepts/v1:nosuch:thing");
    await waitFor(() =>
      expect(screen.getByText(/declares no concept with that id/)).toBeTruthy(),
    );
  });

  it("keeps the rows already loaded when a page fails mid-walk, and resumes on retry", async () => {
    const h = harness({ failOnce: ["after-3"] });
    renderBrowser(h, `/concepts/${NODE}`);
    await waitFor(() => expect(screen.getAllByText(/^pod-\d$/)).toHaveLength(PAGE_SIZE));

    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    await waitFor(() =>
      expect(screen.getByText(/Paging stopped after 3 rows/)).toBeTruthy(),
    );
    // The first page survives -- a mid-walk failure is not an empty list.
    expect(screen.getAllByText(/^pod-\d$/)).toHaveLength(PAGE_SIZE);

    fireEvent.click(screen.getByRole("button", { name: /Retry from where it stopped/ }));
    await waitFor(() => expect(screen.getAllByText(/^pod-\d$/)).toHaveLength(6));
    // Resumed, not restarted: the same cursor again, and no repeat of page one.
    expect(h.browseCalls).toEqual(["", "after-3", "after-3"]);
    const shown = screen.getAllByText(/^pod-\d$/).map((el) => el.textContent);
    expect(new Set(shown).size).toBe(shown.length);
  });

  it("does not claim a concept is empty when the FIRST page failed", async () => {
    const h = harness({ failOnce: [""] });
    renderBrowser(h, `/concepts/${NODE}`);
    await waitFor(() => expect(screen.getByText(/Could not read rows/)).toBeTruthy());
    // "No rows for node." next to "could not read rows" is two contradictory
    // answers to the same question.
    expect(screen.queryByText("No rows for node.")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: /Retry from where it stopped/ }));
    await waitFor(() => expect(screen.getAllByText(/^pod-\d$/)).toHaveLength(PAGE_SIZE));
  });
});

describe("live updates", () => {
  it("puts a newly created row in its own band, leaving the paged window and cursor alone", async () => {
    const h = harness();
    renderBrowser(h, `/concepts/${NODE}`);
    await waitFor(() => expect(screen.getByText("pod-0")).toBeTruthy());
    expect(h.browseCalls).toEqual([""]);

    h.emit({
      subscriptionId: "s",
      kind: "NODE_CREATED",
      timestamp: new Date(),
      payload: { id: "node-900", concept: NODE, name: "pod-brand-new", state: "healthy" },
    });

    await waitFor(() => expect(screen.getByText(/New since you opened this/)).toBeTruthy());
    expect(screen.getByText("pod-brand-new")).toBeTruthy();
    // The paged window is untouched: still page one, still three rows.
    expect(screen.getAllByText(/^pod-\d$/)).toHaveLength(PAGE_SIZE);

    // ...and so is the cursor. The next page is still the one the server
    // handed back, not something recomputed around the live row.
    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    await waitFor(() => expect(h.browseCalls).toEqual(["", "after-3"]));
  });

  it("counts updates and deletes and offers a reload rather than editing the window", async () => {
    const h = harness();
    renderBrowser(h, `/concepts/${NODE}`);
    await waitFor(() => expect(screen.getByText("pod-0")).toBeTruthy());

    h.emit({
      subscriptionId: "s",
      kind: "NODE_UPDATED",
      timestamp: new Date(),
      payload: { id: "node-000", concept: NODE },
    });

    await waitFor(() =>
      expect(screen.getByText(/1 existing row changed/)).toBeTruthy(),
    );
    expect(screen.getByRole("button", { name: "Reload the list" })).toBeTruthy();
  });

  it("restarts the walk from page one on reload, clearing the band", async () => {
    const h = harness();
    renderBrowser(h, `/concepts/${NODE}`);
    await waitFor(() => expect(screen.getByText("pod-0")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    await waitFor(() => expect(screen.getAllByText(/^pod-\d$/)).toHaveLength(6));

    h.emit({
      subscriptionId: "s",
      kind: "NODE_CREATED",
      timestamp: new Date(),
      payload: { id: "node-900", concept: NODE, name: "pod-brand-new" },
    });
    await waitFor(() => expect(screen.getByText(/New since you opened this/)).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Reload the list" }));

    await waitFor(() => expect(screen.queryByText(/New since you opened this/)).toBeNull());
    await waitFor(() => expect(screen.getAllByText(/^pod-\d$/)).toHaveLength(PAGE_SIZE));
    expect(h.browseCalls).toEqual(["", "after-3", ""]);
  });

  it("says out loud when live updates could not be turned on", async () => {
    const h = harness({ subscribeThrows: new Error("subscription manager stopped") });
    renderBrowser(h, `/concepts/${NODE}`);
    await waitFor(() => expect(screen.getByText(/Live updates are off/)).toBeTruthy());
    // The pane still works -- a dead subscription is not a dead query, and an
    // ordinary read succeeding must NOT erase the notice.
    await waitFor(() => expect(screen.getByText("pod-0")).toBeTruthy());
    expect(screen.getByText(/Live updates are off/)).toBeTruthy();
  });
});

describe("the schema view", () => {
  it("shows the concept's declared fields, types and annotations at its own address", async () => {
    renderBrowser(harness(), `/concepts/${NODE}/schema`);

    await waitFor(() =>
      expect(screen.getByRole("columnheader", { name: "Field" })).toBeTruthy(),
    );
    expect(screen.getByText("from the declaration")).toBeTruthy();

    // Scoped to the table: "name" is also the display card's primary slot a
    // few lines up, and an unscoped query would match both.
    const table = within(screen.getByRole("table"));
    expect(table.getByText("name")).toBeTruthy();
    expect(table.getByText("The node's stable name")).toBeTruthy();
    expect(table.getByText("secretToken")).toBeTruthy();
    // The x-* keyword surfaces generically -- no list of known annotations.
    expect(table.getByText("secret")).toBeTruthy();
    // Required fields sort first, so "what must I supply" is a glance.
    const firstField = within(screen.getAllByRole("row")[1] as HTMLElement);
    expect(firstField.getByText("name")).toBeTruthy();
    expect(firstField.getByText("yes")).toBeTruthy();
    // The raw document is available underneath the readable table, for
    // whoever is chasing a validation error rather than reading a summary.
    expect(
      screen.getByText(/The JSON Schema the engine validates writes against/),
    ).toBeTruthy();
  });

  it("says the display card is DECLARED when the concept declares one", async () => {
    renderBrowser(harness(), `/concepts/${NODE}/schema`);
    await waitFor(() => expect(screen.getByText("Display card")).toBeTruthy());
    expect(screen.getByText("declared")).toBeTruthy();
  });

  it("says the display card is INFERRED when the concept declares none", async () => {
    renderBrowser(harness(), `/concepts/${AUDIT}/schema`);
    await waitFor(() => expect(screen.getByText("Display card")).toBeTruthy());
    expect(screen.getByText("inferred")).toBeTruthy();
  });

  it("is reachable from the rows view and back, by link", async () => {
    renderBrowser(harness(), `/concepts/${NODE}`);
    await waitFor(() => expect(screen.getByText("pod-0")).toBeTruthy());

    fireEvent.click(screen.getByRole("link", { name: "Schema" }));
    await waitFor(() => expect(screen.getByText("Display card")).toBeTruthy());

    fireEvent.click(screen.getByRole("link", { name: "Rows" }));
    await waitFor(() => expect(screen.getByText("pod-0")).toBeTruthy());
  });
});
