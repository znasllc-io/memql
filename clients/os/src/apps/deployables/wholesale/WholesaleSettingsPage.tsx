import { useEffect, useState, type ReactNode } from "react";

import { Button, ChoiceRow, ContentSkeleton, Details, DetailsSection, Notice, SettingRow } from "../../../kit";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import type { Reading } from "../../../cluster/reading";
import { ReviewsPane } from "../reviews/ReviewsPane";
import { routeWords, type WholesaleSettings, type WholesaleSettingsChanges } from "./model";
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
// IN THIS RELEASE the settings are whether applications are open and the
// route a buyer's access goes through. Nothing notifies the store's people
// yet, so nothing asks where to.

/** The routes in the order Settings offers them; "" is none. */
const ROUTES: readonly string[] = ["customerTag", "shopifyB2B"];

const NOT_SET_UP = "This store's wholesale program is not set up yet. The cluster's operators set it up.";

const draftOf = (s: WholesaleSettings): WholesaleSettingsChanges => ({ applicationsOpen: s.applicationsOpen, entitlementAdapter: s.entitlementAdapter });

/** Only what changed. */
function changesFrom(from: WholesaleSettings, draft: WholesaleSettingsChanges): Partial<WholesaleSettingsChanges> {
  const changes: Partial<WholesaleSettingsChanges> = {};
  if (draft.applicationsOpen !== from.applicationsOpen) changes.applicationsOpen = draft.applicationsOpen;
  if (draft.entitlementAdapter !== from.entitlementAdapter) changes.entitlementAdapter = draft.entitlementAdapter;
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
  const [draft, setDraft] = useState<WholesaleSettingsChanges | null>(null);
  // A fresh answer from the engine is the form's new starting point.
  useEffect(() => {
    if (value !== null) setDraft(draftOf(value));
  }, [value]);

  const context = { page: "Wholesale settings" };
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
  // THE ROUTES THIS ENGINE HAS, in Settings' order, and none.
  const routes = [...ROUTES.filter((r) => value.adapters.length === 0 || value.adapters.includes(r)), ""];

  async function save() {
    if (Object.keys(changes).length === 0) return;
    const outcome = await writes.updateSettings(changes);
    if (outcome !== "refused") onWritten();
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
      <Details>
        <DetailsSection title="Applications">
          <SettingRow label="New applications">
            {canEdit ? (
              <ChoiceRow label="New applications" value={draft.applicationsOpen ? "open" : "closed"}
                onChange={(next) => setDraft({ ...draft, applicationsOpen: next === "open" })}
                options={[{ value: "open", label: "Open" }, { value: "closed", label: "Closed" }]} />
            ) : (
              <span>{value.applicationsOpen ? "Open" : "Closed"}</span>
            )}
          </SettingRow>
        </DetailsSection>
        <DetailsSection title="Access">
          <SettingRow label="Route">
            {canEdit ? (
              <ChoiceRow label="Route" value={draft.entitlementAdapter}
                onChange={(next) => setDraft({ ...draft, entitlementAdapter: next })}
                options={routes.map((r) => ({ value: r, label: routeWords(r) }))} />
            ) : (
              <span>{routeWords(value.entitlementAdapter)}</span>
            )}
          </SettingRow>
        </DetailsSection>
      </Details>
    </ReviewsPane>
  );
}
