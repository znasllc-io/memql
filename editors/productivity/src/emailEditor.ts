// The email editor: the custom text editor of `*.email.json` files. Design (the page, emailPage.ts and
// emailView.ts) and Source (VS Code's own text editor) show one buffer, the TextDocument; this host keeps the page
// in step with it and carries out what the person does on the page.
//
// ONE BUFFER. The page sends edits as operations (EmailEdit). Each is applied to the current text with editEmail,
// written with writeEmail and put back as ONE replacement of the span that changed (minimalEdit), so a Source editor
// beside the page keeps its cursor and every edit is one undo step. A CRLF file stays CRLF. Edits are refused while
// the text is not valid JSON, and they apply one after another, each to the text the one before it left.
//
// ONE RENDERER. The preview is the cluster's emailRender of the text, never a render made here: after every change and
// every new sample value, once the person has paused, one render at a time, and an answer older than the newest request
// is dropped. Each render the page is sent carries a seq that only grows for as long as the panel lives.
//
// NEVER CONNECTS ON ITS OWN. A MemQL file is shown with what it says until the person connects (Connect asks the core
// extension for the file's cluster; a local email chooses one). Nothing is read from the cluster before that, not even
// the file's own record, which would connect.
//
// THE BAR. The state and the acts are editorBar's (emailPublish.ts), from what this host knows: the connection, whether
// the text can be read, the organization, the render's issues (a refused render counts as one), whether the email is
// saved and what the cluster published. Publishing and saving a layout run through EmailPublisher, one flight per email,
// and never open a tab. Every act the page sends is answered with the bar, or with a problem. While the organization and
// the template are being found for a connection, the bar is not sent at all, rather than one that would be wrong.
//
// Shown in words, never by id: an organization the person does not list, and a refusal (the cluster's sentence, which
// userMessage takes out of its frames).

import * as vscode from "vscode";
import { buildEmailLayouts, buildEmailRender, buildLibraryArtifactForFile, buildLibraryFilesForOwner } from "@znasllc-io/memql-sdk-core/client";
import type { ConnectionLease, EditorConnectionAPI } from "../../vscode/src/connection/api.js";
import { resourceFrom, type Documents, type OpenDocument } from "./documents.js";
import { EMAIL_COPY } from "./emailCopy.js";
import { addedBlockId, editEmail, findBlock, isLayoutEmail, minimalEdit, readEmail, sameEmail, sampleKeys, writeEmail, type EmailDocument, type EmailEdit } from "./emailDocument.js";
import { emailPage } from "./emailPage.js";
import {
  chooseOrganization, editorBar, EmailPublisher, issuesOf, PublishBlocked, resolveOrganization, templateFor,
  type BarAct, type EmailIssue, type EmailTarget, type Organization, type PublishPorts, type TemplateState,
} from "./emailPublish.js";
import { reportProblem, UserInputError } from "./problems.js";
import { showProblem } from "./problemUI.js";
import { refreshQueue } from "./refreshQueue.js";

const C = EMAIL_COPY.editor;
const H = EMAIL_COPY.host;
const OPERATION = H.operations;

const VIEW_TYPE = "memql.productivity.email";
/** The page state of one email (ViewState, and the organization chosen for it), by its address. */
const PAGE_STATE = "memql.emailView:";
/** EmailPublisher's record of a layout being created (emailPublish.ts); the key this host gives it follows. */
const LAYOUT_PENDING = "memql.emailLayoutPending:";
/** A change or a new sample value is rendered once the person has paused this long. */
const PAUSE = 300;
/** How long the bar says that a layout was saved. */
const SAVED_FOR = 4000;
/** The largest picture an email may carry (MaxImageBytes in component/compose/emaildoc). */
const MAX_PICTURE_BYTES = 1024 * 1024;
const EMAIL_FILE = /\.email\.json$/i;
/** A bare id the cluster gives a file, a picture or a layout. */
const ID = /^[A-Za-z0-9_-]{1,160}$/;
/** The pictures an email can carry, by the extensions the upload dialog offers. */
const PICTURE_TYPES: Readonly<Record<string, string>> = { png: "image/png", jpg: "image/jpeg", jpeg: "image/jpeg", gif: "image/gif" };
const PICTURE_MIMES: ReadonlySet<string> = new Set(Object.values(PICTURE_TYPES));
/** The merge tags a sample value is kept for (sampleKeys in emailDocument.ts). */
const SAMPLE_KEY = /^(?:displayName|email|accountName|campaignName|fields\.[^{}\r\n]{1,120})$/;
const MAX_SAMPLES = 64;
const MAX_SAMPLE_CHARACTERS = 2000;

type Width = "desktop" | "mobile" | "plain";
/** The page's own state, as emailView.ts keeps it. */
interface ViewState { width: Width; selected?: string; themeOpen?: boolean; samplesOpen?: boolean; samples?: Record<string, string> }
/** What the page last reported it shows (the host suite's test hook). */
interface Shown { version: number; subject: string; state: string; blocks: number }

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);
const text = (row: Record<string, unknown> | undefined, key: string): string => typeof row?.[key] === "string" ? row[key] as string : "";
/** The last segment of an address, as VS Code gives the path (decoded). */
const nameOf = (uri: vscode.Uri): string => uri.path.split("/").pop() ?? "";
const sameIssue = (a: EmailIssue, b: EmailIssue) => a.code === b.code && a.block === b.block && a.message === b.message;

/** A saved page state, read as data: anything that is not what the page keeps is left out, and no width is no state. */
function asViewState(value: unknown): ViewState | undefined {
  if (!isRecord(value) || (value.width !== "desktop" && value.width !== "mobile" && value.width !== "plain")) return undefined;
  const state: ViewState = { width: value.width };
  if (typeof value.selected === "string" && value.selected.length <= 64) state.selected = value.selected;
  if (typeof value.themeOpen === "boolean") state.themeOpen = value.themeOpen;
  if (typeof value.samplesOpen === "boolean") state.samplesOpen = value.samplesOpen;
  if (isRecord(value.samples)) {
    const samples = Object.entries(value.samples)
      .filter((entry): entry is [string, string] => SAMPLE_KEY.test(entry[0]) && typeof entry[1] === "string" && entry[1].length <= MAX_SAMPLE_CHARACTERS)
      .slice(0, MAX_SAMPLES);
    if (samples.length > 0) state.samples = Object.fromEntries(samples);
  }
  return state;
}

/** The cluster a MemQL file belongs to, or nothing for a file anywhere else. */
function domainOf(uri: vscode.Uri): string | undefined {
  if (uri.scheme !== "memql-file") return undefined;
  try { return resourceFrom(uri.toString()).domain; } catch { return uri.authority.toLowerCase(); }
}

/** What an opened MemQL file is to the publisher: a template, or a Library file with its labels. */
function targetOf(opened: OpenDocument): EmailTarget {
  const { resource } = opened;
  if (resource.kind === "templates" && opened.template) {
    return { kind: "template", domain: resource.domain, templateId: opened.sourceId, name: opened.template.name, accountId: opened.template.accountId,
      status: opened.template.status, revision: opened.revision ?? "" };
  }
  return { kind: "file", domain: resource.domain, artifactId: resource.id, fileId: opened.sourceId, name: resource.name, labels: [...(opened.accountIds ?? [])] };
}

/** The type of a picture picked to upload, from its extension, when its bytes say the same; otherwise the person is told what to pick. */
function pictureMime(name: string, bytes: Uint8Array): string {
  const extension = /\.([A-Za-z]+)$/.exec(name)?.[1]?.toLowerCase() ?? "";
  const mime = Object.hasOwn(PICTURE_TYPES, extension) ? PICTURE_TYPES[extension] : undefined;
  const starts = (...values: number[]) => values.every((value, index) => bytes[index] === value);
  const matches = mime === "image/png" ? starts(137, 80, 78, 71, 13, 10, 26, 10)
    : mime === "image/jpeg" ? starts(255, 216, 255)
    : mime === "image/gif" ? starts(71, 73, 70, 56) && (bytes[4] === 55 || bytes[4] === 57) && bytes[5] === 97
    : false;
  if (mime === undefined || !matches) throw new UserInputError(H.pictureType);
  return mime;
}

/** What one email's panels share: the publisher (one flight per email) and the order its edits apply in. */
interface Session {
  readonly ports: PublishPorts;
  readonly publisher: EmailPublisher;
  /** Runs `work` after every edit queued before it, whether that one worked or not. */
  queue(work: () => Promise<void>): Promise<void>;
  panels: number;
}

/** What a panel needs of the editor. */
interface Host {
  readonly context: vscode.ExtensionContext;
  readonly api: EditorConnectionAPI;
  readonly files: Documents;
  readonly load: (uri: vscode.Uri) => Promise<OpenDocument>;
  show(mode: "design" | "source" | "split", uri?: vscode.Uri): Promise<void>;
  activated(panel: EmailPanel): void;
  shown(panel: EmailPanel, shown: Shown): void;
  closed(panel: EmailPanel): void;
}

export class EmailEditor implements vscode.CustomTextEditorProvider {
  private active?: EmailPanel;
  private readonly panels = new Set<EmailPanel>();
  private readonly sessions = new Map<string, Session>();
  private readonly shownBy = new Map<string, { panel: EmailPanel; shown: Shown }>();
  private readonly host: Host;

  constructor(context: vscode.ExtensionContext, private readonly api: EditorConnectionAPI, files: Documents, load: (uri: vscode.Uri) => Promise<OpenDocument>) {
    this.host = {
      context, api, files, load,
      show: (mode, uri) => this.show(mode, uri),
      activated: panel => { this.active = panel; },
      shown: (panel, shown) => { this.shownBy.set(panel.document.uri.toString(), { panel, shown }); },
      closed: panel => this.closed(panel),
    };
  }

  /** Test hook: what the page shows of `uri` (at `version`, when given), once it has said so. */
  async whenShown(uri: vscode.Uri, version?: number): Promise<{ subject: string; state: string; blocks: number }> {
    const end = Date.now() + 15000;
    while (Date.now() < end) {
      const shown = this.shownBy.get(uri.toString())?.shown;
      if (shown && (version === undefined || shown.version === version)) return { subject: shown.subject, state: shown.state, blocks: shown.blocks };
      await new Promise(resolve => setTimeout(resolve, 40));
    }
    throw new Error(`The email editor did not show this version (${JSON.stringify({ expected: version, shown: this.shownBy.get(uri.toString())?.shown })}).`);
  }

  /**
   * Design, Source, or both side by side, on the one buffer. Source shows the text in the page's column and closes the
   * page; Design opens the page in the text's column, where the text's tab stays (closing it could save the buffer);
   * Split shows the text beside the page, and a pair already showing is kept as it is.
   */
  async show(mode: "design" | "source" | "split", uri?: vscode.Uri): Promise<void> {
    const target = uri ?? vscode.window.activeTextEditor?.document.uri ?? this.active?.document.uri;
    if (!target || !EMAIL_FILE.test(target.path)) throw new UserInputError(H.openEmail);
    const document = await vscode.workspace.openTextDocument(target);
    const key = target.toString();
    const mine = [...this.panels].filter(panel => panel.document.uri.toString() === key);
    const current = mine.find(panel => panel.panel.visible) ?? mine[0];
    const source = vscode.window.visibleTextEditors.find(editor => editor.document.uri.toString() === key);
    if (mode === "source") {
      await vscode.window.showTextDocument(document, { viewColumn: source?.viewColumn ?? current?.panel.viewColumn, preview: false });
      for (const panel of mine) panel.panel.dispose();
    } else if (mode === "design") {
      await vscode.commands.executeCommand("vscode.openWith", target, VIEW_TYPE, { viewColumn: source?.viewColumn ?? current?.panel.viewColumn ?? vscode.ViewColumn.Active, preview: false });
      this.closeOtherDesigns(document);
    } else if (source && current?.panel.visible && source.viewColumn !== current.panel.viewColumn) {
      // The pair is there already: another Split must not add an editor group.
      current.panel.reveal(current.panel.viewColumn);
    } else if (current) {
      current.panel.reveal(current.panel.viewColumn);
      await vscode.window.showTextDocument(document, { viewColumn: vscode.ViewColumn.Beside, preview: false });
    } else {
      await vscode.window.showTextDocument(document, { viewColumn: source?.viewColumn, preview: false });
      await vscode.commands.executeCommand("vscode.openWith", target, VIEW_TYPE, { viewColumn: vscode.ViewColumn.Beside, preview: false });
      this.closeOtherDesigns(document);
    }
  }

  /** VS Code may open a second page for the email in another group: the one now active is kept. */
  private closeOtherDesigns(document: vscode.TextDocument): void {
    const keep = [...this.panels].find(panel => panel.document === document && panel.panel.active);
    if (!keep) return;
    for (const panel of [...this.panels]) if (panel.document === document && panel !== keep) panel.panel.dispose();
  }

  async resolveCustomTextEditor(document: vscode.TextDocument, panel: vscode.WebviewPanel): Promise<void> {
    const entry = new EmailPanel(this.host, document, panel, this.session(document));
    this.panels.add(entry);
    this.active = entry;
    entry.open();
  }

  /** The session of an email, made with its first panel. Calls go to the email's own cluster, on the connection in use now. */
  private session(document: vscode.TextDocument): Session {
    const key = document.uri.toString();
    let session = this.sessions.get(key);
    if (!session) {
      const domain = domainOf(document.uri);
      const ports: PublishPorts = {
        execute: async (name, call) => {
          const lease = this.lease(domain);
          if (!lease) throw new UserInputError(H.connectFirst);
          return this.api.execute(lease, name, call);
        },
        state: this.host.context.workspaceState,
      };
      let tail: Promise<void> = Promise.resolve();
      session = {
        ports, publisher: new EmailPublisher(ports), panels: 0,
        queue: work => {
          const run = tail.then(work, work);
          tail = run.catch(() => undefined);
          return run;
        },
      };
      this.sessions.set(key, session);
    }
    session.panels++;
    return session;
  }

  private lease(domain: string | undefined): ConnectionLease | undefined {
    const current = this.api.current();
    return current && (domain === undefined || current.domain === domain) ? current : undefined;
  }

  private closed(panel: EmailPanel): void {
    this.panels.delete(panel);
    if (this.active === panel) this.active = undefined;
    const key = panel.document.uri.toString();
    if (this.shownBy.get(key)?.panel === panel) this.shownBy.delete(key);
    const session = this.sessions.get(key);
    if (session && --session.panels <= 0) this.sessions.delete(key);
  }
}

/** One page of one email. */
class EmailPanel {
  private disposed = false;
  private readonly domain: string | undefined;
  private readonly stateKey: string;
  private readonly subscriptions: vscode.Disposable[] = [];
  /** Messages go to the page in the order they were made. */
  private outbox: Promise<void> = Promise.resolve();
  private lastBar = "";
  /** Every render this panel sends has a seq above the one before. */
  private seq = 0;
  /** The connection what is known was learned on (`known`), and the connection seen last. */
  private known?: string;
  private seen?: string;
  /** The last attempt to find the organization failed: the next change of the text, or a reload of the page, tries again. */
  private unresolved = false;
  private organization?: Organization;
  private choices: Organization[] = [];
  /** The MemQL file as it was last read (a template's revision and status move in it as it is saved). */
  private opened?: OpenDocument;
  /** The template a Library file was published as. A template resource reads its own from `opened`. */
  private template?: TemplateState;
  /** The name of the layout this email was saved as, when there is one. */
  private layoutName?: string;
  private busy?: "publish" | "layout" | "copy";
  private layoutSaved = false;
  private layoutTimer?: ReturnType<typeof setTimeout>;
  private renderTimer?: ReturnType<typeof setTimeout>;
  /** The last render: its issues, or that the cluster refused it. */
  private rendered?: { issues: EmailIssue[]; refused: boolean };
  /** The last publish that failed, while the text is the text it failed on: the issues its save found, and whether the email is still being looked at again. */
  private failed?: { version: number; issues: EmailIssue[]; settling: boolean };

  constructor(private readonly host: Host, readonly document: vscode.TextDocument, readonly panel: vscode.WebviewPanel, private readonly session: Session) {
    this.domain = domainOf(document.uri);
    this.stateKey = PAGE_STATE + document.uri.toString();
  }

  open(): void {
    const root = vscode.Uri.joinPath(this.host.context.extensionUri, "out");
    this.panel.webview.options = { enableScripts: true, localResourceRoots: [root] };
    const script = this.panel.webview.asWebviewUri(vscode.Uri.joinPath(root, "emailView.js"));
    const nonce = globalThis.crypto.randomUUID().replace(/-/g, "");
    this.seen = this.connectionKey();
    this.subscriptions.push(
      vscode.workspace.onDidChangeTextDocument(event => { if (event.document === this.document) this.changed(); }),
      vscode.workspace.onDidSaveTextDocument(saved => { if (saved === this.document) void this.postBar(); }),
      this.host.api.onDidChange(() => this.connectionChanged()),
      this.panel.onDidChangeViewState(() => { if (this.panel.active) this.host.activated(this); }),
      this.panel.webview.onDidReceiveMessage(message => this.receive(message)),
    );
    this.panel.onDidDispose(() => this.dispose());
    this.panel.webview.html = emailPage({ title: nameOf(this.document.uri), script: String(script), nonce });
  }

  private dispose(): void {
    this.disposed = true;
    if (this.renderTimer !== undefined) clearTimeout(this.renderTimer);
    if (this.layoutTimer !== undefined) clearTimeout(this.layoutTimer);
    for (const subscription of this.subscriptions) subscription.dispose();
    this.host.closed(this);
  }

  // -- The connection and what is known on it ----------------------------------------------------------------

  /** The connection this email can use now: the current one, when it is to the email's own cluster (any, for a local email). */
  private lease(): ConnectionLease | undefined {
    const current = this.host.api.current();
    return current && (this.domain === undefined || current.domain === this.domain) ? current : undefined;
  }

  private connectionKey(): string {
    const lease = this.lease();
    return lease ? JSON.stringify([lease.domain, lease.name, lease.generation]) : "";
  }

  /** Whether what the bar depends on is known for the connection in use. Without one there is nothing to find. */
  private isKnown(): boolean {
    const key = this.connectionKey();
    return key === "" || this.known === key;
  }

  private connectionChanged(): void {
    if (this.disposed) return;
    const key = this.connectionKey();
    if (key === this.seen) return;
    this.seen = key;
    // What was known belongs to the connection it was learned on.
    this.known = undefined;
    this.organization = undefined;
    this.choices = [];
    this.template = undefined;
    this.layoutName = undefined;
    this.rendered = undefined;
    this.failed = undefined;
    void this.resolve();
  }

  /**
   * The organization, the template and the layout of the email, found on the connection in use; then a render, and the bar.
   * Without the organization nothing can be said, so a failure to find it leaves the bar as it was and the problem in words.
   * A failure to find what is published is told too, and the bar then offers Publish rather than say the email is published.
   */
  private readonly resolve = refreshQueue(async current => {
    if (this.disposed) return;
    const key = this.connectionKey();
    if (key === "") {
      this.known = key;
      await this.postContext();
      await this.postBar();
      return;
    }
    const stale = () => this.disposed || !current() || key !== this.connectionKey();
    let target: EmailTarget;
    try {
      target = await this.target();
      const { organization, choices } = await resolveOrganization(target, this.session.ports, this.remembered());
      if (stale()) return;
      this.organization = organization;
      this.choices = choices;
      this.unresolved = false;
    } catch (error) {
      if (stale()) return;
      this.unresolved = true;
      await this.problem(error, OPERATION.organization);
      return;
    }
    await this.postContext();
    void this.render();
    let template: TemplateState | undefined, layoutName: string | undefined;
    const organization = this.organization;
    if (organization) {
      try {
        [template, layoutName] = await Promise.all([templateFor(target, organization, this.session.ports), this.layoutNamed(target, organization)]);
      } catch (error) {
        if (stale()) return;
        await this.problem(error, OPERATION.published);
      }
    }
    if (stale()) return;
    this.template = template;
    this.layoutName = layoutName;
    this.known = key;
    await this.postBar();
  });

  /** What the email is to the publisher. Reading a MemQL file's record is for a connected email only: it would connect. */
  private async target(): Promise<EmailTarget> {
    if (this.document.uri.scheme !== "memql-file") return { kind: "local", name: nameOf(this.document.uri) };
    this.opened = await this.host.load(this.document.uri);
    return targetOf(this.opened);
  }

  /** The name of the layout this email was saved as: found by the file it was saved from, or by the id kept for one being made. */
  private async layoutNamed(target: EmailTarget, organization: Organization): Promise<string | undefined> {
    if (target.kind === "local" || !this.layoutEmail()) return undefined;
    const kept = this.host.context.workspaceState.get<unknown>(LAYOUT_PENDING + this.layoutKey(organization));
    const keptId = typeof kept === "string" && ID.test(kept) ? kept : undefined;
    if (target.kind !== "file" && keptId === undefined) return undefined;
    // A row may come without its document (documentOmitted): the name is all this needs.
    const rows = await this.session.ports.execute("emailLayouts", buildEmailLayouts({ accountId: organization.id }));
    const row = rows.find(row => text(row, "layoutId") !== "" && (text(row, "layoutId") === keptId || (target.kind === "file" && text(row, "sourceArtifactId") === target.artifactId)));
    return text(row, "name") || undefined;
  }

  /** The key EmailPublisher keeps a layout being made under: the email and the organization it is saved for. */
  private layoutKey(organization: Organization): string {
    return JSON.stringify([this.document.uri.toString(), organization.id]);
  }

  private email(): EmailDocument | undefined {
    try { return readEmail(this.document.getText()); } catch { return undefined; }
  }

  private layoutEmail(): boolean {
    const email = this.email();
    return email !== undefined && isLayoutEmail(email);
  }

  // -- The page state -----------------------------------------------------------------------------------------

  private stored(): Record<string, unknown> {
    const value = this.host.context.workspaceState.get<unknown>(this.stateKey);
    return isRecord(value) ? value : {};
  }

  /** The organization chosen on the page for an email whose file cannot carry the choice. */
  private remembered(): string | undefined {
    const organization = this.stored().organization;
    return typeof organization === "string" ? organization : undefined;
  }

  private async keep(view: ViewState | undefined, organization: string | undefined): Promise<void> {
    await this.host.context.workspaceState.update(this.stateKey, { ...(view ?? {}), ...(organization === undefined ? {} : { organization }) });
  }

  private async viewState(value: unknown): Promise<void> {
    const state = asViewState(value);
    if (!state) return;
    const before = asViewState(this.stored());
    await this.keep(state, this.remembered());
    if (JSON.stringify(before?.samples ?? {}) !== JSON.stringify(state.samples ?? {})) this.scheduleRender();
  }

  /** The sample values of the merge tags the email uses, as emailRender takes them. */
  private sample(email: EmailDocument): Record<string, unknown> | undefined {
    const samples = asViewState(this.stored())?.samples ?? {};
    const values: Record<string, unknown> = {}, fields: Record<string, string> = {};
    for (const key of sampleKeys(email)) {
      const value = Object.hasOwn(samples, key) ? samples[key] : undefined;
      if (value === undefined || value === "") continue;
      if (key.startsWith("fields.")) fields[key.slice("fields.".length)] = value;
      else values[key] = value;
    }
    if (Object.keys(fields).length > 0) values.fields = fields;
    return Object.keys(values).length > 0 ? values : undefined;
  }

  // -- Messages ----------------------------------------------------------------------------------------------------

  private post(message: Record<string, unknown>): Promise<void> {
    const sending = this.outbox.then(async () => {
      if (this.disposed) return;
      try { await this.panel.webview.postMessage(message); } catch { /* a page that has gone hears nothing */ }
    });
    this.outbox = sending;
    return sending;
  }

  private problem(error: unknown, operation: string): Promise<void> {
    return this.post({ type: "problem", problem: reportProblem(error, operation) });
  }

  private postDocument(): Promise<void> {
    try {
      return this.post({ type: "document", version: this.document.version, email: readEmail(this.document.getText()) });
    } catch (error) {
      return this.post({ type: "document", version: this.document.version, email: null, invalid: error instanceof UserInputError ? error.message : C.invalidJson });
    }
  }

  private postContext(): Promise<void> {
    const connected = this.lease() !== undefined;
    const organization = connected && this.organization ? { id: this.organization.id, name: this.organizationName(this.organization) } : undefined;
    return this.post({ type: "context", connected, ...(organization ? { organization } : {}), choices: connected ? this.choices : [] });
  }

  /**
   * The bar, unless it would say what it said last (`force` sends it anyway: an act is answered by it). While what the
   * bar depends on is being found for a new connection, nothing is sent.
   */
  private postBar(force = false): Promise<void> {
    if (this.disposed || (!this.busy && !this.isKnown())) return Promise.resolve();
    const { state, acts } = this.bar();
    const message = { type: "bar", state, acts, ...(acts.includes("saveLayout") ? { layoutName: this.layoutName ?? nameOf(this.document.uri).replace(EMAIL_FILE, "") } : {}) };
    const key = JSON.stringify(message);
    if (!force && key === this.lastBar) return Promise.resolve();
    this.lastBar = key;
    return this.post(message);
  }

  private bar(): { state: string; acts: BarAct[] } {
    const email = this.email();
    const template = this.templateState();
    const fromTemplate = this.opened?.resource.kind === "templates";
    return editorBar({
      connected: this.lease() !== undefined, domain: this.domain, invalid: email === undefined, local: this.document.uri.scheme !== "memql-file",
      organization: this.organization && { id: this.organization.id, name: this.organizationName(this.organization) },
      mode: email !== undefined && !isLayoutEmail(email) ? "custom" : "layout",
      dirty: this.document.isDirty, issues: this.issueCount(), template,
      // A template is its own content; a Library file is unchanged while it says what the cluster published.
      unchanged: template !== undefined && (fromTemplate ? !this.document.isDirty : email !== undefined && template.document !== undefined && sameEmail(email, template.document)),
      busy: this.busy, layoutSaved: this.layoutSaved,
    });
  }

  /** The template the email has been published as: a template resource's own, a Library file's as the cluster said. */
  private templateState(): TemplateState | undefined {
    const opened = this.opened;
    if (opened?.resource.kind === "templates" && opened.template) return { templateId: opened.sourceId, status: opened.template.status, revision: opened.revision ?? "" };
    return this.template;
  }

  /** An organization by its name; one the person does not list (a template's, named by its id alone) in words. */
  private organizationName(organization: Organization): string {
    return organization.name === organization.id ? H.unlistedOrganization : organization.name;
  }

  /** The issues to list: the render's, and those the save of a publish that failed found in this same text. */
  private issues(): EmailIssue[] {
    const issues = [...(this.rendered?.issues ?? [])];
    if (this.failed?.version === this.document.version) for (const issue of this.failed.issues) if (!issues.some(known => sameIssue(known, issue))) issues.push(issue);
    return issues;
  }

  /** What the bar counts: the issues, and at least one while the render was refused or a publish that failed is looked at again. */
  private issueCount(): number {
    const failed = this.failed?.version === this.document.version ? this.failed : undefined;
    return Math.max(this.issues().length, this.rendered?.refused || failed?.settling ? 1 : 0);
  }

  private async receive(message: unknown): Promise<void> {
    if (!isRecord(message) || this.disposed) return;
    try {
      switch (message.type) {
        case "ready": await this.ready(); break;
        case "edit": await this.edit(message.op); break;
        case "viewState": await this.viewState(message.state); break;
        case "act": await this.act(message.act); break;
        case "saveLayout": await this.saveLayout(message.name); break;
        case "organization": await this.choose(message.id); break;
        case "picture": await this.picture(message.block, message.action); break;
        case "mode": if (message.mode === "source" || message.mode === "split") await this.host.show(message.mode, this.document.uri); break;
        case "shown":
          if (typeof message.version === "number" && typeof message.subject === "string" && typeof message.state === "string" && typeof message.blocks === "number") {
            this.host.shown(this, { version: message.version, subject: message.subject, state: message.state, blocks: message.blocks });
          }
          break;
        case "copyProblemReference":
          if (typeof message.reference === "string" && /^tools-[a-zA-Z0-9-]{1,100}$/.test(message.reference)) await vscode.env.clipboard.writeText(message.reference);
          break;
      }
    } catch (error) {
      await this.problem(error, OPERATION.view);
    }
  }

  /** A page that has just loaded (or reloaded) is told everything: its state, the connection, the bar, then the document. */
  private async ready(): Promise<void> {
    const view = asViewState(this.stored());
    if (view) await this.post({ type: "restore", state: view });
    this.lastBar = "";
    await this.postContext();
    await this.postBar(true);
    await this.postDocument();
    if (this.isKnown()) void this.render();
    else void this.resolve();
  }

  // -- Rendering -------------------------------------------------------------------------------------------------

  private changed(): void {
    void this.postBar();
    void this.postDocument();
    if (this.unresolved && this.lease()) void this.resolve();
    else this.scheduleRender();
  }

  private scheduleRender(): void {
    if (this.renderTimer !== undefined) clearTimeout(this.renderTimer);
    this.renderTimer = setTimeout(() => { this.renderTimer = undefined; void this.render(); }, PAUSE);
  }

  /** One render at a time; a request made while one runs is the next, and the one running is then out of date and dropped. */
  private readonly render = refreshQueue(async current => {
    const lease = this.lease(), organization = this.organization, email = this.email();
    if (this.disposed || !lease || !organization || !email) return;
    const seq = ++this.seq, sample = this.sample(email);
    let message: Record<string, unknown>;
    try {
      const rows = await this.host.api.execute(lease, "emailRender", buildEmailRender({ document: email as unknown as Record<string, unknown>, accountId: organization.id, ...(sample ? { sample } : {}) }));
      if (this.disposed || !current()) return;
      const row = rows[0] ?? {};
      this.rendered = { issues: issuesOf(row.issues), refused: false };
      message = { type: "render", seq, html: text(row, "html"), text: text(row, "text"), subject: text(row, "subject"), issues: [] };
    } catch (error) {
      if (this.disposed || !current()) return;
      this.rendered = { issues: [], refused: true };
      message = { type: "renderFailed", seq, problem: reportProblem(error, OPERATION.render) };
    }
    if (message.type === "render") message.issues = this.issues();
    await this.post(message);
    await this.postBar();
  });

  // -- Edits ---------------------------------------------------------------------------------------------------

  /**
   * One change to the text, made of the edits `edits` gives for the email as it is when the change's turn comes: written
   * as one replacement of the span that changed, after every change queued before it. Refused while the text is not valid JSON.
   */
  private applyEdits(edits: (email: EmailDocument) => EmailEdit[]): Promise<void> {
    return this.session.queue(async () => {
      const before = this.document.getText();
      let email: EmailDocument;
      try { email = readEmail(before); } catch { throw new UserInputError(H.editsNeedValidJson); }
      let next = email;
      for (const edit of edits(email)) next = editEmail(next, edit);
      let after = writeEmail(next);
      if (this.document.eol === vscode.EndOfLine.CRLF) after = after.replace(/\n/g, "\r\n");
      const span = minimalEdit(before, after);
      if (span.start === span.end && span.text === "") return;
      const change = new vscode.WorkspaceEdit();
      change.replace(this.document.uri, new vscode.Range(this.document.positionAt(span.start), this.document.positionAt(span.end)), span.text);
      if (!await vscode.workspace.applyEdit(change)) throw new Error("VS Code did not apply the change to the email.");
    });
  }

  private async edit(op: unknown): Promise<void> {
    if (!isRecord(op) || typeof op.kind !== "string") return;
    const edit = op as unknown as EmailEdit;
    try {
      let added: string | undefined;
      await this.applyEdits(email => {
        if (edit.kind === "add" && isLayoutEmail(email)) added = addedBlockId(email, edit);
        return [edit];
      });
      // The block the person added is selected, with its first field ready to type in.
      if (added !== undefined) await this.post({ type: "select", block: added });
    } catch (error) {
      await this.problem(error, OPERATION.edit);
    }
  }

  // -- Pictures ------------------------------------------------------------------------------------------------

  private async picture(block: unknown, action: unknown): Promise<void> {
    if (typeof block !== "string") return;
    try {
      if (action === "remove") await this.applyEdits(() => [{ kind: "field", block, field: "src", value: "" }]);
      else if (action === "choose") await this.choosePicture(block);
      else if (action === "upload") await this.uploadPicture(block);
    } catch (error) {
      await this.problem(error, OPERATION.picture);
    }
  }

  /** A picture from Files: the person's own pictures an email can carry, newest first. */
  private async choosePicture(block: string): Promise<void> {
    const lease = this.lease();
    if (!lease) throw new UserInputError(H.connectFirst);
    const rows = await this.host.api.execute(lease, "libraryFilesForOwner", buildLibraryFilesForOwner({}));
    const pictures = rows.filter(row => PICTURE_MIMES.has(text(row, "mimeType")) && row.archived !== true && ID.test(text(row, "id")));
    if (pictures.length === 0) {
      void vscode.window.showInformationMessage(H.noPictures);
      return;
    }
    const chosen = await vscode.window.showQuickPick(pictures.map(row => ({ label: text(row, "name") || text(row, "id"), fileId: text(row, "id") })),
      { title: H.choosePicture, placeHolder: H.recentPictures, ignoreFocusOut: true });
    if (!chosen) return;
    const artifactId = text((await this.host.api.execute(lease, "libraryArtifactForFile", buildLibraryArtifactForFile({ fileId: chosen.fileId })))[0], "id");
    if (!ID.test(artifactId)) throw new Error("The cluster did not return the picture's file link.");
    await this.setPicture(block, artifactId, chosen.label);
  }

  /** A picture from this computer, uploaded to Files as it is: at most 1 MiB, a PNG, JPEG or GIF whose bytes say so. */
  private async uploadPicture(block: string): Promise<void> {
    const lease = this.lease();
    if (!lease) throw new UserInputError(H.connectFirst);
    const picked = (await vscode.window.showOpenDialog({ title: H.uploadPicture, openLabel: H.usePicture, canSelectMany: false, canSelectFiles: true, canSelectFolders: false,
      filters: { [H.pictures]: Object.keys(PICTURE_TYPES) } }))?.[0];
    if (!picked) return;
    if ((await vscode.workspace.fs.stat(picked)).size > MAX_PICTURE_BYTES) throw new UserInputError(H.pictureTooLarge);
    const bytes = await vscode.workspace.fs.readFile(picked);
    if (bytes.length > MAX_PICTURE_BYTES) throw new UserInputError(H.pictureTooLarge);
    const name = nameOf(picked);
    const uri = await this.host.files.createFile(lease, name, pictureMime(name, bytes), bytes);
    await this.setPicture(block, resourceFrom(uri).id, name);
  }

  /** The block's picture, and its description from the file's name when it has none: one change. */
  private setPicture(block: string, artifactId: string, fileName: string): Promise<void> {
    const description = fileName.replace(/\.[^.]+$/, "").replace(/[-_]+/g, " ").trim();
    return this.applyEdits(email => {
      const edits: EmailEdit[] = [{ kind: "field", block, field: "src", value: `artifact:${artifactId}` }];
      const alt = isLayoutEmail(email) ? findBlock(email, block)?.block.alt : undefined;
      if (description !== "" && (typeof alt !== "string" || alt.trim() === "")) edits.push({ kind: "field", block, field: "alt", value: description });
      return edits;
    });
  }

  // -- The organization ----------------------------------------------------------------------------------------

  /** The person's choice of organization: a Library file without labels is labelled with it, anything else keeps it with the page. */
  private async choose(id: unknown): Promise<void> {
    const chosen = this.choices.find(choice => choice.id === id);
    if (!chosen || !this.lease()) { await this.postContext(); return; }
    try {
      const target = await this.target();
      if (await chooseOrganization(target, chosen, this.session.ports) === "labelled") {
        if (this.opened) this.opened.accountIds = [chosen.id];
      } else {
        await this.keep(asViewState(this.stored()), chosen.id);
      }
    } catch (error) {
      await this.problem(error, OPERATION.organization);
      return;
    }
    this.known = undefined;
    await this.resolve();
  }

  // -- Acts ------------------------------------------------------------------------------------------------------

  private async act(act: unknown): Promise<void> {
    try {
      if (act === "connect") await this.connect();
      else if (act === "openSource") await this.host.show("source", this.document.uri);
      else if (act === "saveToMemQL") await this.saveToMemQL();
      else if (act === "publish") await this.publish();
    } catch (error) {
      await this.problem(error, OPERATION.view);
    } finally {
      await this.postBar(true);
    }
  }

  /** A MemQL file connects to its own cluster; a local email to the cluster the person chooses. */
  private async connect(): Promise<void> {
    try {
      if (this.domain !== undefined) await this.host.api.connect(this.domain);
      else await vscode.commands.executeCommand("memql.clusters.select");
    } catch (error) {
      await this.problem(error, OPERATION.connect);
    }
  }

  /** Publishes in place: nothing opens. A publish that fails is told in the cluster's words, and the email is looked at again. */
  private async publish(): Promise<void> {
    const organization = this.organization;
    if (this.busy || !organization || !this.lease() || this.document.uri.scheme !== "memql-file") return;
    this.busy = "publish";
    await this.postBar();
    let saved = -1;
    try {
      const opened = await this.host.load(this.document.uri);
      this.opened = opened;
      const result = await this.session.publisher.publish({
        // Read after the save: saving a template moves the revision it is published at.
        target: () => targetOf(opened),
        organization,
        email: () => readEmail(this.document.getText()),
        save: async () => {
          if (this.document.isDirty && !await this.document.save()) return false;
          saved = this.document.version;
          return true;
        },
        stillCurrent: () => !this.document.isDirty && this.document.version === saved,
      });
      if (opened.resource.kind === "templates" && opened.template) {
        // The template's next save expects the revision the publication left.
        opened.revision = result.revision;
        opened.template.status = result.status;
      } else this.template = result;
      this.failed = undefined;
    } catch (error) {
      const failed = { version: this.document.version, issues: error instanceof PublishBlocked ? [...error.issues] : [], settling: true };
      this.failed = failed;
      await this.problem(error, OPERATION.publish);
      void this.recheck(failed);
    } finally {
      this.busy = undefined;
    }
  }

  /**
   * After a publish that failed: the template is read again, then the email rendered again, and only then does the bar count
   * what is so (until then it counts an issue, so it offers nothing the email may not be ready for).
   */
  private async recheck(failed: NonNullable<EmailPanel["failed"]>): Promise<void> {
    const organization = this.organization;
    try {
      const target = await this.target();
      if (organization && target.kind === "file") this.template = await templateFor(target, organization, this.session.ports);
    } catch {
      // Unknown is not published: the bar offers Publish again rather than say it is done.
      this.template = undefined;
    }
    await this.render();
    failed.settling = false;
    await this.postBar();
  }

  private async saveLayout(name: unknown): Promise<void> {
    const organization = this.organization;
    try {
      if (this.busy || !organization || !this.lease() || typeof name !== "string") return;
      this.busy = "layout";
      await this.postBar();
      try {
        const target = await this.target();
        await this.session.publisher.saveLayout({ target, organization, email: readEmail(this.document.getText()), name, key: this.layoutKey(organization) });
        this.layoutName = name.trim();
        this.layoutSaved = true;
        if (this.layoutTimer !== undefined) clearTimeout(this.layoutTimer);
        this.layoutTimer = setTimeout(() => { this.layoutSaved = false; void this.postBar(); }, SAVED_FOR);
      } finally {
        this.busy = undefined;
      }
    } catch (error) {
      await this.problem(error, OPERATION.layout);
    } finally {
      await this.postBar(true);
    }
  }

  /**
   * A local email goes to MemQL as the person has it: saved, uploaded under its own name, labelled with its organization,
   * and opened in this column in place of the local one, whose tab closes. Publishing is from there.
   */
  private async saveToMemQL(): Promise<void> {
    const lease = this.lease(), organization = this.organization;
    if (this.busy || !lease || !organization || this.document.uri.scheme === "memql-file") return;
    this.busy = "copy";
    await this.postBar();
    try {
      if (this.document.isDirty && !await this.document.save()) throw new UserInputError(H.saveFirst);
      const name = nameOf(this.document.uri);
      const uri = await this.host.files.createFile(lease, name, "application/json", new TextEncoder().encode(this.document.getText()));
      const artifactId = resourceFrom(uri).id;
      try {
        await chooseOrganization({ kind: "file", domain: lease.domain, artifactId, fileId: "", name, labels: [] }, organization, this.session.ports);
      } catch (error) {
        // The email is in MemQL: unlabelled, it asks for its organization when it opens.
        void showProblem(error, OPERATION.organization);
      }
      await vscode.commands.executeCommand("vscode.openWith", vscode.Uri.parse(uri), VIEW_TYPE, { viewColumn: this.panel.viewColumn, preview: false });
      const local = this.document.uri.toString();
      const tabs = vscode.window.tabGroups.all.flatMap(group => group.tabs)
        .filter(tab => tab.input instanceof vscode.TabInputCustom && tab.input.viewType === VIEW_TYPE && tab.input.uri.toString() === local);
      if (tabs.length > 0) await vscode.window.tabGroups.close(tabs);
    } catch (error) {
      await this.problem(error, OPERATION.copy);
    } finally {
      this.busy = undefined;
    }
  }
}
