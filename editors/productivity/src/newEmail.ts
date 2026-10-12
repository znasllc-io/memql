// A New email link is a page, not a file. Its persisted request is the recovery
// record: reopening follows the same composition, even if its first reply was lost.
import * as vscode from "vscode";
import {
  buildEmailLayouts, buildEmailLayoutById, buildEmailRender, buildLibraryFilesForOwner,
  followComposition, startMaterialize, MaterializeCancelled, MaterializeFailed,
  type MaterializeRequest,
} from "@znasllc-io/memql-sdk-core/client";
import type { ConnectionLease, EditorConnectionAPI } from "../../vscode/src/connection/api.js";
import { newEmailResource, resourceFrom } from "./documents.js";
import { DocumentDictation } from "./dictation.js";
import { organizations, type Organization } from "./emailPublish.js";
import { newEmailPage, emailNameProblem } from "./newEmailPage.js";
import { EMAIL_COPY } from "./emailCopy.js";
import { reportProblem, UserInputError } from "./problems.js";

const H = EMAIL_COPY.host;
const VIEW = "memql.productivity.newEmail";
const PENDING = "memql.newEmail.pending:", DRAFT = "memql.newEmail.draft:", ORGANIZATION = "memql.newEmail.organization:";
const allowed = /\.(png|jpe?g|gif|zip|txt|md|markdown|html?|css|json|csv|svg)$/i;
const kind = (name: string): Reference["kind"] => /\.(png|jpe?g|gif)$/i.test(name) ? "picture" : /\.zip$/i.test(name) ? "bundle" : "file";
const mime = (name: string) => /\.png$/i.test(name) ? "image/png" : /\.jpe?g$/i.test(name) ? "image/jpeg" : /\.gif$/i.test(name) ? "image/gif" : /\.zip$/i.test(name) ? "application/zip" : "text/plain";
const text = (row: Record<string, unknown>, key: string) => typeof row[key] === "string" ? row[key] as string : "";
interface Reference { fileId: string; label: string; include: boolean; kind: "picture" | "bundle" | "file"; size?: number }
interface Draft { organizationId?: string; layoutId?: string; name?: string; description?: string; references: Reference[] }
interface Pending { request: MaterializeRequest; compositionId?: string }
class PageClosed extends Error {}

export class NewEmail implements vscode.CustomReadonlyEditorProvider<vscode.CustomDocument> {
  private readonly shown = new Map<string, string>();
  // Duplicate tabs for one link share one owner; no simultaneous resends of a request.
  private readonly panels = new Map<string, vscode.WebviewPanel>();
  constructor(private readonly context: vscode.ExtensionContext, private readonly api: EditorConnectionAPI, private readonly dictation: DocumentDictation) {}
  openCustomDocument(uri: vscode.Uri): vscode.CustomDocument {
    if (resourceFrom(uri.toString()).kind !== "new-email") throw new UserInputError(H.openEmail);
    return { uri, dispose() {} };
  }
  async open(): Promise<void> {
    const lease = this.api.current();
    if (!lease) throw new UserInputError(H.connectFirst);
    const choices = await organizations({ execute: (name, call) => this.api.execute(lease, name, call), state: this.context.workspaceState });
    const remembered = this.context.globalState.get<string>(ORGANIZATION + lease.domain);
    const organization = choices.find(choice => choice.id === remembered) ?? choices[0];
    if (!organization) throw new UserInputError(H.addOrganization);
    await vscode.commands.executeCommand("vscode.open", vscode.Uri.parse(newEmailResource(lease.domain, organization.id)));
  }
  async whenShown(uri: vscode.Uri): Promise<{ state: string }> {
    const deadline = Date.now() + 15000;
    while (Date.now() < deadline) {
      const state = this.shown.get(uri.toString());
      if (state) return { state };
      await new Promise(resolve => setTimeout(resolve, 40));
    }
    throw new Error("The New email page did not report its state.");
  }
  async resolveCustomEditor(document: vscode.CustomDocument, panel: vscode.WebviewPanel): Promise<void> {
    const link = document.uri.toString(), resource = resourceFrom(link), domain = resource.domain;
    this.panels.get(link)?.dispose();
    this.panels.set(link, panel);
    this.shown.delete(link);
    let disposed = false, generation = 0, running = false, attaching = false;
    let abort: AbortController | undefined;
    let draft: Draft = { organizationId: resource.id, references: [], ...this.context.workspaceState.get<Draft>(DRAFT + link) };
    let choices: Organization[] = [], layouts: Record<string, unknown>[] = [];
    const thumbnails = new Map<string, string>();
    const subscriptions: vscode.Disposable[] = [];
    const lease = () => { const current = this.api.current(); return current?.domain === domain ? current : undefined; };
    const post = async (message: unknown) => { if (!disposed) await panel.webview.postMessage(message); };
    const keep = () => this.context.workspaceState.update(DRAFT + link, draft);
    const references = (problem?: unknown) => post({ type: "references", items: draft.references, ...(problem ? { problem: reportProblem(problem, H.operations.references, lease()) } : {}) });
    const execute = (connection: ConnectionLease) => async (name: string, call: string) => {
      if (disposed) throw new PageClosed();
      const rows = await this.api.execute(connection, name, call);
      if (disposed) throw new PageClosed();
      return rows;
    };
    const setup = async () => {
      const epoch = ++generation, connection = lease();
      if (!connection) { await post({ type: "setup", connected: false, domain, organizations: [], layouts: [], dictation: false }); return; }
      try {
        const nextChoices = await organizations({ execute: execute(connection), state: this.context.workspaceState });
        if (epoch !== generation || disposed) return;
        choices = nextChoices;
        if (!choices.some(choice => choice.id === draft.organizationId)) draft.organizationId = choices.length === 1 ? choices[0]!.id : undefined;
        const organizationId = draft.organizationId;
        const nextLayouts = organizationId ? await execute(connection)("emailLayouts", buildEmailLayouts({ accountId: organizationId })) : [];
        if (epoch !== generation || disposed) return;
        layouts = nextLayouts;
        // The listing can omit large documents. The separate bare row id is
        // the lookup key; the card's id remains opaque all the way to creation.
        await post({ type: "setup", connected: true, domain, organizations: choices, organizationId, layouts, dictation: await this.dictation.available() });
        let next = 0;
        await Promise.all(Array.from({ length: Math.min(3, layouts.length) }, async () => {
          while (next < layouts.length && epoch === generation && !disposed) {
            const row = layouts[next++]!, id = text(row, "id"), key = JSON.stringify([connection.generation, organizationId, id, row.revision]);
            let html = thumbnails.get(key);
            if (html === undefined) {
              try {
                let doc = row.document;
                if (row.documentOmitted === true) {
                  const found = await execute(connection)("emailLayoutById", buildEmailLayoutById({ layoutId: text(row, "layoutId") }));
                  doc = found[0]?.document;
                }
                if (!doc || typeof doc !== "object" || Array.isArray(doc)) throw new Error("Layout document unavailable.");
                const rendered = await execute(connection)("emailRender", buildEmailRender({ accountId: organizationId!, document: doc as Record<string, unknown> }));
                html = text(rendered[0] ?? {}, "html");
                thumbnails.set(key, html);
              } catch (error) { if (error instanceof PageClosed) return; html = ""; }
            }
            if (epoch === generation) await post({ type: "thumbnail", id, html });
          }
        }));
      } catch (error) {
        if (epoch === generation && !(error instanceof PageClosed)) await post({ type: "layoutsFailed", problem: reportProblem(error, H.operations.layouts, connection) });
      }
    };
    const follow = async (pending: Pending) => {
      if (running || disposed) return;
      const connection = lease();
      if (!connection) { await setup(); return; }
      running = true;
      abort = new AbortController();
      await post({ type: "writing", layoutId: pending.request.layoutId });
      try {
        // Persist before dispatch. If the response is lost, the identical request
        // survives; only a confirmed failed/cancelled composition starts anew.
        await keep();
        await this.context.workspaceState.update(PENDING + link, pending);
        if (!pending.compositionId) {
          pending.compositionId = await startMaterialize(execute(connection), pending.request);
          await this.context.workspaceState.update(PENDING + link, pending);
        }
        const result = await followComposition({ execute: execute(connection), signal: abort.signal }, pending.compositionId);
        if (disposed) return;
        const name = /\.email\.json$/i.test(result.name) ? result.name : result.name + ".email.json";
        const uri = vscode.Uri.from({ scheme: "memql-file", authority: domain, path: `/artifacts/${result.artifactId}/${name.replace(/[\\/]/g, "_")}` });
        resourceFrom(uri.toString());
        await vscode.commands.executeCommand("vscode.openWith", uri, "memql.productivity.email", { viewColumn: panel.viewColumn, preview: false });
        await this.context.workspaceState.update(PENDING + link, undefined);
        await this.context.workspaceState.update(DRAFT + link, undefined);
        await this.context.globalState.update(ORGANIZATION + domain, pending.request.accountIds?.[0]);
        panel.dispose();
      } catch (error) {
        if (disposed || error instanceof PageClosed) return;
        if (error instanceof MaterializeCancelled || error instanceof MaterializeFailed) await this.context.workspaceState.update(PENDING + link, undefined);
        await post(error instanceof MaterializeCancelled ? { type: "idle" } : { type: "failed", problem: reportProblem(error, H.operations.write, connection) });
      } finally { running = false; abort = undefined; }
    };
    const attach = async () => {
      if (attaching || running) return;
      const connection = lease();
      if (!connection) { await setup(); return; }
      attaching = true;
      try {
        const from = await vscode.window.showQuickPick([H.fromComputer, H.fromFiles], { title: H.addReferences });
        if (!from || disposed) return;
        if (from === H.fromComputer) {
          const selected = await vscode.window.showOpenDialog({ canSelectMany: true, canSelectFiles: true, canSelectFolders: false, title: H.chooseReferences, openLabel: H.addLabel,
            filters: { References: ["png", "jpg", "jpeg", "gif", "zip", "txt", "md", "html", "css", "json", "csv", "svg"] } });
          if (!selected?.length) return;
          if (draft.references.length + selected.length > 64) throw new UserInputError(H.tooMany);
          const files = await Promise.all(selected.map(async uri => {
            const label = uri.path.split("/").pop() ?? "";
            if (!allowed.test(label)) throw new UserInputError(H.unsupported);
            return { uri, label, size: (await vscode.workspace.fs.stat(uri)).size };
          }));
          const existing = draft.references.reduce((sum, ref) => sum + (ref.size ?? 0), 0);
          if (files.reduce((sum, file) => sum + file.size, existing) > 16 * 1024 * 1024) throw new UserInputError(H.tooLarge);
          const contents = await Promise.all(files.map(file => vscode.workspace.fs.readFile(file.uri)));
          if (contents.reduce((sum, bytes) => sum + bytes.length, existing) > 16 * 1024 * 1024) throw new UserInputError(H.tooLarge);
          for (let i = 0; i < files.length && !disposed; i++) {
            const file = files[i]!, bytes = contents[i]!;
            const saved = await this.api.uploadFile(connection, file.label, mime(file.label), bytes);
            draft.references.push({ fileId: saved.fileId, label: file.label, include: false, kind: kind(file.label), size: bytes.length });
            await keep(); await references();
          }
        } else {
          const rows = await execute(connection)("libraryFilesForOwner", buildLibraryFilesForOwner({}));
          const selected = await vscode.window.showQuickPick(rows.filter(row => allowed.test(text(row, "name"))).map(row => ({ label: text(row, "name"), fileId: text(row, "id"), size: typeof row.size === "number" ? row.size : 0 })), { placeHolder: H.recentFiles, canPickMany: true });
          if (!selected?.length) return;
          if (draft.references.length + selected.length > 64) throw new UserInputError(H.tooMany);
          if (selected.reduce((sum, file) => sum + file.size, draft.references.reduce((sum, ref) => sum + (ref.size ?? 0), 0)) > 16 * 1024 * 1024) throw new UserInputError(H.tooLarge);
          for (const file of selected) if (!draft.references.some(ref => ref.fileId === file.fileId)) draft.references.push({ ...file, kind: kind(file.label), include: false });
          await keep(); await references();
        }
      } catch (error) { if (!(error instanceof PageClosed)) await references(error); }
      finally { attaching = false; }
    };
    const receive = async (message: any) => {
      if (disposed || !message || typeof message !== "object") return;
      try {
        switch (message.type) {
          case "shown": if (typeof message.state === "string") this.shown.set(link, message.state); break;
          case "ready":
            await post({ type: "restore", draft }); await references();
            await setup();
            { const pending = this.context.workspaceState.get<Pending>(PENDING + link); if (pending) await follow(pending); }
            break;
          case "connect": await this.api.connect(domain); await setup(); break;
          case "retry": await setup(); break;
          case "organization":
            if (!running && choices.some(choice => choice.id === message.id)) { draft.organizationId = message.id; draft.layoutId = undefined; await keep(); await setup(); }
            break;
          case "draft":
            if (!running && message.draft && typeof message.draft === "object") {
              for (const key of ["name", "description", "layoutId"] as const) if (typeof message.draft[key] === "string") draft[key] = message.draft[key];
              await keep();
            }
            break;
          case "start": case "write": {
            if (running || attaching) return;
            const pending = this.context.workspaceState.get<Pending>(PENDING + link);
            if (pending) { await follow(pending); break; }
            if (!lease()) { await setup(); break; }
            if (!draft.organizationId || !choices.some(choice => choice.id === draft.organizationId)) throw new UserInputError(H.addOrganization);
            if (!layouts.some(row => row.id === message.layoutId)) throw new UserInputError(EMAIL_COPY.newEmail.chooseLayout);
            const name = typeof message.name === "string" ? message.name.trim().replace(/\.email\.json$/i, "") : "";
            if (emailNameProblem(name)) throw new UserInputError(EMAIL_COPY.newEmail.nameInvalid);
            const statement = typeof message.description === "string" ? message.description.trim() : "";
            if (message.type === "write" && (!statement || statement.length > 8000)) throw new UserInputError(H.descriptionRequired);
            const request: MaterializeRequest = { name, format: "json", outputKind: "email_template", accountIds: [draft.organizationId], layoutId: message.layoutId, requestId: globalThis.crypto.randomUUID(),
              ...(message.type === "write" ? { statement, sources: draft.references.map(ref => ({ kind: "library_file" as const, ref: ref.fileId, label: ref.label, content: true, ...(ref.include ? { includeImages: true } : {}) })) } : {}) };
            draft = { ...draft, name: message.name, layoutId: message.layoutId, ...(message.type === "write" ? { description: statement } : {}) };
            // Claim the flight synchronously before any persistence await.
            await follow({ request }); break;
          }
          case "cancel": abort?.abort(); break;
          case "attach": await attach(); break;
          case "include": case "removeReference":
            if (!running && !attaching && Number.isInteger(message.index) && draft.references[message.index]) {
              if (message.type === "removeReference") draft.references.splice(message.index, 1);
              else draft.references[message.index]!.include = message.on === true && draft.references[message.index]!.kind !== "file";
              await keep(); await references();
            }
            break;
          case "dictationStart": {
            const connection = lease(); if (!connection) { await setup(); break; }
            await this.dictation.start(panel, connection, H.operations.description); break;
          }
          case "dictationStop": await this.dictation.stop(panel); break;
          case "dictationCancel": this.dictation.cancel(panel); break;
          case "copyProblemReference": if (typeof message.reference === "string" && /^tools-[a-zA-Z0-9-]{1,100}$/.test(message.reference)) await vscode.env.clipboard.writeText(message.reference); break;
        }
      } catch (error) { if (!(error instanceof PageClosed)) await post({ type: "failed", problem: reportProblem(error, H.operations.write, lease()) }); }
    };
    subscriptions.push(panel.webview.onDidReceiveMessage(receive), this.api.onDidChange(() => { void setup(); }));
    panel.onDidDispose(() => {
      disposed = true; generation++; this.dictation.cancel(panel);
      for (const subscription of subscriptions) subscription.dispose();
      if (this.panels.get(link) === panel) { this.panels.delete(link); this.shown.delete(link); }
      // Closing only detaches. Aborting here would cancel work the person still wants.
    });
    const root = vscode.Uri.joinPath(this.context.extensionUri, "out");
    panel.webview.options = { enableScripts: true, localResourceRoots: [root] };
    panel.webview.html = newEmailPage({ script: panel.webview.asWebviewUri(vscode.Uri.joinPath(root, "newEmailView.js")).toString(), nonce: globalThis.crypto.randomUUID().replace(/-/g, "") });
  }
}
