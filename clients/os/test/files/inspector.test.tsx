import { readFileSync } from "node:fs";
import { join } from "node:path";
import { screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

// The inspector's presentation pass: the horizontal scroll, the copyable
// values, the Ask affordance moving into the header, and the move picker
// leaving.
//
// The connection seam is mocked at the MODULE, as every other Files suite
// does, so the real LiveCollection retain/seed path runs against the
// harness's executeNamed fake (harness.tsx's header says why that matters).
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { artifactRow, click, fakeConnection, fileRow, folderRow, renderFiles, openDetails } from "./harness";

/** What a real artifact id looks like: one unbreakable word, wider than the
 *  panel it lands in. The whole overflow story is about this string. */
const LONG_ID = "v1:library:artifact:8f2c41d9a7b6e05c3f1a9d84b2c7e6f0";

beforeEach(() => {
  h.connection = null;
});

async function openInspector(title: string): Promise<HTMLElement> {
  await openDetails(title);
  return screen.getByRole("region", { name: "File details" });
}

it("explains failed uploads and offers no open or download for missing bytes", async () => {
  h.connection = fakeConnection({
    artifacts: [artifactRow({ id: "failed-artifact", kind: "file", title: "notes.md", sourceConceptRef: "failed-file" })],
    files: [{ id: "failed-file", name: "notes.md", status: "failed", blobUrl: "library/person/file/notes.md", versionNumber: 1 }],
  });
  await renderFiles();
  const inspector = await openInspector("notes\\.md");
  expect(within(inspector).getByText("This upload did not finish.")).toBeTruthy();
  expect(within(inspector).getByText("Upload the original file again from Files.")).toBeTruthy();
  expect(within(inspector).queryByRole("button", { name: "Open" })).toBeNull();
  expect(within(inspector).queryByRole("button", { name: "Download" })).toBeNull();
});

it("keeps a stored file available when only its analysis failed", async () => {
  h.connection = fakeConnection({
    artifacts: [artifactRow({ id: "saved-artifact", kind: "file", title: "notes.md", sourceConceptRef: "saved-file" })],
    files: [{ id: "saved-file", name: "notes.md", status: "failed", blobUrl: "https://storage.example/container/library/person/file/notes.md", versionNumber: 1 }],
  });
  await renderFiles();
  const inspector = await openInspector("notes\\.md");
  expect(within(inspector).queryByText("This upload did not finish.")).toBeNull();
  expect(within(inspector).getByRole("button", { name: "Open" })).toBeTruthy();
});

describe("the inspector header", () => {
  it("declares the selected file for the window Ask without a duplicate icon", async () => {
    h.connection = fakeConnection({ artifacts: [artifactRow({ id: "a-1", title: "brief.pdf" })] });
    await renderFiles();
    const inspector = await openInspector("brief\\.pdf");
    expect(within(inspector).queryByRole("button", { name: "Ask about brief.pdf" })).toBeNull();
    expect(JSON.parse(inspector.getAttribute("data-os-page-context")!)).toMatchObject({page:"File details",fileId:"a-1",name:"brief.pdf"});
  });

  it("names the kind in the file details", async () => {
    h.connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "brief.pdf", kind: "document" })],
    });
    await renderFiles();
    const inspector = await openInspector("brief\\.pdf");

    // The glyph is the same mark the list row carries and it is NAMED, so the
    // kind survives for a reader who never sees a glyph...
    expect(within(inspector).getByText("document")).toBeTruthy();
    // ...and it is not also a row in the facts table.
    expect(within(inspector).getByText("Kind")).toBeTruthy();
  });
});

// Item 11 (José, Nov 19: "this is how details should show up", on Fleet's
// machine Details tab; Jesús picked draft A, Nov 20).
describe("the details page in Fleet's layout", () => {
  it("names each section in its own column, in today's order, with copy only where Fleet offers it", async () => {
    h.connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "q3.pdf", format: "pdf", sourceConceptRef: "v1:library:file:f-1" })],
      files: [fileRow({ id: "f-1", name: "q3.pdf", versionNumber: 1 })],
      versions: [],
    });
    await renderFiles();
    const inspector = await openInspector("q3\\.pdf");

    const details = inspector.querySelector(".os-details") as HTMLElement;
    const sections = Array.from(details.querySelectorAll(":scope > .os-details-section"));
    expect(sections.map((section) => section.getAttribute("aria-label"))).toEqual(["Details", "Labels", "Client", "Version history"]);
    expect(sections.map((section) => section.querySelector(".os-subhead")?.firstChild?.textContent)).toEqual(["Details", "Labels", "Client", "Versions"]);

    // Names, formats, exact times and identifiers copy; the origin sentence
    // and the kind are read, not copied (DESIGN.md "Quiet copying").
    const facts = within(sections[0] as HTMLElement);
    for (const label of ["Filed in", "Format", "Created", "Id"]) {
      expect(facts.getByRole("button", { name: `Copy ${label}` })).toBeTruthy();
    }
    expect(facts.queryByRole("button", { name: "Copy Origin" })).toBeNull();
    expect(facts.queryByRole("button", { name: "Copy Kind" })).toBeNull();

    // Client is one short row: the picker alone.
    expect(within(sections[2] as HTMLElement).getByLabelText("The client this file is about")).toBeTruthy();
    // The bottom bar stays.
    expect(within(inspector).getByRole("group", { name: "What you can do with this" })).toBeTruthy();
  });
});

describe("the move picker", () => {
  it("is gone from the inspector -- re-filing belongs to the row", async () => {
    h.connection = fakeConnection({
      folders: [folderRow({ id: "f-vid", name: "Client videos" })],
      artifacts: [artifactRow({ id: "a-1", title: "brief.pdf" })],
    });
    await renderFiles();
    const inspector = await openInspector("brief\\.pdf");

    expect(within(inspector).queryByLabelText("Move to folder")).toBeNull();
    expect(within(inspector).queryByText("Move to folder")).toBeNull();
    // The reachable positive: the panel IS rendered, so an empty query is
    // evidence about the panel rather than about a selector that matches
    // nothing anywhere.
    expect(within(inspector).getByText("Uploaded here")).toBeTruthy();
  });
});

describe("a copyable value", () => {
  // Item 11 (Nov 20): the id is a copy row of Fleet's Details layout -- the
  // whole value in the row, its copy icon at the row's end.
  it("renders the id as a copy row, with the whole value on its title", async () => {
    h.connection = fakeConnection({
      artifacts: [artifactRow({ id: LONG_ID, title: "brief.pdf" })],
    });
    await renderFiles();
    const inspector = await openInspector("brief\\.pdf");

    // Layout is not checkable in jsdom (every offsetWidth here is 0), so what
    // is checked is that the id is handed to the copy row's text element, with
    // the whole string on `title`. The rendered proof is a screenshot.
    const line = inspector.querySelector(`.os-copyvalue-row .os-copyvalue-text[title="${LONG_ID}"]`) as HTMLElement | null;
    expect(line).toBeTruthy();
    expect(line?.textContent).toBe(LONG_ID);
    expect(line?.getAttribute("title")).toBe(LONG_ID);
  });

  it("copies the WHOLE value and confirms in place", async () => {
    h.connection = fakeConnection({
      artifacts: [artifactRow({ id: LONG_ID, title: "brief.pdf" })],
    });
    await renderFiles();
    const inspector = await openInspector("brief\\.pdf");

    // Typed argument: an untyped vi.fn gives `mock.calls` an empty tuple
    // type, so indexing it is a typecheck error rather than a test failure.
    const writeText = vi.fn(async (_text: string) => {});
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    await click(within(inspector).getByRole("button", { name: "Copy Id" }));

    // The FULL value, not the truncated line -- which is the entire reason
    // this control exists rather than leaving somebody to select the text.
    expect(writeText).toHaveBeenCalledOnce();
    expect(writeText.mock.calls[0]![0]).toBe(LONG_ID);
    // The confirmation is the accessible name, so it reaches a screen reader
    // and not only somebody watching the glyph swap.
    expect(within(inspector).getByRole("button", { name: "Copied Id" })).toBeTruthy();
    expect(within(inspector).queryByRole("button", { name: "Copy Id" })).toBeNull();
    vi.unstubAllGlobals();
  });

  it("says so when the clipboard refuses, rather than doing nothing", async () => {
    h.connection = fakeConnection({
      artifacts: [artifactRow({ id: LONG_ID, title: "brief.pdf" })],
    });
    await renderFiles();
    const inspector = await openInspector("brief\\.pdf");

    vi.stubGlobal("navigator", {
      clipboard: { writeText: vi.fn(async () => Promise.reject(new Error("denied"))) },
    });
    await click(within(inspector).getByRole("button", { name: "Copy Id" }));

    // A copy button that lies is worse than none: the value here is
    // TRUNCATED, so a silent failure leaves the reader with no route to it at
    // all. The button stays uncopied, and it stays offering the copy.
    expect(within(inspector).getByText(/refused the copy/)).toBeTruthy();
    expect(within(inspector).getByRole("button", { name: "Copy Id" })).toBeTruthy();
    vi.unstubAllGlobals();
  });

  it("says so when the browser offers no clipboard at all", async () => {
    h.connection = fakeConnection({
      artifacts: [artifactRow({ id: LONG_ID, title: "brief.pdf" })],
    });
    await renderFiles();
    const inspector = await openInspector("brief\\.pdf");

    vi.stubGlobal("navigator", {});
    await click(within(inspector).getByRole("button", { name: "Copy Id" }));

    expect(within(inspector).getByText(/offers no clipboard/)).toBeTruthy();
    vi.unstubAllGlobals();
  });

  it("offers no button for a value there is nothing to copy of", async () => {
    h.connection = fakeConnection({
      // No plan produced this file, so the Plan fact does not render at all.
      artifacts: [artifactRow({ id: LONG_ID, title: "brief.pdf", producedByRunId: "" })],
    });
    await renderFiles();
    const inspector = await openInspector("brief\\.pdf");

    expect(within(inspector).queryByRole("button", { name: "Copy Plan" })).toBeNull();
    expect(within(inspector).getByRole("button", { name: "Copy Id" })).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// The overflow contract
// ---------------------------------------------------------------------------

const CSS = readFileSync(join(__dirname, "..", "..", "src", "styles", "index.css"), "utf8");

/**
 * Every declaration a selector carries, from EVERY rule that names it.
 *
 * A selector legitimately appears more than once -- the panel's own rule sets
 * its geometry and this pass's region adds the overflow guard beside its
 * reason -- so reading only the first block would report the guard missing
 * while it sits ten lines further down.
 */
function block(selector: string): string {
  const rules: string[] = [];
  let from = 0;
  for (;;) {
    const at = CSS.indexOf(`\n${selector} {`, from);
    if (at === -1) break;
    const open = CSS.indexOf("{", at);
    const close = CSS.indexOf("}", open);
    rules.push(CSS.slice(open + 1, close));
    from = close;
  }
  if (rules.length === 0) throw new Error(`no rule in index.css for selector: ${selector}`);
  return rules.join("\n");
}

describe("the inspector never scrolls sideways", () => {
  // WHAT IS ONLY CHECKABLE VISUALLY, said plainly: jsdom implements no layout,
  // so `scrollWidth`, `clientWidth` and `getComputedStyle` on a grid track all
  // answer nothing here. A test that asserted "the panel is not wider than its
  // container" would pass in this environment against the broken CSS as
  // readily as against the fixed CSS -- which is worse than no test, because
  // it would be read as coverage. So this pins the DECLARATIONS that remove
  // the overflow, and the rendered proof is a screenshot at real size.
  //
  // The reachable positive is `block()` itself: it throws when a selector is
  // absent, so a rule that gets renamed fails here rather than passing
  // vacuously against a regex that matches nothing.

  it("sizes the facts value column so it can shrink below its longest word", () => {
    // `1fr` carries an implicit min-content floor, and an artifact id has no
    // break opportunity in it -- so the track, not the value, is what pushed
    // the panel wider than its container.
    expect(block(".os-facts")).toContain("grid-template-columns: auto minmax(0, 1fr);");
    expect(block(".os-facts dd")).toContain("min-width: 0;");
  });

  it("truncates a long value rather than letting it widen the line", () => {
    expect(block(".os-copyvalue-text")).toContain("text-overflow: ellipsis;");
    expect(block(".os-copyvalue-text")).toContain("white-space: nowrap;");
    // The same min-content floor, one level down: without this the flex item
    // widens and the ellipsis is never reached.
    expect(block(".os-copyvalue-text")).toContain("min-width: 0;");
  });

  it("breaks the unbreakable token a story sentence can contain", () => {
    // THE SECOND SOURCE, and the one the guard would have hidden best: a
    // story once carried a forty-character id ("Produced by plan <id>") and
    // still carries a machine's name, either one a token with no break
    // opportunity inside an ordinary sentence. A cell truncates; a sentence
    // wraps.
    expect(block(".os-files-story > span:not(.os-dot)")).toContain("overflow-wrap: anywhere;");
    expect(block(".os-facts dd")).toContain("overflow-wrap: anywhere;");
  });

  it("keeps the guard on the panel itself, for the value nobody has added yet", () => {
    expect(block(".os-files-inspector")).toContain("overflow-x: hidden;");
  });

  it("keeps the explicit copy button visible and in the layout", () => {
    const copy = block(".os-copyvalue-copy");
    expect(copy).not.toContain("opacity: 0;");
    expect(copy).not.toContain("display: none");
    expect(copy).toContain("flex: 0 0 28px;");
  });
});
