// The composer (memql#3320), end to end against a fake cluster.
//
// TWO CLAIMS CARRY THE ISSUE, and this file is where they are proved rather
// than asserted in prose:
//
//   1. A CONCEPT THIS REPOSITORY HAS NEVER SEEN composes into a usable view
//      with no code change. The concept below (`v9:madeup:sensorReading`) is
//      not in dsl/, not in any registry, not named by any element, and not
//      mentioned anywhere outside this file and view-kit's own arrangement
//      suite. If the composer produces a rendering view of it, "works the day
//      it is declared" is a property of the code.
//
//   2. IT ALL WORKS WITH NO AI PROVIDER. Every test here runs against a
//      cluster whose suggest surface REFUSES -- which is exactly what a
//      cluster with no provider configured, or one that has not registered the
//      viewArrangement domain, does today. The composer reaches a saved,
//      rendering view in that state; the suggestion is an offer that was
//      declined by the server rather than by the person.
//
// The successful-suggestion path is exercised too, because "the AI is
// optional" is only interesting if the AI also works.

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import {
  Result,
  type AccessSummary,
  type Concept,
  type Connection,
  type QueryClient,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { VIEW_CONCEPT_ID } from "../src/compose/savedViews";

// ---------------------------------------------------------------------------
// A concept nothing in this repository declares
// ---------------------------------------------------------------------------
//
// Note what it does NOT have: a @displayCard. An undeclared card is the harder
// case and the commoner one for a concept somebody just wrote, and view-kit's
// documented inference is what has to carry it.
const INVENTED = "v9:madeup:sensorReading";

const CONCEPTS: Concept[] = [
  {
    id: INVENTED,
    version: "v9",
    domain: "madeup",
    entity: "sensorReading",
    type: "concept",
    description: "A temperature reading from somewhere in the plant",
  },
  {
    id: VIEW_CONCEPT_ID,
    version: "v1",
    domain: "portalviews",
    entity: "view",
    type: "concept",
    description: "A view a person composed",
    displayCard: {
      primary: "name",
      secondary: "description",
      tertiary: "updatedAt",
      status: "status",
    },
  },
];

function row(concept: string, id: string, payload: Record<string, unknown>): Row {
  return { id, concept, type: "concept", createdBy: "system", createdAt: "2026-08-08T10:00:00Z", payload };
}

const INVENTED_ROWS: Row[] = [
  row(INVENTED, "r1", { label: "north inlet", zone: "intake", degrees: 41.2, takenAt: "2026-08-01T06:00:00Z", faulty: false }),
  row(INVENTED, "r2", { label: "south inlet", zone: "intake", degrees: 39.8, takenAt: "2026-08-01T07:00:00Z", faulty: false }),
  row(INVENTED, "r3", { label: "return line", zone: "return", degrees: 55.4, takenAt: "2026-08-01T08:00:00Z", faulty: true }),
];

const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
};

// ---------------------------------------------------------------------------
// The fake cluster
// ---------------------------------------------------------------------------

interface Harness {
  // What the suggest surface does. "refuse" is the DEFAULT because it is the
  // real behaviour of a cluster with no AI provider configured -- and of every
  // cluster until the viewArrangement suggest-domain handler is registered
  // server-side. Testing the refusal as the default is the honest posture.
  suggest?: "refuse" | "propose" | "garbage";
  // Reject the write, to exercise the save-failure branch.
  saveFails?: boolean;
}

interface Cluster {
  written: { name: string; call: string }[];
  suggestCalls: Record<string, unknown>[];
}

function renderCompose(path: string, harness: Harness = {}) {
  const cluster: Cluster = { written: [], suggestCalls: [] };
  // The saved views this fake cluster holds. A store rather than a fixed
  // reply, so a save followed by a read returns what was written -- which is
  // the only way to test that a composed view round-trips.
  const stored = new Map<string, Record<string, unknown>>();

  const access: AccessSummary = {
    requestId: "r1",
    userId: "user-1",
    primaryEmail: "ada@example.com",
    clusterRole: "owner",
  };

  const executeNamed = vi.fn(async (name: string, call: string) => {
    // The concept browse path, which is how every section reads its rows.
    const browse = /concept==([^,)\s]+)/.exec(call);
    if (browse) {
      const id = browse[1];
      return new Result({
        bundle: { nodes: id === INVENTED ? INVENTED_ROWS : [] },
        meta: { cursor: "" },
      });
    }

    if (name === "createComposedView" || name === "updateComposedView") {
      if (harness.saveFails) throw new Error("write refused");
      cluster.written.push({ name, call });
      // Parse just enough of the literal back out to serve the read. The
      // engine would store the whole payload; the id and the name are what
      // the read-back assertions need.
      const id = /viewId: "([^"]*)"/.exec(call)?.[1] ?? "";
      const viewName = /name: "([^"]*)"/.exec(call)?.[1] ?? "";
      const arrangements = /arrangements: (\[.*\]), conceptIds/.exec(call)?.[1] ?? "[]";
      // Stored WIRE-SHAPED (payload nested), because that is what comes back
      // off the stream and what Result.rows() flattens.
      stored.set(id, {
        id,
        concept: VIEW_CONCEPT_ID,
        createdAt: "2026-08-08T11:00:00Z",
        payload: {
          name: viewName,
          description: "",
          conceptIds: [INVENTED],
          // The written literal is MemQL, not JSON. For the read-back the
          // section only needs a valid arrangement, so echo a canonical one;
          // what was ACTUALLY sent is asserted on the literal itself.
          arrangements: [
            {
              conceptId: INVENTED,
              elements: [
                { element: "statTile", band: "reading" },
                { element: "table", band: "roll" },
              ],
            },
          ],
          origin: "manual",
          status: "active",
          updatedAt: "2026-08-08T11:00:00Z",
          rawArrangements: arrangements,
        },
      });
      return new Result({ bundle: { nodes: [] }, meta: { cursor: "" } });
    }

    if (name === "composedViewById") {
      const id = /viewId: "([^"]*)"/.exec(call)?.[1] ?? "";
      const found = stored.get(id);
      return new Result({
        bundle: { nodes: found ? [found as unknown as Row] : [] },
        meta: { cursor: "" },
      });
    }

    if (name === "composedViews") {
      return new Result({
        bundle: { nodes: [...stored.values()] as unknown as Row[] },
        meta: { cursor: "" },
      });
    }

    return new Result({ bundle: { nodes: [] }, meta: { cursor: "" } });
  });

  const query = {
    listConcepts: vi.fn(async () => CONCEPTS),
    getMyAccess: vi.fn(async () => access),
    executeNamed,
  } as unknown as QueryClient;

  const dispatcher = {
    sendAndWait: vi.fn(async (msg: Record<string, unknown>) => {
      const suggest = msg["aiSuggest"] as
        | { domain: string; payload: Record<string, unknown> }
        | undefined;
      if (suggest === undefined) return {};
      cluster.suggestCalls.push(suggest.payload);

      if (harness.suggest === "propose") {
        return {
          aiSuggestResult: {
            domain: suggest.domain,
            result: {
              reasoning: "Readings are a time series, so plot them and list the rest.",
              elements: [
                { element: "chart.line", band: "shape" },
                { element: "rowList", band: "roll" },
              ],
            },
          },
        };
      }
      if (harness.suggest === "garbage") {
        return {
          aiSuggestResult: {
            domain: suggest.domain,
            result: { elements: [{ element: "sankeyDiagram", band: "roll" }] },
          },
        };
      }
      // The default: the engine's typed error for a domain with no registered
      // handler, which is what a cluster with no viewArrangement handler --
      // and, downstream of that, one with no AI provider at all -- returns.
      return {
        queryError: { error: { message: 'unsupported suggest domain "viewArrangement"' } },
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

  const utils = render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("the composer tests must make no identity calls");
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

  return { ...utils, cluster };
}

const COMPOSER_PATH = `/compose/new?concept=${encodeURIComponent(INVENTED)}`;

// Everything a composed view draws comes out of view-kit, so its markup
// carries a vk- class. Asserting on the class rather than the text is what
// makes these tests about COMPOSITION -- text could be printed by anything.
function viewKitClasses(container: HTMLElement): Set<string> {
  const out = new Set<string>();
  for (const node of container.querySelectorAll("[class]")) {
    for (const cls of (node.getAttribute("class") ?? "").split(/\s+/)) {
      if (cls.startsWith("vk-")) out.add(cls);
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// 1. A concept nobody wrote code for
// ---------------------------------------------------------------------------

describe("a concept this repository has never seen", () => {
  it("composes into a rendering view with no code change and no model", async () => {
    const { container } = renderCompose(COMPOSER_PATH);

    // The section names the concept -- the one thing that tells a person which
    // rows they are looking at.
    await waitFor(() => expect(screen.getByText(INVENTED)).toBeTruthy());
    // And the rows are on screen, through view-kit, before anything has been
    // clicked. No provider was configured and none was asked.
    await waitFor(() => expect(screen.getAllByText(/north inlet/).length).toBeGreaterThan(0));

    const classes = viewKitClasses(container);
    // A reading and a roll at minimum: the deterministic proposal fills the
    // bands from the concept's shape alone.
    expect(classes.has("vk-stat")).toBe(true);
    expect([...classes].some((c) => c.startsWith("vk-table") || c.startsWith("vk-row"))).toBe(true);

    // Nothing on this page came from a suggestion, and the composer says so
    // only when asked.
    expect(screen.queryByText(/No suggestion available/)).toBeNull();
  });

  it("explains what fits and what does not, in view-kit's words", async () => {
    renderCompose(COMPOSER_PATH);
    await screen.findAllByText(/north inlet/);
    expect(screen.getByText(/What fits sensorReading/)).toBeTruthy();

    // The map requires coordinates this concept does not carry. It is OFFERED
    // as unusable with a reason rather than hidden, because "why can't I pick
    // the map" is the question a person actually has.
    expect(screen.getByText(/Map cannot render sensorReading/)).toBeTruthy();
    expect(screen.getAllByText(/does not fit/).length).toBeGreaterThan(0);
  });

  it("lets a person add and remove elements by hand", async () => {
    const { container } = renderCompose(COMPOSER_PATH);
    // Wait for the ROWS, not just the header: candidacy is computed from the
    // profile, and an unloaded section has an honestly empty one.
    await screen.findAllByText(/north inlet/);

    const before = viewKitClasses(container).size;
    const addButtons = screen.getAllByRole("button", { name: "Add" });
    expect(addButtons.length).toBeGreaterThan(0);
    fireEvent.click(addButtons[0]!);
    await waitFor(() => expect(viewKitClasses(container).size).toBeGreaterThanOrEqual(before));

    const removes = screen.getAllByRole("button", { name: "Remove" });
    const bands = removes.length;
    fireEvent.click(removes[0]!);
    await waitFor(() =>
      expect(screen.getAllByRole("button", { name: "Remove" }).length).toBe(bands - 1),
    );
  });
});

// ---------------------------------------------------------------------------
// 2. With the provider unavailable
// ---------------------------------------------------------------------------

describe("with no AI provider", () => {
  it("says so when asked, and leaves the working view in place", async () => {
    const { container } = renderCompose(COMPOSER_PATH);
    await screen.findAllByText(/north inlet/);

    fireEvent.click(screen.getByRole("button", { name: "Suggest an arrangement" }));

    await waitFor(() =>
      expect(screen.getByText(/No suggestion available/)).toBeTruthy(),
    );
    // The failure names the reason -- an operator can act on "no such domain"
    // and cannot act on "something went wrong".
    expect(screen.getByText(/unsupported suggest domain/)).toBeTruthy();

    // And the view is untouched: the bands, the rows and the elements are all
    // still there.
    expect(viewKitClasses(container).has("vk-stat")).toBe(true);
    expect(screen.getAllByText(/north inlet/).length).toBeGreaterThan(0);
  });

  it("still reaches a saved view that renders", async () => {
    const { cluster } = renderCompose(COMPOSER_PATH);
    await screen.findAllByText(/north inlet/);

    // Refused suggestion first, so the save happens in the degraded state
    // rather than in a pristine one.
    fireEvent.click(screen.getByRole("button", { name: "Suggest an arrangement" }));
    await waitFor(() => expect(screen.getByText(/No suggestion available/)).toBeTruthy());

    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Plant readings" } });
    fireEvent.click(screen.getByRole("button", { name: "Save view" }));

    // A row was written, through the composed-view mutation, carrying the
    // arrangement -- and NOT carrying an owner, because ownerUserId is
    // @serverSet and the engine stamps it.
    await waitFor(() => expect(cluster.written.length).toBe(1));
    expect(cluster.written[0]!.name).toBe("createComposedView");
    expect(cluster.written[0]!.call).toContain("Plant readings");
    expect(cluster.written[0]!.call).toContain("arrangements:");
    expect(cluster.written[0]!.call).not.toContain("ownerUserId");
    // Saved as manual: no model contributed to it.
    expect(cluster.written[0]!.call).toContain('origin: "manual"');

    // The composer navigated to the saved view, and it renders -- the rows
    // arrive again through the same walk, through the same elements.
    await waitFor(() => expect(screen.getByText("Plant readings")).toBeTruthy());
    await waitFor(() => expect(screen.getAllByText(/north inlet/).length).toBeGreaterThan(0));
    // The saved view says out loud that it is a row, with a link to it.
    expect(screen.getByRole("link", { name: "See it as a row" })).toBeTruthy();
  });

  it("reports a refused write instead of pretending it saved", async () => {
    renderCompose(COMPOSER_PATH, { saveFails: true });
    await screen.findAllByText(/north inlet/);
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Plant readings" } });
    fireEvent.click(screen.getByRole("button", { name: "Save view" }));
    await waitFor(() =>
      expect(screen.getByText(/Could not save the view: write refused/)).toBeTruthy(),
    );
  });
});

// ---------------------------------------------------------------------------
// 3. With a provider
// ---------------------------------------------------------------------------

describe("with a model available", () => {
  it("proposes an arrangement with reasoning, and declining keeps the composer working", async () => {
    const { cluster } = renderCompose(COMPOSER_PATH, { suggest: "propose" });
    await screen.findAllByText(/north inlet/);

    fireEvent.click(screen.getByRole("button", { name: "Suggest an arrangement" }));
    await waitFor(() => expect(screen.getByText(/Readings are a time series/)).toBeTruthy());

    // The model was shown field shapes and NOT row values. A layout decision
    // has no business reading somebody's data.
    const payload = JSON.stringify(cluster.suggestCalls[0]!);
    expect(payload).toContain("degrees");
    expect(payload).not.toContain("north inlet");
    expect(payload).not.toContain("41.2");

    // DECLINING is not an undo. The proposal disappears and the view a person
    // already had is exactly as it was.
    fireEvent.click(screen.getByRole("button", { name: "Keep mine" }));
    await waitFor(() => expect(screen.queryByText(/Readings are a time series/)).toBeNull());
    expect(screen.getAllByText(/north inlet/).length).toBeGreaterThan(0);
  });

  it("applies an accepted proposal, and records that it came from one", async () => {
    const { cluster } = renderCompose(COMPOSER_PATH, { suggest: "propose" });
    await screen.findAllByText(/north inlet/);

    fireEvent.click(screen.getByRole("button", { name: "Suggest an arrangement" }));
    fireEvent.click(await screen.findByRole("button", { name: "Use it" }));

    // The proposal is applied: the roll band is now a list rather than the
    // table the deterministic answer chose, and the band caption says so.
    await waitFor(() =>
      expect(screen.getAllByRole("heading", { name: "List" }).length).toBe(1),
    );

    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Plant readings" } });
    fireEvent.click(screen.getByRole("button", { name: "Save view" }));
    await waitFor(() => expect(cluster.written.length).toBe(1));
    expect(cluster.written[0]!.call).toContain('element: "rowList"');
  });

  it("corrects a proposal naming an element that does not exist", async () => {
    renderCompose(COMPOSER_PATH, { suggest: "garbage" });
    await screen.findAllByText(/north inlet/);

    fireEvent.click(screen.getByRole("button", { name: "Suggest an arrangement" }));
    // The problem is reported rather than swallowed -- a model that keeps
    // inventing elements is a fact about the prompt.
    await waitFor(() =>
      expect(screen.getByText(/does not have/)).toBeTruthy(),
    );
    // And what it offers is still renderable: readArrangement fell back to the
    // deterministic baseline rather than to nothing.
    fireEvent.click(screen.getByRole("button", { name: "Use it" }));
    await waitFor(() => expect(screen.getAllByText(/north inlet/).length).toBeGreaterThan(0));
  });
});

// ---------------------------------------------------------------------------
// 4. The front door
// ---------------------------------------------------------------------------

describe("the compose page", () => {
  it("offers every concept the cluster publishes, marking the designed ones", async () => {
    renderCompose("/compose");
    await waitFor(() => expect(screen.getByText(INVENTED)).toBeTruthy());
    // The saved-view concept is itself composable -- it is a concept like any
    // other, which is the point of storing a view as a row.
    expect(screen.getByText(VIEW_CONCEPT_ID)).toBeTruthy();
    expect(screen.getByText(/You have not composed a view yet/)).toBeTruthy();
  });

  it("carries the selection into the composer as a link", async () => {
    renderCompose("/compose");
    await waitFor(() => expect(screen.getByText(INVENTED)).toBeTruthy());
    const list = screen.getByLabelText("Search concepts").closest("section")!;
    const checkbox = within(list).getAllByRole("checkbox")[0]!;
    fireEvent.click(checkbox);
    fireEvent.click(screen.getByRole("button", { name: /^Compose/ }));
    await waitFor(() => expect(screen.getByText("Compose a view")).toBeTruthy());
  });
});
