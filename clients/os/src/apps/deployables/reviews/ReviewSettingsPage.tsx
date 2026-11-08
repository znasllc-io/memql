import { useEffect, useRef, useState, type ReactNode } from "react";

import { Button, Caption, ContentSkeleton, Details, DetailsSection, InlineSkeleton, Input, Notice, RecordList, RecordListSkeleton, RecordRow, SettingRow, Switch } from "../../../kit";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import { SearchPicker } from "../../../kit/SearchPicker";
import type { Reading } from "../../../cluster/reading";
import { bare, deployedByLabel, type NameOf } from "../people";
import { personCalled, type ReviewPeople, type ReviewSettings, type ReviewSettingsValues } from "./model";
import { ReviewsPane } from "./ReviewsPane";
import { useReviewPeople, type ReviewsScope, type ReviewWrites } from "./useReviews";

// A STORE'S REVIEW SETTINGS, the last of its Reviews tabs, laid out as Fleet
// lays out a machine's Settings (DESIGN.md, "Details and settings content"):
// a heading beside each group, a muted name beside each control. Edited in
// place with the store's part (packs/reviewspack/settings.go). What is in force
// is the engine's answer, defaults applied; a change is sent as only what
// changed, and the engine merges it into the store's one settings row.
//
// WHO MAY CHANGE WHAT IS THE ENGINE'S TOO (reviewsSettings' `may`): a store
// with no settings is set up by the cluster's operators, and only the store's
// current owner, or an operator, names who approves removals -- so, with
// nobody named, only an operator does. A field this person may not change is
// shown, not offered.
//
// THE REMOVAL APPROVER IS ONE OF THE STORE'S PEOPLE WHO CAN MODERATE IT
// (reviewsPeople), so they are chosen from those people by name, never typed
// as an id, and the engine refuses anybody else. Whoever may name one may also
// name nobody (Clear the approver, sent as ""). The same read is the "Who can
// moderate" list, which says in words which of them approves removals -- and
// says when the engine listed only the first of them by name (`truncated`).
// An approver past that cut is on no row, so who approves is said from the
// settings, never from the list.

type Draft = {
  publicDisplay: boolean;
  requestsEnabled: boolean;
  requestDelayDays: string;
  linkLifetimeDays: string;
  requestTemplateId: string;
  requestSenderIdentityId: string;
  notifyEmail: string;
  removalApproverUserId: string;
};

const draftOf = (v: ReviewSettingsValues): Draft => ({
  publicDisplay: v.publicDisplay,
  requestsEnabled: v.requestsEnabled,
  requestDelayDays: String(v.requestDelayDays),
  linkLifetimeDays: String(v.linkLifetimeDays),
  requestTemplateId: v.requestTemplateId,
  requestSenderIdentityId: v.requestSenderIdentityId,
  notifyEmail: v.notifyEmail,
  removalApproverUserId: v.removalApproverUserId,
});

/** A whole number of days within bounds, or null. */
function days(text: string, min: number, max: number): number | null {
  const t = text.trim();
  if (!/^\d+$/.test(t)) return null;
  const n = Number(t);
  return n >= min && n <= max ? n : null;
}

/** Only what changed, typed as the engine takes it; or the first thing wrong with it. */
export function settingsChanges(from: ReviewSettingsValues, draft: Draft, may: { nameApprover: boolean; nameSender: boolean }): { changes: Partial<ReviewSettingsValues>; problem: string } {
  const changes: Partial<ReviewSettingsValues> = {};
  if (draft.publicDisplay !== from.publicDisplay) changes.publicDisplay = draft.publicDisplay;
  if (draft.requestsEnabled !== from.requestsEnabled) changes.requestsEnabled = draft.requestsEnabled;
  const delay = days(draft.requestDelayDays, 0, 365);
  if (delay === null) return { changes, problem: "The days before asking are a whole number from 0 to 365." };
  if (delay !== from.requestDelayDays) changes.requestDelayDays = delay;
  const life = days(draft.linkLifetimeDays, 1, 365);
  if (life === null) return { changes, problem: "The days a link stays open are a whole number from 1 to 365." };
  if (life !== from.linkLifetimeDays) changes.linkLifetimeDays = life;
  const template = draft.requestTemplateId.trim();
  if (template !== from.requestTemplateId) changes.requestTemplateId = template;
  const sender = draft.requestSenderIdentityId.trim();
  if (may.nameSender && sender !== from.requestSenderIdentityId) changes.requestSenderIdentityId = sender;
  const email = draft.notifyEmail.trim();
  if (email !== "" && !/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(email)) return { changes, problem: `${email} is not an email address.` };
  if (email !== from.notifyEmail) changes.notifyEmail = email;
  const approver = draft.removalApproverUserId.trim();
  if (may.nameApprover && approver !== from.removalApproverUserId) changes.removalApproverUserId = approver;
  return { changes, problem: "" };
}

export function ReviewSettingsView({ heading, scope, settings, writes, nameOf, viewerUserId }: {
  /** The Reviews heading and its tabs. */
  heading: ReactNode;
  scope: ReviewsScope;
  settings: Reading<ReviewSettings>;
  writes: ReviewWrites;
  nameOf: NameOf;
  viewerUserId: string;
}) {
  const value = settings.value;
  const people = useReviewPeople(scope, writes.revision);
  const [draft, setDraft] = useState<Draft | null>(null);
  // A fresh answer from the engine is the form's new starting point.
  useEffect(() => {
    if (value !== null) setDraft(draftOf(value.values));
  }, [value]);
  // WHERE FOCUS GOES WHEN CLEAR GOES. Clear is drawn only while someone is
  // named, so pressing it takes away the button that had focus. Focus moves to
  // the approver's line, which now says nobody approves -- and, to the approver
  // who is no operator, what saving leaves -- once the render that says so is
  // drawn, so a keyboard is never left on a control that has gone.
  const approverLine = useRef<HTMLDivElement>(null);
  const [cleared, setCleared] = useState(0);
  useEffect(() => {
    if (cleared > 0) approverLine.current?.focus();
  }, [cleared]);

  const context = { page: "Review settings" };
  if (value === null || draft === null) {
    return (
      <ReviewsPane heading={heading} bar={<ActionBar state="" tone={settings.state === "failed" ? "none" : "busy"} />} context={context}>
        {settings.state === "failed" ? (
          <Notice tone="error" sentence={settings.error || "The settings could not be read."}><Button onClick={settings.reread}>Try again</Button></Notice>
        ) : (
          <ContentSkeleton label="Reading the settings" kind="form" />
        )}
      </ReviewsPane>
    );
  }

  const canEdit = value.may.edit;
  const { changes, problem } = settingsChanges(value.values, draft, value.may);
  const dirty = Object.keys(changes).length > 0 || problem !== "";
  const saving = writes.busy === "settings";
  const exporting = writes.busy === "export";
  const set = <K extends keyof Draft>(key: K) => (next: Draft[K]) => setDraft((d) => (d === null ? d : { ...d, [key]: next }));
  const moderators = people.value?.people ?? [];
  // Named from the store's people first: they are who an approver is chosen from.
  const nameOfPerson: NameOf = (userId) => {
    const person = moderators.find((p) => bare(p.userId) === bare(userId));
    return person === undefined ? nameOf(userId) : personCalled(person);
  };
  const approverId = draft.removalApproverUserId.trim();
  const approverName = approverId === "" ? "" : deployedByLabel(approverId, viewerUserId, nameOfPerson);
  // WHO APPROVES IS STILL BEING READ: the settings name somebody nothing on
  // hand names, and the people who might are not read yet. Until they are,
  // the approver's place has a name's shape, never a sentence about them.
  const approverReading = approverId !== "" && approverName === "" && people.value === null && people.state !== "failed";
  const namesApprover = canEdit && value.may.nameApprover;
  const choosesApprover = namesApprover && moderators.length > 0;
  // THE PICKER SEARCHES ONLY THE PEOPLE LISTED. When the engine cut the list,
  // the person typed may be past the cut -- the approver among them -- so
  // finding nobody by that name is said of those listed, never of everybody
  // who can moderate.
  const noMatch = people.value?.truncated
    ? `Nobody among the first ${moderators.length} by that name.`
    : "Nobody who can moderate this store by that name.";
  // NOBODY NAMED, ONLY AN OPERATOR CAN NAME ONE: besides an operator, the
  // store's current approver is the only one who may (writeSettings). So the
  // approver who is no operator is told beside Clear, and once it is pressed on
  // the line that says nobody approves, that saving nobody leaves them unable
  // to name anybody again. Both halves are the settings read's answers
  // (packs/reviewspack/reads.go, readSettings): `approvesRemovals` is true only
  // for the viewer the settings name, and only when they are no operator -- an
  // operator named reads false -- and `nameSender` is true for an operator
  // alone.
  const clearingLocksOut = value.viewer.approvesRemovals && !value.may.nameSender;
  // That approver has cleared themselves in the draft: Clear and the sentence
  // beside it are gone, and what Save does still has to be said.
  const clearedYourself = clearingLocksOut && approverId === "";

  async function save() {
    if (problem !== "" || Object.keys(changes).length === 0) return;
    await writes.saveSettings(changes);
  }

  async function exportReviews() {
    const file = await writes.exportStore();
    if (file !== null) download(file.fileName, file.json);
  }

  const acts: Act[] = canEdit && dirty
    ? [
        { label: "Discard", text: true, onAct: () => setDraft(draftOf(value.values)) },
        ...(problem === "" ? [{ label: "Save", tone: "primary" as const, busy: saving, onAct: () => void save() }] : []),
      ]
    : [];
  const bar = <ActionBar state={saving ? "Saving" : dirty ? "Unsaved changes" : value.exists ? "Saved" : "Not set up"} tone={saving ? "busy" : dirty ? "paused" : "none"} acts={acts} />;

  return (
    <ReviewsPane heading={heading} bar={bar} context={context}>
      {!value.exists ? (
        <Notice sentence="This store's reviews are not set up yet." next={canEdit ? "Saving these settings sets them up." : "The cluster's operators set them up; after that, the store's people can change them here."} />
      ) : null}
      {writes.refusal ? <Notice tone="error" sentence={writes.refusal} /> : null}
      {problem !== "" ? <Notice tone="warn" sentence={problem} /> : null}
      <div className="os-review-settings">
        <Details>
          <DetailsSection title="On the site">
            <SettingRow label="Published reviews">
              <Switch checked={draft.publicDisplay} disabled={!canEdit} onChange={set("publicDisplay")}>Show published reviews on the site</Switch>
              <Caption>Only reviews somebody published are ever shown. Off, the site shows none.</Caption>
            </SettingRow>
          </DetailsSection>
          <DetailsSection title="Asking buyers">
            <SettingRow label="Review requests">
              <Switch checked={draft.requestsEnabled} disabled={!canEdit} onChange={set("requestsEnabled")}>Ask every buyer for a review</Switch>
              <Caption>Each paid product of a paid order gets its own link, sent once.</Caption>
            </SettingRow>
            <SettingRow label="Days before asking">
              <Input id="review-request-delay" label="Days after an order before asking" value={draft.requestDelayDays} disabled={!canEdit} onChange={set("requestDelayDays")} />
            </SettingRow>
            <SettingRow label="Days a link stays open">
              <Input id="review-link-lifetime" label="Days a review link stays open" value={draft.linkLifetimeDays} disabled={!canEdit} onChange={set("linkLifetimeDays")} />
              <Caption>The buyer can change their review through the link until it is published or the link expires.</Caption>
            </SettingRow>
            <SettingRow label="Email template">
              <Input id="review-request-template" label="Request email template" code value={draft.requestTemplateId} disabled={!canEdit} onChange={set("requestTemplateId")} placeholder="A Campaigns template id" />
            </SettingRow>
            <SettingRow label="Sent from">
              <Input id="review-request-sender" label="Request sender" code value={draft.requestSenderIdentityId} disabled={!canEdit || !value.may.nameSender} onChange={set("requestSenderIdentityId")} placeholder="A Campaigns sender identity id" />
              {value.may.nameSender ? null : <Caption>Only the cluster&rsquo;s operators choose the mailbox requests leave from.</Caption>}
            </SettingRow>
          </DetailsSection>
          <DetailsSection title="Moderation">
            <SettingRow label="Email when one waits">
              <Input id="review-notify-email" label="Email when a review is waiting" value={draft.notifyEmail} disabled={!canEdit} onChange={set("notifyEmail")} placeholder="moderators@example.com" />
            </SettingRow>
            <SettingRow label="Removal approver">
              {choosesApprover ? (
                <SearchPicker
                  id="review-removal-approver"
                  label="Choose who approves removals"
                  listLabel="People who can moderate"
                  placeholder="Search the people who can moderate"
                  choices={moderators.map((p) => ({ id: p.userId, name: personCalled(p), detail: p.displayName === "" ? undefined : p.email }))}
                  emptyText={noMatch}
                  onChoose={(choice) => set("removalApproverUserId")(choice.id)}
                />
              ) : namesApprover && people.state === "failed" ? (
                // THE ABSENT PICKER IS EXPLAINED WHERE IT WOULD STAND: the
                // people it chooses from could not be read. Who can moderate
                // carries the notice and its Try again.
                <Caption>Nobody can be chosen, because who can moderate this store could not be read.</Caption>
              ) : null}
              {/* The approver's line holds focus after Clear (approverLine):
                  never a tab stop, only where focus is put. */}
              <div ref={approverLine} tabIndex={-1}>
                <Caption>
                  {approverReading ? (
                    <InlineSkeleton label="Reading who approves removals" />
                  ) : (
                    <>
                      {approverWords(approverId, approverName, moderators.length > 0)}
                      {value.may.nameApprover
                        ? clearedYourself
                          ? " Once you save, only the cluster's operators can name someone."
                          : ""
                        : approverId === ""
                          ? " Only the cluster's operators can name one."
                          : " Only that owner, or the cluster's operators, can name someone else."}
                    </>
                  )}
                </Caption>
              </div>
              {/* NAMING NOBODY: the draft's approver set to "", which Save sends
                  as "" and the engine takes as nobody. Offered to whoever may
                  name one, only while someone is named, with what it leaves
                  beside it for the approver who is no operator; pressed, it
                  goes, and the approver's line takes focus and keeps saying
                  what it leaves until Save. */}
              {namesApprover && approverId !== "" ? (
                <>
                  <Button
                    tone="quiet"
                    onClick={() => {
                      set("removalApproverUserId")("");
                      setCleared((n) => n + 1);
                    }}
                  >
                    Clear the approver
                  </Button>
                  {clearingLocksOut ? <Caption>Once you clear the approver and save, only the cluster&rsquo;s operators can name someone.</Caption> : null}
                </>
              ) : null}
            </SettingRow>
            <SettingRow label="Who can moderate">
              <Moderators people={people} />
            </SettingRow>
          </DetailsSection>
          <DetailsSection title="Data">
            <SettingRow label="Export">
              <div><Button tone="quiet" busy={exporting} onClick={() => void exportReviews()}>Export reviews</Button></div>
              <Caption>Every review of this store and every decision on it, as one file.</Caption>
            </SettingRow>
          </DetailsSection>
        </Details>
      </div>
    </ReviewsPane>
  );
}

/**
 * Who approves removals, in words, drawn from the settings: nobody only when
 * the settings name nobody, so a list never decides that nobody approves.
 * Somebody this page cannot name is still somebody, said by what is on the
 * page: past the people listed when some are (the engine cut the list), and
 * with no list to point at when none are -- its read failed, or it found
 * nobody. A read still in flight says nothing (`approverReading`).
 */
function approverWords(approverId: string, approverName: string, listsPeople: boolean): string {
  const role = "the one person who can remove a published review or erase a writer's data.";
  if (approverId === "") return "Nobody can remove a published review until an owner is named.";
  if (approverName === "") return listsPeople ? `Someone not among the people listed is ${role}` : `Someone this page cannot name is ${role}`;
  return approverName === "you" ? `You are ${role}` : `${approverName} is ${role}`;
}

/**
 * Everybody who can moderate the store, by name, and in words which of them
 * approves removals: no chip, no badge.
 */
function Moderators({ people }: { people: Reading<ReviewPeople> }) {
  if (people.state === "failed") {
    return <Notice tone="warn" sentence="Who can moderate this store could not be read." detail={people.error}><Button onClick={people.reread}>Try again</Button></Notice>;
  }
  if (people.value === null) return <RecordListSkeleton label="Reading who can moderate this store" rows={2} />;
  const { people: listed, truncated } = people.value;
  if (listed.length === 0) return <Caption>Nobody in this store&rsquo;s organization can moderate its reviews.</Caption>;
  return (
    <>
      <RecordList as="ul" density="compact" label="Who can moderate this store's reviews">
        {listed.map((person) => (
          <RecordRow key={person.userId} name={personCalled(person)} secondary={person.displayName === "" ? undefined : person.email}>
            {person.mayRemove ? <span>Approves removals</span> : null}
          </RecordRow>
        ))}
      </RecordList>
      {/* THE ENGINE CUT THE LIST, and says so: these are the first by what
          each is called, and more can moderate the store. */}
      {truncated ? <Caption>{`Only the first ${listed.length} people, by name, are listed.`}</Caption> : null}
    </>
  );
}

/** Hands the browser a file, the way a download link would. */
function download(fileName: string, json: string) {
  const url = URL.createObjectURL(new Blob([json], { type: "application/json" }));
  const link = document.createElement("a");
  link.href = url;
  link.download = fileName;
  document.body.appendChild(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
