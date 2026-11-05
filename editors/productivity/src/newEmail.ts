// A New email link is a page, not a file. It lists the built-in layouts as wireframes
// the server drew, then either makes the template from the layout at once (Start from
// the layout) or starts MemQL writing it in the background (Write it). Either way the
// template opens in the email editor and the page closes.
import * as vscode from "vscode";
import {
  buildCampaignComposeEmail, buildCampaignCreateTemplateFromLayout, buildEmailLayouts, buildLibraryFilesForOwner,
} from "@znasllc-io/memql-sdk-core/client";
import type { ConnectionLease, EditorConnectionAPI } from "../../vscode/src/connection/api.js";
import { newEmailResource, resourceFrom } from "./documents.js";
import { DocumentDictation } from "./dictation.js";
import { organizations, type Organization } from "./emailPublish.js";
import { newEmailPage, emailNameProblem } from "./newEmailPage.js";
import { EMAIL_COPY } from "./emailCopy.js";
import { reportProblem, UserInputError } from "./problems.js";

const H = EMAIL_COPY.host;
const DRAFT = "memql.newEmail.draft:", ORGANIZATION = "memql.newEmail.organization:";
const allowed = /\.(png|jpe?g|gif|zip|txt|md|markdown|html?|css|json|csv|svg)$/i;
const kind = (name: string): Reference["kind"] => /\.(png|jpe?g|gif)$/i.test(name) ? "picture" : /\.zip$/i.test(name) ? "bundle" : "file";
const mime = (name: string) => /\.png$/i.test(name) ? "image/png" : /\.jpe?g$/i.test(name) ? "image/jpeg" : /\.gif$/i.test(name) ? "image/gif" : /\.zip$/i.test(name) ? "application/zip" : "text/plain";
const text = (row: Record<string, unknown>, key: string) => typeof row[key] === "string" ? row[key] as string : "";
const SIXTEEN_MIB = 16 * 1024 * 1024;
const pictureOnly = /\.(png|jpe?g|gif)$/i;
interface Reference { fileId: string; label: string; kind: "picture" | "bundle" | "file"; size?: number }
/** `reference` is the picture of the finished email: one picture, apart from the material files in `references`. */
interface Draft { organizationId?: string; layoutId?: string; name?: string; description?: string; references: Reference[]; reference?: Reference }
class PageClosed extends Error {}
/** The template was made, but the editor would not open it. */
class OpenFailed extends Error { constructor(cause: unknown) { super("The template could not be opened.", { cause }); } }

export class NewEmail implements vscode.CustomReadonlyEditorProvider<vscode.CustomDocument> {
  private readonly shown = new Map<string, { state: string; loading: boolean }>();
  // Duplicate tabs for one link share one owner.
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
      const shown = this.shown.get(uri.toString());
      if (shown && !shown.loading) return { state: shown.state };
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
    let draft: Draft = { organizationId: resource.id, references: [], ...this.context.workspaceState.get<Draft>(DRAFT + link) };
    let choices: Organization[] = [], layouts: Record<string, unknown>[] = [];
    const subscriptions: vscode.Disposable[] = [];
    const lease = () => { const current = this.api.current(); return current?.domain === domain ? current : undefined; };
    const post = async (message: unknown) => { if (!disposed) await panel.webview.postMessage(message); };
    const keep = () => this.context.workspaceState.update(DRAFT + link, draft);
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
        const rows = organizationId ? await execute(connection)("emailLayouts", buildEmailLayouts({ accountId: organizationId })) : [];
        if (epoch !== generation || disposed) return;
        // Only the built-in layouts are offered, each as the wireframe the server drew.
        layouts = rows.filter(row => row.kind === "builtin" && text(row, "id") && text(row, "name"));
        await post({ type: "setup", connected: true, domain, organizations: choices, organizationId, dictation: await this.dictation.available(),
          layouts: layouts.map(row => ({ id: text(row, "id"), name: text(row, "name"), category: text(row, "category"), description: text(row, "description"), wireframe: text(row, "wireframe") })) });
      } catch (error) {
        if (epoch === generation && !(error instanceof PageClosed)) await post({ type: "layoutsFailed", problem: reportProblem(error, H.operations.layouts, connection) });
      }
    };
    // The template made by the last act, kept while it could not be opened: the next act opens it rather than making another.
    let made: { templateId: string; name: string; organizationId: string } | undefined;
    const open = async (templateId: string, name: string, organizationId: string) => {
      const file = /\.email\.json$/i.test(name) ? name : name + ".email.json";
      const uri = vscode.Uri.from({ scheme: "memql-file", authority: domain, path: `/templates/${templateId}/${file.replace(/[\\/]/g, "_")}` });
      resourceFrom(uri.toString());
      try { await vscode.commands.executeCommand("vscode.openWith", uri, "memql.productivity.email", { viewColumn: panel.viewColumn, preview: false }); }
      catch (error) { made = { templateId, name, organizationId }; throw new OpenFailed(error); }
      made = undefined;
      await this.context.workspaceState.update(DRAFT + link, undefined);
      await this.context.globalState.update(ORGANIZATION + domain, organizationId);
      panel.dispose();
    };
    const create = async (message: Record<string, unknown>) => {
      if (running || attaching || disposed) return;
      const connection = lease();
      if (!connection) { await setup(); return; }
      running = true;
      try {
        if (made) { await open(made.templateId, made.name, made.organizationId); return; }
        const organizationId = draft.organizationId;
        if (!organizationId || !choices.some(choice => choice.id === organizationId)) throw new UserInputError(H.addOrganization);
        const layoutId = text(message, "layoutId");
        if (!layouts.some(row => row.id === layoutId)) throw new UserInputError(EMAIL_COPY.newEmail.chooseLayout);
        const name = typeof message.name === "string" ? message.name.trim().replace(/\.email\.json$/i, "") : "";
        if (emailNameProblem(name)) throw new UserInputError(EMAIL_COPY.newEmail.nameInvalid);
        const writing = message.type === "write";
        const description = typeof message.description === "string" ? message.description.trim() : "";
        if (writing && (!description || description.length > 8000)) throw new UserInputError(H.descriptionRequired);
        draft = { ...draft, name, layoutId, ...(writing ? { description } : {}) };
        await keep();
        const call = writing
          ? buildCampaignComposeEmail({ accountId: organizationId, name, layoutId, description, fileIds: draft.references.map(ref => ref.fileId), ...(draft.reference ? { referenceFileId: draft.reference.fileId } : {}) })
          : buildCampaignCreateTemplateFromLayout({ accountId: organizationId, name, layoutId });
        const rows = await execute(connection)(writing ? "campaignComposeEmail" : "campaignCreateTemplateFromLayout", call);
        const templateId = text(rows[0] ?? {}, "templateId");
        if (!templateId) throw new Error("The answer named no template.");
        // A composition that could not start still made the template: the editor shows that and offers Retry.
        await open(templateId, name, organizationId);
      } catch (error) {
        if (!disposed && !(error instanceof PageClosed)) await post({ type: "failed", problem: error instanceof OpenFailed ? { message: H.openFailed } : reportProblem(error, H.operations.write, connection) });
      } finally { running = false; }
    };
    const references = async (problem?: unknown) => post({ type: "references", items: draft.references, ...(problem ? { problem: reportProblem(problem, H.operations.references, lease()) } : {}) });
    const finished = async (problem?: unknown) => post({ type: "reference", item: draft.reference ? { label: draft.reference.label } : null, ...(problem ? { problem: reportProblem(problem, H.operations.references, lease()) } : {}) });
    // Material files are many; the picture of the finished email is one PNG, JPEG or GIF and is not counted with them.
    const attach = async (picture: boolean) => {
      if (attaching || running) return;
      const connection = lease();
      if (!connection) { await setup(); return; }
      attaching = true;
      const accepted = picture ? pictureOnly : allowed, wrong = picture ? H.pictureType : H.unsupported;
      const taken = picture ? 0 : draft.references.length, used = picture ? 0 : draft.references.reduce((sum, ref) => sum + (ref.size ?? 0), 0);
      const keepOne = async (item: Reference) => {
        if (picture) { draft.reference = item; await keep(); await finished(); } else { draft.references.push(item); await keep(); await references(); }
      };
      try {
        const from = await vscode.window.showQuickPick([H.fromComputer, H.fromFiles], { title: picture ? H.chooseFinished : H.addReferences });
        if (!from || disposed) return;
        if (from === H.fromComputer) {
          const selected = await vscode.window.showOpenDialog({ canSelectMany: !picture, canSelectFiles: true, canSelectFolders: false, title: picture ? H.chooseFinished : H.chooseReferences, openLabel: H.addLabel,
            filters: picture ? { Pictures: ["png", "jpg", "jpeg", "gif"] } : { References: ["png", "jpg", "jpeg", "gif", "zip", "txt", "md", "html", "css", "json", "csv", "svg"] } });
          if (!selected?.length) return;
          if (taken + selected.length > 64) throw new UserInputError(H.tooMany);
          const files = await Promise.all(selected.map(async uri => {
            const label = uri.path.split("/").pop() ?? "";
            if (!accepted.test(label)) throw new UserInputError(wrong);
            return { uri, label, size: (await vscode.workspace.fs.stat(uri)).size };
          }));
          if (files.reduce((sum, file) => sum + file.size, used) > SIXTEEN_MIB) throw new UserInputError(H.tooLarge);
          const contents = await Promise.all(files.map(file => vscode.workspace.fs.readFile(file.uri)));
          if (contents.reduce((sum, bytes) => sum + bytes.length, used) > SIXTEEN_MIB) throw new UserInputError(H.tooLarge);
          for (let i = 0; i < files.length && !disposed; i++) {
            const file = files[i]!, bytes = contents[i]!;
            const saved = await this.api.uploadFile(connection, file.label, mime(file.label), bytes);
            await keepOne({ fileId: saved.fileId, label: file.label, kind: kind(file.label), size: bytes.length });
          }
        } else {
          const rows = await execute(connection)("libraryFilesForOwner", buildLibraryFilesForOwner({}));
          const choices = rows.filter(row => accepted.test(text(row, "name"))).map(row => ({ label: text(row, "name"), fileId: text(row, "id"), size: typeof row.size === "number" ? row.size : 0 }));
          const selected = await vscode.window.showQuickPick(choices, { placeHolder: picture ? H.recentPictures : H.recentFiles, canPickMany: !picture });
          const picked = Array.isArray(selected) ? selected : selected ? [selected] : [];
          if (!picked.length) return;
          if (taken + picked.length > 64) throw new UserInputError(H.tooMany);
          if (picked.reduce((sum, file) => sum + file.size, used) > SIXTEEN_MIB) throw new UserInputError(H.tooLarge);
          for (const file of picked) if (picture || !draft.references.some(ref => ref.fileId === file.fileId)) await keepOne({ ...file, kind: kind(file.label) });
        }
      } catch (error) { if (!(error instanceof PageClosed)) await (picture ? finished : references)(error); }
      finally { attaching = false; }
    };
    const receive = async (message: any) => {
      if (disposed || !message || typeof message !== "object") return;
      try {
        switch (message.type) {
          case "shown":
            if (typeof message.state === "string" && typeof message.loading === "boolean") this.shown.set(link, { state: message.state, loading: message.loading });
            break;
          case "ready":
            await post({ type: "restore", draft }); await references(); await finished();
            await setup();
            break;
          case "connect": await this.api.connect(domain); await setup(); break;
          case "retry": await setup(); break;
          case "organization":
            if (!running && choices.some(choice => choice.id === message.id)) { draft.organizationId = message.id; draft.layoutId = undefined; await keep(); await setup(); }
            break;
          case "draft":
            if (!running && message.draft && typeof message.draft === "object") {
              // A changed request is a new one: the template made for the last is left as it is.
              made = undefined;
              for (const key of ["name", "description", "layoutId"] as const) if (typeof message.draft[key] === "string") draft[key] = message.draft[key];
              await keep();
            }
            break;
          case "start": case "write": await create(message); break;
          case "attach": await attach(false); break;
          case "attachReference": await attach(true); break;
          case "clearReference":
            if (!running && !attaching) { draft.reference = undefined; await keep(); await finished(); }
            break;
          case "removeReference":
            if (!running && !attaching && Number.isInteger(message.index) && draft.references[message.index]) {
              draft.references.splice(message.index, 1);
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
