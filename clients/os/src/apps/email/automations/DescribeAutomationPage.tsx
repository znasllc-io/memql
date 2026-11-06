import { useEffect, useRef, useState } from "react";
import { newShortId, refusalSentence } from "@znasllc-io/memql-sdk-core/client";
import { AutoRefresh, Head, Notice, TextArea } from "../../../kit";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import { useJourneyWrites } from "../actions";
import { audienceProjection, senderProjection, templateProjection } from "../rows";
import { EMAIL_REFRESH_MS, useSenderChecks, type EmailFeeds } from "../useEmail";
import { blockedSentence } from "./automationActs";
import { Flow } from "./Flow";
import { triggerSentence, type Trigger, type FlowStep } from "./flowModel";
import { senderRefusalIssue, type Issue } from "./issues";

export interface Proposal { name: string; trigger: Trigger; steps: FlowStep[] }
/** Describe validates without writing. The person can save, edit, or activate
 * the proposal directly; there is no second check that secretly creates data. */
export function DescribeAutomationPage({ accountId, feeds, onBack, onEdit, onDone }: { accountId: string; feeds: EmailFeeds; onBack: () => void; onEdit: (proposal: Proposal) => void; onDone: (id: string) => void }) {
  const [sentence, setSentence] = useState(""); const [proposal, setProposal] = useState<Proposal | null>(null); const [explanation, setExplanation] = useState(""); const [issues, setIssues] = useState<Issue[]>([]);
  const [id] = useState(newShortId); const [revision, setRevision] = useState(""); const [busy, setBusy] = useState(false); const [asking, setAsking] = useState(false); const [unsupported, setUnsupported] = useState(""); const working = useRef(false);
  // The words remain editable during inference. A reply and its refusal belong
  // only to the words that requested them, even if this page has since closed.
  const generation = useRef(0), mounted = useRef(true);
  const [descriptionCurrent, setDescriptionCurrent] = useState(false);
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; generation.current++; };
  }, []);
  const writes = useJourneyWrites();
  async function draft() {
    if (working.current || !sentence.trim()) return; working.current = true; setBusy(true); setUnsupported("");
    const requested = ++generation.current;
    setDescriptionCurrent(true);
    try {
      const result = await writes.describe.call(accountId, sentence);
      if (!mounted.current || generation.current !== requested) return;
      if (result) { const p = result.proposal as Proposal | undefined; setExplanation(typeof result.explanation === "string" ? result.explanation : ""); if (p && typeof p.name === "string" && p.trigger && Array.isArray(p.steps)) { setProposal(p); setIssues(Array.isArray(result.issues) ? result.issues as Issue[] : []); } else setUnsupported(typeof result.explanation === "string" ? result.explanation : "That automation couldn't be drafted."); }
    } finally { working.current = false; if (mounted.current) setBusy(false); }
  }
  async function save(activate: boolean) {
    if (!proposal || working.current) return; working.current = true; setBusy(true); setAsking(false);
    try { const receipt = await writes.save.call({ journeyId: id, accountId, ...proposal, ...(revision ? { expectedRevision: revision } : {}) }); if (!receipt) return; setRevision(receipt.revision); setIssues(receipt.issues); feeds.journeys.reseed(); if (!activate) { onDone(id); return; } if (receipt.issues.length) return; const active = await writes.setState.call(id, "activate", receipt.revision); if (active && !active.issues.length) { feeds.journeys.reseed(); onDone(id); } else if (active) setIssues(active.issues); }
    finally { working.current = false; setBusy(false); }
  }
  const audienceName = proposal ? audienceProjection(feeds.audiences.snapshot.rows).find(a => a.id === proposal.trigger.audienceId)?.name ?? "Unavailable audience" : "";
  // The engine's sending check for each sender the proposal's steps name, as
  // the automation's own page asks it: a sender the provider would refuse keeps
  // Turn on away, and says why, for anyone who may turn it on.
  const senders = senderProjection(feeds.senders.snapshot.rows);
  const stepSenders = proposal ? senders.filter(sender => sender.accountId === accountId && proposal.steps.some(step => step.senderIdentityId === sender.id)) : [];
  const checks = useSenderChecks(accountId, stepSenders);
  const senderIssues = proposal ? proposal.steps.flatMap(step => {
    const sender = stepSenders.find(candidate => candidate.id === step.senderIdentityId);
    const issue = sender ? senderRefusalIssue(step.id, sender.address, checks.value.get(sender.id) ?? "", true) : null;
    return issue ? [issue] : [];
  }) : [];
  const shown = [...issues, ...senderIssues];
  const turnsOn = !shown.length && checks.state !== "loading";
  const error = (descriptionCurrent ? writes.describe.error : "") || writes.save.error || writes.setState.error || unsupported;
  const acts: Act[] = busy ? [] : asking ? [{ label: "Not yet", text: true, onAct: () => setAsking(false) }, ...(turnsOn ? [{ label: "Turn on", tone: "primary" as const, onAct: () => { void save(true); } }] : [])] : proposal ? [{ label: "Edit as steps", text: true, onAct: () => revision ? onDone(id) : onEdit(proposal) }, { label: "Save draft", text: true, onAct: () => { void save(false); } }, ...(turnsOn ? [{ label: "Turn on", tone: "primary" as const, onAct: () => setAsking(true) }] : [])] : [{ label: "Cancel", text: true, onAct: onBack }, ...(sentence.trim() ? [{ label: "Draft it", tone: "primary" as const, onAct: () => { void draft(); } }] : [])];
  const detail = [proposal && senderIssues.length ? blockedSentence("draft", senderIssues) : "", proposal && !revision ? "Nothing is saved yet." : ""].filter(Boolean).join(" ") || undefined;
  return <div className="os-action-pane"><div className="os-action-body os-app-stack"><Head title="Describe an automation" back={{ label: "Add an automation", onSelect: onBack }} /><AutoRefresh intervalMs={EMAIL_REFRESH_MS} onRefresh={checks.reload} busy={checks.state === "loading"} enabled={!!proposal} />
    <div className="email-describe-input"><TextArea id="email-describe" label="Describe the automation" value={sentence} onChange={value => { generation.current++; setDescriptionCurrent(false); setSentence(value); setProposal(null); setIssues([]); setAsking(false); writes.describe.reset(); setUnsupported(""); }} placeholder="When someone joins Newsletter, ask them to confirm, then send Welcome and, three days later, the next email." /></div>
    {error ? <Notice tone="warn" sentence={refusalSentence(error)} /> : null}
    {proposal ? <>{explanation ? <p>{explanation}</p> : null}<Flow proposal trigger={triggerSentence(proposal.trigger, audienceName)} accountId={accountId} steps={proposal.steps} templates={templateProjection(feeds.templates.snapshot.rows)} senders={senders} issues={shown} /></> : null}
  </div><ActionBar state={busy ? proposal ? "Saving" : "Drafting" : revision ? "Saved as a draft" : proposal ? "Proposal" : "Describe the automation"} detail={detail} tone={busy ? "busy" : "none"} acts={acts}>{asking ? <span className="os-actbar-confirm">Turn on “{proposal?.name}”? It sends email to people automatically from now on.</span> : null}</ActionBar></div>;
}
