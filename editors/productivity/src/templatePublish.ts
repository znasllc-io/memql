import { UserInputError } from "./problems.js";
import * as vscode from "vscode";
import { buildClientAccountsAll, buildCampaignSaveTemplate } from "@znasllc-io/memql-sdk-core/client";
import type { EditorConnectionAPI } from "../../vscode/src/connection/api.js";
import type { OpenDocument } from "./documents.js";
import { readTemplate } from "./templates.js";

const text = (row: Record<string, unknown>, key: string) => typeof row[key] === "string" ? row[key] as string : "";
interface Publication { domain: string; templateId: string; accountId: string; name: string; revision: string }

export class TemplatePublisher {
  private readonly pending = new Map<string, Promise<void>>();
  constructor(private readonly api: EditorConnectionAPI, private readonly context: vscode.ExtensionContext,
    private readonly load: (uri: vscode.Uri) => Promise<OpenDocument>) {}
  publish(document: vscode.TextDocument): Promise<void> {
    const key = document.uri.toString();
    const existing = this.pending.get(key);
    if (existing) return existing;
    const operation = this.publishDocument(document).finally(() => this.pending.delete(key));
    this.pending.set(key, operation);
    return operation;
  }
  private async publishDocument(document: vscode.TextDocument): Promise<void> {
    if (document.isDirty && !await document.save()) throw new UserInputError("Save this email before publishing it.");
    const content = readTemplate(document.getText());
    const version = document.version;
    const source = document.uri.scheme === "memql-file" ? await this.load(document.uri) : undefined;
    const lease = source?.lease ?? this.api.current();
    if (!lease) throw new UserInputError("Connect and sign in with MemQL first.");
    let publication: Publication | undefined = source?.template ? { domain: lease.domain, templateId: source.sourceId,
      accountId: source.template.accountId, name: source.template.name, revision: source.revision || "" } : undefined;
    const key = `memql.campaignPublication:${document.uri.toString()}`;
    if (!publication) {
      publication = this.context.workspaceState.get<Publication>(key);
      if (publication && publication.domain !== lease.domain) throw new UserInputError("This document's draft belongs to a different cluster. Reconnect to that cluster.");
      if (!publication) {
        const accounts = await this.api.execute(lease, "clientAccountsAll", buildClientAccountsAll({}));
        const organization = await vscode.window.showQuickPick(accounts.map(row => ({ label: text(row,"name") || text(row,"id"), id: text(row,"id") })),
          { title: "Publish email · Organization", placeHolder: "Choose the organization this email will represent", ignoreFocusOut: true });
        if (!organization) return;
        const name = await vscode.window.showInputBox({ title: "Publish email · Name", value: source?.resource.name.replace(/\.email\.json$/i, "") || "Email template", ignoreFocusOut: true,
          // The name is at most 200 bytes; the person reads only that it is missing or too long.
          validateInput: value => !value.trim() ? "Enter a name." : new TextEncoder().encode(value.trim()).length > 200 ? "Enter a shorter name." : /[\u0000-\u001f\u007f-\u009f]/.test(value) ? "Enter a name without control characters." : undefined });
        if (!name) return;
        publication = { domain: lease.domain, templateId: globalThis.crypto.randomUUID(), accountId: organization.id, name, revision: "" };
        // Persist BEFORE the request, so a lost response retries the same ID.
        await this.context.workspaceState.update(key, publication);
      }
    }
    const choice = await vscode.window.showQuickPick(["Save draft", "Publish template for campaigns"], {
      title: publication.name, placeHolder: "Publishing makes this reviewed copy available to campaigns. It does not send mail.", ignoreFocusOut: true });
    if (!choice) return;
    if (document.version !== version || document.isDirty) throw new UserInputError("The email changed during review. Save and review it again.");
    let revision = publication.revision;
    if (!source?.template || choice === "Save draft") {
      const result = (await this.api.execute(lease, "campaignSaveTemplate", buildCampaignSaveTemplate({ ...publication, content: { ...content }, expectedRevision: revision, action: "save" })))[0];
      if (!result?.saved || !text(result,"revision")) throw new Error("Campaigns did not confirm the draft. Retry to recover its receipt.");
      revision = text(result,"revision");
      if (source?.template) { source.revision = revision; source.template.status = "draft"; }
      else await this.context.workspaceState.update(key, { ...publication, revision });
    }
    if (choice === "Publish template for campaigns") {
      if (document.version !== version || document.isDirty) throw new UserInputError("The email changed. Its saved draft is available, but review again before publishing.");
      const result = (await this.api.execute(lease, "campaignSaveTemplate", buildCampaignSaveTemplate({ ...publication, content: { ...content }, expectedRevision: revision, action: "publish" })))[0];
      if (!result?.saved || !text(result,"revision")) throw new Error("Campaigns did not confirm publication. Retry to recover its receipt.");
      revision = text(result,"revision");
      if (source?.template) { source.revision = revision; source.template.status = "ready"; }
      else await this.context.workspaceState.update(key, { ...publication, revision });
    }
    const uri = vscode.Uri.from({ scheme: "memql-file", authority: lease.domain, path: `/templates/${publication.templateId}/${publication.name.replace(/[\\/]/g,"_")}.email.json` });
    await vscode.commands.executeCommand("vscode.openWith", uri, "memql.productivity.email", { preview: false });
    void vscode.window.showInformationMessage(choice === "Publish template for campaigns" ? "Template published. Choose it in this organization's campaigns." : "Draft saved.");
  }
}
