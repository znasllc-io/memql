import { EMAIL_REFRESH_MS } from "../useEmail";
import { useRef, useState } from "react";
import { Globe } from "lucide-react";
import { refusalSentence, type Row } from "@znasllc-io/memql-sdk-core/client";
import { AutoRefresh, Button, ChoiceStack, CopyField, Fact, Facts, Field, Head, Input, Notice, Panel, RecordList, RecordRow, RecordListSkeleton, Select, Subhead, formatMoment } from "../../../kit";
import { AddButton } from "../../../kit/AddButton";
import { ActionBar } from "../../../kit/ActionBar";
import { Wizard } from "../../../kit/Wizard";
import type { Stop } from "../../../kit/Rail";
import { flatten } from "../../../kit/rows";
import type { LiveCollectionHandle } from "../../../live/useLiveCollection";
import { useConfigureNewsletter, useRetryNewsletterWelcome } from "../actions";
import { type ActSpec, toKitActs } from "../acts";
import { isPublished, templateFromRow, type AudienceRow } from "../rows";
import { knowledgeOf, listKnowledge, useNewsletterWelcomes, type EmailFeeds, type EmailRecordFeeds } from "../useEmail";

const text = (row: Record<string, unknown>, key: string) => typeof row[key] === "string" ? row[key] as string : "";
const escapeHTML = (value: string) => value.replaceAll("&", "&amp;").replaceAll('"', "&quot;").replaceAll("<", "&lt;").replaceAll(">", "&gt;");
export function newsletterFormHTML(row: Record<string, unknown>): string {
  return `<form method="post" action="/_memql/forms/campaigns/subscribe">\n  <label>Email <input type="email" name="email" required maxlength="320" autocomplete="email"></label>\n  <label>Name <input name="displayName" maxlength="120" autocomplete="name"></label>\n  <label><input type="checkbox" name="consent" value="yes" required> ${escapeHTML(text(row, "consentText"))}</label>\n  <input type="hidden" name="consentRevision" value="${escapeHTML(text(row, "createdAt"))}">\n  <button type="submit">Subscribe</button>\n</form>`;
}
type Shared = { audience: AudienceRow; feeds: EmailFeeds; signupForms: EmailRecordFeeds["signupForms"]; sites: LiveCollectionHandle<Row> };
const siteTitle = (sites: LiveCollectionHandle<Row>, id: string) => { const site = sites.snapshot.rows.map(flatten).find(s => s.id === id); return site ? text(site, "title") || text(site, "hostname") : "Unavailable deployable"; };
export function SignupForms({ audience, signupForms, sites, onAdd, onOpen }: Pick<Shared, "audience" | "signupForms" | "sites"> & { onAdd: () => void; onOpen: (id: string) => void }) {
  const rows = signupForms.snapshot.rows.map(flatten).filter(r => r.audienceId === audience.id);
  const { knowledge, refusal } = knowledgeOf(signupForms, audience.id);
  return <Panel label="Signup forms"><Subhead actions={audience.status !== "archived" && knowledge === "known" ? <AddButton label="Connect a signup form" onClick={onAdd} /> : undefined}>Signup forms</Subhead>
    {knowledge === "reading" ? <RecordListSkeleton label="Loading signup forms" /> : null}
    {knowledge === "refused" || knowledge === "unconfirmed" ? <Notice tone="warn" sentence={refusalSentence(refusal) || "Signup forms couldn't be confirmed."}><Button onClick={() => signupForms.refresh(audience.id)}>Try again</Button></Notice> : null}
    <RecordList>{rows.map(row => <RecordRow key={text(row, "id")} name={siteTitle(sites, text(row, "siteId"))} state={row.enabled ? "Accepting signups" : "Off"} onOpen={() => onOpen(text(row, "id"))} />)}</RecordList>
    {!rows.length && knowledge === "known" ? <p className="os-caption">No signup forms yet.</p> : null}
  </Panel>;
}

export function signupFormActs(binding: Record<string, unknown>, editing: boolean): ActSpec[] {
  if (editing) return [{ id: "cancel", label: "Cancel", text: true }, { id: "save", label: "Save", tone: "primary" }];
  return [{ id: "edit", label: "Edit", text: true }, binding.enabled ? { id: "toggle", label: "Stop accepting signups", text: true } : { id: "toggle", label: "Accept signups", tone: "primary" }];
}

export function SignupFormPage({ audience, feeds, signupForms, sites, binding, onBack }: Shared & { binding: Row; onBack: () => void }) {
  const row = flatten(binding);
  const [editing, setEditing] = useState(false);
  const [templateId, setTemplate] = useState(text(row, "templateId"));
  const [senderIdentityId, setSender] = useState(text(row, "senderIdentityId"));
  const [consentText, setConsent] = useState(text(row, "consentText"));
  const [expectedRevision, setRevision] = useState(text(row, "createdAt"));
  const write = useConfigureNewsletter();
  const retry = useRetryNewsletterWelcome();
  const welcomes = useNewsletterWelcomes(audience.id);
  const working = useRef(false);
  const templates = feeds.templates.snapshot.rows.map(templateFromRow).filter(t => t.accountId === audience.accountId && isPublished(t));
  const senders = feeds.senders.snapshot.rows.map(flatten).filter(s => s.accountId === audience.accountId && s.status !== "disabled");
  const known = [feeds.templates, feeds.senders].every(f => listKnowledge(f) === "known");
  const name = siteTitle(sites, text(row, "siteId"));
  async function save(toggle = false) {
    if (working.current) return; working.current = true;
    try { if (await write.call({ siteId: text(row, "siteId"), audienceId: audience.id, templateId: toggle ? text(row, "templateId") || undefined : templateId || undefined, senderIdentityId: toggle ? text(row, "senderIdentityId") : senderIdentityId, consentText: toggle ? text(row, "consentText") : consentText, enabled: toggle ? !row.enabled : row.enabled === true, expectedRevision: toggle ? text(row, "createdAt") : expectedRevision })) { setEditing(false); signupForms.refresh(audience.id); } }
    finally { working.current = false; }
  }
  function run(id: string) {
    if (id === "edit") { setTemplate(text(row, "templateId")); setSender(text(row, "senderIdentityId")); setConsent(text(row, "consentText")); setRevision(text(row, "createdAt")); setEditing(true); }
    if (id === "cancel") setEditing(false);
    if (id === "save") void save();
    if (id === "toggle") void save(true);
  }
  const specs = signupFormActs(row, editing).filter(a => a.id !== "save" || known && !!senderIdentityId && consentText.trim().length >= 10 && (audience.confirmation !== "required" || !templateId));
  return <div className="os-action-pane"><div className="os-action-body os-app-stack"><Head title={name} back={{ label: audience.name, onSelect: onBack }} />
    <Panel label="Signup form"><Facts><Fact label="Audience" value={audience.name} /><Fact label="Welcome email" value={text(row, "templateId") ? templates.find(t => t.id === row.templateId)?.name || "Unavailable template" : "No welcome email"} /><Fact label="Sender" value={senders.find(s => s.id === row.senderIdentityId)?.address as string || "Unavailable sender"} /><Fact label="Consent" value={text(row, "consentText")} /></Facts>
      {editing ? <><WelcomeField templates={templates} audience={audience} value={templateId} onChange={setTemplate} /><SenderField senders={senders} value={senderIdentityId} onChange={setSender} /><Field label="Consent"><Input id="email-form-consent" label="Consent" value={consentText} onChange={setConsent} /></Field></> : <><CopyField label="the signup form" value={newsletterFormHTML(row)} /><p className="os-caption">Add it to the deployable's page in the editor, with /newsletter/thank-you and /newsletter/problem pages for the replies.</p></>}
    </Panel>
    <Panel label="Recent welcomes"><Subhead>Recent welcomes</Subhead><AutoRefresh intervalMs={EMAIL_REFRESH_MS} onRefresh={welcomes.reload} busy={welcomes.state === "loading"} />{!welcomes.readAt && welcomes.state === "loading" ? <RecordListSkeleton label="Loading welcomes" /> : null}{welcomes.error ? <Notice tone="warn" sentence={refusalSentence(welcomes.error)} /> : null}<RecordList>{welcomes.value.map(flatten).filter(r => !r.siteId || r.siteId === row.siteId).map(w => <RecordRow key={text(w, "id")} name={text(w, "displayName") || text(w, "email") || "Subscriber"} secondary={refusalSentence(text(w, "lastError")) || formatMoment(text(w, "requestedAt"))} state={{ pending: "Waiting", sent: "Sent", blocked: "Blocked", failed: "Failed", skipped: "Skipped" }[text(w, "status")] || text(w, "status")} actions={w.status === "blocked" ? <Button busy={retry.busy} onClick={async () => { if (await retry.call({ signupId: text(w, "id"), expectedRevision: text(w, "createdAt") })) void welcomes.reload(); }}>Check again</Button> : undefined} />)}</RecordList>{welcomes.state === "ready" && !welcomes.value.length ? <p className="os-caption">No welcomes yet.</p> : null}{retry.error ? <Notice tone="warn" sentence={refusalSentence(retry.error)} /> : null}</Panel>
    {write.error ? <Notice tone="warn" sentence={refusalSentence(write.error)} /> : null}
  </div><ActionBar state={write.busy ? "Saving" : editing ? "Editing" : row.enabled ? "Accepting signups" : "Off"} tone={write.busy ? "busy" : "none"} acts={write.busy ? [] : toKitActs(specs, run)} /></div>;
}

function WelcomeField({ templates, audience, value, onChange }: { templates: ReturnType<typeof templateFromRow>[]; audience: AudienceRow; value: string; onChange: (id: string) => void }) {
  return <Field label="Welcome email"><Select id="email-form-template" label="Welcome email" value={value} onChange={onChange}><option value="">No welcome email</option>{audience.confirmation !== "required" ? templates.map(t => <option key={t.id} value={t.id}>{t.name}</option>) : null}</Select></Field>;
}
function SenderField({ senders, value, onChange }: { senders: Row[]; value: string; onChange: (id: string) => void }) {
  return <Field label="Sender"><Select id="email-form-sender" label="Sender" value={value} onChange={onChange}><option value="">Select</option>{senders.map(s => <option key={text(s, "id")} value={text(s, "id")}>{text(s, "address")}</option>)}</Select></Field>;
}

export function ConnectSignupFormWizard({ audience, feeds, signupForms, sites, onBack, onDone }: Shared & { onBack: () => void; onDone: () => void }) {
  const [step, setStep] = useState("deployable");
  const [siteId, setSite] = useState("");
  const [templateId, setTemplate] = useState("");
  const [senderIdentityId, setSender] = useState("");
  const [consentText, setConsent] = useState("Email me your newsletter.");
  const write = useConfigureNewsletter();
  const working = useRef(false);
  const templates = feeds.templates.snapshot.rows.map(templateFromRow).filter(t => t.accountId === audience.accountId && isPublished(t));
  const senders = feeds.senders.snapshot.rows.map(flatten).filter(s => s.accountId === audience.accountId && s.status !== "disabled");
  const bound = signupForms.snapshot.rows.map(flatten).map(r => r.siteId);
  const available = sites.snapshot.rows.map(flatten).filter(s => s.accountId === audience.accountId && !s.systemOwned && s.status !== "disabled" && !bound.includes(s.id));
  const resources = [sites, feeds.templates, feeds.senders];
  const known = resources.every(f => listKnowledge(f) === "known") && knowledgeOf(signupForms, audience.id).knowledge === "known";
  const order = ["deployable", "welcome", "sender", "consent", "review"];
  const bodies = {
    deployable: <ChoiceStack name="signup-site" label="Deployable" value={siteId} onChange={setSite} options={available.map(s => ({ value: text(s, "id"), label: text(s, "title") || text(s, "hostname") }))} />,
    welcome: <WelcomeField templates={templates} audience={audience} value={templateId} onChange={setTemplate} />,
    sender: <SenderField senders={senders} value={senderIdentityId} onChange={setSender} />,
    consent: <Field label="Consent"><Input id="email-form-consent" label="Consent" value={consentText} onChange={setConsent} /></Field>,
    review: <p>Connect {siteTitle(sites, siteId)} to {audience.name}. {templateId ? `Send ${templates.find(t => t.id === templateId)?.name || "the welcome email"} to new subscribers.` : "No welcome email is sent by this form."} Enabling also turns on this deployable's public forms.</p>,
  };
  async function connect() { if (working.current) return; working.current = true; try { if (await write.call({ siteId, audienceId: audience.id, templateId: templateId || undefined, senderIdentityId, consentText, enabled: true })) { signupForms.refresh(audience.id); onDone(); } } finally { working.current = false; } }
  const valid = known && (step === "deployable" ? !!siteId : step === "sender" ? !!senderIdentityId : step === "consent" ? consentText.trim().length >= 10 : true);
  const steps: Stop[] = order.map((id, i) => ({ id, name: id[0]!.toUpperCase() + id.slice(1), state: id === step ? "open" : i < order.indexOf(step) ? "done" : "ahead", body: bodies[id as keyof typeof bodies] }));
  return <Wizard title="Connect a signup form" icon={<Globe size={20} aria-hidden />} label="Connecting a signup form" back={{ label: audience.name, onSelect: onBack }} steps={steps} open={step} onOpen={setStep} status={{ word: write.busy ? "Connecting" : "Connect a signup form", tone: write.busy ? "busy" : undefined }} notices={<>{write.error ? <Notice tone="warn" sentence={refusalSentence(write.error)} /> : null}{resources.filter(f => listKnowledge(f) === "refused").map((f, i) => <Notice key={i} tone="warn" sentence={refusalSentence(f.snapshot.error)}><Button onClick={f.reseed}>Try again</Button></Notice>)}</>} acts={write.busy ? [] : [{ label: "Cancel", text: true, onAct: onBack }, ...(valid ? [{ label: step === "review" ? "Connect" : "Next", tone: "primary" as const, onAct: () => { if (step === "review") void connect(); else setStep(order[order.indexOf(step) + 1]!); } }] : [])]} />;
}
