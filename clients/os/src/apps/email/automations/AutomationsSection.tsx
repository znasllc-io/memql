import { emailState } from "../words";
import { useState } from "react";
import { Workflow, Zap } from "lucide-react";
import { refusalSentence, type Row } from "@znasllc-io/memql-sdk-core/client";
import { AutoRefresh, Button, EmptyState, Head, LiveList, Notice, RecordRow, Refine, Select, listCount } from "../../../kit";
import { AddButton } from "../../../kit/AddButton";
import { useTwoFeedView } from "../../../live/mergedView";
import type { CampaignWrites } from "../actions";
import { recordNamed, useOpenRequest, type OpenRequest } from "../openRequest";
import { audienceProjection, conceptEntity, emailRuleFromRow, journeyFingerprint, journeyFromRow, ruleFingerprint, type EmailRuleRow, type JourneyRow } from "../rows";
import { EMAIL_REFRESH_MS, useJourneyTotals, listKnowledge, type EmailFeeds } from "../useEmail";
import { triggerSentence } from "./flowModel";
import { AutomationPage } from "./AutomationPage";
import { RulePage, AutomationSwitch } from "./RulePage";
import { AddAutomationWizard } from "./AddAutomationWizard";

type Item = { kind: "journey"; row: JourneyRow } | { kind: "rule"; row: EmailRuleRow };
export function AutomationsSection({ feeds, writes, includeArchived, openRequest }: { feeds: EmailFeeds; writes: CampaignWrites; includeArchived: boolean; openRequest?: OpenRequest }) {
  const [openId, setOpenId] = useState(""); const [adding, setAdding] = useState(false); const [search, setSearch] = useState(""); const [starts, setStarts] = useState("");
  useOpenRequest(openRequest, target => { setAdding("add" in target); setOpenId("id" in target ? target.id : ""); });
  const counts = useJourneyTotals(feeds.journeys.snapshot.rows.map(row => journeyFromRow(row).id));
  const enrolled = (id: string) => { const figure = counts.value.get(id); return figure?.kind === "measured" ? `${figure.value.toLocaleString()}${figure.atLeast ? " or more" : ""} enrolled` : ""; };
  const audienceName = (id: string) => audienceProjection(feeds.audiences.snapshot.rows).find(a => a.id === id)?.name || "Unavailable audience";
  const key = `${search}:${starts}:${includeArchived}`;
  const source = useTwoFeedView<Row, Row, Item>(feeds.journeys.source, feeds.rules.source, key, (journeys, rules) => ([...journeys.map(journeyFromRow).map(row => ({ kind: "journey" as const, row })), ...rules.map(emailRuleFromRow).map(row => ({ kind: "rule" as const, row }))] as Item[]).filter(item => (includeArchived || item.row.status !== "archived") && item.row.name.toLowerCase().includes(search.toLowerCase()) && (!starts || (item.kind === "journey" ? item.row.trigger.kind : "event") === starts)));
  const journey = recordNamed(feeds.journeys.snapshot.rows, journeyFromRow, openId); const rule = recordNamed(feeds.rules.snapshot.rows, emailRuleFromRow, openId);
  if (adding) return <AddAutomationWizard feeds={feeds} writes={writes} onBack={() => setAdding(false)} onDone={id => { setAdding(false); setOpenId(id); }} />;
  if (journey) return <AutomationPage key={journey.id} journey={journey} feeds={feeds} onBack={() => setOpenId("")} />;
  if (rule) return <RulePage key={rule.id} rule={rule} feeds={feeds} writes={writes} onBack={() => setOpenId("")} />;
  const error = feeds.journeys.snapshot.error || feeds.rules.snapshot.error;
  return <div className="os-app-stack"><Head title="Automations" meta={listCount(source?.snapshot)}>{feeds.journeys.snapshot.rows.length + feeds.rules.snapshot.rows.length > 0 ? <Refine iconOnly label="Refine automations" search={search} onSearch={setSearch} chips={starts ? [{ id: "starts", label: starts === "joined" ? "Someone joins" : starts === "confirmed" ? "Someone confirms" : "Something happens in MemQL", onRemove: () => setStarts("") }] : []}><Select id="email-automation-starts" label="Starts when" value={starts} onChange={setStarts}><option value="">Any trigger</option><option value="joined">Someone joins</option><option value="confirmed">Someone confirms</option><option value="event">Something happens in MemQL</option></Select></Refine> : null}<AddButton label="Add an automation" onClick={() => setAdding(true)} /></Head><AutomationSwitch /><AutoRefresh intervalMs={EMAIL_REFRESH_MS} onRefresh={counts.reload} busy={counts.state === "loading"} />
    {openId && [feeds.rules, feeds.journeys].every(f => listKnowledge(f) === "known" && !f.snapshot.truncated) ? <Notice tone="info" sentence="That automation isn't available." /> : null}
    <LiveList<Item> key={key} source={source} rowId={i => `${i.kind}:${i.row.id}`} fingerprint={i => i.kind === "journey" ? journeyFingerprint(i.row) : ruleFingerprint(i.row)} label="Automations" errorContent={<Notice tone="warn" sentence={refusalSentence(error) || "Automations couldn't be read."}><Button onClick={() => { feeds.rules.reseed(); feeds.journeys.reseed(); }}>Try again</Button></Notice>} emptyContent={<EmptyState icon={Workflow} title={search || starts ? "No matching automations" : "No automations yet"}>{search || starts ? "Try another name or trigger." : "Add an automation to send email when something happens."}</EmptyState>} renderRow={item => <RecordRow icon={item.kind === "journey" ? <Workflow size={16} aria-hidden /> : <Zap size={16} aria-hidden />} name={item.row.name} secondary={item.kind === "journey" ? triggerSentence(item.row.trigger, audienceName(item.row.trigger.audienceId)) : <>{`When a ${conceptEntity(item.row.triggerConcept)} ${item.row.eventKind === "updated" ? "changes" : "is created"}`}{item.row.condition ? <code className="os-mono">{` · Only when ${item.row.condition}`}</code> : null}</>} state={emailState(item.kind, item.row.status)} tone={item.row.lastError ? "warn" : item.row.status === "active" ? "accent" : "muted"} onOpen={() => setOpenId(item.row.id)}>{item.kind === "rule" ? <span>Fired {item.row.firedCount} times</span> : <span>{enrolled(item.row.id)}</span>}</RecordRow>} />
  </div>;
}
