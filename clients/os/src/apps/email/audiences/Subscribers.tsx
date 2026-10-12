import { useState } from "react";
import { refusalSentence } from "@znasllc-io/memql-sdk-core/client";
import { Button, Notice, Panel, RecordList, RecordRow, RecordListSkeleton, Refine, Select, Subhead } from "../../../kit";
import { AddButton } from "../../../kit/AddButton";
import { recipientFromRow, recipientIsSendable } from "../rows";
import type { CampaignWrites } from "../actions";
import type { useAudienceRecipients } from "../useEmail";

export const SUBSCRIPTION_STATES = [{ value: "subscribed", label: "Subscribed" }, { value: "pending", label: "Pending confirmation" }, { value: "unsubscribed", label: "Unsubscribed" }, { value: "bounced", label: "Bounced" }, { value: "complained", label: "Reported as spam" }];
export function Subscribers({ roster, writes, onAdd, archived }: { roster: ReturnType<typeof useAudienceRecipients>; writes: CampaignWrites; onAdd: () => void; archived: boolean }) {
  const [search, setSearch] = useState("");
  const [state, setState] = useState("");
  const [limit, setLimit] = useState(50);
  const people = roster.value.map(recipientFromRow);
  const needle = search.trim().toLowerCase();
  const filtered = people.filter(p => (!state || p.subscriptionStatus === state) && `${p.email} ${p.displayName}`.toLowerCase().includes(needle));
  const blocked = people.filter(p => !recipientIsSendable(p)).length;
  return <Panel label="People"><Subhead meta={people.length >= 100 ? "Latest 100" : undefined} actions={<>
    {people.length ? <Refine iconOnly label="Refine people" search={search} onSearch={setSearch} chips={state ? [{ id: "state", label: SUBSCRIPTION_STATES.find(s => s.value === state)?.label ?? "State", onRemove: () => setState("") }] : []}><Select id="email-people-state" label="Subscription state" value={state} onChange={setState}><option value="">Any state</option>{SUBSCRIPTION_STATES.map(s => <option key={s.value} value={s.value}>{s.label}</option>)}</Select></Refine> : null}
    {!archived ? <AddButton label="Add people" onClick={onAdd} /> : null}
  </>}>People</Subhead>
    {roster.state === "loading" && !roster.readAt ? <RecordListSkeleton label="Loading people" /> : null}
    {roster.error ? <Notice tone="warn" sentence={refusalSentence(roster.error)}><Button busy={roster.state === "loading"} onClick={() => { void roster.reload(); }}>Try again</Button></Notice> : null}
    <RecordList label="People in this audience">{filtered.slice(0, limit).map(person => <RecordRow key={person.id} name={person.email} secondary={person.displayName} state={SUBSCRIPTION_STATES.find(s => s.value === person.subscriptionStatus)?.label} actions={!archived ? <Select id={`email-person-${person.id}`} label={`Subscription state for ${person.email}`} value={person.subscriptionStatus} onChange={async next => { if (await writes.setSubscription.set(person.id, next)) void roster.reload(); }}>{SUBSCRIPTION_STATES.map(s => <option key={s.value} value={s.value}>{s.label}</option>)}</Select> : undefined} />)}</RecordList>
    {filtered.length > limit ? <div className="os-panel-actions"><Button onClick={() => setLimit(n => n + 50)}>Show more</Button></div> : null}
    {roster.state === "ready" && !filtered.length ? <p className="os-caption">{people.length ? "No matching people." : "Nobody on this list yet."}</p> : null}
    {blocked ? <p className="os-caption">{blocked} of these can't be mailed. They stay on the list so a later import can't add them back.</p> : null}
    {writes.setSubscription.error ? <Notice tone="warn" sentence={refusalSentence(writes.setSubscription.error)} /> : null}
  </Panel>;
}
