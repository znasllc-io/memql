import { useEffect, useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { FileText } from "lucide-react";
import { AccountChip, AccountPicker } from "../accounts/AccountPicker";
import { useAccountOptions } from "../accounts/tie";
import { organizationChosen, useDefaultOrganization } from "../accounts/organization";
import { useSession } from "../../chrome/access";
import { Button, EmptyState, Field, Head, Input, LiveList, Notice, Panel, RecordRow } from "../../kit";
import { AddButton } from "../../kit/AddButton";
import { listCount } from "../../kit/RecordRow";
import { useLiveView } from "../../live/liveView";
import { editorTemplateURL, readEditorPreference } from "../../items/editorPreference";
import { browserHandoffPorts, openHandoff, VSCODE_NO_ANSWER_MESSAGE } from "../../items/vscode";
import type { CampaignWrites } from "./actions";
import { templateFingerprint, templateFromRow, templateIsArchived, templateName, type TemplateRow } from "./rows";
import type { CampaignFeeds } from "./useCampaigns";

// Campaigns organizes reusable templates. File content is edited and previewed
// by Productivity Tools, using the same revision-checked backend on both hosts.
export function TemplatesSection({ feeds, writes, showFiled }: { feeds: CampaignFeeds; writes: CampaignWrites; showFiled: boolean }) {
  const { config } = useSession();
  const accounts = useAccountOptions();
  const [openId, setOpenId] = useState("");
  const [adding, setAdding] = useState(false);
  const [notice, setNotice] = useState("");
  const source = useLiveView<Row, TemplateRow>(feeds.templates.source, `filed:${showFiled}`, rows => {
    const templates = rows.map(templateFromRow).filter(t => t.id !== "");
    return showFiled ? templates : templates.filter(t => !templateIsArchived(t));
  });
  const open = useMemo(() => source?.snapshot.rows.find(t => t.id === openId), [source, source?.snapshot, openId]);
  const edit = (id: string, name: string) => openHandoff(editorTemplateURL(config.domain, id, name), () => setNotice(VSCODE_NO_ANSWER_MESSAGE));
  if (adding) return <div className="os-app-stack">
    <Head title="New template" back={{ label: "Templates", onSelect: () => setAdding(false) }} />
    <NewTemplate writes={writes} onCreated={(id, name, tab) => { setAdding(false); setOpenId(id); if (tab) tab.navigate(editorTemplateURL(config.domain,id,name)); else edit(id,name); }} />
  </div>;
  if (open) return <div className="os-app-stack" data-os-page-context={JSON.stringify({ page: "Templates", name: templateName(open), id: open.id })}>
    <Head title={templateName(open)} back={{ label: "Templates", onSelect: () => setOpenId("") }}>
      <Button onClick={() => edit(open.id, templateName(open))}>Open in editor</Button>
    </Head>
    <Panel label="Template">
      <p>{open.subject}</p>
      <AccountChip name={accounts.find(account => account.id === open.accountId)?.name || open.accountId} />
      <p className="os-caption">{open.status === "ready" ? "Ready to use in campaigns" : open.status === "archived" ? "Archived" : "Draft · review and publish in the editor"}</p>
      <p className="os-caption">View the email, edit its source, or create a new design from examples in Productivity Tools.</p>
      <Button disabled={writes.updateTemplate.busy} onClick={() => void writes.updateTemplate.update(open.id, { ...open, expectedRevision: open.createdAt, status: open.status === "archived" ? "draft" : "archived" })}>
        {open.status === "archived" ? "Restore draft" : "Archive"}
      </Button>
      {writes.updateTemplate.error ? <Notice tone="error" sentence={writes.updateTemplate.error} /> : null}
    </Panel>
    {notice ? <Notice sentence={notice} /> : null}
  </div>;
  return <div className="os-app-stack">
    <Head title="Templates" meta={listCount(source?.snapshot)}><AddButton onClick={() => setAdding(true)} label="New template" /></Head>
    {feeds.templates.snapshot.error ? <Notice tone="error" sentence="This cluster did not return your templates."><Button onClick={feeds.templates.reseed}>Try again</Button></Notice> : null}
    <LiveList<TemplateRow> key={`templates:${showFiled}`} source={source} rowId={t => t.id} fingerprint={templateFingerprint} label="Your templates"
      emptyText="No templates yet."
      emptyContent={<EmptyState icon={FileText} title="No templates yet">Create a template and shape it in Productivity Tools.</EmptyState>}
      renderRow={(template, tick) => <RecordRow icon={<FileText size={16} aria-hidden />} name={templateName(template)}
        current={!templateIsArchived(template)} dim={templateIsArchived(template)} open={false} onOpen={() => setOpenId(template.id)}
        stateExtra={tick ? <span className="os-change-tick">{tick}</span> : null} state={template.status} secondary={template.subject} />}
    />
  </div>;
}

export function NewTemplate({ writes, onCreated, initialAccountId = "" }: { initialAccountId?: string; writes: CampaignWrites; onCreated: (id: string, name: string, tab?: { navigate(url: string): void; close(): void }) => void }) {
  const [name, setName] = useState("");
  const [accountId, setAccountId] = useState(initialAccountId);
  const accounts = useAccountOptions();
  const defaultAccount = useDefaultOrganization(accounts);
  useEffect(() => { if (!accountId && defaultAccount) setAccountId(defaultAccount); }, [accountId, defaultAccount]);
  return <Panel label="New template">
    <Field label="Name"><Input id="template-name" label="Name" value={name} onChange={setName} placeholder="Welcome email" /></Field>
    <AccountPicker id="template-organization" label="Organization" required value={accountId} onChange={setAccountId} accounts={accounts} />
    <Button disabled={!name.trim() || !organizationChosen(accounts, accountId) || writes.createTemplate.busy} onClick={async () => {
      // Reserve during the click: a saved record's async response must not be
      // lost to the browser's popup blocker.
      const tab = readEditorPreference() === "browser" ? browserHandoffPorts.reserve?.() : undefined;
      try {
        const id = await writes.createTemplate.create({ name, accountId, subject: "Your subject", textBody: "Your message", htmlBody: "<p>Your message</p>", status: "draft" });
        if (id) onCreated(id, name, tab); else tab?.close();
      } catch (error) { tab?.close(); throw error; }
    }}>Create template</Button>
    {writes.createTemplate.error ? <Notice tone="error" sentence={writes.createTemplate.error} /> : null}
  </Panel>;
}
