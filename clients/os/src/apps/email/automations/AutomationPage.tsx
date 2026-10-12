import { EMAIL_REFRESH_MS } from "../useEmail";
import { useEffect, useRef, useState } from "react";
import { refusalSentence, versionOrder } from "@znasllc-io/memql-sdk-core/client";
import { AutoRefresh, Button, Fact, Facts, Head, Notice, Panel, formatMoment } from "../../../kit";
import { ActionBar } from "../../../kit/ActionBar";
import { useAccountOptions } from "../../accounts/tie";
import { useJourneyWrites } from "../actions";
import { toKitActs } from "../acts";
import { organizationName } from "../organizations";
import { audienceProjection, senderProjection, templateProjection, type JourneyRow } from "../rows";
import { listKnowledge, useConfirmTemplates, useEnrollmentCounts, type EmailFeeds } from "../useEmail";
import { journeyActs, journeyReading, type Asking } from "./automationActs";
import { Flow } from "./Flow";
import { triggerSentence } from "./flowModel";
import { journeyIssues, type Issue } from "./issues";

export function AutomationPage({ journey, feeds, onBack }: { journey: JourneyRow; feeds: EmailFeeds; onBack: () => void }) {
  const [draft, setDraft] = useState(journey);
  const [asking, setAsking] = useState<Asking>("");
  const [serverIssues, setServerIssues] = useState<Issue[]>([]);
  const base = useRef(journey);
  const dirty = JSON.stringify([draft.name, draft.description, draft.steps]) !== JSON.stringify([base.current.name, base.current.description, base.current.steps]);
  useEffect(() => { if (!dirty && (versionOrder(journey.createdAt, base.current.createdAt) ?? 0) > 0) { base.current = journey; setDraft(journey); } }, [journey, dirty]);
  const accounts = useAccountOptions();
  const writes = useJourneyWrites();
  const working = useRef(false);
  const [busy, setBusy] = useState(false);
  const audiences = audienceProjection(feeds.audiences.snapshot.rows), templates = templateProjection(feeds.templates.snapshot.rows), senders = senderProjection(feeds.senders.snapshot.rows);
  const confirms = useConfirmTemplates(draft.steps, feeds.templates.snapshot.rows);
  const counts = useEnrollmentCounts(journey.id, journey.steps);
  const issues = [...journeyIssues(draft, { audiences, templates, senders, confirms: confirms.value }), ...serverIssues];
  const known = [feeds.audiences, feeds.templates, feeds.senders].every(f => listKnowledge(f) === "known") && confirms.state === "ready";
  const audience = audiences.find(a => a.id === journey.trigger.audienceId)?.name || "Unavailable audience";
  const countWords = new Map<string, string>();
  for (const c of counts.value) if (c.figure.kind === "measured" && (c.figure.value > 0 || !["left early", "failed"].includes(c.label))) countWords.set(c.cardId, [countWords.get(c.cardId), `${c.figure.value.toLocaleString()}${c.figure.atLeast ? " or more" : ""} ${c.label}`].filter(Boolean).join(", "));
  async function run(id: string) {
    if (id === "keep") { setAsking(""); return; }
    if (id === "turn-on" || id === "retire") { setAsking(id); return; }
    if (id === "discard") { base.current = journey; setDraft(journey); setServerIssues([]); return; }
    if (working.current) return; working.current = true; setBusy(true);
    try {
      if (id === "save") {
        const receipt = await writes.save.call({ journeyId: journey.id, accountId: journey.accountId, name: draft.name, description: draft.description, trigger: draft.trigger, steps: draft.steps, expectedRevision: base.current.createdAt });
        if (receipt) { const saved = { ...draft, createdAt: receipt.revision }; base.current = saved; setDraft(saved); setServerIssues(receipt.issues); feeds.journeys.reseed(); }
      } else {
        const action = id === "pause" ? "pause" : asking === "retire" ? "archive" : "activate";
        const receipt = await writes.setState.call(journey.id, action, base.current.createdAt);
        if (receipt) { setServerIssues(receipt.issues); feeds.journeys.reseed(); }
        setAsking("");
      }
    } finally { working.current = false; setBusy(false); }
  }
  const failure = writes.save.error || writes.setState.error;
  const question = asking === "retire" ? `Retire “${journey.name}”? Everyone still in it leaves, and it can't be turned on again.` : asking === "turn-on" ? `Turn on “${journey.name}”? People who ${journey.trigger.kind === "joined" ? "join" : "confirm their subscription to"} ${audience} from now on get these emails.${journey.status === "paused" ? " People who joined while it was paused are included." : ""}` : "";
  const specs = journeyActs(journey, dirty, issues, asking).filter(a => a.id !== "turn-on" || known).filter(a => a.id !== "save" || !!draft.name.trim());
  return <div className="os-action-pane"><div className="os-action-body os-app-stack"><Head title={journey.name} back={{ label: "Automations", onSelect: onBack }} />
    <AutoRefresh intervalMs={EMAIL_REFRESH_MS} onRefresh={counts.reload} busy={counts.state === "loading"} />
    <Panel label="Automation details"><Facts><Fact label="Organization" value={organizationName(accounts, journey.accountId)} /><Fact label="Starts" value={triggerSentence(journey.trigger, audience)} />{journey.activatedAt ? <Fact label="Turned on" value={formatMoment(journey.activatedAt)} /> : null}</Facts></Panel>
    {journey.lastError ? <Notice tone="warn" sentence={refusalSentence(journey.lastError)} /> : null}
    {failure && refusalSentence(failure) !== refusalSentence(journey.lastError) ? <Notice tone="warn" sentence={refusalSentence(failure)} /> : null}
    {confirms.error ? <Notice tone="warn" sentence={refusalSentence(confirms.error)}><Button onClick={() => { void confirms.reload(); }}>Try again</Button></Notice> : null}
    {counts.error ? <Notice tone="warn" sentence={refusalSentence(counts.error)} /> : null}
    <Flow trigger={triggerSentence(journey.trigger, audience)} steps={draft.steps} accountId={journey.accountId} templates={templates} senders={senders} editable={journey.status === "draft" || journey.status === "paused"} issues={issues} counts={countWords} name={draft.name} description={draft.description} onName={name => { setDraft(d => ({ ...d, name })); setServerIssues([]); }} onDescription={description => { setDraft(d => ({ ...d, description })); setServerIssues([]); }} onChange={steps => { setDraft(d => ({ ...d, steps })); setServerIssues([]); }} />
  </div><ActionBar state={journeyReading(journey, dirty, issues).word} detail={journeyReading(journey, dirty, issues).detail} tone={busy ? "busy" : journeyReading(journey, dirty, issues).tone} acts={busy ? [] : toKitActs(specs, id => { void run(id); })}>{question ? <span className="os-actbar-confirm">{question}</span> : null}</ActionBar></div>;
}
