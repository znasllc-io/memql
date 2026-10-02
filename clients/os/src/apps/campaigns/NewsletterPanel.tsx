import { useState } from "react";
import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";
import { AttentionDestination, AttentionMarker } from "../../attention/Attention";
import { AddButton } from "../../kit/AddButton";
import { Button, Caption, Check, Field, Input, Notice, Panel, RecordList, RecordRow, Select, Subhead, formatMoment } from "../../kit";
import { RecordListSkeleton } from "../../kit/RecordListSkeleton";
import { flatten } from "../../kit/rows";
import { useLiveCollection, feedIsBehind } from "../../live/useLiveCollection";
import { useSites } from "../deployables/useSites";
import { useConfigureNewsletter, useRetryNewsletterWelcome } from "./actions";
import { useNewsletterWelcomes, type CampaignFeeds } from "./useCampaigns";

const text = (row: Record<string, unknown>, key: string) => typeof row[key] === "string" ? row[key] as string : "";
const escapeHTML = (value: string) => value.replaceAll("&", "&amp;").replaceAll('"', "&quot;").replaceAll("<", "&lt;").replaceAll(">", "&gt;");
export function newsletterFormHTML(row: Record<string, unknown>): string {
  return `<form method="post" action="/_memql/forms/campaigns/subscribe">\n  <label>Email <input type="email" name="email" required maxlength="320" autocomplete="email"></label>\n  <label>Name <input name="displayName" maxlength="120" autocomplete="name"></label>\n  <label><input type="checkbox" name="consent" value="yes" required> ${escapeHTML(text(row, "consentText"))}</label>\n  <input type="hidden" name="consentRevision" value="${escapeHTML(text(row, "createdAt"))}">\n  <button type="submit">Subscribe</button>\n</form>`;
}

export function NewsletterPanel({ audienceId, accountId, resources }: { audienceId: string; accountId: string; resources: Pick<CampaignFeeds, "templates" | "senders"> }) {
  const sites = useSites();
  const feed = useLiveCollection<Row>(`campaigns:newsletters:${audienceId}`, connection => ({
    concept: "v1:campaigns:newsletterBinding",
    seed: async (_cursor, signal) => ({ rows: (await connection.query.newslettersForAudience({ audienceId }, { signal })).rows(), nextCursor: "" }),
    reread: async (id, signal) => (await getRowByConceptAndId(connection.query, "v1:campaigns:newsletterBinding", id, { signal })) as Row | null,
    inScope: row => text(flatten(row), "audienceId") === audienceId,
    paged: false,
  }));
  const write = useConfigureNewsletter();
  const retry = useRetryNewsletterWelcome();
  const welcomes = useNewsletterWelcomes(audienceId);
  const [editing, setEditing] = useState<Record<string, unknown> | null>(null);
  const [siteId, setSiteId] = useState("");
  const [templateId, setTemplateId] = useState("");
  const [senderIdentityId, setSender] = useState("");
  const [consentText, setConsent] = useState("Email me your newsletter.");
  const [enabled, setEnabled] = useState(true);
  const [formFor, setFormFor] = useState("");
  const [copied, setCopied] = useState(false);
  const [copyError, setCopyError] = useState("");
  const rows = feed.snapshot.rows.map(flatten);
  const siteRows = sites.snapshot.rows.map(flatten).filter(row => row.accountId === accountId && !row.systemOwned && row.status !== "disabled");
  const templateRows = resources.templates.snapshot.rows.map(flatten).filter(row => row.accountId === accountId && (row.status === "ready" || row.id === templateId));
  const senderRows = resources.senders.snapshot.rows.map(flatten).filter(row => row.accountId === accountId && (row.status !== "disabled" || row.id === senderIdentityId));
  const unavailable = [sites, resources.templates, resources.senders].filter(value => !!value.snapshot.error || feedIsBehind(value.snapshot.state));
  const blocked = [feed, sites, resources.templates, resources.senders].some(value => value.snapshot.state === "seeding" || feedIsBehind(value.snapshot.state) || !!value.snapshot.error);
  const edit = (row: Record<string, unknown>) => {
    write.reset(); setEditing(row); setSiteId(text(row, "siteId")); setTemplateId(text(row, "templateId")); setSender(text(row, "senderIdentityId"));
    setConsent(text(row, "consentText") || "Email me your newsletter."); setEnabled(row.enabled !== false); setFormFor("");
  };
  const save = async () => {
    if (await write.call({ siteId, audienceId, templateId, senderIdentityId, consentText, enabled, expectedRevision: editing ? text(editing, "createdAt") || undefined : undefined })) {
      setEditing(null); feed.reseed();
    }
  };
  const siteName = (id: string) => {
    const row = sites.snapshot.rows.map(flatten).find(row => row.id === id);
    return row ? text(row, "title") || text(row, "hostname") : "Unavailable deployable";
  };
  const copying = async (row: Record<string, unknown>) => {
    setCopied(false); setCopyError("");
    try { await navigator.clipboard.writeText(newsletterFormHTML(row)); setCopied(true); }
    catch { setCopyError("The form could not be copied. Allow clipboard access and try again."); }
  };
  return <Panel label="Storefront signups">
    <div className="os-campaign-detail-head"><Subhead>Storefront signups</Subhead>
      {!editing && !formFor ? <span><AddButton label="Connect a signup form" onClick={() => edit({})} disabled={blocked || write.busy} /><AttentionMarker appId="campaigns" sectionId="audiences" target="newsletter" /></span> : null}
    </div>
    {feed.snapshot.state === "seeding" && !rows.length ? <RecordListSkeleton rows={1} label="Newsletter connections" /> : null}
    {feed.snapshot.error || feedIsBehind(feed.snapshot.state) ? <Notice tone="warn" sentence="Newsletter connections could not be confirmed."><Button onClick={feed.reseed}>Try again</Button></Notice> : null}
    {unavailable.length ? <Notice tone="warn" sentence="Signup resources could not be confirmed."><Button onClick={() => unavailable.forEach(value => value.reseed())}>Try again</Button></Notice> : null}
    {!rows.length && !editing && feed.snapshot.state === "live" ? <Caption>Connect a deployable to collect subscribers and send a welcome email.</Caption> : null}
    {rows.length && !editing && !formFor ? <RecordList>{rows.map(row => <RecordRow key={text(row,"id")} name={siteName(text(row,"siteId"))} state={row.enabled ? "Accepting signups" : "Off"} onOpen={() => edit(row)} actions={<Button onClick={() => { setFormFor(text(row,"id")); setCopied(false); setCopyError(""); }}>Get form</Button>} />)}</RecordList> : null}
    {editing ? <AttentionDestination appId="campaigns" sectionId="audiences" target="newsletter">
      <div className="os-form">
        <Field label="Deployable">{editing.id ? <Caption>{siteName(siteId)}</Caption> : <Select id={`newsletter-site-${audienceId}`} label="Newsletter deployable" value={siteId} onChange={setSiteId}>
          <option value="">Select</option>{siteRows.filter(row => row.id === siteId || !rows.some(binding => binding.siteId === row.id)).map(row => <option key={text(row,"id")} value={text(row,"id")}>{text(row,"title") || text(row,"hostname")}</option>)}
        </Select>}</Field>
        <Field label="Welcome template"><Select id={`newsletter-template-${audienceId}`} label="Welcome template" value={templateId} onChange={setTemplateId}><option value="">Select a published template</option>{templateRows.map(row => <option key={text(row,"id")} value={text(row,"id")}>{text(row,"name")}{row.status !== "ready" ? " · Not published" : ""}</option>)}{templateId && !templateRows.some(row => row.id === templateId) ? <option value={templateId}>Unavailable template</option> : null}</Select></Field>
        <Field label="Sender"><Select id={`newsletter-sender-${audienceId}`} label="Welcome sender" value={senderIdentityId} onChange={setSender}><option value="">Select</option>{senderRows.map(row => <option key={text(row,"id")} value={text(row,"id")}>{text(row,"fromName") || text(row,"address")}{row.status === "disabled" ? " · Disabled" : ""}</option>)}{senderIdentityId && !senderRows.some(row => row.id === senderIdentityId) ? <option value={senderIdentityId}>Unavailable sender</option> : null}</Select></Field>
        <Field label="Consent sentence"><Input id={`newsletter-consent-${audienceId}`} value={consentText} onChange={setConsent} label="Signup consent sentence" /></Field>
        <Check checked={enabled} onChange={setEnabled}>Accept signups and send a welcome</Check>
        <Caption>Enabling also turns on this deployable’s public forms. Existing opt-outs stay blocked.</Caption>
        <div className="os-panel-actions"><Button disabled={write.busy} onClick={() => setEditing(null)}>Cancel</Button><Button busy={write.busy} disabled={blocked || !siteId || !templateId || !senderIdentityId || consentText.trim().length < 10} onClick={() => void save()}>Save</Button></div>
      </div>
    </AttentionDestination> : null}
    {write.error ? <Notice tone="error" sentence="The newsletter was not changed." detail={write.error} /> : null}
    {formFor && rows.find(row => row.id === formFor) ? <div className="os-form">
      <Caption>Add this form to the deployable in VS Code. Include /newsletter/thank-you and /newsletter/problem pages. Replace the form after changing this connection.</Caption>
      <div className="os-panel-actions"><Button onClick={() => setFormFor("")}>Done</Button><Button onClick={() => void copying(rows.find(row => row.id === formFor)!)}>{copied ? "Copied" : "Copy form"}</Button></div>
      {copyError ? <Notice tone="error" sentence={copyError} /> : null}
    </div> : null}
    {rows.length && !editing && !formFor ? <>
      <Subhead>Recent welcomes</Subhead>
      {welcomes.state === "loading" && !welcomes.value.length ? <RecordListSkeleton rows={2} label="Recent welcome messages" /> : null}
      {welcomes.error ? <Notice tone="error" sentence="Welcome outcomes could not be read." detail={welcomes.error} /> : null}
      <RecordList>{welcomes.value.map(flatten).map(row => <RecordRow key={text(row,"id")} name={text(row,"displayName") || text(row,"email") || "Subscriber"} secondary={text(row,"lastError") || formatMoment(text(row,"requestedAt"))} state={text(row,"status")} actions={row.status === "blocked" ? <Button busy={retry.busy} onClick={() => void retry.call({ signupId: text(row,"id"), expectedRevision: text(row,"createdAt") }).then(ok => { if (ok) welcomes.reload(); })}>Check again</Button> : undefined} />)}</RecordList>
      {welcomes.state === "ready" && !welcomes.value.length ? <Caption>No welcomes yet.</Caption> : null}
      <Caption>Latest 50 · Updates automatically</Caption>
      {retry.error ? <Notice tone="error" sentence="The welcome was not requeued." detail={retry.error} /> : null}
    </> : null}
  </Panel>;
}
