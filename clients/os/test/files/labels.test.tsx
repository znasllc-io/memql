import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

// The connection seam, mocked at the MODULE as every other Files suite does,
// so the real LiveCollection retain/seed path and the real generated builders
// run against the harness's executeNamed fake.
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { ARTIFACT_CONCEPT } from "../../src/apps/files/concepts";
import { chooseOption, openSelect } from "../selectControl";
import { artifactRow, click, emit, fakeConnection, renderFiles } from "./harness";

// Artifact labels in Files (epic memql#5009): the FACET behind Refine, and
// the editor in the inspector.

beforeEach(() => {
  h.connection = null;
});

const BRIEF = artifactRow({ id: "a-1", title: "brief.pdf", labels: ["client-acme", "urgent"] });
const VIDEO = artifactRow({ id: "a-2", title: "demo.mp4", labels: ["urgent"] });
const PLAIN = artifactRow({ id: "a-3", title: "notes.txt" });

async function openRefine() {
  await click(screen.getByRole("button", { name: "Refine files" }));
}

async function openInspector(title: string): Promise<HTMLElement> {
  await click(screen.getByRole("button", { name: new RegExp(title) }));
  return screen.getByRole("complementary", { name: "File details" });
}

describe("the label facet", () => {
  it("shows NO standing label control while Refine is collapsed", async () => {
    h.connection = fakeConnection({ artifacts: [BRIEF, VIDEO, PLAIN] });
    await renderFiles();
    await screen.findByRole("button", { name: /brief\.pdf/ });

    // THE RULE-2 REGRESSION, pinned. A label rail beside the folder tree, or
    // a select on the Head, is exactly the filter chrome rule 2 removes --
    // and the rail is the app's PLACE language, which a label is not.
    expect(screen.queryByLabelText("Label")).toBeNull();
    expect(screen.queryByRole("combobox", { name: "Label" })).toBeNull();

    // ...and no label rail beside the folder tree. The rail is REACHED first,
    // so a rename of its own label cannot turn this into an assertion about a
    // node that is not there (the positive control for the negative below).
    const rail = screen.getByRole("navigation", { name: "Places and folders" });
    expect(within(rail).getByText("Library")).toBeTruthy();
    expect(within(rail).queryByText("client-acme")).toBeNull();
    expect(within(rail).queryByText("urgent")).toBeNull();

    // The one affordance IS there -- so the absence above is about the label
    // control, not about a Head that failed to render.
    expect(screen.getByRole("button", { name: "Refine files" })).toBeTruthy();
  });

  it("offers the labels the browse already holds, once Refine is asked", async () => {
    h.connection = fakeConnection({ artifacts: [BRIEF, VIDEO, PLAIN] });
    await renderFiles();
    await screen.findByRole("button", { name: /brief\.pdf/ });

    await openRefine();
    // The kit's Select is a trigger plus a portalled listbox, so the options
    // do not exist until it is opened (test/selectControl.ts).
    const list = openSelect(screen.getByLabelText("Label"));
    // Folded from the seeded population, de-duplicated and alphabetical --
    // a client-side fold like every other facet here.
    expect(
      within(list)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual(["Any label", "client-acme", "urgent"]);
  });

  it("is ABSENT when nothing carries a label -- a question with no answers", async () => {
    h.connection = fakeConnection({ artifacts: [PLAIN] });
    await renderFiles();
    await screen.findByRole("button", { name: /notes\.txt/ });

    await openRefine();
    expect(screen.getByLabelText("Source")).toBeTruthy();
    expect(screen.queryByLabelText("Label")).toBeNull();
  });

  it("narrows the list and shows a REMOVABLE chip beside Refine", async () => {
    h.connection = fakeConnection({ artifacts: [BRIEF, VIDEO, PLAIN] });
    await renderFiles();
    await screen.findByRole("button", { name: /brief\.pdf/ });

    await openRefine();
    // Driven the way a person drives it: the facet is the kit's own listbox,
    // so a `change` event fired at the trigger would reach nothing.
    chooseOption(screen.getByLabelText("Label"), "client-acme");
    await act(async () => {});

    await waitFor(() => expect(screen.queryByRole("button", { name: /demo\.mp4/ })).toBeNull());
    expect(screen.getByRole("button", { name: /brief\.pdf/ })).toBeTruthy();
    expect(screen.queryByText(/notes\.txt/)).toBeNull();

    // The chip is the active constraint, removable in place (rule 2). It
    // stands beside the Refine control, and it is still there once the panel
    // is shut -- the state of the question never hides.
    const refine = document.querySelector(".os-refine") as HTMLElement;
    const remove = within(refine).getByRole("button", { name: "Remove client-acme" });
    await click(remove);
    expect(await screen.findByRole("button", { name: /demo\.mp4/ })).toBeTruthy();
  });

  it("says the filter is why the list is empty, and offers the way out", async () => {
    h.connection = fakeConnection({ artifacts: [BRIEF, VIDEO] });
    await renderFiles();
    await screen.findByRole("button", { name: /brief\.pdf/ });

    await openRefine();
    // Driven the way a person drives it: the facet is the kit's own listbox,
    // so a `change` event fired at the trigger would reach nothing.
    chooseOption(screen.getByLabelText("Label"), "client-acme");
    await act(async () => {});
    // Narrow to nothing: kind=Generated over a label only files carry.
    await click(screen.getByRole("radio", { name: "Generated" }));

    expect(await screen.findByText(/Nothing matches/)).toBeTruthy();
    // An empty list that does not explain itself is the rule-4 failure in a
    // different costume -- so the sentence comes with the act that undoes it.
    const clear = screen.getByRole("button", { name: "Clear the search and filters" });
    await click(clear);
    expect(await screen.findByRole("button", { name: /brief\.pdf/ })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Clear the search and filters" })).toBeNull();
  });
});

describe("the label editor in the inspector", () => {
  it("adds a label optimistically and writes it with the artifact's own id", async () => {
    const connection = fakeConnection({ artifacts: [BRIEF] });
    h.connection = connection;
    await renderFiles();
    const inspector = await openInspector("brief\\.pdf");

    fireEvent.change(within(inspector).getByLabelText("Add a label"), {
      target: { value: "  reviewed  " },
    });
    await click(within(inspector).getByRole("button", { name: "Add" }));

    // Trimmed, and on screen before the echo: the control is TYPED, so a chip
    // that waited for the round trip would read as a dropped keystroke.
    expect(within(inspector).getByText("reviewed")).toBeTruthy();
    expect(connection.callsNamed("libraryAddArtifactLabel")).toEqual([
      'builtin libraryAddArtifactLabel(artifactId: "a-1", label: "reviewed")',
    ]);
  });

  it("refuses a BLANK label rather than sending one", async () => {
    const connection = fakeConnection({ artifacts: [BRIEF] });
    h.connection = connection;
    await renderFiles();
    const inspector = await openInspector("brief\\.pdf");

    const input = within(inspector).getByLabelText("Add a label") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "   " } });
    // The Add control is not even reachable with whitespace in the field.
    expect((within(inspector).getByRole("button", { name: "Add" }) as HTMLButtonElement).disabled).toBe(
      true,
    );
    fireEvent.keyDown(input, { key: "Enter" });
    expect(connection.callsNamed("libraryAddArtifactLabel")).toEqual([]);
  });

  it("ROLLS BACK a refused add and shows the server's own sentence", async () => {
    const connection = fakeConnection({
      artifacts: [BRIEF],
      refuse: { libraryAddArtifactLabel: "label_refused: this artifact is archived" },
    });
    h.connection = connection;
    await renderFiles();
    const inspector = await openInspector("brief\\.pdf");

    fireEvent.change(within(inspector).getByLabelText("Add a label"), {
      target: { value: "reviewed" },
    });
    await click(within(inspector).getByRole("button", { name: "Add" }));

    // A label that appears and is silently not there at the next reload is
    // worse than one that refuses. The chip goes back, verbatim reason shown.
    await waitFor(() => expect(within(inspector).queryByText("reviewed")).toBeNull());
    expect(within(inspector).getByText("label_refused: this artifact is archived")).toBeTruthy();
    expect(within(inspector).getByText("The label was not changed.")).toBeTruthy();
    // ...and the labels it DID have are untouched.
    expect(within(inspector).getByText("client-acme")).toBeTruthy();
    expect(within(inspector).getByText("urgent")).toBeTruthy();
  });

  it("ROLLS BACK a refused remove", async () => {
    const connection = fakeConnection({
      artifacts: [BRIEF],
      refuse: { libraryRemoveArtifactLabel: "label_refused: not yours to change" },
    });
    h.connection = connection;
    await renderFiles();
    const inspector = await openInspector("brief\\.pdf");

    await click(within(inspector).getByRole("button", { name: "Remove label urgent" }));

    await waitFor(() =>
      expect(within(inspector).getByText("label_refused: not yours to change")).toBeTruthy(),
    );
    expect(within(inspector).getByText("urgent")).toBeTruthy();
    expect(connection.callsNamed("libraryRemoveArtifactLabel")).toEqual([
      'builtin libraryRemoveArtifactLabel(artifactId: "a-1", label: "urgent")',
    ]);
  });

  it("removes a label optimistically when the write is taken", async () => {
    const connection = fakeConnection({ artifacts: [BRIEF] });
    h.connection = connection;
    await renderFiles();
    const inspector = await openInspector("brief\\.pdf");

    await click(within(inspector).getByRole("button", { name: "Remove label urgent" }));
    await waitFor(() => expect(within(inspector).queryByText("urgent")).toBeNull());
    expect(within(inspector).getByText("client-acme")).toBeTruthy();
  });

  it("hands authority back to the row when the broadcast echo lands", async () => {
    const connection = fakeConnection({ artifacts: [BRIEF] });
    h.connection = connection;
    await renderFiles();
    const inspector = await openInspector("brief\\.pdf");

    fireEvent.change(within(inspector).getByLabelText("Add a label"), {
      target: { value: "reviewed" },
    });
    await click(within(inspector).getByRole("button", { name: "Add" }));

    // v1:library:artifact broadcasts `updated`, so the real answer arrives on
    // the feed. Once the row carries the change the overlay has nothing left
    // to say -- and dropping it is what lets an edit made elsewhere reach
    // this panel.
    await emit(
      connection,
      ARTIFACT_CONCEPT,
      artifactRow({ id: "a-1", title: "brief.pdf", labels: ["client-acme", "urgent", "reviewed"] }),
    );
    expect(within(inspector).getByText("reviewed")).toBeTruthy();

    // A label removed in ANOTHER tab arrives the same way and wins.
    await emit(
      connection,
      ARTIFACT_CONCEPT,
      artifactRow({ id: "a-1", title: "brief.pdf", labels: ["client-acme"] }),
    );
    await waitFor(() => expect(within(inspector).queryByText("reviewed")).toBeNull());
    expect(within(inspector).queryByText("urgent")).toBeNull();
  });

  it("says the free-text rule ONCE, in the editor and not on every chip", async () => {
    h.connection = fakeConnection({ artifacts: [BRIEF] });
    await renderFiles();
    const inspector = await openInspector("brief\\.pdf");

    expect(within(inspector).getAllByText(/Labels are free text/)).toHaveLength(1);
  });

  it("says None yet rather than hiding the control on a file with no labels", async () => {
    h.connection = fakeConnection({ artifacts: [PLAIN] });
    await renderFiles();
    const inspector = await openInspector("notes\\.txt");

    // The editor is how a label gets ONTO a file, so hiding it when there are
    // none is hiding the only way in.
    expect(within(inspector).getByText("None yet.")).toBeTruthy();
    expect(within(inspector).getByLabelText("Add a label")).toBeTruthy();
  });
});
