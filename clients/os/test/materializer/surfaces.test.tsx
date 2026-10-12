import { useState } from "react";
import {
  act,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { Concepts, type Row } from "@znasllc-io/memql-sdk-core/client";
import { readTheBar } from "../readTheBar";
const h = vi.hoisted(() => ({
  connection: null as unknown,
  download: vi.fn(async () => {}),
}));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));
vi.mock("../../src/apps/files/actions/download", () => ({
  downloadArtifact: h.download,
}));
const { MaterializerApp } =
  await import("../../src/apps/materializer/MaterializerApp");
const {
  fakeConnection,
  compositionRow,
  templateRow,
  recipeRow,
  libraryFileRow,
  memoryStore,
  refused,
  withSession,
} = await import("./harness");
const { MATERIALIZER_OPENING_SECTIONS, sanitizeSettings } =
  await import("../../src/apps/materializer/settings");
const { formatMoment } = await import("../../src/kit");
type Conn = ReturnType<typeof fakeConnection>;
beforeEach(() => {
  localStorage.clear();
  h.download.mockClear();
});
function mount(
  conn: Conn,
  section = "materialized",
  settings: Record<string, unknown> = {},
  compositionId?: string,
) {
  h.connection = conn;
  const store = memoryStore(settings),
    tab = { navigate: vi.fn(), close: vi.fn() },
    ports = {
      reserve: vi.fn(() => tab),
      navigate: vi.fn(),
      schedule: vi.fn(() => () => {}),
    };
  let go!: (s: string) => void;
  function Harness() {
    const [sectionId, setSection] = useState(section);
    go = setSection;
    return withSession(
      <MaterializerApp
        sectionId={sectionId}
        navigate={setSection}
        askContext={() => {}}
        store={store}
        handoffPorts={ports}
        intent={
          compositionId ? { id: "open", payload: { compositionId } } : undefined
        }
      />,
    );
  }
  const view = render(<Harness />);
  return { ...view, store, tab, ports, go: (s: string) => act(() => go(s)) };
}
async function press(name: string) {
  const button = await screen.findByRole("button", { name });
  await readTheBar();
  fireEvent.click(button);
}
const c = (over: Record<string, unknown> = {}) =>
  compositionRow({ id: "made", name: "Quarterly report", ...over });
const comp = "v1:compose:composition",
  tpl = Concepts.COMPOSE_COMPOSE_TEMPLATE,
  rec = Concepts.COMPOSE_RECIPE;

describe("Materialized", () => {
  it("opens on Materialized and starts the wizard on From from its plus", async () => {
    const conn = fakeConnection();
    mount(conn);
    expect(await screen.findByText("No files made yet")).toBeTruthy();
    expect(conn.query.libraryFilesForOwner).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Make a file" }));
    expect(
      await screen.findByRole("textbox", {
        name: "Search your files and records",
      }),
    ).toBeTruthy();
  });
  it("uses the kit select to refine by kind", async () => {
    mount(fakeConnection({ compositions: [c()] }));
    fireEvent.click(
      screen.getByRole("button", { name: "Refine what is listed" }),
    );
    const select = screen.getByRole("combobox", { name: "Kind of file" });
    fireEvent.click(select);
    expect(
      await screen.findByRole("option", { name: "Any kind" }),
    ).toBeTruthy();
    expect(document.querySelector("select")).toBeNull();
  });
  it("reads every page once without an archive filter and hides archived records", async () => {
    const conn = fakeConnection({
      compositions: [
        ...Array.from({ length: 51 }, (_, i) =>
          c({ id: `c${i}`, name: `Report ${i}` }),
        ),
        c({ id: "hidden", name: "Filed away", archived: true }),
      ],
    });
    const { container } = mount(conn);
    await screen.findByText("Report 50");
    expect(conn.query.compositions).toHaveBeenCalledTimes(2);
    expect(conn.query.compositions.mock.calls[0]?.[0]).toEqual({});
    expect(screen.queryByText("Filed away")).toBeNull();
    expect(container.querySelector(".os-head-meta")?.textContent).toContain(
      "51",
    );
  });
  it("reveals archived rows from preferences and explains an empty hidden set", async () => {
    const { go } = mount(
      fakeConnection({ compositions: [c({ archived: true })] }),
    );
    expect(await screen.findByText(/Archived ones are hidden/)).toBeTruthy();
    go("settings");
    fireEvent.click(screen.getByRole("switch", { name: "Include archived" }));
    go("materialized");
    expect(await screen.findByText("Quarterly report")).toBeTruthy();
  });
  it("says when a read failed and retries without claiming emptiness", async () => {
    const conn = fakeConnection();
    conn.query.compositions.mockRejectedValueOnce(
      refused("compositions", "Read denied"),
    );
    mount(conn);
    expect(
      await screen.findByText("This cluster did not return what you made."),
    ).toBeTruthy();
    expect(screen.queryByText("No files made yet")).toBeNull();
    await press("Try again");
    expect(await screen.findByText("No files made yet")).toBeTruthy();
  });
  it("preserves the filter after opening a record and returning", async () => {
    mount(
      fakeConnection({
        compositions: [c(), c({ id: "other", name: "Other" })],
      }),
    );
    fireEvent.click(
      screen.getByRole("button", { name: "Refine what is listed" }),
    );
    fireEvent.change(screen.getByRole("textbox", { name: "Search" }), {
      target: { value: "Quarterly" },
    });
    fireEvent.click(
      await screen.findByRole("button", { name: /Quarterly report/ }),
    );
    await press("Back to Materialized");
    expect(screen.queryByText("Other")).toBeNull();
    expect(screen.getByText("Quarterly report")).toBeTruthy();
  });
  it("accepts arrivals but never rings for a re-stamped run id", async () => {
    const conn = fakeConnection({ compositions: [c()] });
    const { container } = mount(conn);
    await screen.findByText("Quarterly report");
    act(() => conn.subscriptions.emit(comp, c({ runId: "new-run" })));
    expect(container.querySelector("[data-arrival]")).toBeNull();
    act(() => conn.subscriptions.emit(comp, c({ name: "Renamed" })));
    await screen.findByText("Renamed");
    expect(container.querySelector("[data-arrival]")).not.toBeNull();
    act(() =>
      conn.subscriptions.emit(comp, c({ id: "new", name: "New report" })),
    );
    expect(await screen.findByText("New report")).toBeTruthy();
  });
});

describe("made file pages", () => {
  it("shows the stored facts and provenance", async () => {
    mount(
      fakeConnection({
        compositions: [
          c({
            provenanceNote: "Written into the PDF",
            sources: [
              { kind: "query", ref: "query invoices()", label: "Invoices" },
            ],
            modelsUsed: [],
          }),
        ],
      }),
      "materialized",
      {},
      "made",
    );
    expect(await screen.findByText("Written into the PDF")).toBeTruthy();
    expect(screen.getByText("Invoices")).toBeTruthy();
    expect(screen.getByText("No model")).toBeTruthy();
  });
  it("claims blank and composed files accurately", async () => {
    const conn = fakeConnection({
      compositions: [
        c({
          format: "markdown",
          statement: "",
          sources: [],
          modelsUsed: [],
          provenanceEmbedded: false,
          provenanceNote: "Created blank.",
        }),
      ],
    });
    mount(conn, "materialized", {}, "made");
    expect(
      await screen.findByText("This file was created blank."),
    ).toBeTruthy();
    act(() => conn.subscriptions.emit(comp, c()));
    expect(
      await screen.findByText("The composed text is in the file."),
    ).toBeTruthy();
  });
  it("opens the current Library name and artifact, never a guessed identity", async () => {
    const conn = fakeConnection({
      compositions: [c({ outputFileId: "file" })],
      outputArtifact: { id: "artifact" } as Row,
      outputFile: {
        id: "file",
        name: "Current name.pdf",
        mimeType: "application/pdf",
        size: 90,
      } as Row,
    });
    const { tab } = mount(conn, "materialized", {}, "made");
    await press("Open in editor");
    await waitFor(() => expect(tab.navigate).toHaveBeenCalledOnce());
    expect(tab.navigate.mock.calls[0]?.[0]).toContain("Current%2520name.pdf");
    expect(conn.query.libraryArtifactForFile).toHaveBeenCalledWith({
      fileId: "file",
    });
  });
  it("downloads ZIP packages even with no extension in their name", async () => {
    const conn = fakeConnection({
      compositions: [c({ outputFileId: "file", deployableKind: "spa" })],
      outputArtifact: { id: "artifact" } as Row,
      outputFile: {
        id: "file",
        name: "Site resources",
        mimeType: "application/zip",
        size: 90,
      } as Row,
    });
    const { tab } = mount(conn, "materialized", {}, "made");
    await press("Download");
    await waitFor(() =>
      expect(h.download).toHaveBeenCalledWith(
        expect.objectContaining({
          artifactId: "artifact",
          name: "Site resources",
        }),
      ),
    );
    expect(tab.close).toHaveBeenCalledOnce();
    expect(tab.navigate).not.toHaveBeenCalled();
  });
  it("closes the editor reservation when the output cannot be read", async () => {
    const { tab } = mount(
      fakeConnection({ compositions: [c()] }),
      "materialized",
      {},
      "made",
    );
    await press("Open in editor");
    expect(
      await screen.findByText(/not available in your Library yet/, {
        selector: ".os-notice-line",
      }),
    ).toBeTruthy();
    expect(tab.close).toHaveBeenCalledOnce();
    expect(tab.navigate).not.toHaveBeenCalled();
  });
  it("keeps archive and restore on the page and never archives the output file", async () => {
    const conn = fakeConnection({ compositions: [c()] });
    mount(conn, "materialized", {}, "made");
    await press("Archive");
    expect(conn.query.archiveComposition).toHaveBeenCalledWith({
      compositionId: "made",
    });
    act(() => conn.subscriptions.emit(comp, c({ archived: true })));
    expect(await screen.findByText("The file is still in Files.")).toBeTruthy();
    await press("Restore");
    expect(conn.query.restoreComposition).toHaveBeenCalledWith({
      compositionId: "made",
    });
  });
  it("saves an email recipe with layout, content choices and organization once", async () => {
    const conn = fakeConnection({
      compositions: [
        c({
          outputKind: "email_template",
          layoutId: "layout",
          accountIds: ["org"],
          sources: [
            {
              kind: "library_file",
              ref: "reference",
              label: "Reference",
              content: true,
              includeImages: false,
            },
          ],
        }),
      ],
    });
    mount(conn, "materialized", {}, "made");
    await press("Save as recipe");
    await waitFor(() =>
      expect(conn.query.createComposeRecipe).toHaveBeenCalledOnce(),
    );
    expect(conn.query.createComposeRecipe.mock.calls[0]?.[0]).toMatchObject({
      layoutId: "layout",
      accountIds: ["org"],
      outputKind: "email_template",
      sourceSelectors: [
        {
          kind: "library_file",
          selector: "reference",
          label: "Reference",
          content: true,
          includeImages: false,
        },
      ],
    });
    expect(screen.queryByRole("button", { name: "Save as recipe" })).toBeNull();
  });
  it.each(["failed", "cancelled", "abandoned"])(
    "follows a %s run from another replica",
    async (status) => {
      const row = c({
        status: "composing",
        outputFileId: "",
        runId: "run",
        goalId: "goal",
        ownerUserId: "me",
      });
      const conn = fakeConnection({
        compositions: [row],
        runs: [
          {
            id: "run",
            goalId: "goal",
            ownerUserId: "me",
            status: "running",
          } as Row,
        ],
      });
      mount(conn, "materialized", {}, "made");
      await screen.findByRole("button", { name: "Stop" });
      act(() =>
        conn.subscriptions.emit("v1:work:run", {
          id: "run",
          goalId: "goal",
          ownerUserId: "me",
          status,
          errorMessage: "Node stopped",
          createdAt: "2026-10-12T12:00:00Z",
        } as Row),
      );
      expect(
        await screen.findByRole("button", { name: "Start over" }),
      ).toBeTruthy();
    },
  );
});

describe("Templates", () => {
  it("retries a failed Library read when adding a template", async () => {
    const conn = fakeConnection({
      libraryFiles: [libraryFileRow({ id: "file", name: "Recovered.md" })],
    });
    conn.query.libraryFilesForOwner.mockRejectedValueOnce(
      refused("libraryFilesForOwner", "Read denied"),
    );
    mount(conn, "templates");
    fireEvent.click(
      await screen.findByRole("button", { name: "Add a template" }),
    );
    expect(
      await screen.findByText("This cluster did not return your files."),
    ).toBeTruthy();
    await press("Try again");
    expect(
      await screen.findByRole("button", { name: /Recovered.md/ }),
    ).toBeTruthy();
  });

  it("lists templates and what they ask for and waits for the read before counting", async () => {
    const conn = fakeConnection({
      templates: [
        templateRow({
          id: "tpl",
          name: "Report shell",
          placeholders: [{ name: "Quarter", required: true }],
        }),
      ],
    });
    mount(conn, "templates");
    const head = screen
      .getByRole("heading", { name: "Templates" })
      .closest(".os-head")!;
    expect(head.querySelector(".os-head-meta")).toBeNull();
    expect(await screen.findByText("Asks for Quarter")).toBeTruthy();
    expect(head.querySelector(".os-head-meta")?.textContent).toBe("1");
    expect(conn.query.libraryFilesForOwner).not.toHaveBeenCalled();
  });
  it("explains an empty list and accepts new templates live", async () => {
    const conn = fakeConnection();
    mount(conn, "templates");
    expect(
      await screen.findByText(
        "A template is a Library file whose look a new file can take.",
      ),
    ).toBeTruthy();
    act(() =>
      conn.subscriptions.emit(
        tpl,
        templateRow({ id: "tpl", name: "Report shell" }),
      ),
    );
    expect(await screen.findByText("Report shell")).toBeTruthy();
  });
  it("adds a template one step at a time, once, using the file's own id", async () => {
    const conn = fakeConnection({
      libraryFiles: [libraryFileRow({ id: "file", name: "Report shell.docx" })],
    });
    mount(conn, "templates");
    await press("Add a template");
    fireEvent.click(
      await screen.findByRole("button", { name: /Report shell.docx/ }),
    );
    await press("Add template");
    await waitFor(() =>
      expect(conn.query.createComposeTemplate).toHaveBeenCalledOnce(),
    );
    expect(conn.query.createComposeTemplate.mock.calls[0]?.[0]).toMatchObject({
      fileId: "file",
      name: "Report shell",
      format: "docx",
    });
    expect(
      conn.query.createComposeTemplate.mock.calls[0]?.[0],
    ).not.toHaveProperty("accountIds");
  });
  it("keeps the name and template id on a refused retry", async () => {
    const conn = fakeConnection({
      libraryFiles: [libraryFileRow({ id: "file", name: "Report.docx" })],
      writeError: new Error("The file was archived."),
    });
    mount(conn, "templates");
    await press("Add a template");
    fireEvent.click(await screen.findByRole("button", { name: /Report.docx/ }));
    await press("Add template");
    expect(await screen.findByText("The template was not added.")).toBeTruthy();
    await press("Add template");
    const calls = conn.query.createComposeTemplate.mock.calls;
    expect(calls[1]?.[0]?.templateId).toBe(calls[0]?.[0]?.templateId);
    await waitFor(() =>
      expect(conn.query.createComposeTemplate).toHaveBeenCalledTimes(2),
    );
    expect(conn.query.createComposeTemplate.mock.calls[1]?.[0]?.name).toBe(
      "Report",
    );
  });
  it("archives and restores from the template page", async () => {
    const conn = fakeConnection({
      templates: [templateRow({ id: "tpl", name: "Report shell" })],
    });
    mount(conn, "templates");
    fireEvent.click(
      await screen.findByRole("button", { name: /Report shell/ }),
    );
    await press("Archive");
    expect(conn.query.archiveComposeTemplate).toHaveBeenCalledWith({
      templateId: "tpl",
    });
    act(() =>
      conn.subscriptions.emit(
        tpl,
        templateRow({ id: "tpl", name: "Report shell", archived: true }),
      ),
    );
    await press("Restore");
    expect(conn.query.restoreComposeTemplate).toHaveBeenCalledWith({
      templateId: "tpl",
    });
  });
});

describe("Recipes", () => {
  it("lists the run count as a moment and never rings when it runs", async () => {
    const conn = fakeConnection({
      recipes: [recipeRow({ id: "r", name: "Recipe", runCount: 2 })],
    });
    const { container } = mount(conn, "recipes");
    expect(
      await screen.findByText(
        `Made 2 times, last on ${formatMoment("2026-06-30T09:00:00Z")}`,
      ),
    ).toBeTruthy();
    act(() =>
      conn.subscriptions.emit(
        rec,
        recipeRow({
          id: "r",
          name: "Recipe",
          runCount: 3,
          lastRunAt: "2026-10-12T12:00:00Z",
        }),
      ),
    );
    expect(container.querySelector("[data-arrival]")).toBeNull();
  });
  it("says not run yet and opens a recipe on its own page", async () => {
    mount(
      fakeConnection({
        recipes: [
          recipeRow({ id: "r", name: "Recipe", runCount: 0, lastRunAt: "" }),
        ],
      }),
      "recipes",
    );
    expect(await screen.findByText("Not run yet")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Run it again" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /Recipe/ }));
    expect(
      await screen.findByRole("button", { name: "Run it again" }),
    ).toBeTruthy();
  });
  it("runs once and enters the making stage before a row arrives", async () => {
    const conn = fakeConnection({
      recipes: [recipeRow({ id: "r", name: "Recipe" })],
      materializeReply: { compositionId: "made" } as Row,
    });
    mount(conn, "recipes");
    fireEvent.click(await screen.findByRole("button", { name: /Recipe/ }));
    await press("Run it again");
    expect(await screen.findByRole("button", { name: "Stop" })).toBeTruthy();
    expect(conn.query.composeRunRecipe).toHaveBeenCalledWith({ recipeId: "r" });
  });
  it("an empty list explains where recipes come from and accepts arrivals", async () => {
    const conn = fakeConnection();
    mount(conn, "recipes");
    expect(
      await screen.findByText(
        "Save a recipe from a made file to make it again.",
      ),
    ).toBeTruthy();
    act(() =>
      conn.subscriptions.emit(rec, recipeRow({ id: "r", name: "New recipe" })),
    );
    expect(await screen.findByText("New recipe")).toBeTruthy();
  });
});

describe("Preferences and ingredients", () => {
  it("keeps four preferences, with switches and only useful opening tabs", async () => {
    const { store } = mount(fakeConnection(), "settings");
    expect(screen.getByRole("heading", { name: "Settings" })).toBeTruthy();
    expect(screen.getAllByRole("switch")).toHaveLength(2);
    fireEvent.click(screen.getByRole("switch", { name: "Include archived" }));
    expect(store.load().showArchived).toBe(true);
    expect(MATERIALIZER_OPENING_SECTIONS).toEqual([
      "materialized",
      "composer",
      "templates",
      "recipes",
    ]);
    for (const defaultSection of ["logs", "settings", "unknown"])
      expect(sanitizeSettings({ defaultSection }).defaultSection).toBe(
        "materialized",
      );
    expect(screen.queryByText(/Spending/)).toBeNull();
  });
  it("offers only readable record types, marked first, and emits complete queries", async () => {
    const conn = fakeConnection({
      composables: {
        concepts: [
          { id: "other", as: "Other", list: "others", marked: false },
          { id: "invoice", as: "Invoices", list: "openInvoices", marked: true },
          { id: "missing", as: "No reader", list: "", marked: true },
        ],
        registryAvailable: true,
      } as Row,
    });
    mount(conn, "composer", { showUnmarkedConcepts: true });
    const invoices = await screen.findByRole("button", { name: /Invoices/ });
    expect(screen.queryByRole("button", { name: /No reader/ })).toBeNull();
    fireEvent.click(invoices);
    await waitFor(() =>
      expect(conn.query.composeResolveSources).toHaveBeenCalledWith({
        sources: [
          { kind: "query", ref: "query openInvoices()", label: "Invoices" },
        ],
      }),
    );
    expect(
      screen.getByRole("button", { name: "Remove Invoices" }),
    ).toBeTruthy();
  });
  it("shows a read failure without exposing internal frames", async () => {
    const conn = fakeConnection();
    conn.query.composableConcepts.mockRejectedValue(
      refused("composableConcepts", "compose: Not readable here."),
    );
    mount(conn, "composer");
    expect(await screen.findByText("Not readable here.")).toBeTruthy();
    expect(document.body.textContent).not.toMatch(
      /MemQL engine|composableConcepts|Details:/,
    );
  });
  it("searches older files and removes picked ones", async () => {
    const conn = fakeConnection({
      libraryFiles: Array.from({ length: 22 }, (_, i) =>
        libraryFileRow({
          id: `f${i}`,
          name: `File ${i}.md`,
          createdAt: `2026-10-${String(i + 1).padStart(2, "0")}T00:00:00Z`,
        }),
      ),
    });
    mount(conn, "composer");
    await screen.findByText("Search to find older files.");
    expect(screen.queryByText("File 0.md")).toBeNull();
    fireEvent.change(
      screen.getByRole("textbox", { name: "Search your files and records" }),
      { target: { value: "File 0" } },
    );
    fireEvent.click(await screen.findByRole("button", { name: /File 0.md/ }));
    fireEvent.click(screen.getByRole("button", { name: "Remove File 0.md" }));
    expect(
      screen.queryByRole("button", { name: "Remove File 0.md" }),
    ).toBeNull();
  });
});

describe("a composition whose executing node stopped", () => {
  const RUN = "v1:work:run";
  const run = (over: Record<string, unknown> = {}): Row =>
    ({
      id: "r-interrupted",
      ownerUserId: "me",
      goalId: "g-interrupted",
      status: "running",
      createdAt: "2026-09-09T17:16:44Z",
      nodeId: "agent-that-stopped",
      ...over,
    }) as Row;
  const composition = (over: Record<string, unknown> = {}) =>
    compositionRow({
      id: "c-interrupted",
      name: "Interrupted document",
      ownerUserId: "me",
      goalId: "g-interrupted",
      runId: "r-interrupted",
      status: "composing",
      outputFileId: "",
      ...over,
    });

  it("updates the retained list from another replica's run event and follows a resume", async () => {
    const conn = fakeConnection({
      compositions: [composition()],
      runs: [run()],
    });
    mount(conn, "materialized");
    await screen.findByText("Composing");
    conn.subscriptions.emit(
      RUN,
      run({
        status: "abandoned",
        createdAt: "2026-09-09T17:20:00Z",
        errorMessage: "The cluster lost the executing node. Resume from Nexus.",
        nodeId: "sweep-on-other-replica",
      }),
    );
    expect(await screen.findByText("Failed")).toBeTruthy();
    expect(
      screen
        .getByText("Interrupted document")
        .closest("[data-arrival]")
        ?.getAttribute("data-arrival"),
    ).toBeTruthy();
    conn.subscriptions.emit(
      RUN,
      run({ status: "running", createdAt: "2026-09-09T17:21:00Z" }),
    );
    expect(await screen.findByText("Composing")).toBeTruthy();
    expect(conn.query.composeCancel).not.toHaveBeenCalled();
  });

  it.each(["failed", "cancelled", "abandoned"])(
    "reads an already %s run when opening the composer",
    async (status) => {
      const conn = fakeConnection({
        compositions: [composition()],
        runs: [
          run({
            status,
            errorMessage: "The node stopped before the file was written.",
          }),
        ],
      });
      mount(conn, "materialized", {}, "c-interrupted");
      expect(
        await screen.findByRole("button", { name: "Start over" }),
      ).toBeTruthy();
      expect(screen.queryByRole("button", { name: "Stop" })).toBeNull();
      if (status !== "cancelled")
        expect(
          screen.getAllByText(/The node stopped before the file was written/)
            .length,
        ).toBeGreaterThan(0);
      expect(conn.query.workRunForOwner).toHaveBeenCalledWith(
        { runId: "r-interrupted" },
        expect.anything(),
      );
    },
  );

  it.each([
    { status: "ready", outputFileId: "file-completed", expected: "Ready" },
    { runId: "r-new-attempt", expected: "Composing" },
    { ownerUserId: "someone-else", expected: "Composing" },
    { goalId: "g-other", expected: "Composing" },
  ])(
    "preserves a completed or unrelated composition: %j",
    async ({ expected, ...over }) => {
      const conn = fakeConnection({
        compositions: [composition(over)],
        runs: [run({ status: "abandoned" })],
      });
      mount(conn, "materialized");
      expect(await screen.findByText(expected)).toBeTruthy();
      conn.subscriptions.emit(
        RUN,
        run({ status: "failed", createdAt: "2026-09-09T17:20:00Z" }),
      );
      await waitFor(() => expect(screen.queryByText("Failed")).toBeNull());
    },
  );
});
