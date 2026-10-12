import { EMAIL_REFRESH_MS } from "../useEmail";
import { useEffect, useRef, useState } from "react";
import { refusalSentence, versionOrder } from "@znasllc-io/memql-sdk-core/client";
import { AutoRefresh, Fact, Facts, Head, Notice, Panel } from "../../../kit";
import { ActionBar } from "../../../kit/ActionBar";
import { useAccountOptions } from "../../accounts/tie";
import type { CampaignWrites, RuleFacts } from "../actions";
import { toKitActs } from "../acts";
import { organizationName } from "../organizations";
import { conceptEntity, senderProjection, templateProjection, type EmailRuleRow, type RecipientMode } from "../rows";
import { useAuthoredAutomations, type EmailFeeds } from "../useEmail";
import { Flow } from "./Flow";
import { ruleActs, type Asking } from "./automationActs";
import { emptyRule, RuleField } from "./ruleFields";

export function AutomationSwitch() {
  const authored = useAuthoredAutomations();
  return <><AutoRefresh intervalMs={EMAIL_REFRESH_MS} onRefresh={authored.reload} busy={authored.state === "loading"} />{authored.value === "halted" ? <Notice tone="warn" sentence="Event emails are switched off across this cluster, so no rule is sending, including those marked On." next="An owner can turn them back on in Settings, under Cluster." /> : authored.state === "error" ? <p className="os-caption">This cluster didn't say whether event emails are switched on.</p> : null}</>;
}
export function RulePage({ rule, feeds, writes, onBack }: { rule: EmailRuleRow; feeds: EmailFeeds; writes: CampaignWrites; onBack: () => void }) {
  const [draft, setDraft] = useState<RuleFacts>({ ...emptyRule(rule.accountId), ...rule, recipientMode: rule.recipientMode as RecipientMode });
  const [baseline, setBaseline] = useState(draft);
  const [asking, setAsking] = useState<Asking>("");
  const [busy, setBusy] = useState(false);
  const working = useRef(false);
  const dirty = JSON.stringify(draft) !== JSON.stringify(baseline);
  const readRevision = useRef(rule.createdAt);
  useEffect(() => { if (!dirty && (versionOrder(rule.createdAt, readRevision.current) ?? 0) > 0) { const next = { ...emptyRule(rule.accountId), ...rule, recipientMode: rule.recipientMode as RecipientMode }; setDraft(next); setBaseline(next); readRevision.current = rule.createdAt; } }, [rule, dirty]);
  const accounts = useAccountOptions();
  const templates = templateProjection(feeds.templates.snapshot.rows), senders = senderProjection(feeds.senders.snapshot.rows);
  const trigger = `When a ${conceptEntity(draft.triggerConcept)} ${draft.eventKind === "updated" ? "changes" : "is created"}`;
  const error = writes.updateRule.error || writes.ruleArming.error;
  const failure = refusalSentence(rule.lastError);
  async function run(id: string) {
    if (id === "keep") { setAsking(""); return; }
    if (id === "turn-on" || id === "retire") { writes.ruleArming.reset(); setAsking(id); return; }
    if (id === "discard") { setDraft(baseline); return; }
    if (working.current) return; working.current = true; setBusy(true);
    try {
      let done = false;
      if (id === "save") { done = await writes.updateRule.update(rule.id, draft); if (done) setBaseline(draft); }
      else if (id === "pause") done = await writes.ruleArming.setStatus(rule.id, "paused");
      else if (asking === "retire") done = await writes.ruleArming.retire(rule.id);
      else done = await writes.ruleArming.activate(rule.id);
      if (done) feeds.rules.reseed();
      setAsking("");
    } finally { working.current = false; setBusy(false); }
  }
  const status = dirty ? "Unsaved changes" : ({ draft: "Draft", active: "On", paused: "Paused", failed: "Refused" }[rule.status] || "");
  return <div className="os-action-pane"><div className="os-action-body os-app-stack"><Head title={rule.name} back={{ label: "Automations", onSelect: onBack }} /><AutomationSwitch />
    <Panel label="Automation details"><Facts><Fact label="Organization" value={organizationName(accounts, rule.accountId)} /></Facts></Panel>
    {failure ? <Notice tone="warn" sentence={failure} /> : null}{error && refusalSentence(error) !== failure ? <Notice tone="warn" sentence={refusalSentence(error)} /> : null}
    <Flow fixed trigger={trigger} triggerDetail={draft.condition ? <code className="os-mono">Only when {draft.condition}</code> : undefined} accountId={draft.accountId} steps={[{ id: "email", kind: "send", templateId: draft.templateId, senderIdentityId: draft.senderIdentityId }]} templates={templates} senders={senders} editable={rule.status !== "active"} onChange={() => {}} counts={new Map([["end", `Fired ${rule.firedCount} times${rule.lastFiredAt ? `, last ${new Date(rule.lastFiredAt).toLocaleDateString()}` : ""}`]])} inspector={selected => <>{(selected === "trigger" ? ["what", "event", "condition"] : selected === "email" ? ["template", "who", ...(draft.recipientMode !== "cluster_roles" ? ["sender"] : [])] : ["name"]).map(field => <RuleField key={field} field={field} draft={draft} onChange={setDraft} feeds={feeds} />)}</>} />
  </div><ActionBar state={busy ? "Saving" : status} tone={busy ? "busy" : rule.status === "active" ? "live" : "none"} acts={busy ? [] : toKitActs(ruleActs(rule, dirty, asking).filter(a => a.id !== "save" || !!draft.name.trim()), id => { void run(id); })}>{asking ? <span className="os-actbar-confirm">{asking === "retire" ? `Retire “${rule.name}”? It stops sending and keeps its history.` : `Turn on “${rule.name}”? From then on it emails people every time ${trigger.replace(/^When /, "")}.`}</span> : null}</ActionBar></div>;
}
