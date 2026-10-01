import * as vscode from "vscode";
import type { EditorConnectionAPI } from "../../vscode/src/connection/api.js";
import { Documents, isZip, resourceFrom, type OpenDocument } from "./documents.js";
import { newTemplate } from "./templates.js";
import { TemplateEditor } from "./templateEditor.js";
import { TemplateExamples } from "./templateExamples.js";
import { MarkdownEditor } from "./markdownEditor.js";
import { PDFEditor } from "./pdfEditor.js";

class MemQLFiles implements vscode.FileSystemProvider {
  readonly changed = new vscode.EventEmitter<vscode.FileChangeEvent[]>();
  readonly onDidChangeFile = this.changed.event;
  private readonly documents = new Map<string, OpenDocument>();
  private readonly pending = new Map<string, Promise<OpenDocument>>();
  constructor(private readonly files: Documents) {}
  async latest(uri: vscode.Uri): Promise<OpenDocument> { return this.files.read(uri.toString()); }
  useBase(uri: vscode.Uri, latest: OpenDocument): void { this.documents.set(uri.toString(), latest); }
  close(uri: vscode.Uri): void { this.documents.delete(uri.toString()); this.pending.delete(uri.toString()); }
  watch(): vscode.Disposable { return new vscode.Disposable(() => {}); }
  async load(uri: vscode.Uri): Promise<OpenDocument> {
    const key = uri.toString();
    const existing = this.documents.get(key);
    if (existing) return existing;
    let pending = this.pending.get(key);
    if (!pending) {
      pending = this.files.read(key).then(async doc => {
        if (isZip(doc.resource.name, doc.mime, doc.content)) {
          const destination = await vscode.window.showSaveDialog({ saveLabel: "Download ZIP", defaultUri: vscode.Uri.file(doc.resource.name) });
          if (destination && destination.scheme !== "memql-file") await vscode.workspace.fs.writeFile(destination, doc.content);
          throw vscode.FileSystemError.Unavailable("ZIP files are downloaded intact and cannot be opened as editor workspaces.");
        }
        if (this.pending.get(key) === pending) this.documents.set(key, doc);
        return doc;
      }).finally(() => { if (this.pending.get(key) === pending) this.pending.delete(key); });
      this.pending.set(key, pending);
    }
    return pending;
  }
  async stat(uri: vscode.Uri): Promise<vscode.FileStat> {
    const doc = await this.load(uri);
    return { type: vscode.FileType.File, ctime: 0, mtime: doc.version, size: doc.content.length,
      permissions: ["file", "generated_output"].includes(doc.kind) ? undefined : vscode.FilePermission.Readonly };
  }
  async readFile(uri: vscode.Uri): Promise<Uint8Array> { return new Uint8Array((await this.load(uri)).content); }
  async writeFile(uri: vscode.Uri, content: Uint8Array): Promise<void> {
    const doc = this.documents.get(uri.toString());
    if (!doc) throw vscode.FileSystemError.NoPermissions("Open and read the current file before saving a new version.");
    await this.files.save(doc, content);
    this.changed.fire([{ type: vscode.FileChangeType.Changed, uri }]);
  }
  readDirectory(): [string, vscode.FileType][] { throw vscode.FileSystemError.NoPermissions("Open an individual file from MemQL Files."); }
  createDirectory(): void { throw vscode.FileSystemError.NoPermissions(); }
  delete(): void { throw vscode.FileSystemError.NoPermissions("Manage files in MemQL Files."); }
  rename(): void { throw vscode.FileSystemError.NoPermissions("Rename files in MemQL Files."); }
}

export async function activate(context: vscode.ExtensionContext) {
  const core = vscode.extensions.getExtension<{ connection: EditorConnectionAPI }>("znasllc.memql");
  if (!core) throw new Error("Install the MemQL extension to use Productivity Tools.");
  const { connection } = await core.activate();
  if (connection?.version !== 1) throw new Error("Update the MemQL extension to use Productivity Tools.");
  const files = new Documents(connection);
  const provider = new MemQLFiles(files);
  const markdown = new MarkdownEditor(context, files, uri => provider.load(uri));
  const pdf = new PDFEditor(context, {
    base: async uri => { const doc = await provider.load(uri); return { version: doc.version, revision: doc.revision, sourceId: doc.sourceId }; },
    recover: async (uri, base) => {
      const latest = await provider.latest(uri);
      if (latest.version !== base.version || latest.revision !== base.revision || latest.sourceId !== base.sourceId) throw new Error("This PDF changed while the editor was closed. Save As a local copy to preserve your recovered edits, then compare with the latest MemQL PDF.");
      provider.useBase(uri, latest);
    },
    refresh: async uri => { provider.useBase(uri, await provider.latest(uri)); },
    release: uri => { if (!vscode.workspace.textDocuments.some(doc => doc.uri.toString() === uri.toString())) provider.close(uri); },
  });
  const templates = new TemplateEditor(context);
  const examples = new TemplateExamples(connection, context);
  const comparisons = new Map<string, { snapshot: vscode.Uri; document: OpenDocument }>();
  const snapshots = new Map<string, string>();
  let snapshotNumber = 0;
  context.subscriptions.push(provider.changed,
    vscode.workspace.onDidCloseTextDocument(document => {
      const key = document.uri.toString();
      if (document.uri.scheme === "memql-file" && !pdf.hasOpen(document.uri)) provider.close(document.uri);
      const comparison = comparisons.get(key);
      if (comparison) { snapshots.delete(comparison.snapshot.toString()); comparisons.delete(key); }
      if (document.uri.scheme === "memql-review") snapshots.delete(key);
    }),
    vscode.window.registerCustomEditorProvider("memql.productivity.email", templates, { supportsMultipleEditorsPerDocument: true }),
    ...(["source", "preview", "split"] as const).map(mode => vscode.commands.registerCommand(`memql.productivity.template.${mode}`, async (uri?: vscode.Uri) => {
      try { await templates.show(mode, uri); } catch (error) { void vscode.window.showErrorMessage((error as Error).message); }
    })),
    vscode.commands.registerCommand("memql.productivity.templateFromExamples", async () => {
      try { await examples.start(); } catch (error) { void vscode.window.showErrorMessage((error as Error).message); }
    }),
    vscode.commands.registerCommand("memql.productivity.resumeEmailComposition", async () => {
      try { await examples.resume(); } catch (error) { void vscode.window.showErrorMessage((error as Error).message); }
    }),
    vscode.window.registerCustomEditorProvider("memql.productivity.markdown", markdown, { supportsMultipleEditorsPerDocument: true }),
    ...(["source", "reading", "split"] as const).map(mode => vscode.commands.registerCommand(`memql.productivity.markdown.${mode}`, async (uri?: vscode.Uri) => {
      try { await markdown.show(mode, uri); } catch (error) { void vscode.window.showErrorMessage((error as Error).message); }
    })),
    vscode.workspace.registerTextDocumentContentProvider("memql-review", { provideTextDocumentContent: uri => snapshots.get(uri.toString()) ?? "" }),
    vscode.commands.registerCommand("memql.productivity.compareLatest", async () => {
      try {
        const current = vscode.window.activeTextEditor?.document;
        if (!current || current.uri.scheme !== "memql-file") throw new Error("Open a MemQL text document to compare its latest revision.");
        const latest = await provider.latest(current.uri);
        const content = new TextDecoder("utf-8", { fatal: true }).decode(latest.content);
        const snapshot = vscode.Uri.parse(`memql-review:/latest-${++snapshotNumber}/${encodeURIComponent(latest.resource.name)}`);
        snapshots.set(snapshot.toString(), content);
        comparisons.set(current.uri.toString(), { snapshot, document: latest });
        await vscode.commands.executeCommand("vscode.diff", snapshot, current.uri, `Latest v${latest.version} ↔ Your edits`);
      } catch (error) { void vscode.window.showErrorMessage((error as Error).message); }
    }),
    vscode.commands.registerCommand("memql.productivity.useComparedRevision", async () => {
      try {
        const current = vscode.window.activeTextEditor?.document;
        const compared = current && comparisons.get(current.uri.toString());
        if (!current || !compared) throw new Error("Run Compare with Latest Revision, then select your edited document on the right.");
        const answer = await vscode.window.showWarningMessage(`Keep your current edits and use the compared revision as the base for your next save to ${compared.document.resource.domain}?`, { modal: true }, "Use compared revision");
        if (answer !== "Use compared revision") return;
        provider.useBase(current.uri, compared.document);
        comparisons.delete(current.uri.toString());
        void vscode.window.showInformationMessage("Your edits are unchanged. Save when your review is complete.");
      } catch (error) { void vscode.window.showErrorMessage((error as Error).message); }
    }),
    vscode.workspace.registerFileSystemProvider("memql-file", provider, { isCaseSensitive: true }),
    vscode.window.registerCustomEditorProvider("memql.productivity.pdf", pdf, { supportsMultipleEditorsPerDocument: true }),
    vscode.commands.registerCommand("memql.productivity.newTemplate", async () => {
      const doc = await vscode.workspace.openTextDocument({ language: "json", content: newTemplate() });
      await vscode.window.showTextDocument(doc);
    }),
    vscode.commands.registerCommand("memql.productivity.previewTemplate", async () => {
      try { await templates.show("split"); } catch (error) { void vscode.window.showErrorMessage((error as Error).message); }
    }),
    vscode.commands.registerCommand("memql.productivity.openFile", async () => {
      const uri = await vscode.window.showInputBox({ title: "MemQL file link", placeHolder: "memql-file://cluster.example/artifacts/id/document.md" });
      if (uri) await open(uri);
    }),
    vscode.window.registerUriHandler({ handleUri: async uri => {
      try {
        const resource = new URLSearchParams(uri.query).get("resource");
        if (uri.path !== "/open" || !resource) throw new Error("Invalid MemQL file link.");
        await open(resource);
      } catch (error) { void vscode.window.showErrorMessage((error as Error).message); }
    } }),
  );
  async function open(resource: string) {
    resourceFrom(resource);
    await vscode.commands.executeCommand("vscode.open", vscode.Uri.parse(resource));
  }
  return { connectionVersion: connection.version, ...(context.extensionMode === vscode.ExtensionMode.Test ? { templateReady: (uri: vscode.Uri, version?: number) => templates.whenRendered(uri, version), pdfReady: (uri: vscode.Uri) => pdf.whenRendered(uri), markdownReady: (uri: vscode.Uri, version?: number) => markdown.whenRendered(uri, version) } : {}) };
}
