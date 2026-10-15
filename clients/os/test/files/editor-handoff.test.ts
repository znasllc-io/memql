import { afterEach, expect, it, vi } from "vitest";
import { openCheckedArtifact } from "../../src/items/openFile";
import { editorFilename, editorNewEmailURI, editorNewEmailURL, saveEditorPreference } from "../../src/items/editorPreference";

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
  expect(url.origin).toBe("https://vscode.memql.localhost");
  expect(url.pathname).toBe("/editor/");
  expect(url.searchParams.get("resource")).toBe("memql-file://memql.localhost/artifacts/artifact/review.md");
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

// THE NEW EMAIL LINK (memql#6033, Ruling 9): the productivity tool's New email
// page for one organization. A resource kind of its own, so three path segments
// and no query -- the editor's validators refuse a query.
const NEW_EMAIL_OPENS = {
  browser: "https://vscode.memql.localhost/editor/",
  vscode: "vscode://znasllc.memql-productivity-tools/open",
  cursor: "cursor://znasllc.memql-productivity-tools/open",
} as const;

it.each(["browser", "vscode", "cursor"] as const)("builds the New email page resource for %s", preference => {
  const url = new URL(editorNewEmailURL("memql.localhost", "acme", preference));
  expect(`${url.protocol}//${url.host}${url.pathname}`).toBe(NEW_EMAIL_OPENS[preference]);
  const resource = url.searchParams.get("resource")!;
  expect(resource).toBe("memql-file://memql.localhost/new-email/acme/New%20email");
  const link = new URL(resource);
  expect(link.pathname.split("/").filter(Boolean)).toEqual(["new-email", "acme", "New%20email"]);
  expect(link.search).toBe("");
});

it("matches the editor's own spelling", () => {
  // The OS cannot import editors/, so this literal is the contract: the same
  // string editors/productivity/test/documents.test.ts pins for
  // newEmailResource("memql.example.com", "self").
  expect(editorNewEmailURI("memql.example.com", "self")).toBe("memql-file://memql.example.com/new-email/self/New%20email");
});

it("encodes the organization id as one path segment", () => {
  // An id is the server's to spell; whatever it holds stays one segment, so the
  // resource keeps its three.
  expect(editorNewEmailURI("memql.example.com", "a/b")).toBe("memql-file://memql.example.com/new-email/a%2Fb/New%20email");
});

it("spells the domain in lower case, as the editor does", () => {
  expect(editorNewEmailURI("MemQL.Example.COM", "self")).toBe("memql-file://memql.example.com/new-email/self/New%20email");
  // The browser editor's address is read out of the resource, and a custom
  // scheme keeps a host's case, so the case would otherwise leak into it.
  expect(editorNewEmailURL("MemQL.Example.COM", "self", "browser")).toMatch(/^https:\/\/vscode\.memql\.example\.com\/editor\/\?resource=/);
});

it("opens in the editor the person chose when none is given", () => {
  saveEditorPreference("cursor");
  expect(editorNewEmailURL("memql.localhost", "acme")).toMatch(/^cursor:\/\/znasllc\.memql-productivity-tools\/open\?resource=/);
});
