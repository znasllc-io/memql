import { afterEach, expect, it, vi } from "vitest";
import { openCheckedArtifact } from "../../src/items/openFile";
import { editorFilename, saveEditorPreference } from "../../src/items/editorPreference";

const file = { name: "review.md", mimeType: "text/markdown", fileId: "file", sizeBytes: 100 };
function setup(metadata = file) {
  const tab = { navigate: vi.fn(), close: vi.fn() };
  const ports = { reserve: vi.fn(() => tab), navigate: vi.fn(), schedule: vi.fn(() => vi.fn()) };
  return { tab, input: { domain: "memql.localhost", artifactId: "artifact", title: "review.md", ports,
    load: vi.fn(async () => metadata), download: vi.fn(async () => {}), onNoAnswer: vi.fn() } };
}
afterEach(() => localStorage.clear());
it("reserves the browser tab before reading current metadata and opens that file", async () => {
  const { tab, input } = setup();
  input.load.mockImplementation(async () => { expect(input.ports.reserve).toHaveBeenCalledOnce(); return file; });
  await openCheckedArtifact(input);
  const url = new URL(tab.navigate.mock.calls[0]![0]);
  expect(JSON.parse(url.searchParams.get("payload")!)).toEqual([["openFile", "memql-file://memql.localhost/artifacts/artifact/review.md"]]);
  expect(input.ports.schedule).not.toHaveBeenCalled();
});
it("downloads a renamed ZIP using fresh MIME metadata and closes its reserved tab", async () => {
  const metadata = { ...file, mimeType: "application/zip" };
  const { tab, input } = setup(metadata);
  await openCheckedArtifact(input);
  expect(input.download).toHaveBeenCalledWith(metadata);
  expect(tab.close).toHaveBeenCalledOnce();
  expect(tab.navigate).not.toHaveBeenCalled();
  expect(input.ports.navigate).not.toHaveBeenCalled();
});
it("does not reserve a tab for a ZIP filename", async () => {
  const { input } = setup({ ...file, name: "archive.zip" });
  input.title = "archive.zip";
  await openCheckedArtifact(input);
  expect(input.ports.reserve).not.toHaveBeenCalled();
  expect(input.download).toHaveBeenCalledOnce();
});
it("refuses to guess when current metadata is unavailable", async () => {
  const { tab, input } = setup();
  input.load.mockRejectedValue(new Error("No access"));
  await expect(openCheckedArtifact(input)).rejects.toThrow("No access");
  expect(tab.close).toHaveBeenCalledOnce();
  expect(tab.navigate).not.toHaveBeenCalled();
  expect(input.download).not.toHaveBeenCalled();
});
it("honors the installed Cursor preference without opening a browser tab", async () => {
  saveEditorPreference("cursor");
  const { input } = setup();
  await openCheckedArtifact(input);
  expect(input.ports.reserve).not.toHaveBeenCalled();
  expect(input.ports.navigate.mock.calls[0]![0]).toMatch(/^cursor:\/\/znasllc.memql-productivity-tools\/open/);
  expect(input.ports.schedule).toHaveBeenCalledOnce();
});

it("gives generated Markdown titles an editor suffix without renaming uploaded files", () => {
  expect(editorFilename({title: "Client brief",kind: "generated_output",format: "markdown"})).toBe("Client brief.md");
  expect(editorFilename({title: "README",kind: "file",format: "markdown"})).toBe("README");
  expect(editorFilename({title: "Client.pdf",format: "pdf"})).toBe("Client.pdf");
});

it("does not open or download an unresolved artifact reference", async () => {
  const { tab, input } = setup();
  await expect(openCheckedArtifact({ ...input, artifactId: undefined })).rejects.toThrow("no editor reference");
  expect(tab.close).toHaveBeenCalledOnce();
  expect(tab.navigate).not.toHaveBeenCalled();
  expect(input.download).not.toHaveBeenCalled();
});
