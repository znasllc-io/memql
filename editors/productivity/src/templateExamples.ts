import { UserInputError } from "./problems.js";
import * as vscode from "vscode";
import {
  buildClientAccountsAll, buildLibraryFilesForOwner, buildCreateComposeRecipe,
  followComposition, startMaterialize, MaterializeCancelled, type MaterializeExecute,
} from "@znasllc-io/memql-sdk-core/client";
import type { ConnectionLease, EditorConnectionAPI } from "../../vscode/src/connection/api.js";

interface Reference { kind: "library_file"; ref: string; label: string; content: true; includeImages?: boolean }
const value = (row: Record<string, unknown>, key: string) => typeof row[key] === "string" ? row[key] as string : "";
const allowed = /\.(png|jpg|jpeg|gif|zip|txt|md|markdown|html?|css|json|csv|svg)$/i;
const mimeFor = (name: string) => /\.png$/i.test(name) ? "image/png" : /\.jpe?g$/i.test(name) ? "image/jpeg" : /\.gif$/i.test(name) ? "image/gif" : /\.zip$/i.test(name) ? "application/zip" : "text/plain";

export class TemplateExamples {
  constructor(private readonly api: EditorConnectionAPI, private readonly context: vscode.ExtensionContext) {}
  private starting: Promise<void> | undefined;
  start(): Promise<void> {
    if (!this.starting) this.starting = this.create().finally(() => { this.starting = undefined; });
    return this.starting;
  }
  private async create(): Promise<void> {
    const lease = this.api.current();
    if (!lease) throw new UserInputError("Connect and sign in using the MemQL extension before creating from examples.");
    const accounts = await this.api.execute(lease, "clientAccountsAll", buildClientAccountsAll({}));
    const organization = await vscode.window.showQuickPick(accounts.filter(row => value(row, "status") === "active").map(row => ({ label: value(row, "name") || value(row, "id"), id: value(row, "id") })),
      { title: "Create email · Organization", placeHolder: "Choose the client this template belongs to", ignoreFocusOut: true });
    if (!organization) return;
    const from = await vscode.window.showQuickPick(["Choose files from this device", "Choose files already in MemQL"],
      { title: "Create email · Examples", placeHolder: "A reference PNG, brand files, or a ZIP of resources", ignoreFocusOut: true });
    if (!from) return;
    const sources: Reference[] = [];
    if (from === "Choose files from this device") {
      const selected = await vscode.window.showOpenDialog({ canSelectMany: true, canSelectFiles: true, canSelectFolders: false,
        title: "Choose examples and resources", openLabel: "Use as references", filters: { References: ["png", "jpg", "jpeg", "gif", "zip", "txt", "md", "html", "css", "json", "csv", "svg"] } });
      if (!selected?.length) return;
      if (selected.length > 64) throw new UserInputError("Choose up to 64 reference files.");
      let total = 0;
      // Validate all selected sizes before uploading the first file.
      for (const uri of selected) { total += (await vscode.workspace.fs.stat(uri)).size; }
      if (total > 16 * 1024 * 1024) throw new UserInputError("Reference files must total 16 MiB or less. ZIP contents have the same expanded limit.");
      await vscode.window.withProgress({ location: vscode.ProgressLocation.Notification, title: "Saving references to MemQL Files" }, async progress => {
        for (const uri of selected) {
          const name = uri.path.split("/").pop() || "reference";
          if (!allowed.test(name)) throw new Error(`Unsupported reference: ${name}`);
          progress.report({ message: name });
          const uploaded = await this.api.uploadFile(lease, name, mimeFor(name), await vscode.workspace.fs.readFile(uri));
          sources.push({ kind: "library_file", ref: uploaded.fileId, label: name, content: true });
        }
      });
    } else {
      const rows = await this.api.execute(lease, "libraryFilesForOwner", buildLibraryFilesForOwner({}));
      const selected = await vscode.window.showQuickPick(rows.filter(row => allowed.test(value(row, "name"))).map(row => ({ label: value(row, "name"), id: value(row, "id"), description: value(row, "mimeType") })),
        { title: "Create email · References", placeHolder: "Select reference files from your recent Library files", canPickMany: true, ignoreFocusOut: true });
      if (!selected?.length) return;
      for (const file of selected) sources.push({ kind: "library_file", ref: file.id, label: file.label, content: true });
    }
    const imageSources = sources.filter(source => /\.(png|jpe?g|gif|zip)$/i.test(source.label));
    if (imageSources.length) {
      const assets = await vscode.window.showQuickPick(imageSources.map(source => ({ label: source.label, source, description: /\.zip$/i.test(source.label) ? "Include images from this bundle" : "Include this image" })),
        { title: "Create email · Images to include", placeHolder: "Select logos or photos (1 MiB total). Leave layout examples unselected; Enter keeps them as references only.", canPickMany: true, ignoreFocusOut: true });
      if (!assets) return;
      for (const asset of assets) asset.source.includeImages = true;
    }
    const name = await vscode.window.showInputBox({ title: "Create email · Name", prompt: "What should we call this template?", placeHolder: "Autumn welcome", ignoreFocusOut: true,
      // The name is also the draft's file name, and a name is not a place to file into: a folder path is refused.
      validateInput: text => !text.trim() ? "Give the template a name." : /[\\/]/.test(text) ? "Use a name without slashes." : undefined });
    if (!name) return;
    const brief = await vscode.window.showInputBox({ title: "Create email · Brief", prompt: "What should the email say, and what should it borrow from the examples? Include any approved links.", placeHolder: "Use the layout in example.png, our green brand color, and the welcome copy in brief.md.", ignoreFocusOut: true, validateInput: text => text.trim() ? undefined : "Describe the email you want." });
    if (!brief) return;
    const action = await vscode.window.showQuickPick(["Create draft", "Save as recipe and create draft"], {
      title: `${name} · ${organization.label}`, placeHolder: `Use ${sources.length} reference(s) with Materializer. ZIP resources are inspected; nothing is published or sent.`, ignoreFocusOut: true });
    if (!action) return;
    let recipeId: string | undefined;
    if (action === "Save as recipe and create draft") {
      recipeId = globalThis.crypto.randomUUID();
      await this.api.execute(lease, "createComposeRecipe", buildCreateComposeRecipe({ recipeId, name, description: brief, outputKind: "email_template", format: "json", accountIds: [organization.id],
        sourceSelectors: sources.map(source => ({ kind: "library_file", selector: source.ref, label: source.label, content: true, includeImages: source.includeImages === true })) }));
    }
    // A new request id for each draft asked for: the cluster uses it to recognise a repeat of this one ask.
    const compositionId = await startMaterialize(this.executeAs(lease), { name, statement: brief, outputKind: "email_template", format: "json",
      accountIds: [organization.id], sources, recipeId, requestId: globalThis.crypto.randomUUID() });
    await this.context.workspaceState.update("memql.lastEmailComposition", { domain: lease.domain, compositionId });
    await this.follow(lease, compositionId);
  }
  async resume(): Promise<void> {
    const saved = this.context.workspaceState.get<{ domain: string; compositionId: string }>("memql.lastEmailComposition");
    if (!saved) throw new UserInputError("No email composition has been started in this workspace.");
    await this.follow(await this.api.connect(saved.domain), saved.compositionId);
  }
  private executeAs(lease: ConnectionLease): MaterializeExecute {
    return (name, call) => this.api.execute(lease, name, call);
  }
  private async follow(lease: ConnectionLease, compositionId: string): Promise<void> {
    await vscode.window.withProgress({ location: vscode.ProgressLocation.Notification, title: "Creating email", cancellable: true }, async (progress, cancel) => {
      const stop = new AbortController();
      const subscription = cancel.onCancellationRequested(() => stop.abort());
      if (cancel.isCancellationRequested) stop.abort();
      try {
        const draft = await followComposition({ execute: this.executeAs(lease), signal: stop.signal,
          onStatus: status => progress.report({ message: status === "composing" ? "Studying references and composing the draft" : status }) }, compositionId);
        const name = draft.name || "template";
        const filename = name.endsWith(".email.json") ? name : `${name}.email.json`;
        const uri = vscode.Uri.from({ scheme: "memql-file", authority: lease.domain, path: `/artifacts/${draft.artifactId}/${filename.replace(/[\\/]/g, "_")}` });
        await vscode.commands.executeCommand("vscode.openWith", uri, "memql.productivity.email", { preview: false });
      } catch (error) {
        // Stopping the draft, from here or from elsewhere, is not a failure.
        if (!(error instanceof MaterializeCancelled)) throw error;
      } finally {
        subscription.dispose();
      }
    });
  }
}
