import { fireEvent, screen, waitFor, within } from "@testing-library/react";
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
import { artifactRow, click, emit, fakeConnection, renderFiles, openDetails } from "./harness";

// Artifact labels in Files (epic memql#5009): the editor in the inspector.
// The label FACET is a group in the search panel's kind menu (Jesús, Nov 20
// 2026), tested in places.test.tsx.

beforeEach(() => {
  h.connection = null;
});

const BRIEF = artifactRow({ id: "a-1", title: "brief.pdf", labels: ["client-acme", "urgent"] });
const VIDEO = artifactRow({ id: "a-2", title: "demo.mp4", labels: ["urgent"] });
const PLAIN = artifactRow({ id: "a-3", title: "notes.txt" });

async function openInspector(title: string): Promise<HTMLElement> {
  await openDetails(title);
  return screen.getByRole("region", { name: "File details" });
}

describe("labels on the browse", () => {
  it("keeps labels in details, without putting chips on file rows", async () => {
    h.connection = fakeConnection({ artifacts: [BRIEF, VIDEO, PLAIN] });
    await renderFiles();
    const item = screen.getByRole("button", { name: /brief\.pdf/ });
    expect(within(item).queryByText("client-acme")).toBeNull();
    expect(within(item).queryByText("urgent")).toBeNull();
    const details = await openInspector("brief\\.pdf");
    expect(within(details).getByRole("button", { name: "Remove label client-acme" })).toBeTruthy();
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

    expect(within(inspector).getAllByText(/Use labels to find related files/)).toHaveLength(1);
  });

  it("keeps the input available without an empty label list", async () => {
    h.connection = fakeConnection({ artifacts: [PLAIN] });
    await renderFiles();
    const inspector = await openInspector("notes\\.txt");

    // The editor is how a label gets ONTO a file, so hiding it when there are
    // none is hiding the only way in.
    expect(within(inspector).queryByText("None yet.")).toBeNull();
    expect(within(inspector).queryByRole("list", { name: "Labels" })).toBeNull();
    expect(within(inspector).getByLabelText("Add a label")).toBeTruthy();
  });
});
