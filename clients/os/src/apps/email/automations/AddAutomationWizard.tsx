import { isPublished } from "../rows";
import { useRef, useState } from "react";
import { Workflow } from "lucide-react";
import { newShortId, refusalSentence } from "@znasllc-io/memql-sdk-core/client";
import { ChoiceStack, Field, Input, Notice } from "../../../kit";
import { Wizard } from "../../../kit/Wizard";
import type { Stop } from "../../../kit/Rail";
import type { Act } from "../../../kit/ActionBar";
import { useAccountOptions } from "../../accounts/tie";
import { useDefaultOrganization } from "../../accounts/organization";
import { organizationName, selectableOrganizations, isOperatorOrganization } from "../organizations";
import { useJourneyWrites, type CampaignWrites } from "../actions";
import { audienceProjection, senderProjection, templateProjection, ruleSentence, emailRuleFromRow } from "../rows";
import { listKnowledge, useConfirmTemplates, type EmailFeeds } from "../useEmail";
import { useOpenRecord } from "../openRequest";
import { addSteps, type AddStep, type TriggerChoice } from "./addModel";
import { Flow } from "./Flow";
import { newStep, triggerSentence, type FlowStep, type Trigger } from "./flowModel";
import { journeyIssues, type Issue } from "./issues";
import { emptyRule, RuleField } from "./ruleFields";
import { DescribeAutomationPage, type Proposal } from "./DescribeAutomationPage";

export function AddAutomationWizard({ feeds, writes, onBack, onDone }: { feeds: EmailFeeds; writes: CampaignWrites; onBack: () => void; onDone: (id: string) => void }) {
  const accounts = useAccountOptions(); const organizations = selectableOrganizations(accounts); const fallback = useDefaultOrganization(accounts);
  const [picked, setPicked] = useState(""); const accountId = picked || fallback || (organizations.length === 1 ? organizations[0]!.id : "");
  const [choice, setChoice] = useState<TriggerChoice | null>(null); const [step, setStep] = useState<AddStep>("trigger");
  const [audienceId, setAudience] = useState(""); const [steps, setSteps] = useState<FlowStep[]>([newStep("send")]);
  const [name, setName] = useState(""); const [description, setDescription] = useState(""); const [rule, setRule] = useState(emptyRule(accountId));
  const [describing, setDescribing] = useState(false); const [asking, setAsking] = useState<"" | "turn-on" | "cancel">("");
  const [id] = useState(newShortId); const [revision, setRevision] = useState(""); const [savedRule, setSavedRule] = useState(false);
  const [serverIssues, setServerIssues] = useState<Issue[]>([]); const [busy, setBusy] = useState(false); const working = useRef(false);
  const journeyWrites = useJourneyWrites(); const openRecord = useOpenRecord();
  const audiences = audienceProjection(feeds.audiences.snapshot.rows).filter(a => a.accountId === accountId && a.status !== "archived");
  const templates = templateProjection(feeds.templates.snapshot.rows), senders = senderProjection(feeds.senders.snapshot.rows);
  const audience = audiences.find(a => a.id === audienceId);
  const trigger: Trigger = { kind: choice === "confirmed" ? "confirmed" : "joined", audienceId };
  const confirms = useConfirmTemplates(steps, feeds.templates.snapshot.rows);
  const issues = choice === "event" ? [] : [...journeyIssues({ accountId, trigger, steps }, { audiences, templates, senders, confirms: confirms.value }), ...serverIssues];
  const resourceKnown = [feeds.audiences, feeds.templates, feeds.senders].every(f => listKnowledge(f) === "known");
  const known = resourceKnown && (choice === "event" || confirms.state === "ready");
  const ruleDraft = { ...rule, accountId, name };
  const ruleReady = !!rule.triggerConcept && !!rule.templateId && templates.some(t => t.id === rule.templateId && t.accountId === accountId && isPublished(t)) && (rule.recipientMode === "cluster_roles" || (isOperatorOrganization(accountId) || senders.some(s => s.id === rule.senderIdentityId && s.accountId === accountId && s.status !== "disabled")) && (rule.recipientMode === "audience" ? !!rule.audienceId : !!rule.recipientField.trim()));
  const clean = known && (choice === "event" ? ruleReady : !issues.length) && !!name.trim();
  const open = !accountId ? "organization" : step;
  const order = addSteps(choice, rule.recipientMode, organizations.length !== 1);
  const label: Record<AddStep, string> = { organization: "Organization", trigger: "Trigger", audience: "Audience", steps: "Steps", what: "What", event: "When", condition: "Only when", who: "Who", template: "Template", sender: "Sender", review: "Review" };
  function editProposal(proposal: Proposal) { setChoice(proposal.trigger.kind); setAudience(proposal.trigger.audienceId); setSteps(proposal.steps); setName(proposal.name); setStep("steps"); setDescribing(false); }
  async function save(activate: boolean) {
    if (working.current) return; working.current = true; setBusy(true); setAsking("");
    try {
      if (choice === "event") {
        if (!savedRule) { const created = await writes.createRule.create(ruleDraft, id); if (!created) return; setSavedRule(true); }
        if (activate && !await writes.ruleArming.activate(id)) return;
        feeds.rules.reseed(); onDone(id); return;
      }
      const receipt = await journeyWrites.save.call({ journeyId: id, accountId, name, description, trigger, steps, ...(revision ? { expectedRevision: revision } : {}) });
      if (!receipt) return;
      setRevision(receipt.revision); setServerIssues(receipt.issues); feeds.journeys.reseed();
      if (!activate) { onDone(id); return; }
      if (receipt.issues.length) return;
      const turnedOn = await journeyWrites.setState.call(id, "activate", receipt.revision);
      if (turnedOn && !turnedOn.issues.length) { feeds.journeys.reseed(); onDone(id); }
      else if (turnedOn) setServerIssues(turnedOn.issues);
    } finally { working.current = false; setBusy(false); }
  }
  async function cancel() {
    if (!revision && !savedRule) { onBack(); return; }
    if (asking !== "cancel") { setAsking("cancel"); return; }
    if (working.current) return; working.current = true; setBusy(true);
    try { const done = savedRule ? await writes.ruleArming.retire(id) : await journeyWrites.setState.call(id, "archive", revision); if (done) onBack(); }
    finally { working.current = false; setBusy(false); }
  }
  const body = (at: AddStep) => at === "organization" ? <ChoiceStack name="automation-organization" label="Organization" value={accountId} options={organizations.map(a => ({ value: a.id, label: organizationName(accounts, a.id) }))} onChange={value => { setPicked(value); setAudience(""); setRule(emptyRule(value)); setSteps([newStep("send")]); setStep("trigger"); }} /> : at === "trigger" ? <><ChoiceStack name="automation-trigger" label="Trigger" value={choice ?? ""} options={[{ value: "joined", label: "Someone joins an audience" }, { value: "confirmed", label: "Someone confirms their subscription" }, { value: "event", label: "Something happens in MemQL" }]} onChange={value => { setChoice(value as TriggerChoice); setStep(value === "event" ? "what" : "audience"); }} /><button type="button" className="os-link" onClick={() => setDescribing(true)}>Describe it instead</button></> : at === "audience" ? <ChoiceStack name="automation-audience" label="Audience" value={audienceId} options={audiences.map(a => ({ value: a.id, label: a.name, description: a.confirmation === "required" ? "Asks people to confirm" : undefined }))} onChange={value => { setAudience(value); const a = audiences.find(a => a.id === value); setSteps([newStep(choice === "joined" && a?.confirmation === "required" ? "confirm" : "send")]); if (!name) setName(`${a?.name ?? "Audience"} welcome`); setStep("steps"); }} /> : at === "steps" || at === "review" && choice !== "event" ? <>{at === "review" ? <Field label="Name"><Input id="email-automation-name" label="Name" value={name} onChange={setName} /></Field> : null}<Flow trigger={triggerSentence(trigger, audience?.name ?? "Unavailable audience")} accountId={accountId} steps={steps} templates={templates} senders={senders} editable={at === "steps"} onChange={next => { setSteps(next); setServerIssues([]); }} name={name} onName={setName} description={description} onDescription={setDescription} issues={issues} /></> : at === "review" ? <><Field label="Name"><Input id="email-automation-name" label="Name" value={name} onChange={setName} /></Field><p>{ruleSentence(emailRuleFromRow({ ...ruleDraft, id }), { template: templates.find(t => t.id === rule.templateId)?.name ?? "", audience: audiences.find(a => a.id === rule.audienceId)?.name ?? "" })}</p></> : <RuleField field={at} draft={ruleDraft} onChange={next => { setRule(next); setName(next.name); }} feeds={feeds} />;
  const valid = open === "what" ? !!rule.triggerConcept : open === "who" ? rule.recipientMode === "cluster_roles" || (rule.recipientMode === "audience" ? !!rule.audienceId : !!rule.recipientField.trim()) : open === "template" ? !!rule.templateId : open === "sender" ? isOperatorOrganization(accountId) || !!rule.senderIdentityId : true;
  const acts: Act[] = busy ? [] : asking ? [{ label: "Not yet", text: true, onAct: () => setAsking("") }, { label: asking === "cancel" ? "Retire draft" : "Turn on", tone: asking === "cancel" ? "danger" : "primary", onAct: () => { if (asking === "cancel") void cancel(); else void save(true); } }] : open === "review" ? [...(name.trim() || revision || savedRule ? [{ label: revision || savedRule ? "Cancel" : "Save as draft", text: true, onAct: () => { if (revision || savedRule) void cancel(); else void save(false); } }] : []), ...(clean ? [{ label: "Turn on", tone: "primary" as const, onAct: () => setAsking("turn-on") }] : [])] : [{ label: "Cancel", text: true, onAct: () => { void cancel(); } }, ...(open === "audience" && known && !audiences.length ? [{ label: "Add an audience", tone: "primary" as const, onAct: () => openRecord("audiences", { add: true }) }] : !["organization", "trigger", "audience"].includes(open) && valid ? [{ label: "Next", tone: "primary" as const, onAct: () => setStep(order[order.indexOf(open) + 1]!) }] : [])];
  if (describing) return <DescribeAutomationPage accountId={accountId} feeds={feeds} onBack={() => setDescribing(false)} onEdit={editProposal} onDone={onDone} />;
  const error = journeyWrites.save.error || journeyWrites.setState.error || writes.createRule.error || writes.ruleArming.error || confirms.error;
  const stops: Stop[] = order.map((id, i) => ({ id, name: label[id], state: id === open ? "open" : i < order.indexOf(open) ? "done" : "ahead", body: body(id) }));
  return <Wizard title="Add an automation" icon={<Workflow size={20} aria-hidden />} label="Adding an automation" back={{ label: "Automations", onSelect: onBack }} steps={stops} open={open} onOpen={id => setStep(id as AddStep)} status={{ word: busy ? "Saving" : revision ? "Saved as a draft" : open === "review" ? "Review the automation" : label[open], tone: busy ? "busy" : undefined }} acts={acts} confirm={asking ? asking === "cancel" ? "Retire this draft? It stays in history." : `Turn on “${name}”? It sends email to people automatically from now on.` : undefined} notices={<>{serverIssues.length ? <Notice tone="warn" sentence="Saved as a draft. It wasn't turned on." /> : null}{error ? <Notice tone="warn" sentence={refusalSentence(error)} /> : null}</>} />;
}
