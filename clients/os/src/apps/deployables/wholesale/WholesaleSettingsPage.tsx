import { useEffect, useState, type ReactNode } from "react";

import { Button, Caption, Check, ChoiceRow, ContentSkeleton, Details, DetailsSection, Input, Notice, Select, SettingRow, TextArea } from "../../../kit";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import type { Reading } from "../../../cluster/reading";
import { ReviewsPane } from "../reviews/ReviewsPane";
import { EMAIL_TEMPLATES, NOT_SET_UP, routeWords, type WholesaleSettings, type WholesaleSettingsChanges } from "./model";
import type { WholesaleWrites } from "./useWholesale";

// A STORE'S WHOLESALE SETTINGS, the last of its Wholesale tabs, laid out as
// Fleet lays out a machine's Settings: a heading beside each group, a muted
// name beside each choice. What is in force is the engine's answer
// (wholesaleStoreSettings); a change is sent as only what changed, and the
// engine merges it into the store's one row (wholesaleUpdateSettings).
//
// WHO MAY DO WHAT IS THE ENGINE'S (`may`). A store with no settings row is set
// up by the cluster's operators alone (`may.setUp`), closed and with no route;
// once it is, whoever the engine answers `may.update` for changes it here. A
// choice this person may not change is said in words, never drawn disabled.
//
// WHETHER THE OUTBOX DELIVERS TO THE STAFF ADDRESS is the engine's answer for
// the address it holds (`staffEmailAdmitted`), said beneath the stored
// address and not of one still being typed. A refusal -- terms required before
// the documents exist, a link lifetime out of bounds -- is the engine's
// sentence; the page judges nothing itself.
//
// THE TEMPLATES ARE THE ENGINE'S LIST for this person (`templates`: the
// published transactional templates they may choose), Off first. The sender
// is chosen by the cluster's operators and is not offered here.

/** The routes in the order Settings offers them; "" is none. */
const ROUTES: readonly string[] = ["customerTag", "shopifyB2B"];

/** The draft: the settings as typed, the link lifetime as its text. */
type Draft = Omit<WholesaleSettingsChanges, "linkLifetimeDays"> & { linkLifetimeDays: string };

const draftOf = (s: WholesaleSettings): Draft => ({
  applicationsOpen: s.applicationsOpen,
  entitlementAdapter: s.entitlementAdapter,
  staffEmail: s.staffEmail,
  termsRequired: s.termsRequired,
  linkLifetimeDays: String(s.linkLifetimeDays),
  acceptanceText: s.acceptanceText,
  termsTemplateId: s.termsTemplateId,
  approvedTemplateId: s.approvedTemplateId,
  rejectedTemplateId: s.rejectedTemplateId,
  revokedTemplateId: s.revokedTemplateId,
});

/**
 * Only what changed. A lifetime that is a whole number is sent as one; any
 * other text is sent as typed, so the engine refuses it in its own words.
 */
function changesFrom(from: WholesaleSettings, draft: Draft): Record<string, unknown> {
  const changes: Record<string, unknown> = {};
  const was = draftOf(from);
  for (const key of Object.keys(draft) as (keyof Draft)[]) {
    if (key === "linkLifetimeDays") continue;
    const next = typeof draft[key] === "string" ? (draft[key] as string).trim() : draft[key];
    const before = typeof was[key] === "string" ? (was[key] as string).trim() : was[key];
    if (next !== before) changes[key] = key === "acceptanceText" ? draft[key] : next;
  }
  const days = draft.linkLifetimeDays.trim();
  if (days !== was.linkLifetimeDays) changes.linkLifetimeDays = /^\d+$/.test(days) ? Number(days) : days;
  return changes;
}

export function WholesaleSettingsPage({ heading, settings, writes, onWritten }: {
  /** The Wholesale heading and its tabs. */
  heading: ReactNode;
  settings: Reading<WholesaleSettings>;
  writes: WholesaleWrites;
  /** Read the settings again: after a write that was not refused, or when asked. */
  onWritten: () => void;
}) {
  const value = settings.value;
  const [draft, setDraft] = useState<Draft | null>(null);
  // A fresh answer from the engine is the form's new starting point.
  useEffect(() => {
    if (value !== null) setDraft(draftOf(value));
  }, [value]);

  const context = { page: "Wholesale settings" };
  if (value === null || draft === null) {
    return (
      <ReviewsPane heading={heading} bar={<ActionBar state="" tone={settings.state === "failed" ? "none" : "busy"} />} context={context}>
        {settings.state === "failed" ? (
          <Notice tone="error" sentence={settings.error}><Button onClick={settings.reread}>Try again</Button></Notice>
        ) : (
          <ContentSkeleton label="Reading the settings" kind="form" />
        )}
      </ReviewsPane>
    );
  }

  const reload = () => {
    writes.clearRefusal();
    onWritten();
  };
  const refusal = writes.refusal ? (
    <Notice tone="error" sentence={writes.refusal}><Button onClick={reload}>Reload</Button></Notice>
  ) : null;

  if (!value.exists) {
    const settingUp = writes.busy === "setUp";
    const acts: Act[] = value.may.setUp
      ? [{ label: "Set up wholesale for this store", tone: "primary", busy: settingUp, onAct: () => void writes.setUpStore().then((outcome) => { if (outcome !== "refused") onWritten(); }) }]
      : [];
    return (
      <ReviewsPane heading={heading} bar={<ActionBar state="Not set up" tone={settingUp ? "busy" : "none"} acts={acts} />} context={context}>
        <Notice sentence={NOT_SET_UP} />
        {refusal}
      </ReviewsPane>
    );
  }

  const canEdit = value.may.update;
  const changes = changesFrom(value, draft);
  const dirty = Object.keys(changes).length > 0;
  const saving = writes.busy === "settings";
  const exporting = writes.busy === "export";
  // THE ROUTES THIS ENGINE HAS, in Settings' order, and none.
  const routes = [...ROUTES.filter((r) => value.adapters.length === 0 || value.adapters.includes(r)), ""];
  const set = <K extends keyof Draft>(key: K) => (next: Draft[K]) => setDraft((d) => (d === null ? d : { ...d, [key]: next }));
  const templateName = (id: string) => (id === "" ? "Off" : value.templates.find((t) => t.id === id)?.name ?? id);
  // The stored address, unchanged in the draft: only of that does the engine's answer speak.
  const storedAddress = value.staffEmail !== "" && draft.staffEmail.trim() === value.staffEmail;

  async function save() {
    if (Object.keys(changes).length === 0) return;
    const outcome = await writes.updateSettings(changes as Partial<WholesaleSettingsChanges>);
    if (outcome !== "refused") onWritten();
  }

  async function download() {
    const file = await writes.exportStore();
    if (file !== null) saveFile(file.fileName, file.json);
  }

  const acts: Act[] = canEdit && dirty
    ? [
        { label: "Discard", text: true, onAct: () => { writes.clearRefusal(); setDraft(draftOf(value)); } },
        { label: "Save", tone: "primary", busy: saving, onAct: () => void save() },
      ]
    : [];
  const bar = <ActionBar state={saving ? "Saving" : dirty ? "Unsaved changes" : "Saved"} tone={saving ? "busy" : dirty ? "paused" : "none"} acts={acts} />;

  return (
    <ReviewsPane heading={heading} bar={bar} context={context}>
      {refusal}
      <div className="os-review-settings">
        <Details>
          <DetailsSection title="Applications">
            <SettingRow label="New applications">
              {canEdit ? (
                <ChoiceRow label="New applications" value={draft.applicationsOpen ? "open" : "closed"}
                  onChange={(next) => set("applicationsOpen")(next === "open")}
                  options={[{ value: "open", label: "Open" }, { value: "closed", label: "Closed" }]} />
              ) : (
                <span>{value.applicationsOpen ? "Open" : "Closed"}</span>
              )}
            </SettingRow>
            <SettingRow label="Terms required">
              {canEdit ? (
                <Check checked={draft.termsRequired} onChange={set("termsRequired")}>Applicants accept the terms first</Check>
              ) : (
                <span>{value.termsRequired ? "Yes" : "No"}</span>
              )}
            </SettingRow>
            <SettingRow label="Link lifetime in days">
              {canEdit ? (
                <Input id="wholesale-link-lifetime" label="Link lifetime in days" value={draft.linkLifetimeDays} onChange={set("linkLifetimeDays")} />
              ) : (
                <span>{value.linkLifetimeDays}</span>
              )}
            </SettingRow>
          </DetailsSection>
          <DetailsSection title="Staff">
            <SettingRow label="Staff address">
              {canEdit ? (
                <Input id="wholesale-staff-email" label="Staff address" value={draft.staffEmail} onChange={set("staffEmail")} placeholder="orders@example.com" />
              ) : (
                <span>{value.staffEmail || "None"}</span>
              )}
              {storedAddress ? (
                <Caption>{value.staffEmailAdmitted ? "The outbox delivers to this address." : "The outbox does not deliver to this address yet. The cluster's operators add it."}</Caption>
              ) : null}
            </SettingRow>
          </DetailsSection>
          <DetailsSection title="Access">
            <SettingRow label="Route">
              {canEdit ? (
                <ChoiceRow label="Route" value={draft.entitlementAdapter}
                  onChange={set("entitlementAdapter")}
                  options={routes.map((r) => ({ value: r, label: routeWords(r) }))} />
              ) : (
                <span>{routeWords(value.entitlementAdapter)}</span>
              )}
            </SettingRow>
          </DetailsSection>
          <DetailsSection title="Emails">
            {EMAIL_TEMPLATES.map(([key, name]) => (
              <SettingRow key={key} label={name}>
                {canEdit ? (
                  <Select id={`wholesale-${key}`} label={name} value={draft[key]} onChange={set(key)}>
                    <option value="">Off</option>
                    {value.templates.map((t) => <option key={t.id} value={t.id}>{t.name}</option>)}
                    {/* A template the store holds that this person is not offered stays what it is. */}
                    {value[key] !== "" && !value.templates.some((t) => t.id === value[key]) ? <option value={value[key]}>{value[key]}</option> : null}
                  </Select>
                ) : (
                  <span>{templateName(value[key])}</span>
                )}
              </SettingRow>
            ))}
          </DetailsSection>
          <DetailsSection title="Acceptance">
            <SettingRow label="Acceptance sentence">
              {canEdit ? (
                <TextArea id="wholesale-acceptance" label="Acceptance sentence" rows={3} value={draft.acceptanceText} onChange={set("acceptanceText")} />
              ) : (
                <span>{value.acceptanceText}</span>
              )}
            </SettingRow>
          </DetailsSection>
          <DetailsSection title="Records">
            <div><Button tone="quiet" busy={exporting} onClick={() => void download()}>Download every record</Button></div>
          </DetailsSection>
        </Details>
      </div>
    </ReviewsPane>
  );
}

/** Hands the browser a file made from the reply, the way a download link would. */
function saveFile(fileName: string, json: string) {
  const url = URL.createObjectURL(new Blob([json], { type: "application/json" }));
  const link = document.createElement("a");
  link.href = url;
  link.download = fileName;
  document.body.appendChild(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
