import * as vscode from "vscode";
import {
  buildClientAccountsAll, buildLibraryFilesForOwner, buildComposeMaterialize, buildCompositionById,
  buildComposeCancel, buildLibraryArtifactBySourceConceptRef, buildCreateComposeRecipe,
} from "@znasllc-io/memql-sdk-core/client";
import type { ConnectionLease, EditorConnectionAPI } from "../../vscode/src/connection/api.js";

interface Reference { kind: "library_file"; ref: string; label: string; content: true }
const value = (row: Record<string, unknown>, key: string) => typeof row[key] === "string" ? row[key] as string : "";
const allowed = /\.(png|jpg|jpeg|gif|zip|txt|md|markdown|html?|css|json|csv|svg)$/i;
const mimeFor = (name: string) => /\.png$/i.test(name) ? "image/png" : /\.jpe?g$/i.test(name) ? "image/jpeg" : /\.gif$/i.test(name) ? "image/gif" : /\.zip$/i.test(name) ? "application/zip" : "text/plain";

export class TemplateExamples {
  constructor(private readonly api: EditorConnectionAPI, private readonly context: vscode.ExtensionContext) {}
  async start(): Promise<void> {
    const lease = this.api.current();
    if (!lease) throw new Error("Connect and sign in using the MemQL extension before creating from examples.");
    const accounts = await this.api.execute(lease, "clientAccountsAll", buildClientAccountsAll({}));
    const organization = await vscode.window.showQuickPick(accounts.map(row => ({ label: value(row, "name") || value(row, "id"), id: value(row, "id") })),
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
      if (selected.length > 64) throw new Error("Choose up to 64 reference files.");
      let total = 0;
      // Validate all selected sizes before uploading the first file.
      for (const uri of selected) { total += (await vscode.workspace.fs.stat(uri)).size; }
      if (total > 16 * 1024 * 1024) throw new Error("Reference files must total 16 MiB or less. ZIP contents have the same expanded limit.");
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
    const name = await vscode.window.showInputBox({ title: "Create email · Name", prompt: "What should we call this template?", placeHolder: "Autumn welcome", ignoreFocusOut: true, validateInput: text => text.trim() ? undefined : "Give the template a name." });
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
        sourceSelectors: sources.map(source => ({ kind: "library_file", selector: source.ref, label: source.label, content: true })) }));
    }
    const result = (await this.api.execute(lease, "composeMaterialize", buildComposeMaterialize({ name, statement: brief,
      outputKind: "email_template", format: "json", accountIds: [organization.id], sources: sources.map(source => ({ ...source })), ...(recipeId ? { recipeId } : {}) })))[0];
    const compositionId = result && value(result, "compositionId");
    if (!compositionId) throw new Error("Materializer did not confirm a composition. Check its history before retrying.");
    await this.context.workspaceState.update("memql.lastEmailComposition", { domain: lease.domain, compositionId });
    await this.follow(lease, compositionId);
  }
  async resume(): Promise<void> {
    const saved = this.context.workspaceState.get<{ domain: string; compositionId: string }>("memql.lastEmailComposition");
    if (!saved) throw new Error("No email composition has been started in this workspace.");
    await this.follow(await this.api.connect(saved.domain), saved.compositionId);
  }
  private async follow(lease: ConnectionLease, compositionId: string): Promise<void> {
    await vscode.window.withProgress({ location: vscode.ProgressLocation.Notification, title: "Materializer · Creating email", cancellable: true }, async (progress, cancel) => {
      for (;;) {
        if (cancel.isCancellationRequested) {
          await this.api.execute(lease, "composeCancel", buildComposeCancel({ compositionId, reason: "Stopped from Productivity Tools" }));
          return;
        }
        const row = (await this.api.execute(lease, "compositionById", buildCompositionById({ compositionId })))[0];
        if (!row) throw new Error("The composition is no longer readable. Your work remains in Materializer and Nexus.");
        const status = value(row, "status");
        progress.report({ message: status === "composing" ? "Studying references and composing the draft" : status });
        if (status === "failed") throw new Error(value(row, "failureReason") || "Materializer could not finish this draft.");
        if (status === "cancelled") return;
        if (status === "ready") {
          const artifacts = await this.api.execute(lease, "libraryArtifactBySourceConceptRef", buildLibraryArtifactBySourceConceptRef({ sourceConceptRef: value(row, "outputFileId") }));
          const artifact = artifacts[0];
          if (artifact && value(artifact, "id")) {
            const name = value(artifact, "title") || value(row, "name") || "template";
            const filename = name.endsWith(".email.json") ? name : `${name}.email.json`;
            const uri = vscode.Uri.from({ scheme: "memql-file", authority: lease.domain, path: `/artifacts/${value(artifact, "id")}/${filename.replace(/[\\/]/g, "_")}` });
            await vscode.commands.executeCommand("vscode.openWith", uri, "memql.productivity.email", { preview: false });
            return;
          }
          // The artifact index is populated asynchronously after the output file.
          progress.report({ message: "Draft saved; waiting for its Files entry" });
        }
        await new Promise(resolve => setTimeout(resolve, 2000));
      }
    });
  }
}
