import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown, download: vi.fn(async () => {}) }));
vi.mock("../../src/apps/files/actions/download", () => ({ downloadArtifact: h.download }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { MaterializerApp } = await import("../../src/apps/materializer/MaterializerApp");
const { LocalMaterializerSettingsStore } = await import("../../src/apps/materializer/settings");
const { compositionRow, fakeConnection, recipeRow, refused, templateRow, withSession } = await import("./harness");

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
    // With no source or brief, the bar explains what is needed.
    expect(await screen.findByText(/Describe what to make/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Materialize" })).toBeNull();
  });

  it.each([
    ["What do you want made?", "statement", "Write a sample inventory."],
  ])("materializes from %s without requiring a graph source", async (label, field, value) => {
    const conn = fakeConnection({ materializeReply: { compositionId: "content-only" } as unknown as Row });
    mount(conn);
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Inventory" } });
    fireEvent.change(screen.getByLabelText(label), { target: { value: "   " } });
    expect(screen.queryByRole("button", { name: "Materialize" })).toBeNull();
    fireEvent.change(screen.getByLabelText(label), { target: { value } });
    fireEvent.click(screen.getByLabelText("Organization"));
    fireEvent.click(await screen.findByRole("option", { name: "Client A" }));
    fireEvent.click(await screen.findByRole("button", { name: "Materialize" }));
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    const args = conn.query.composeMaterialize.mock.calls[0]?.[0] ?? {};
    expect(args).toMatchObject({ name: "Inventory", [field]: value, accountIds: ["client-a"] });
    expect(args).not.toHaveProperty("sources");
    expect(args).not.toHaveProperty("draft");
    // Every ask carries a request id (a repeat of it finds this composition
    // rather than starting a second); startingSetup.test.tsx pins when it is
    // reused and when it is renewed.
    expect(args["requestId"]).toEqual(expect.stringMatching(/^[A-Za-z0-9_-]{8,111}$/));
    expect(screen.queryByLabelText("Draft")).toBeNull();
  });

  it("requires an explicit organization before composing, even when there is only one", async () => {
    const conn = fakeConnection();
    mount(conn);
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Welcome" } });
    fireEvent.change(screen.getByLabelText("What do you want made?"), { target: { value: "Use our reference image." } });
    expect(screen.queryByRole("button", { name: "Materialize" })).toBeNull();
    expect(conn.query.composeMaterialize).not.toHaveBeenCalled();
    fireEvent.click(screen.getByLabelText("Organization"));
    fireEvent.click(await screen.findByRole("option", { name: "Client A" }));
    expect(screen.getByRole("button", { name: "Materialize" })).toBeTruthy();
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
    fireEvent.click(screen.getByLabelText("Organization"));
    fireEvent.click(await screen.findByRole("option", { name: "Client A" }));
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
    fireEvent.click(screen.getByLabelText("Organization"));
    fireEvent.click(await screen.findByRole("option", { name: "Client A" }));
    fireEvent.click(await screen.findByRole("button", { name: "Materialize" }));

    // The rule and the words are the editor's (the SDK's fileNameProblem), shown where the name is typed.
    expect(await screen.findByText("Enter a file name.")).toBeTruthy();
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
    fireEvent.click(screen.getByLabelText("Organization"));
    fireEvent.click(await screen.findByRole("option", { name: "Client A" }));
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
  it("opens the generated file in the browser editor with its current Library name", async () => {
    const conn = fakeConnection({ compositions: [compositionRow({ id: "c-output", outputFileId: "f-output" })],
      outputArtifact: { id: "artifact-output" } as Row,
      outputFile: { id: "f-output", name: "Q3 report.pdf", mimeType: "application/pdf", size: 900 } as Row });
    h.connection = conn;
    const tab = { navigate: vi.fn(), close: vi.fn() };
    const ports = { reserve: vi.fn(() => tab), navigate: vi.fn(), schedule: vi.fn(() => () => {}) };
    render(withSession(<MaterializerApp sectionId="composer" navigate={() => {}} askContext={() => {}}
      intent={{ id: "open", payload: { compositionId: "c-output" } }} store={memoryStore()} handoffPorts={ports} />));
    fireEvent.click(await screen.findByRole("button", { name: "Open Q3 report" }));
    expect(ports.reserve).toHaveBeenCalledOnce();
    await waitFor(() => expect(tab.navigate).toHaveBeenCalledOnce());
    const link = new URL(tab.navigate.mock.calls[0]![0]);
    expect(link.origin).toBe("https://vscode.memql.example.com");
    expect(link.searchParams.get("resource")).toBe("memql-file://memql.example.com/artifacts/artifact-output/Q3%20report.pdf");
    expect(conn.query.libraryArtifactForFile).toHaveBeenCalledWith({ fileId: "f-output" });
    expect(tab.close).not.toHaveBeenCalled();
  });

  it("downloads a generated ZIP intact even when its displayed name has no extension", async () => {
    h.connection = fakeConnection({ compositions: [compositionRow({ id: "archive", outputFileId: "bundle", deployableKind: "site" })],
      outputArtifact: { id: "bundle-artifact" } as Row,
      outputFile: { id: "bundle", name: "Site resources", mimeType: "application/zip", size: 40 } as Row });
    const tab = { navigate: vi.fn(), close: vi.fn() };
    render(withSession(<MaterializerApp sectionId="composer" navigate={() => {}} askContext={() => {}}
      intent={{ id: "open", payload: { compositionId: "archive" } }} store={memoryStore()}
      handoffPorts={{ reserve: () => tab, navigate: vi.fn(), schedule: () => () => {} }} />));
    fireEvent.click(await screen.findByRole("button", { name: "Open Q3 report" }));
    await waitFor(() => expect(h.download).toHaveBeenCalledWith(expect.objectContaining({ artifactId: "bundle-artifact", fileId: "bundle", name: "Site resources" })));
    expect(tab.close).toHaveBeenCalledOnce();
    expect(tab.navigate).not.toHaveBeenCalled();
  });

  it("closes the reserved editor tab and shows a missing output without guessing its identity", async () => {
    h.connection = fakeConnection({ compositions: [compositionRow({ id: "missing", outputFileId: "absent" })] });
    const tab = { navigate: vi.fn(), close: vi.fn() };
    render(withSession(<MaterializerApp sectionId="composer" navigate={() => {}} askContext={() => {}}
      intent={{ id: "open", payload: { compositionId: "missing" } }} store={memoryStore()}
      handoffPorts={{ reserve: () => tab, navigate: vi.fn(), schedule: () => () => {} }} />));
    fireEvent.click(await screen.findByRole("button", { name: "Open Q3 report" }));
    expect(await screen.findByText(/not available in your Library yet/)).toBeTruthy();
    expect(tab.close).toHaveBeenCalledOnce();
    expect(tab.navigate).not.toHaveBeenCalled();
  });

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

  // THE RECORD'S OWN SENTENCE, WHEN IT HAS ONE. The stamp step writes provenanceNote
  // knowing which branch it took, and the format alone cannot: a blank Markdown file
  // carries no front matter because it was filed blank, not because Markdown has
  // nowhere to put it.
  it("states the record's own provenance sentence", async () => {
    const conn = fakeConnection({ compositions: [compositionRow({ id: `${COMPOSITION}:c1` })] });
    mountOn(conn, `${COMPOSITION}:c1`);
    expect(await screen.findByText("Provenance is in this PDF's XMP packet and Info dictionary.")).toBeTruthy();
    expect(screen.queryByText("the file carries it")).toBeNull();
  });

  it("falls back to the claim the format can make for a record with no sentence of its own", async () => {
    const conn = fakeConnection({
      compositions: [
        compositionRow({ id: `${COMPOSITION}:c1`, provenanceNote: "" }),
        compositionRow({ id: `${COMPOSITION}:c2`, name: "Figures", format: "csv", provenanceEmbedded: false, provenanceNote: "" }),
      ],
    });
    mountOn(conn, `${COMPOSITION}:c1`);
    expect(await screen.findByText("the file carries it")).toBeTruthy();
    cleanup();
    mountOn(conn, `${COMPOSITION}:c2`);
    expect(await screen.findByText(/only copy/)).toBeTruthy();
  });

  it("says a blank file was created blank, and never that its format has nowhere to put provenance", async () => {
    const note = "Created blank. Its history is kept on its composition, not in the file.";
    for (const [id, format] of [["c-md", "markdown"], ["c-html", "html"]] as const) {
      const conn = fakeConnection({
        compositions: [compositionRow({ id: `${COMPOSITION}:${id}`, name: "Notes", format, statement: "", sources: [], modelsUsed: [], provenanceEmbedded: false, provenanceNote: note })],
      });
      mountOn(conn, `${COMPOSITION}:${id}`);
      expect(await screen.findByText(note)).toBeTruthy();
      expect(screen.queryByText(/nowhere to put it/)).toBeNull();
      expect(screen.getByText("This file was created blank.")).toBeTruthy();
      expect(screen.queryByText(/composed text/)).toBeNull();
      // A recipe of a blank file would have no brief and no sources, which a recipe run refuses.
      expect(screen.getByRole("button", { name: "Open Notes" })).toBeTruthy();
      expect(screen.queryByRole("button", { name: "Save as recipe" })).toBeNull();
      cleanup();
    }
  });

  it("says the composed text is in the file for a file a model wrote", async () => {
    const conn = fakeConnection({ compositions: [compositionRow({ id: `${COMPOSITION}:c1` })] });
    mountOn(conn, `${COMPOSITION}:c1`);
    expect(await screen.findByText("The composed text is in the file. Open it to read what was made.")).toBeTruthy();
  });

  it("claims no file while the composition is still being made", async () => {
    const conn = fakeConnection({ compositions: [compositionRow({ id: `${COMPOSITION}:c1`, status: "composing", outputFileId: "" })] });
    mountOn(conn, `${COMPOSITION}:c1`);
    await screen.findByRole("button", { name: "Stop" });
    expect(screen.queryByText(/composed text|created blank|created from a description/)).toBeNull();
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

  // FOUND IN A BROWSER, PINNED HERE. The settled Sources column used to
  // render `picked` -- this window's own form state -- so a composition
  // opened from the list or from another app's intent showed an EMPTY
  // "Made from" over a record holding two sources. jsdom renders the same
  // DOM, so nothing but a rendered pass would have found it; this is what
  // stops it coming back.
  it("a settled composition's sources come from the record, not the form", async () => {
    const conn = fakeConnection({ compositions: [compositionRow({ id: `${COMPOSITION}:c1` })] });
    mountOn(conn, `${COMPOSITION}:c1`);
    expect(await screen.findByText("Made from")).toBeTruthy();
    expect(screen.getByText("INV-1001")).toBeTruthy();
    expect(screen.getByText("INV-1002")).toBeTruthy();
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

// EVERY FEED IS TESTED FOR LIVENESS, NOT JUST THE ONE THE CUE IS ABOUT.
//
// A test that seeds rows and renders passes identically against a feed that
// SEEDED AND NEVER SUBSCRIBED -- correct on load, frozen afterwards, which
// on screen is indistinguishable from a surface with nothing new to show.
// The concept constant is the thing that breaks: `TEMPLATE_CONCEPT` was
// re-pointed when the concept was renamed to `composeTemplate`, and a
// subscription on the OLD id would still have seeded (the seed is a query
// call) while receiving nothing forever.
//
// So each of the three feeds gets one case that emits AFTER mount. The
// negative control is the rename itself: point any of these at the wrong
// concept id and only these three cases fail.
describe("the three feeds are live", () => {
  it("a composition arriving after mount reaches the list", async () => {
    const conn = fakeConnection({ compositions: [] });
    mount(conn, "materialized");
    await screen.findByText(/Nothing materialized yet/);

    conn.subscriptions.emit(
      COMPOSITION,
      compositionRow({ id: `${COMPOSITION}:late`, name: "Arrived late" }),
      "NODE_CREATED",
    );
    expect(await screen.findByText("Arrived late")).toBeTruthy();
  });

  it("a template arriving after mount reaches the section", async () => {
    const conn = fakeConnection({ templates: [] });
    mount(conn, "templates");
    await screen.findByText(/No templates yet/);

    conn.subscriptions.emit(
      "v1:compose:composeTemplate",
      templateRow({ id: "v1:compose:composeTemplate:late", name: "Bound late" }),
      "NODE_CREATED",
    );
    expect(await screen.findByText("Bound late")).toBeTruthy();
  });

  it("a recipe arriving after mount reaches the section", async () => {
    const conn = fakeConnection({ recipes: [] });
    mount(conn, "templates");
    await screen.findByText(/No recipes yet/);

    conn.subscriptions.emit(
      "v1:compose:recipe",
      recipeRow({ id: "v1:compose:recipe:late", name: "Saved late" }),
      "NODE_CREATED",
    );
    expect(await screen.findByText("Saved late")).toBeTruthy();
  });
});

describe("templates and recipes", () => {
  it("says a template is a binding to a Library file rather than an upload", async () => {
    mount(fakeConnection(), "templates");
    fireEvent.click(await screen.findByRole("button", { name: "Bind a file" }));
    expect(await screen.findByText(/Upload the file in Files first/)).toBeTruthy();
  });

  it("renders a recipe's run count and its last run as a moment, not a raw timestamp", async () => {
    const conn = fakeConnection({ recipes: [recipeRow({ id: "v1:compose:recipe:r1" })] });
    mount(conn, "templates");
    expect(await screen.findByText("Acme quarterly report")).toBeTruthy();
    // A raw RFC3339 string is the data voice, and "when did this last run"
    // is a question a person asks in months -- so it goes through the kit's
    // own formatter. The rendered pass caught 2026-06-30T09:00:00Z on the
    // page; jsdom renders the same DOM and asserts nothing about either.
    expect(screen.getByText(/Made 2 times, last on/)).toBeTruthy();
    expect(screen.queryByText(/2026-06-30T09:00:00Z/)).toBeNull();
  });

  it("says 'not run yet' rather than a zero and a dash", async () => {
    const conn = fakeConnection({
      recipes: [recipeRow({ id: "v1:compose:recipe:r2", name: "Fresh", runCount: 0, lastRunAt: "" })],
    });
    mount(conn, "templates");
    expect(await screen.findByText(/Not run yet/)).toBeTruthy();
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
    await screen.findByText(/Describe what to make/);
    // An unrelated opener must not move somebody's window, and must not
    // have its instruction eaten.
    expect(consumeIntent).not.toHaveBeenCalled();
  });
});


describe("a composition whose executing node stopped", () => {
  const RUN = "v1:work:run";
  const run = (over: Record<string, unknown> = {}): Row => ({
    id: "r-interrupted", ownerUserId: "me", goalId: "g-interrupted", status: "running",
    createdAt: "2026-09-09T17:16:44Z", nodeId: "agent-that-stopped", ...over,
  } as Row);
  const composition = (over: Record<string, unknown> = {}) => compositionRow({
    id: "c-interrupted", name: "Interrupted document", ownerUserId: "me",
    goalId: "g-interrupted", runId: "r-interrupted", status: "composing", outputFileId: "", ...over,
  });

  it("updates the retained list from another replica's run event and follows a resume", async () => {
    const conn = fakeConnection({ compositions: [composition()], runs: [run()] });
    mount(conn, "materialized");
    await screen.findByText("Composing");
    conn.subscriptions.emit(RUN, run({ status: "abandoned", createdAt: "2026-09-09T17:20:00Z",
      errorMessage: "The cluster lost the executing node. Resume from Nexus.", nodeId: "sweep-on-other-replica" }));
    expect(await screen.findByText("Failed")).toBeTruthy();
    expect(screen.getByText("Interrupted document").closest("[data-arrival]")?.getAttribute("data-arrival")).toBeTruthy();
    conn.subscriptions.emit(RUN, run({ status: "running", createdAt: "2026-09-09T17:21:00Z" }));
    expect(await screen.findByText("Composing")).toBeTruthy();
    expect(conn.query.composeCancel).not.toHaveBeenCalled();
  });

  it.each(["failed", "cancelled", "abandoned"])("reads an already %s run when opening the composer", async (status) => {
    const conn = fakeConnection({ compositions: [composition()], runs: [run({ status,
      errorMessage: "The node stopped before the file was written." })] });
    h.connection = conn;
    render(withSession(<MaterializerApp sectionId="composer" navigate={vi.fn()} askContext={() => {}}
      intent={{ id: "interrupted", payload: { compositionId: "c-interrupted" } }} store={memoryStore()} />));
    expect(await screen.findByRole("button", { name: "Start over from this" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Stop" })).toBeNull();
    if (status !== "cancelled") expect(screen.getAllByText(/The node stopped before the file was written/).length).toBeGreaterThan(0);
    expect(conn.query.workRunForOwner).toHaveBeenCalledWith({ runId: "r-interrupted" }, expect.anything());
  });

  it.each([
    { status: "ready", outputFileId: "file-completed", expected: "Ready" },
    { runId: "r-new-attempt", expected: "Composing" },
    { ownerUserId: "someone-else", expected: "Composing" },
    { goalId: "g-other", expected: "Composing" },
  ])("preserves a completed or unrelated composition: %j", async ({ expected, ...over }) => {
    const conn = fakeConnection({ compositions: [composition(over)], runs: [run({ status: "abandoned" })] });
    mount(conn, "materialized");
    expect(await screen.findByText(expected)).toBeTruthy();
    conn.subscriptions.emit(RUN, run({ status: "failed", createdAt: "2026-09-09T17:20:00Z" }));
    await waitFor(() => expect(screen.queryByText("Failed")).toBeNull());
  });
});


describe("record list count and action boundaries", () => {
  it("counts visible templates and recipes only after their independent reads settle", async () => {
    const conn = fakeConnection({
      templates: [templateRow({ id: "visible", name: "Visible template" }), templateRow({ id: "hidden", name: "Archived template", archived: true })],
      recipes: [recipeRow({ id: "recipe", name: "Runnable recipe" })],
    });
    const { view } = mount(conn, "templates");
    expect(view.container.querySelector(".os-head-meta")).toBeNull();
    await waitFor(() => expect(view.container.querySelector(".os-head-meta")?.textContent).toBe("1"));
    expect(view.container.querySelector(".os-subhead-meta")?.textContent).toBe("1");
    expect(screen.queryByText("Archived template")).toBeNull();
    const run = screen.getByRole("button", { name: "Run it again" });
    expect(run.closest(".os-record-actions")).not.toBeNull();
    expect(run.closest(".os-record-summary")).toBeNull();
    fireEvent.click(run);
    await waitFor(() => expect(conn.query.composeRunRecipe).toHaveBeenCalled());
  });

  it("leaves a denied templates count absent while still counting readable recipes", async () => {
    const conn = fakeConnection({ recipes: [recipeRow({ id: "recipe", name: "Readable recipe" })] });
    conn.query.composeTemplates.mockRejectedValue(new Error("templates denied"));
    const { view } = mount(conn, "templates");
    await screen.findByText("Readable recipe");
    expect(view.container.querySelector(".os-head .os-head-meta")).toBeNull();
    expect(view.container.querySelector(".os-subhead-meta")?.textContent).toBe("1");
  });
});


// A READ THAT FAILS SAYS THE CLUSTER'S SENTENCE ALONE, as a refused write does. A refusal arrives with
// the call's name in front and inside the engine's frame (`refused` gives the real wording), and the
// person reads only the sentence: what the composer can offer, what a source finds, and opening a file.
describe("a read that fails", () => {
  const FRAMES = /composableConcepts|composeResolveSources|libraryArtifactForFile|MemQL engine|Details:|compose:|library:/;
  const line = (text: string) => screen.findByText(text, { selector: ".os-notice-line" });

  it("says why the composer has nothing to offer, in the cluster's words alone", async () => {
    const conn = fakeConnection();
    conn.query.composableConcepts.mockRejectedValue(refused("composableConcepts", "compose: the concept registry is not readable on this node"));
    mount(conn);
    expect(await line("the concept registry is not readable on this node")).toBeTruthy();
    expect(document.body.textContent).not.toMatch(FRAMES);
  });

  it("says why a source could not be resolved, in the cluster's words alone", async () => {
    const conn = fakeConnection({
      composables: { concepts: [{ id: "v1:x:invoice", as: "invoice", fields: [], list: "openInvoices", marked: true }], registryAvailable: true } as unknown as Row,
    });
    conn.query.composeResolveSources.mockRejectedValue(refused("composeResolveSources", "compose: the sources could not be read: query openInvoices is not readable by you"));
    mount(conn);
    fireEvent.click(await screen.findByRole("button", { name: /invoice/ }));
    expect(await line("the sources could not be read: query openInvoices is not readable by you")).toBeTruthy();
    expect(document.body.textContent).not.toMatch(FRAMES);
  });

  it("says why a file could not be opened, in the cluster's words alone", async () => {
    const conn = fakeConnection({ compositions: [compositionRow({ id: "c-output", outputFileId: "f-output" })] });
    conn.query.libraryArtifactForFile.mockRejectedValue(refused("libraryArtifactForFile", "library: the artifact index is not reachable"));
    h.connection = conn;
    const tab = { navigate: vi.fn(), close: vi.fn() };
    render(withSession(<MaterializerApp sectionId="composer" navigate={() => {}} askContext={() => {}}
      intent={{ id: "open", payload: { compositionId: "c-output" } }} store={memoryStore()}
      handoffPorts={{ reserve: () => tab, navigate: vi.fn(), schedule: () => () => {} }} />));
    fireEvent.click(await screen.findByRole("button", { name: "Open Q3 report" }));
    expect(await line("the artifact index is not reachable")).toBeTruthy();
    expect(document.body.textContent).not.toMatch(FRAMES);
  });
});

describe("organization and reference metadata", () => {
  it("saves an email recipe without dropping its reference bundle or client", async () => {
    const conn = fakeConnection({ compositions: [compositionRow({id: "email-result", accountIds: ["client-a"], outputKind: "email_template", format: "json", sources: [
      {kind: "library_file", ref: "bundle", label: "brand.zip", content: true, includeImages: true},
      {kind: "library_file", ref: "example", label: "reference.png", content: true, includeImages: false},
    ]})] });
    h.connection = conn;
    render(withSession(<MaterializerApp sectionId="composer" navigate={vi.fn()} askContext={() => {}}
      intent={{id: "open-email", payload: {compositionId: "email-result"}}} store={memoryStore()} />));
    fireEvent.click(await screen.findByRole("button", {name: "Save as recipe"}));
    await waitFor(() => expect(conn.query.createComposeRecipe).toHaveBeenCalledOnce());
    expect(conn.query.createComposeRecipe.mock.calls[0]?.[0]).toMatchObject({accountIds: ["client-a"], outputKind: "email_template", format: "json", sourceSelectors: [
      {kind: "library_file", selector: "bundle", label: "brand.zip", content: true, includeImages: true},
      {kind: "library_file", selector: "example", label: "reference.png", content: true, includeImages: false},
    ]});
  });

  it("keeps template form entries when the cluster refuses the binding", async () => {
    const conn = fakeConnection({writeError: new Error("File access changed"), files: [
      {id: "artifact-shell", kind: "file", sourceConceptRef: "brand-file", title: "Brand shell.docx"},
    ]});
    mount(conn, "templates");
    fireEvent.click(screen.getByRole("button", {name: "Bind a file"}));
    fireEvent.change(screen.getByLabelText("Name"), {target: {value: "Brand shell"}});
    fireEvent.click(screen.getByLabelText("Library file"));
    fireEvent.click(await screen.findByRole("option", {name: "Brand shell.docx"}));
    fireEvent.click(screen.getByLabelText("Organization"));
    fireEvent.click(await screen.findByRole("option", {name: "Client A"}));
    fireEvent.click(screen.getByRole("button", {name: "Bind it"}));
    await screen.findByText("File access changed");
    expect((screen.getByLabelText("Name") as HTMLInputElement).value).toBe("Brand shell");
    expect(screen.getByLabelText("Library file").textContent).toContain("Brand shell.docx");
  });

  it("selects a named file and stores the organization when binding a template", async () => {
    const conn = fakeConnection({files: [
      {id: "artifact-shell", kind: "file", sourceConceptRef: "brand-file", title: "Brand shell.docx"},
      {id: "artifact-zip", kind: "file", sourceConceptRef: "bundle", title: "assets.zip"},
      {id: "artifact-old", kind: "file", sourceConceptRef: "old", title: "Old shell.docx", archived: true},
    ]});
    mount(conn, "templates");
    fireEvent.click(screen.getByRole("button", {name: "Bind a file"}));
    fireEvent.change(screen.getByLabelText("Name"), {target: {value: "Brand shell"}});
    fireEvent.click(screen.getByLabelText("Library file"));
    fireEvent.click(await screen.findByRole("option", {name: "Brand shell.docx"}));
    expect(screen.queryByRole("option", {name: "assets.zip"})).toBeNull();
    expect(screen.queryByRole("option", {name: "Old shell.docx"})).toBeNull();
    expect(screen.queryByRole("button", {name: "Bind it"})).toBeNull();
    fireEvent.click(screen.getByLabelText("Organization"));
    fireEvent.click(await screen.findByRole("option", {name: "Client A"}));
    fireEvent.click(screen.getByRole("button", {name: "Bind it"}));
    await waitFor(() => expect(conn.query.createComposeTemplate).toHaveBeenCalledOnce());
    expect(conn.query.createComposeTemplate.mock.calls[0]?.[0]).toMatchObject({accountIds: ["client-a"], fileId: "brand-file"});
  });
});
