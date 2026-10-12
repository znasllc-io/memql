import { fireEvent, screen, within } from "@testing-library/react";
import { beforeEach, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as unknown, open: vi.fn() }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
vi.mock("../../src/chrome/state", async () => {
  const real = await vi.importActual<typeof import("../../src/chrome/state")>("../../src/chrome/state");
  return { ...real, useOs: () => { const shell = real.useOs(); return { ...shell, actions: { ...shell.actions, openApp: h.open } }; } };
});
import { artifactRow, click, fakeConnection, folderRow, registryWith, renderFiles } from "./harness";
import { readComposeIntent } from "../../src/items/materialize";
import { draftFromSetup, openingStep } from "../../src/apps/materializer/make";

beforeEach(() => {
  h.open.mockClear();
  h.connection = fakeConnection({ folders: [folderRow({ id: "research", name: "Research" })] });
});
async function addMarkdown() {
  await click(screen.getByRole("button", { name: "Add to this folder" }));
  await click(screen.getByRole("menuitem", { name: "New Markdown file" }));
}
it("opens Make a file with Markdown, this folder and the way back", async () => {
  await renderFiles({ intent: { id: "folder", payload: { place: "library", folderId: "research" } } });
  await addMarkdown();
  expect(h.open).toHaveBeenCalledExactlyOnceWith("materializer", "composer", { compose: { format: "markdown", folderId: "research", returnTo: { appId: "files", sectionId: "browse" } } });
  const setup = readComposeIntent(h.open.mock.calls[0]![2]);
  const draft = draftFromSetup(setup, "pdf");
  expect(draft.fileKind).toEqual({ kind: "document", format: "markdown" });
  expect(openingStep(draft, { templates: [], templatesLoaded: true, inference: null, files: () => undefined })).toBe("name");
});
it.each(["library", "bin", "materializer"])("names no folder at the %s root", async (place) => {
  await renderFiles({ intent: { id: "place", payload: { place } } });
  await addMarkdown();
  expect(h.open.mock.calls[0]![2]).toEqual({ compose: { format: "markdown", returnTo: { appId: "files", sectionId: "browse" } } });
});
it("names no folder while searching from a folder", async () => {
  await renderFiles({ intent: { id: "folder", payload: { place: "library", folderId: "research" } } });
  await click(screen.getByRole("button", { name: "Refine files" }));
  const search = screen.getByRole("textbox", { name: "Search" });
  fireEvent.change(search, { target: { value: "notes" } });
  await addMarkdown();
  expect(h.open.mock.calls[0]![2].compose).not.toHaveProperty("folderId");
});
it("is absent when Materializer cannot be opened", async () => {
  await renderFiles({ registry: registryWith(null, "materializer") });
  await click(screen.getByRole("button", { name: "Add to this folder" }));
  expect(screen.queryByRole("menuitem", { name: "New Markdown file" })).toBeNull();
});
it("lists the three ways to add under the folder's name", async () => {
  await renderFiles({ intent: { id: "folder", payload: { place: "library", folderId: "research" } } });
  await click(screen.getByRole("button", { name: "Add to this folder" }));
  const menu = screen.getByRole("menu", { name: "Add to this folder" });
  expect(within(menu).getByText("Into Research")).toBeTruthy();
  expect(within(menu).getAllByRole("menuitem").map((item) => item.textContent?.trim())).toEqual(["Upload files", "New Markdown file", "New folder"]);
});
it("opens an existing composition on its finished-file page", async () => {
  h.connection = fakeConnection({ artifacts: [artifactRow({ id: "artifact", title: "Report.md", sourceConcept: "v1:library:file", sourceConceptRef: "file" })], compositions: [{ id: "composition", name: "Report", outputFileId: "file", status: "ready", format: "markdown", archived: false }] });
  await renderFiles();
  const row = screen.getByRole("button", { name: /Report.md/ });
  fireEvent.contextMenu(row, { clientX: 30, clientY: 30 });
  await click(screen.getByRole("menuitem", { name: "Open in Materializer" }));
  expect(h.open).toHaveBeenCalledWith("materializer", "materialized", { compositionId: "composition" });
  await click(row);
  await click(within(screen.getByRole("region", { name: "File details" })).getByRole("button", { name: "Open in Materializer" }));
  expect(h.open).toHaveBeenLastCalledWith("materializer", "materialized", { compositionId: "composition" });
});
