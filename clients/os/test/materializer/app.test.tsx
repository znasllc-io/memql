import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { MaterializerApp } = await import("../../src/apps/materializer/MaterializerApp");
const { LocalMaterializerSettingsStore } = await import("../../src/apps/materializer/settings");
const { compositionRow, fakeConnection, recipeRow, templateRow, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

const COMPOSITION = "v1:compose:composition";

function memoryStore(over: Record<string, unknown> = {}) {
  const bag = new Map<string, string>();
  const store = new LocalMaterializerSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
  if (Object.keys(over).length > 0) store.save({ ...store.load(), ...over });
  return store;
}

function mount(connection: Conn, sectionId = "composer", settings: Record<string, unknown> = {}) {
  h.connection = connection;
  const navigate = vi.fn();
  const view = render(
    withSession(
      <MaterializerApp
        sectionId={sectionId}
        navigate={navigate}
        askContext={() => {}}
        store={memoryStore(settings)}
      />,
    ),
  );
  return { view, navigate };
}

describe("the composer", () => {
  it("says what it is waiting for rather than offering a control that would be refused", async () => {
    mount(fakeConnection());
    // AN ILLEGAL ACT IS ABSENT, NEVER DISABLED -- so with nothing picked
    // there is no Materialize button at all, and the bar carries the
    // explanation instead.
    expect(await screen.findByText(/Pick at least one source/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Materialize" })).toBeNull();
  });

  it("offers the marked concepts first and says which are not marked", async () => {
    const conn = fakeConnection({
      composables: {
        concepts: [
          { id: "v1:x:invoice", as: "invoice", fields: ["number"], list: "openInvoices", marked: true },
          { id: "v1:y:widget", as: "widget", fields: [], list: "widgets", marked: false },
        ],
        registryAvailable: true,
      } as unknown as Row,
    });
    mount(conn, "composer", { showUnmarkedConcepts: true });

    const invoice = await screen.findByRole("button", { name: /invoice/ });
    expect(invoice).toBeTruthy();
    // THE MARK IS A RANKING AND A HINT, NEVER A GATE: an unmarked concept
    // is offered and says which it is.
    expect(screen.getByText("unmarked")).toBeTruthy();
  });

  // A concept with no `list` query is one this cluster has no read for, so
  // the control cannot work. It is disabled with the reason on its title
  // rather than absent, because the concept itself is worth SEEING --
  // "the cluster has this and cannot offer it" is the useful statement.
  it("cannot offer a concept that declares no list query", async () => {
    const conn = fakeConnection({
      composables: {
        concepts: [{ id: "v1:x:thing", as: "thing", fields: [], list: "", marked: true }],
        registryAvailable: true,
      } as unknown as Row,
    });
    mount(conn);
    const button = await screen.findByRole("button", { name: /thing/ });
    expect((button as HTMLButtonElement).disabled).toBe(true);
  });

  // "NOTHING IS MARKED" AND "THIS NODE CANNOT SEE THE REGISTRY" look
  // identical from an empty list, and only one is something an operator
  // can fix.
  it("separates an empty registry from an unreadable one", async () => {
    const conn = fakeConnection({
      composables: { concepts: [], registryAvailable: false } as unknown as Row,
    });
    mount(conn);
    expect(await screen.findByText(/cannot read the concept registry/)).toBeTruthy();
  });

  it("sends what was picked, and only the fields that were answered", async () => {
    const conn = fakeConnection({
      composables: {
        concepts: [{ id: "v1:x:invoice", as: "invoice", fields: [], list: "openInvoices", marked: true }],
        registryAvailable: true,
      } as unknown as Row,
      materializeReply: { compositionId: "c1" } as unknown as Row,
    });
    mount(conn);

    fireEvent.click(await screen.findByRole("button", { name: /invoice/ }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Q3 report" } });
    fireEvent.click(await screen.findByRole("button", { name: "Materialize" }));

    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalled());
    const args = conn.query.composeMaterialize.mock.calls[0]?.[0] ?? {};
    expect(args["name"]).toBe("Q3 report");
    expect(args["format"]).toBe("markdown");
    expect(args["sources"]).toEqual([
      { kind: "query", ref: "query openInvoices()", label: "invoice" },
    ]);
    // AN UNANSWERED OPTIONAL IS OMITTED, never sent empty: an optional
    // field given "" is a value the engine writes, and "the caller said
    // nothing" and "the caller said empty" are different statements.
    expect(args).not.toHaveProperty("templateId");
    expect(args).not.toHaveProperty("deployableKind");
  });

  it("answers the one rule a browser can answer without a round trip", async () => {
    const conn = fakeConnection({
      composables: {
        concepts: [{ id: "v1:x:invoice", as: "invoice", fields: [], list: "openInvoices", marked: true }],
        registryAvailable: true,
      } as unknown as Row,
    });
    mount(conn);
    fireEvent.click(await screen.findByRole("button", { name: /invoice/ }));
    fireEvent.click(await screen.findByRole("button", { name: "Materialize" }));

    expect(await screen.findByText(/Give it a name first/)).toBeTruthy();
    expect(conn.query.composeMaterialize).not.toHaveBeenCalled();
  });

  it("renders a refusal verbatim, in surface, with the act still offered", async () => {
    const conn = fakeConnection({
      composables: {
        concepts: [{ id: "v1:x:invoice", as: "invoice", fields: [], list: "openInvoices", marked: true }],
        registryAvailable: true,
      } as unknown as Row,
      writeError: new Error("that template is not readable by you, so nothing was rendered through it"),
    });
    mount(conn);
    fireEvent.click(await screen.findByRole("button", { name: /invoice/ }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Q3" } });
    fireEvent.click(await screen.findByRole("button", { name: "Materialize" }));

    expect(await screen.findByText(/not readable by you/)).toBeTruthy();
    // The act stays offered: a refused write is not a reason to remove the
    // control that produced it.
    expect(screen.getByRole("button", { name: "Materialize" })).toBeTruthy();
  });

  // The two formats named in the brief that this cluster does not offer.
  // An absent option with no account of itself reads as unfinished.
  it("names the formats it does not offer, and says what each is waiting on", async () => {
    mount(fakeConnection());
    expect(await screen.findByText("Not offered yet")).toBeTruthy();
    expect(screen.getByText("Audio")).toBeTruthy();
    expect(screen.getByText("Video")).toBeTruthy();
    expect(screen.getByText(/generation provider this cluster does not have/)).toBeTruthy();
  });
});

describe("the provenance chain", () => {
  // OPENED THROUGH THE INTENT rather than by clicking the list row,
  // because `navigate` is a spy here: a click correctly asks the shell to
  // move the window and the shell is what re-renders it on a different
  // section. Asserting the chain after a click would be asserting against
  // a section this test never left.
  function mountOn(conn: Conn, compositionId: string) {
    h.connection = conn;
    render(
      withSession(
        <MaterializerApp
          sectionId="composer"
          navigate={vi.fn()}
          askContext={() => {}}
          intent={{ id: "open", payload: { compositionId } }}
          consumeIntent={vi.fn()}
          store={memoryStore()}
        />,
      ),
    );
  }

  it("states the claim the format can actually make", async () => {
    const conn = fakeConnection({ compositions: [compositionRow({ id: `${COMPOSITION}:c1` })] });
    mountOn(conn, `${COMPOSITION}:c1`);
    // The chain reads what was made, and the claim is the format's.
    expect(await screen.findByText("the file carries it")).toBeTruthy();
  });

  it("says the record is the only copy where the format has no channel", async () => {
    const conn = fakeConnection({
      compositions: [
        compositionRow({
          id: `${COMPOSITION}:c2`,
          name: "Figures",
          format: "csv",
          provenanceEmbedded: false,
        }),
      ],
    });
    mountOn(conn, `${COMPOSITION}:c2`);
    expect(await screen.findByText(/only copy/)).toBeTruthy();
  });

  // A list row's job is to ASK for the window to move, which is the
  // shell's to do. Pinning the request is what this test can honestly
  // assert.
  it("a list row asks the shell to open the composer", async () => {
    const conn = fakeConnection({ compositions: [compositionRow({ id: `${COMPOSITION}:c1` })] });
    const { navigate } = mount(conn, "materialized");
    fireEvent.click(await screen.findByText("Q3 report"));
    await waitFor(() => expect(navigate).toHaveBeenCalledWith("composer"));
  });

  // THE PRODUCT'S HEADLINE CLAIM, ON SCREEN: a composition that reached no
  // model says so, in words, rather than rendering a blank where the
  // models would be.
  it("says 'no model' on a composition that reached none", async () => {
    const conn = fakeConnection({
      compositions: [compositionRow({ id: `${COMPOSITION}:c3`, name: "Replayed", modelsUsed: [] })],
    });
    mount(conn, "materialized");
    // It is on the ROW as well as in the chain, because that is the fact
    // somebody scanning the list came for.
    expect(await screen.findByLabelText("no model was called")).toBeTruthy();
  });
});

describe("the materialized list", () => {
  it("hides archived records by default and points at the setting", async () => {
    const conn = fakeConnection({
      compositions: [
        compositionRow({ id: `${COMPOSITION}:c1`, name: "Kept" }),
        compositionRow({ id: `${COMPOSITION}:c2`, name: "Filed away", archived: true }),
      ],
    });
    mount(conn, "materialized");
    expect(await screen.findByText("Kept")).toBeTruthy();
    expect(screen.queryByText("Filed away")).toBeNull();
  });

  it("lists them when the preference says so", async () => {
    const conn = fakeConnection({
      compositions: [
        compositionRow({ id: `${COMPOSITION}:c1`, name: "Kept" }),
        compositionRow({ id: `${COMPOSITION}:c2`, name: "Filed away", archived: true }),
      ],
    });
    mount(conn, "materialized", { showArchived: true });
    expect(await screen.findByText("Filed away")).toBeTruthy();
  });

  // THE SEED CARRIES NO ARCHIVE FILTER, deliberately: a read that carried
  // one could only back a toggle that revealed the rows which flipped
  // while the window was open.
  it("reads the whole population once, unfiltered", async () => {
    const conn = fakeConnection({ compositions: [] });
    mount(conn, "materialized");
    await waitFor(() => expect(conn.query.compositions).toHaveBeenCalled());
    expect(conn.query.compositions.mock.calls[0]?.[0]).toEqual({});
  });
});

describe("the arrival cue", () => {
  // BOTH DIRECTIONS, because a test of one half passes against a cue that
  // fires on everything.
  it("rings on a rename and stays silent on a re-stamped runId", async () => {
    const conn = fakeConnection({
      compositions: [compositionRow({ id: `${COMPOSITION}:c1`, name: "Before" })],
    });
    mount(conn, "materialized");
    await screen.findByText("Before");

    conn.subscriptions.emit(
      COMPOSITION,
      compositionRow({ id: `${COMPOSITION}:c1`, name: "Before", runId: "v1:work:run:r2" }),
    );
    await waitFor(() => {
      const row = screen.getByText("Before").closest("[data-arrival]");
      expect(row?.getAttribute("data-arrival") ?? null).toBeNull();
    });

    conn.subscriptions.emit(
      COMPOSITION,
      compositionRow({ id: `${COMPOSITION}:c1`, name: "After" }),
    );
    await waitFor(() => {
      const row = screen.getByText("After").closest("[data-arrival]");
      expect(row?.getAttribute("data-arrival")).toBeTruthy();
    });
  });
});

describe("templates and recipes", () => {
  it("says a template is a binding to a Library file rather than an upload", async () => {
    mount(fakeConnection(), "templates");
    fireEvent.click(await screen.findByRole("button", { name: "Bind a file" }));
    expect(await screen.findByText(/Upload the file in Files first/)).toBeTruthy();
  });

  it("renders a recipe's run count and last run without ringing on either", async () => {
    const conn = fakeConnection({ recipes: [recipeRow({ id: "v1:compose:recipe:r1" })] });
    mount(conn, "templates");
    expect(await screen.findByText("Acme quarterly report")).toBeTruthy();
    expect(screen.getByText("2 times")).toBeTruthy();
  });

  it("says a recipe re-runs the selection rather than copying the rows", async () => {
    mount(fakeConnection(), "templates");
    expect(await screen.findByText(/stores the selection, not the rows/)).toBeTruthy();
  });

  it("offers no template picker entry for one that makes a different format", async () => {
    const conn = fakeConnection({
      templates: [templateRow({ id: "v1:compose:template:t1", name: "PDF only", format: "pdf" })],
    });
    mount(conn, "composer", { defaultFormat: "markdown" });
    expect(await screen.findByText(/None of your templates make a Markdown/)).toBeTruthy();
  });
});

describe("settings", () => {
  it("explains the absent spending control rather than leaving a gap", async () => {
    mount(fakeConnection(), "settings");
    expect(await screen.findByText("Spending")).toBeTruthy();
    expect(screen.getByText(/ceilings .* are set when the goal is accepted/)).toBeTruthy();
  });

  it("says archiving a record never touches the file it names", async () => {
    mount(fakeConnection(), "settings");
    expect(await screen.findByText(/never touches the file it names/)).toBeTruthy();
  });
});

describe("the open intent", () => {
  it("opens the composition an opener named, once, id-matched", async () => {
    const conn = fakeConnection({
      compositions: [compositionRow({ id: `${COMPOSITION}:c9`, name: "From elsewhere" })],
    });
    h.connection = conn;
    const navigate = vi.fn();
    const consumeIntent = vi.fn();
    render(
      withSession(
        <MaterializerApp
          sectionId="composer"
          navigate={navigate}
          askContext={() => {}}
          intent={{ id: "i1", payload: { compositionId: `${COMPOSITION}:c9` } }}
          consumeIntent={consumeIntent}
          store={memoryStore()}
        />,
      ),
    );
    await waitFor(() => expect(consumeIntent).toHaveBeenCalledWith("i1"));
    expect(await screen.findByText("From elsewhere")).toBeTruthy();
  });

  it("leaves an intent it does not understand alone", async () => {
    h.connection = fakeConnection();
    const consumeIntent = vi.fn();
    render(
      withSession(
        <MaterializerApp
          sectionId="composer"
          navigate={vi.fn()}
          askContext={() => {}}
          intent={{ id: "i2", payload: { somethingElse: "x" } }}
          consumeIntent={consumeIntent}
          store={memoryStore()}
        />,
      ),
    );
    await screen.findByText(/Pick at least one source/);
    // An unrelated opener must not move somebody's window, and must not
    // have its instruction eaten.
    expect(consumeIntent).not.toHaveBeenCalled();
  });
});
