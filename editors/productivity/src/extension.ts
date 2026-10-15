import { configureProblems, ProblemReporter, UserInputError, userOperation } from "./problems.js";
import { setProblemOutput, showProblem } from "./problemUI.js";
import * as vscode from "vscode";
import type { EditorConnectionAPI } from "../../vscode/src/connection/api.js";
import { Documents, isZip, NEW_EMAIL_IS_A_PAGE, resourceFrom, type OpenDocument } from "./documents.js";
import { EmailEditor } from "./emailEditor.js";
import { NewEmail } from "./newEmail.js";
import { DocumentDictation } from "./dictation.js";
import { newMarkdown } from "./newMarkdown.js";
import { MarkdownEditor } from "./markdownEditor.js";
import { PDFEditor, PDFDocument } from "./pdfEditor.js";
import { syncDesktopAppearance } from "./themeSync.js";
import { imageMime, rasterImageName } from "./markdownAssets.js";
import { insertImage } from "./insertImage.js";

export class MemQLFiles implements vscode.FileSystemProvider {
  readonly changed = new vscode.EventEmitter<vscode.FileChangeEvent[]>();
  readonly onDidChangeFile = this.changed.event;
  private readonly documents = new Map<string, OpenDocument>();
  private readonly pending = new Map<string, Promise<OpenDocument>>();
  constructor(private readonly files: Documents) {}
  async latest(uri: vscode.Uri): Promise<OpenDocument> { return userOperation("open the file", () => this.files.read(uri.toString())); }
  useBase(uri: vscode.Uri, latest: OpenDocument): void { this.documents.set(uri.toString(), latest); }
  close(uri: vscode.Uri): void { this.documents.delete(uri.toString()); this.pending.delete(uri.toString()); }
  watch(): vscode.Disposable { return new vscode.Disposable(() => {}); }
  async load(uri: vscode.Uri): Promise<OpenDocument> {
    const key = uri.toString();
    const existing = this.documents.get(key);
    if (existing) return existing;
    let pending = this.pending.get(key);
    if (!pending) {
      pending = userOperation("open the file", () => this.files.read(key)).then(async doc => {
        if (rasterImageName(doc.resource.name)) imageMime(doc.resource.name, doc.content);
        if (isZip(doc.resource.name, doc.mime, doc.content)) {
          const destination = await vscode.window.showSaveDialog({ saveLabel: "Download ZIP", defaultUri: vscode.Uri.file(doc.resource.name) });
          if (destination && destination.scheme !== "memql-file") await userOperation("download the ZIP", async () => { await vscode.workspace.fs.writeFile(destination, doc.content); });
          throw vscode.FileSystemError.Unavailable("ZIP files are downloaded intact and cannot be opened as editor workspaces.");
        }
        // Webview images have no TextDocument-close event. Do not retain their
        // bytes indefinitely in the text-editing base cache.
        if (this.pending.get(key) === pending && !rasterImageName(doc.resource.name)) this.documents.set(key, doc);
        return doc;
      }).finally(() => { if (this.pending.get(key) === pending) this.pending.delete(key); });
      this.pending.set(key, pending);
    }
    return pending;
  }
  /**
   * A New email link names a page, not a file, so it answers from the link alone: no network call, no bytes, no writes.
   * Anything that is not a well-formed New email link takes the unchanged path, which refuses a bad link as it always did.
   */
  private isNewEmail(uri: vscode.Uri): boolean {
    try { return resourceFrom(uri.toString()).kind === "new-email"; } catch { return false; }
  }
  async stat(uri: vscode.Uri): Promise<vscode.FileStat> {
    if (this.isNewEmail(uri)) return { type: vscode.FileType.File, ctime: 0, mtime: 0, size: 0, permissions: vscode.FilePermission.Readonly };
    const doc = await this.load(uri);
    return { type: vscode.FileType.File, ctime: 0, mtime: Date.parse(doc.revision || "") || doc.version, size: doc.content.length,
      permissions: ["file", "generated_output", "campaign_template"].includes(doc.kind) ? undefined : vscode.FilePermission.Readonly };
  }
  async readFile(uri: vscode.Uri): Promise<Uint8Array> {
    if (this.isNewEmail(uri)) return new Uint8Array();
    return new Uint8Array((await this.load(uri)).content);
  }
  async writeFile(uri: vscode.Uri, content: Uint8Array): Promise<void> {
    if (this.isNewEmail(uri)) throw vscode.FileSystemError.NoPermissions(NEW_EMAIL_IS_A_PAGE);
    const doc = this.documents.get(uri.toString());
    if (!doc) throw vscode.FileSystemError.NoPermissions("Open and read the current file before saving a new version.");
    await userOperation("save the file", () => this.files.save(doc, content));
    this.changed.fire([{ type: vscode.FileChangeType.Changed, uri }]);
  }
  readDirectory(): [string, vscode.FileType][] { throw vscode.FileSystemError.NoPermissions("Open an individual file from MemQL Files."); }
  createDirectory(): void { throw vscode.FileSystemError.NoPermissions(); }
  delete(): void { throw vscode.FileSystemError.NoPermissions("Manage files in MemQL Files."); }
  rename(): void { throw vscode.FileSystemError.NoPermissions("Rename files in MemQL Files."); }
}

export async function activate(context: vscode.ExtensionContext) {
  const core = vscode.extensions.getExtension<{ connection: EditorConnectionAPI }>("znasllc.memql");
  if (!core) throw new UserInputError("Install the MemQL extension to use Productivity Tools.");
  const { connection } = await core.activate();
  if (connection?.version !== 1) throw new UserInputError("Update the MemQL extension to use Productivity Tools.");
  const output = vscode.window.createOutputChannel("MemQL Productivity Tools");
  context.subscriptions.push(output);
  setProblemOutput(output);
  const problems = new ProblemReporter(connection, line => output.appendLine(line));
  configureProblems(problems);
  context.subscriptions.push(problems);
  const files = new Documents(connection);
  const provider = new MemQLFiles(files);
  const dictation = new DocumentDictation(context, files);
  const markdown = new MarkdownEditor(context, files, uri => provider.load(uri), async document => {
    if (document.isDirty) return false;
    const version = document.version;
    const latest = await provider.latest(document.uri);
    if (document.isDirty || document.version !== version) return false;
    provider.useBase(document.uri, latest);
    provider.changed.fire([{type:vscode.FileChangeType.Changed,uri:document.uri}]);
    return true;
  }, dictation);
  const pdf = new PDFEditor(context, {
    base: async uri => { const doc = await provider.load(uri); return { version: doc.version, revision: doc.revision, sourceId: doc.sourceId }; },
    recover: async (uri, base) => {
      const latest = await provider.latest(uri);
      if (latest.version !== base.version || latest.revision !== base.revision || latest.sourceId !== base.sourceId) throw new UserInputError("This PDF changed while the editor was closed. Save As a local copy to preserve your recovered edits, then compare with the latest MemQL PDF.");
      provider.useBase(uri, latest);
    },
    refresh: async uri => { provider.useBase(uri, await provider.latest(uri)); },
    release: uri => { if (!vscode.workspace.textDocuments.some(doc => doc.uri.toString() === uri.toString())) provider.close(uri); },
  });
  const emails = new EmailEditor(context, connection, files, uri => provider.load(uri));
  const newEmail = new NewEmail(context, connection, dictation);
  const comparisons = new Map<string, { snapshot: vscode.Uri; document: OpenDocument }>();
  const snapshots = new Map<string, string>();
  let snapshotNumber = 0;
  context.subscriptions.push(provider.changed,
    vscode.commands.registerCommand("memql.productivity.checkTools", () => ({
      coreActive: core.isActive, productivityActive: context.extension.isActive, connectionVersion: connection.version,
    })),
    vscode.workspace.onDidCloseTextDocument(document => {
      const key = document.uri.toString();
      if (document.uri.scheme === "memql-file" && !pdf.hasOpen(document.uri)) provider.close(document.uri);
      const comparison = comparisons.get(key);
      if (comparison) { snapshots.delete(comparison.snapshot.toString()); comparisons.delete(key); }
      if (document.uri.scheme === "memql-review") snapshots.delete(key);
    }),
    vscode.window.registerCustomEditorProvider("memql.productivity.email", { resolveCustomTextEditor: (document, panel) => userOperation("open the email", () => emails.resolveCustomTextEditor(document, panel)) }, { supportsMultipleEditorsPerDocument: true }),
    ...(["design", "source", "split"] as const).map(mode => vscode.commands.registerCommand(`memql.productivity.email.${mode}`, async (uri?: vscode.Uri) => {
      try { await emails.show(mode, uri); } catch (error) { void showProblem(error, "open the email view"); }
    })),
    vscode.window.registerCustomEditorProvider("memql.productivity.newEmail", newEmail),
    vscode.commands.registerCommand("memql.productivity.newEmail", async () => {
      try { await newEmail.open(); } catch (error) { void showProblem(error, "open New email"); }
    }),
    vscode.window.registerCustomEditorProvider("memql.productivity.markdown", { resolveCustomTextEditor: (document, panel) => userOperation("open the document", () => markdown.resolveCustomTextEditor(document, panel)) }, { supportsMultipleEditorsPerDocument: true, webviewOptions: { enableFindWidget: false } }),
    vscode.commands.registerCommand("memql.productivity.newMarkdown", async () => {
      try { await newMarkdown(connection, uri => markdown.show("review", uri)); } catch (error) { void showProblem(error, "create the Markdown document"); }
    }),
    vscode.commands.registerCommand("memql.productivity.markdown.insertImage", insertImage(files, uri => provider.load(uri))),
    ...(["source", "reading", "review", "split"] as const).map(mode => vscode.commands.registerCommand(`memql.productivity.markdown.${mode}`, async (uri?: vscode.Uri) => {
      try { await markdown.show(mode, uri); } catch (error) { void showProblem(error, "open the Markdown view"); }
    })),
    vscode.commands.registerCommand("memql.productivity.markdown.note", async () => {
      try { await markdown.feedbackSelection("selectionNote"); } catch (error) { void showProblem(error, "add a note"); }
    }),
    vscode.commands.registerCommand("memql.productivity.markdown.feedback", async () => {
      try { await markdown.feedbackSelection(); } catch (error) { void showProblem(error, "add feedback"); }
    }),
    vscode.commands.registerCommand("memql.productivity.markdown.copy", async () => {
      try { await vscode.commands.executeCommand("editor.action.clipboardCopyAction"); } catch (error) { void showProblem(error, "copy the selection"); }
    }),
    vscode.workspace.registerTextDocumentContentProvider("memql-review", { provideTextDocumentContent: uri => snapshots.get(uri.toString()) ?? "" }),
    vscode.commands.registerCommand("memql.productivity.compareLatest", async () => {
      try {
        const current = vscode.window.activeTextEditor?.document;
        if (!current || current.uri.scheme !== "memql-file") throw new UserInputError("Open a MemQL text document to compare its latest revision.");
        const latest = await provider.latest(current.uri);
        const content = new TextDecoder("utf-8", { fatal: true }).decode(latest.content);
        const snapshot = vscode.Uri.parse(`memql-review:/latest-${++snapshotNumber}/${encodeURIComponent(latest.resource.name)}`);
        snapshots.set(snapshot.toString(), content);
        comparisons.set(current.uri.toString(), { snapshot, document: latest });
        await vscode.commands.executeCommand("vscode.diff", snapshot, current.uri, `Latest v${latest.version} ↔ Your edits`);
      } catch (error) { void showProblem(error, "compare document versions"); }
    }),
    vscode.commands.registerCommand("memql.productivity.useComparedRevision", async () => {
      try {
        const current = vscode.window.activeTextEditor?.document;
        const compared = current && comparisons.get(current.uri.toString());
        if (!current || !compared) throw new UserInputError("Run Compare with Latest Revision, then select your edited document on the right.");
        const answer = await vscode.window.showWarningMessage(`Keep your current edits and use the compared revision as the base for your next save to ${compared.document.resource.domain}?`, { modal: true }, "Use compared revision");
        if (answer !== "Use compared revision") return;
        provider.useBase(current.uri, compared.document);
        comparisons.delete(current.uri.toString());
        void vscode.window.showInformationMessage("Your edits are unchanged. Save when your review is complete.");
      } catch (error) { void showProblem(error, "use the compared revision"); }
    }),
    vscode.workspace.registerFileSystemProvider("memql-file", provider, { isCaseSensitive: true }),
    vscode.window.registerCustomEditorProvider("memql.productivity.pdf", {
      onDidChangeCustomDocument: pdf.onDidChangeCustomDocument,
      openCustomDocument: (uri, backup) => userOperation("open the PDF", () => pdf.openCustomDocument(uri, backup)),
      resolveCustomEditor: (document, panel) => userOperation("show the PDF", () => pdf.resolveCustomEditor(document, panel)),
      saveCustomDocument: (document) => userOperation("save the PDF", () => pdf.saveCustomDocument(document)),
      saveCustomDocumentAs: (document, destination) => userOperation("save a PDF copy", () => pdf.saveCustomDocumentAs(document, destination)),
      revertCustomDocument: (document) => userOperation("reload the PDF", () => pdf.revertCustomDocument(document)),
      backupCustomDocument: (document, backup) => userOperation("back up the PDF", () => pdf.backupCustomDocument(document, backup)),
    } satisfies vscode.CustomEditorProvider<PDFDocument>, { supportsMultipleEditorsPerDocument: true }),
    vscode.commands.registerCommand("memql.productivity.openFile", async () => {
      const uri = await vscode.window.showInputBox({ title: "MemQL file link", placeHolder: "memql-file://cluster.example/artifacts/id/document.md" });
      if (uri) { try { await open(uri); } catch (error) { await showProblem(error, "open the file"); } }
    }),
    vscode.window.registerUriHandler({ handleUri: async uri => {
      try {
        const params = new URLSearchParams(uri.query);
        const resource = params.get("resource");
        if (uri.path !== "/open" || !resource) throw new UserInputError("Invalid MemQL file link.");
        resourceFrom(resource);
        try { await syncDesktopAppearance(params.get("appearance")); }
        catch (error) { void showProblem(error, "match the editor theme"); }
        await open(resource);
      } catch (error) { void showProblem(error, "open the file"); }
    } }),
  );
  async function open(resource: string) {
    resourceFrom(resource);
    await vscode.commands.executeCommand("vscode.open", vscode.Uri.parse(resource));
  }
  return { connectionVersion: connection.version, ...(context.extensionMode === vscode.ExtensionMode.Test ? { newEmailReady: (uri: vscode.Uri) => newEmail.whenShown(uri), emailReady: (uri: vscode.Uri, version?: number) => emails.whenShown(uri, version), pdfReady: (uri: vscode.Uri) => pdf.whenRendered(uri), markdownReady: (uri: vscode.Uri, version?: number) => markdown.whenRendered(uri, version) } : {}) };
}
