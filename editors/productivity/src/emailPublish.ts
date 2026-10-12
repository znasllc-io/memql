// What the email editor asks of the cluster about an email's organization, its publication and its
// layouts, apart from VS Code so each rule can be tested alone. The editor's host supplies the ports.
//
// ORGANIZATION. An email belongs to an organization, and the editor needs it before its first
// preview (emailRender asks for one), so Publish never has to ask. It is resolved from what the
// email already says, first answer wins: a template's own organization, the organization of the
// composition that wrote the file, the file's one organization label, the person's only active
// organization, and then a choice remembered with the page. Otherwise the person chooses once.
//
// PUBLICATION KEEPS NOTHING HERE. One template exists per organization and Library file, and the
// cluster finds it by the file (templateForSource, and campaignSaveTemplate's sourceArtifactId).
// Publishing therefore needs no local record of which template a file became: a response that was
// lost is recovered by asking the cluster again, and the first save of a file cannot make a second
// template. The only record kept is the id of a layout that is being created (see saveLayout).

import {
  buildCampaignSaveLayout, buildCampaignSaveTemplate, buildClientAccountsAll, buildCompositionForOutputFile,
  buildEmailLayouts, buildSetArtifactAccounts, buildTemplateById, buildTemplateForSource,
} from "@znasllc-io/memql-sdk-core/client";
import { EMAIL_COPY, fill } from "./emailCopy.js";
import { isLayoutEmail, type EmailDocument } from "./emailDocument.js";
import { UserInputError } from "./problems.js";

const C = EMAIL_COPY.editor;
const H = EMAIL_COPY.host;

// -- Types --------------------------------------------------------------------

export interface Organization { id: string; name: string }

/** What the open email is: a Library file, a template opened as a file, or a file on this computer. */
export type EmailTarget =
  | { kind: "file"; domain: string; artifactId: string; fileId: string; name: string; labels: readonly string[] }
  | { kind: "template"; domain: string; templateId: string; name: string; accountId: string; status: string; revision: string }
  | { kind: "local"; name: string };

export interface PublishPorts {
  /** Runs one query, builtin or mutation on the cluster the email belongs to. */
  execute(name: string, call: string): Promise<Record<string, unknown>[]>;
  /** The page's memento (workspaceState). */
  state: { get<T>(key: string): T | undefined; update(key: string, value: unknown): PromiseLike<void> };
}

/** A template as the cluster keeps it. `document` is the copy it stores, which sameEmail compares an open email with. */
export interface TemplateState { templateId: string; status: string; revision: string; document?: Record<string, unknown> }

const text = (row: Record<string, unknown> | undefined, key: string): string => {
  const value = row?.[key];
  return typeof value === "string" ? value : "";
};
const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);
const stringList = (value: unknown): string[] => Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : [];
/** What a document was when it was published: nothing the editor does afterwards can change it. */
const snapshot = (email: EmailDocument): Record<string, unknown> => JSON.parse(JSON.stringify(email)) as Record<string, unknown>;

// -- Organization -------------------------------------------------------------

/** The organizations the person can write emails for: the active ones, by name. */
export async function organizations(ports: PublishPorts): Promise<Organization[]> {
  const rows = await ports.execute("clientAccountsAll", buildClientAccountsAll({}));
  return rows
    .filter(row => text(row, "status") === "active" && text(row, "id") !== "")
    .map(row => ({ id: text(row, "id"), name: text(row, "name") || text(row, "id") }))
    .sort((a, b) => a.name.localeCompare(b.name) || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
}

/** The composition that wrote a file, if one did (the newest). Only a Library file has one, and it is looked up by the file, not its artifact. */
async function compositionOf(target: EmailTarget, ports: PublishPorts): Promise<Record<string, unknown> | undefined> {
  if (target.kind !== "file") return undefined;
  return (await ports.execute("compositionForOutputFile", buildCompositionForOutputFile({ fileId: target.fileId })))[0];
}

/**
 * The email's organization, or the choices to make one from. A template says its own; a file has the composition that wrote
 * it (an email composition carries exactly one organization) and its labels; a person with one active organization has no
 * choice to make. A remembered choice (the id the page kept) settles the rest and the list stays, so the choice can be
 * changed; without one, `organization` is undefined and `choices` are the active organizations by name.
 */
export async function resolveOrganization(target: EmailTarget, ports: PublishPorts, remembered?: string): Promise<{ organization?: Organization; choices: Organization[] }> {
  // Neither read waits for the other. Promise.all, not two awaits in a row: when one fails, the other's failure is still handled.
  const [choices, composition] = await Promise.all([organizations(ports), compositionOf(target, ports)]);
  const named = (id: string | undefined) => id === undefined ? undefined : choices.find(choice => choice.id === id);
  if (target.kind === "template") {
    // Where the person does not list it, the render says why it cannot be shown: the template's word stands.
    return { organization: named(target.accountId) ?? { id: target.accountId, name: target.accountId }, choices: [] };
  }
  if (target.kind === "file") {
    const written = [...new Set(stringList(composition?.accountIds))];
    const composed = named(written.length === 1 ? written[0] : undefined);
    if (composed) return { organization: composed, choices: [] };
    const labels = [...new Set(target.labels)];
    const labelled = named(labels.length === 1 ? labels[0] : undefined);
    if (labelled) return { organization: labelled, choices: [] };
  }
  if (choices.length === 1) return { organization: choices[0], choices: [] };
  return { organization: named(remembered), choices };
}

/**
 * Records the person's choice of organization. A Library file with no labels gets the choice as its label, so it knows its
 * organization from then on; anything else (a file with several labels, a file on this computer) is only remembered by the
 * page, which keeps the choice with its state.
 */
export async function chooseOrganization(target: EmailTarget, organization: Organization, ports: PublishPorts): Promise<"labelled" | "remembered"> {
  if (target.kind !== "file" || target.labels.length > 0) return "remembered";
  await ports.execute("setArtifactAccounts", buildSetArtifactAccounts({ artifactId: target.artifactId, accountIds: [organization.id] }));
  return "labelled";
}

/**
 * The template the email has been published as, if it has. A template resource is its own template. A file's is the one
 * published from it: found by the file (which answers its id and revision and nothing of its content), then read whole by id
 * for its status and the document it holds.
 */
export async function templateFor(target: EmailTarget, organization: Organization, ports: PublishPorts): Promise<TemplateState | undefined> {
  if (target.kind === "template") return { templateId: target.templateId, status: target.status, revision: target.revision };
  if (target.kind === "local") return undefined;
  const found = (await ports.execute("templateForSource", buildTemplateForSource({ accountId: organization.id, sourceArtifactId: target.artifactId })))[0];
  if (found === undefined) return undefined;
  const templateId = text(found, "id");
  if (!templateId) throw new Error(NO_TEMPLATE_ID);
  const row = (await ports.execute("templateById", buildTemplateById({ templateId })))[0];
  if (row === undefined) return undefined;
  return { templateId, status: text(row, "status"), revision: text(row, "createdAt"), ...(isRecord(row.document) ? { document: row.document } : {}) };
}

// -- The action bar -----------------------------------------------------------

/** The acts the bar can offer. The page keeps the same list (emailView.ts) and a test holds the two together. */
export type BarAct = "connect" | "openSource" | "saveToMemQL" | "saveLayout" | "publish";

export interface BarInput {
  connected: boolean; domain?: string; invalid: boolean; local: boolean; organization?: Organization;
  mode: "layout" | "custom"; dirty: boolean; issues: number; template?: TemplateState; unchanged: boolean;
  busy?: "publish" | "layout" | "copy"; layoutSaved?: boolean;
}

/**
 * What the email's action bar says and offers: the state in words and the acts that are legal in it, and no others.
 * The first row that fits wins:
 *   busy, not connected, unreadable, no organization, on this computer, issues, unsaved changes,
 *   published and unchanged, published and changed, otherwise a draft.
 * Save as layout is offered only for a layout (custom HTML cannot be one). A layout that has just been saved replaces the
 * state of the last five rows, which is what the host shows for a few seconds.
 */
export function editorBar(input: BarInput): { state: string; acts: BarAct[] } {
  if (input.busy === "publish") return { state: C.publishing, acts: [] };
  if (input.busy === "layout") return { state: C.savingLayout, acts: [] };
  if (input.busy === "copy") return { state: C.savingCopy, acts: [] };
  if (!input.connected) return { state: input.local || !input.domain ? C.notConnected : fill(C.notConnectedTo, { domain: input.domain }), acts: ["connect"] };
  if (input.invalid) return { state: C.unreadable, acts: ["openSource"] };
  if (!input.organization) return { state: C.chooseOrganization, acts: [] };
  if (input.local) return { state: C.local, acts: ["saveToMemQL"] };
  const layout: BarAct[] = input.mode === "layout" ? ["saveLayout"] : [];
  const ready = input.template?.status === "ready";
  let state: string;
  let acts: BarAct[];
  if (input.issues > 0) { state = input.issues === 1 ? C.issuesOne : fill(C.issuesMany, { count: input.issues }); acts = [...layout]; }
  else if (input.dirty) { state = C.unsaved; acts = [...layout, "publish"]; }
  else if (ready && input.unchanged) { state = fill(C.published, { organization: input.organization.name }); acts = [...layout]; }
  else if (ready) { state = C.changed; acts = [...layout, "publish"]; }
  else { state = C.draft; acts = [...layout, "publish"]; }
  return { state: input.layoutSaved ? C.layoutSaved : state, acts };
}

// -- Publishing and layouts ---------------------------------------------------

export interface PublishInput {
  /**
   * Read once to name the work, and again after save(): saving a template resource moves its revision (Documents.save
   * writes a draft), and the revision published is the one the save left.
   */
  target: () => EmailTarget;
  organization: Organization;
  email: () => EmailDocument;
  /** Saves a dirty buffer. Answers false when it could not. */
  save: () => PromiseLike<boolean>;
  /** Whether the text is still the text that was saved: false once the person has typed since. */
  stillCurrent: () => boolean;
}

/** `key` names the email for the record of a layout that is being created: the same key retries the same layout. */
export interface SaveLayoutInput { target: EmailTarget; organization: Organization; email: EmailDocument; name: string; key: string }

type FileTarget = Extract<EmailTarget, { kind: "file" }>;
type TemplateTarget = Extract<EmailTarget, { kind: "template" }>;

const LOCAL_PUBLISH = "A local email is published from MemQL. Save it to MemQL first.";
const NO_TEMPLATE_ID = "The cluster did not return the template id.";
const NO_TEMPLATE_REVISION = "The cluster did not return the template revision.";
const NO_LAYOUT_REVISION = "The cluster did not return the layout revision.";
const SAVE_UNCONFIRMED = "The cluster did not confirm the saved email. Retry to recover it.";
const PUBLISH_UNCONFIRMED = "The cluster did not confirm that the email is published. Retry to recover it.";
const LAYOUT_UNCONFIRMED = "The cluster did not confirm the saved layout. Retry to recover it.";

/** The page memento key for the id of a layout that is being created; the host's key for the email is appended. */
const PENDING_LAYOUT = "memql.emailLayoutPending:";
/** An id the cluster accepts for a layout (the same set it accepts for a template). */
const ID = /^[A-Za-z0-9_-]{1,160}$/;
const FILE_SUFFIX = /\.email\.json$/i;
/** A layout name is one line of at most this many characters, counted as the cluster counts them (code points). */
const MAX_LAYOUT_NAME = 200;
const CONTROL = /[\u0000-\u001f\u007f-\u009f]/;

/** The template made from a file is named after the file, without the suffix. */
function templateName(fileName: string): string {
  return fileName.replace(FILE_SUFFIX, "").trim() || fileName.trim();
}

/** What campaignSaveTemplate answered, or an error when it did not say that the email was saved. */
function savedReceipt(rows: Record<string, unknown>[]): { templateId: string; revision: string; issues: number } {
  const row = rows[0];
  const templateId = text(row, "templateId"), revision = text(row, "revision");
  if (row?.saved !== true || !templateId || !revision) throw new Error(SAVE_UNCONFIRMED);
  return { templateId, revision, issues: Array.isArray(row.issues) ? row.issues.length : 0 };
}

function publishedReceipt(rows: Record<string, unknown>[]): { templateId: string; revision: string; status: string } {
  const row = rows[0];
  const templateId = text(row, "templateId"), revision = text(row, "revision"), status = text(row, "status");
  if (row?.saved !== true || !templateId || !revision || !status) throw new Error(PUBLISH_UNCONFIRMED);
  return { templateId, revision, status };
}

export class EmailPublisher {
  /** The work that is running, by what it works on: a second request for the same thing is the first one. */
  private readonly flights = new Map<string, Promise<unknown>>();
  constructor(private readonly ports: PublishPorts) {}

  /** One run of `work` per key at a time. The key is free again when the run ends, whether it worked or not. */
  private singleFlight<T>(key: string, work: () => Promise<T>): Promise<T> {
    const running = this.flights.get(key);
    if (running) return running as Promise<T>;
    const flight = work().finally(() => { this.flights.delete(key); });
    this.flights.set(key, flight);
    return flight;
  }

  /**
   * Publishes the email as a template of the organization, and answers what is now published.
   *
   * One publication runs per email at a time, so a double click is one publication. The buffer is saved first. A file is
   * then saved to its template as a draft: the template already published from the file is looked up (its revision is what
   * the save expects; none means this save creates the template), and the draft is how the cluster finds what keeps the
   * email from being sent. Those issues stop the publication, and so does an edit made since; otherwise the template is
   * published at the revision the save made. A template resource is published at the revision it was opened or last saved at.
   * Nothing is remembered between attempts: asking the cluster again for the file's template is what recovers a response
   * that was lost.
   */
  async publish(input: PublishInput): Promise<TemplateState> {
    const first = input.target();
    if (first.kind === "local") throw new Error(LOCAL_PUBLISH);
    const key = first.kind === "file" ? ["file", first.domain, first.artifactId, input.organization.id] : ["template", first.domain, first.templateId];
    return this.singleFlight(JSON.stringify(key), () => this.publishNow(input));
  }

  private async publishNow(input: PublishInput): Promise<TemplateState> {
    if (!await input.save()) throw new UserInputError(H.saveBeforePublish);
    const target = input.target();
    const email = input.email();
    if (target.kind === "file") return this.publishFile(input, target, email);
    if (target.kind === "template") return this.publishTemplate(input, target, email);
    throw new Error(LOCAL_PUBLISH);
  }

  private async publishFile(input: PublishInput, target: FileTarget, email: EmailDocument): Promise<TemplateState> {
    const accountId = input.organization.id;
    const name = templateName(target.name);
    const content = { ...email };
    const found = (await this.ports.execute("templateForSource", buildTemplateForSource({ accountId, sourceArtifactId: target.artifactId })))[0];
    const known = text(found, "createdAt");
    if (found !== undefined && !known) throw new Error(NO_TEMPLATE_REVISION);
    // No templateId: the cluster knows which template this file is, and makes it the first time. A save that is repeated
    // after a lost answer finds the template that save made, because it is looked up again.
    const draft = savedReceipt(await this.ports.execute("campaignSaveTemplate", buildCampaignSaveTemplate({
      accountId, name, content, sourceArtifactId: target.artifactId, expectedRevision: known, action: "save",
    })));
    if (draft.issues > 0) throw new UserInputError(H.fixIssues);
    if (!input.stillCurrent()) throw new UserInputError(H.changedWhilePublishing);
    const live = publishedReceipt(await this.ports.execute("campaignSaveTemplate", buildCampaignSaveTemplate({
      templateId: draft.templateId, accountId, name, content, expectedRevision: draft.revision, action: "publish",
    })));
    return { templateId: live.templateId, status: live.status, revision: live.revision, document: snapshot(email) };
  }

  private async publishTemplate(input: PublishInput, target: TemplateTarget, email: EmailDocument): Promise<TemplateState> {
    if (!input.stillCurrent()) throw new UserInputError(H.changedWhilePublishing);
    // The template's own organization and name: the target is the template, whatever organization the editor shows.
    const live = publishedReceipt(await this.ports.execute("campaignSaveTemplate", buildCampaignSaveTemplate({
      templateId: target.templateId, accountId: target.accountId, name: target.name, content: { ...email }, expectedRevision: target.revision, action: "publish",
    })));
    target.status = live.status;
    target.revision = live.revision;
    return { templateId: live.templateId, status: live.status, revision: live.revision, document: snapshot(email) };
  }

  /**
   * Saves the email as one of the organization's layouts. Only a layout can be one (custom HTML cannot), under a name of one
   * line and up to 200 characters. The layout this email already made is updated, at the revision it was listed at: it is found
   * by the file it was saved from, or by the id kept for it. A new layout is created under an id that is kept until the cluster
   * has answered, so a response that was lost retries the same layout and never makes a second one, and an email changed in
   * the meantime updates the layout the lost creation made instead of being refused for it. `key` names the email for that record.
   */
  async saveLayout(input: SaveLayoutInput): Promise<{ layoutId: string; revision: string }> {
    if (!isLayoutEmail(input.email)) throw new UserInputError(H.noLayoutForHtml);
    const name = input.name.trim();
    if (name === "" || CONTROL.test(name) || [...name].length > MAX_LAYOUT_NAME) throw new UserInputError(H.layoutNameInvalid);
    return this.singleFlight(JSON.stringify(["layout", input.key]), () => this.saveLayoutNow(input, name));
  }

  private async saveLayoutNow(input: SaveLayoutInput, name: string): Promise<{ layoutId: string; revision: string }> {
    const { target, organization } = input;
    const pending = PENDING_LAYOUT + input.key;
    const document = { ...input.email };
    const source = target.kind === "file" ? target.artifactId : undefined;
    const kept = this.ports.state.get<string>(pending);
    const keptId = typeof kept === "string" && ID.test(kept) ? kept : undefined;
    // The layout this email already made is found by its file or, for an email that has none (a template opened as a file), by the
    // id kept for it: a creation whose answer was lost may have reached the cluster, and a save that named no revision could never
    // update it. The id in a row is the bare layoutId, never the opaque one.
    const existing = source === undefined && keptId === undefined ? undefined
      : (await this.ports.execute("emailLayouts", buildEmailLayouts({ accountId: organization.id })))
        .find(row => text(row, "layoutId") !== "" && (text(row, "layoutId") === keptId || (source !== undefined && text(row, "sourceArtifactId") === source)));
    let call: string;
    if (existing) {
      const revision = text(existing, "revision");
      if (!revision) throw new Error(NO_LAYOUT_REVISION);
      call = buildCampaignSaveLayout({
        layoutId: text(existing, "layoutId"), accountId: organization.id, name, description: text(existing, "description"),
        document, sourceArtifactId: source, expectedRevision: revision, action: "save",
      });
    } else {
      const layoutId = keptId ?? globalThis.crypto.randomUUID();
      if (layoutId !== kept) await this.ports.state.update(pending, layoutId);
      call = buildCampaignSaveLayout({
        layoutId, accountId: organization.id, name, document, ...(source === undefined ? {} : { sourceArtifactId: source }), expectedRevision: "", action: "save",
      });
    }
    const row = (await this.ports.execute("campaignSaveLayout", call))[0];
    const layoutId = text(row, "layoutId"), revision = text(row, "revision");
    if (row?.saved !== true || !layoutId || !revision) throw new Error(LAYOUT_UNCONFIRMED);
    if (this.ports.state.get(pending) !== undefined) await this.ports.state.update(pending, undefined);
    return { layoutId, revision };
  }
}
