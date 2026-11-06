import { useState } from "react";
import { refusalSentence, type Row } from "@znasllc-io/memql-sdk-core/client";
import { Button, Notice, Panel, RecordList, RecordRow, RecordListSkeleton, Refine, Select, Subhead, Switch } from "../../../kit";
import { InPlaceAsk } from "../../../kit/pressHold";
import { AddButton } from "../../../kit/AddButton";
import { recipientFromRow, recipientIsSendable, type AudienceRow } from "../rows";
import type { CampaignWrites } from "../actions";
import type { useAudienceRecipients } from "../useEmail";

export const SUBSCRIPTION_STATES = [{ value: "subscribed", label: "Subscribed" }, { value: "pending", label: "Pending confirmation" }, { value: "unsubscribed", label: "Unsubscribed" }, { value: "bounced", label: "Bounced" }, { value: "complained", label: "Reported as spam" }];
/** People who left. The engine refuses to subscribe them again: only they can, so no change is offered. */
const LEFT = new Set(["unsubscribed", "bounced", "complained"]);
export function Subscribers({ audience, pinned = null, roster, writes, onAdd, archived }: { audience: AudienceRow; pinned?: Row | null; roster: ReturnType<typeof useAudienceRecipients>; writes: CampaignWrites; onAdd: () => void; archived: boolean }) {
  const [search, setSearch] = useState("");
  const [state, setState] = useState("");
  const [limit, setLimit] = useState(50);
  // Someone being made subscribed in an audience that asks people to confirm: the operator must say they agreed.
  const [marking, setMarking] = useState<{ id: string; email: string } | null>(null);
  const [agreed, setAgreed] = useState(false);
  // The person the duplicate check found is shown first, whatever page of the roster they sit on. The roster does not carry their change, so it is kept here.
  const [changed, setChanged] = useState<Record<string, string>>({});
  const found = pinned === null ? null : recipientFromRow(pinned);
  const rostered = roster.value.map(recipientFromRow);
  const people = found === null ? rostered : [{ ...found, subscriptionStatus: changed[found.id] ?? found.subscriptionStatus }, ...rostered.filter(p => p.id !== found.id)];
  async function set(id: string, next: string, alreadyConfirmed?: boolean): Promise<boolean> {
    if (!await writes.setSubscription.set(id, next, alreadyConfirmed)) return false;
    setChanged(held => ({ ...held, [id]: next }));
    void roster.reload();
    return true;
  }
  async function change(person: { id: string; email: string }, next: string) {
    if (next === "subscribed" && audience.confirmation === "required") { setMarking(person); setAgreed(false); writes.setSubscription.reset(); return; }
    setMarking(null);
    await set(person.id, next);
  }
  async function markSubscribed() {
    if (marking && agreed && await set(marking.id, "subscribed", true)) setMarking(null);
  }
  const needle = search.trim().toLowerCase();
  const filtered = people.filter(p => (!state || p.subscriptionStatus === state) && `${p.email} ${p.displayName}`.toLowerCase().includes(needle));
  const blocked = people.filter(p => !recipientIsSendable(p)).length;
  return <Panel label="People"><Subhead meta={people.length >= 100 ? "Latest 100" : undefined} actions={<>
    {people.length ? <Refine iconOnly label="Refine people" search={search} onSearch={setSearch} chips={state ? [{ id: "state", label: SUBSCRIPTION_STATES.find(s => s.value === state)?.label ?? "State", onRemove: () => setState("") }] : []}><Select id="email-people-state" label="Subscription state" value={state} onChange={setState}><option value="">Any state</option>{SUBSCRIPTION_STATES.map(s => <option key={s.value} value={s.value}>{s.label}</option>)}</Select></Refine> : null}
    {!archived ? <AddButton label="Add people" onClick={onAdd} /> : null}
  </>}>People</Subhead>
    {roster.state === "loading" && !roster.readAt ? <RecordListSkeleton label="Loading people" /> : null}
    {roster.error ? <Notice tone="warn" sentence={refusalSentence(roster.error)}><Button busy={roster.state === "loading"} onClick={() => { void roster.reload(); }}>Try again</Button></Notice> : null}
    <RecordList label="People in this audience">{filtered.slice(0, limit).map(person => <RecordRow key={person.id} name={person.email} secondary={person.displayName} state={SUBSCRIPTION_STATES.find(s => s.value === person.subscriptionStatus)?.label} actions={!archived && !LEFT.has(person.subscriptionStatus) ? <Select id={`email-person-${person.id}`} label={`Subscription state for ${person.email}`} value={person.subscriptionStatus} onChange={next => { void change(person, next); }}>{SUBSCRIPTION_STATES.map(s => <option key={s.value} value={s.value}>{s.label}</option>)}</Select> : undefined} />)}</RecordList>
    {filtered.length > limit ? <div className="os-panel-actions"><Button onClick={() => setLimit(n => n + 50)}>Show more</Button></div> : null}
    {roster.state === "ready" && !filtered.length ? <p className="os-caption">{people.length ? "No matching people." : "Nobody on this list yet."}</p> : null}
    {blocked ? <p className="os-caption">{blocked} of these can't be mailed. They stay on the list so a later import can't add them back.</p> : null}
    {marking ? <InPlaceAsk><Notice tone="info" sentence={`${marking.email} asked to confirm, so they are subscribed once they do. To subscribe them now, say they already agreed.`}><Switch checked={agreed} onChange={setAgreed}>They already agreed to receive these emails</Switch><div className="os-panel-actions">{agreed ? <Button tone="primary" busy={writes.setSubscription.busy} onClick={() => { void markSubscribed(); }}>Mark subscribed</Button> : null}<Button onClick={() => setMarking(null)}>Cancel</Button></div></Notice></InPlaceAsk> : null}
    {writes.setSubscription.error ? <Notice tone="warn" sentence={refusalSentence(writes.setSubscription.error)} /> : null}
  </Panel>;
}
