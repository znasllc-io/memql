// The Campaigns app's own settings, and the store that keeps them.
//
// Same discipline as the Accounts app's (apps/accounts/settings.ts) and a
// separate key for the same reason: the SHELL's DesktopDocument rejects a
// version it does not know, so folding per-app preferences into it would mean
// a person loses their desks because an app learned a checkbox.

import type { OsAppSection } from "../../system/registry";

/**
 * The sections this app declares, in manifest order.
 *
 * Exported because the manifest and the settings picker must offer the SAME
 * set. A second literal is one that can disagree, and a preference naming a
 * section the manifest does not declare leaves the window on the first
 * section with the nav highlighting nothing.
 *
 * CAMPAIGNS IS FIRST and is therefore the section a window opens on: a
 * campaign is what this app is for, and the other four are the things a
 * campaign is made of. Somebody who lives in Templates says so in Settings.
 */
export const CAMPAIGNS_SECTIONS: OsAppSection[] = [
  { id: "campaigns", name: "Campaigns" },
  { id: "audiences", name: "Audiences" },
  { id: "templates", name: "Templates" },
  { id: "senders", name: "Senders" },
  { id: "rules", name: "Rules" },
  // The app's slice of the cluster's logs (epic memql#4895): the lines it
  // tagged and the lines about the things it owns. Admin-floored because
  // every read on the log store is (spec L3), and this is the ONE section
  // whose floor is not this app's to choose.
  { id: "logs", name: "Logs", roles: { min: "admin" } },
  { id: "settings", name: "Settings" },
];

export const CAMPAIGNS_SECTION_IDS = CAMPAIGNS_SECTIONS.map((s) => s.id);

export interface CampaignsSettings {
  version: 1;
  /** The section the app navigates to when its window opens. */
  defaultSection: string;
  /**
   * Whether finished campaigns, archived audiences and templates, and retired
   * senders are listed. OFF by default.
   *
   * ONE TOGGLE FOR FOUR LISTS, deliberately. The standing question all four
   * answer is what you are working with now, and somebody who wants to see a
   * filed-away audience is almost always looking for the campaign that used
   * it. Four separate switches would be four places to be in a different
   * state, and a list that is empty because of a switch on another screen is
   * the worst kind of empty.
   *
   * It is a FILTER over rows already here, not a second read. Every seed asks
   * for everything and the fold narrows -- seeding filtered would make
   * flipping the toggle re-run the read and re-baseline every arrival cue, so
   * revealing rows the browser already had would announce them as new.
   */
  showFiled: boolean;
  /**
   * What the open/click boxes are set to when a campaign is created here.
   *
   * A BROWSER PREFERENCE OVER A CLUSTER FIELD, and it is honest about being
   * exactly that: it decides what the form starts with and nothing else. The
   * campaign row carries its own `trackOpens` / `trackClicks` from the moment
   * it is created, so changing this never reaches a campaign that exists.
   * `createCampaign` defaults both to true, which is what this matches.
   */
  trackByDefault: boolean;
}

export const CAMPAIGNS_SETTINGS_KEY = "memql-os-campaigns-v1";

export const DEFAULT_CAMPAIGNS_SETTINGS: CampaignsSettings = {
  version: 1,
  // Named here as well as in the section list, because this value is what a
  // corrupt or absent document falls back to -- and falling back to "whatever
  // is first in an array" would move with an unrelated edit.
  defaultSection: "campaigns",
  showFiled: false,
  trackByDefault: true,
};

/**
 * Bring a parsed document back to invariants.
 *
 * Every field is repaired INDEPENDENTLY rather than the document being
 * rejected on the first bad value: a garbage `defaultSection` must not cost
 * somebody their tracking choice. A wrong `version` IS wholesale, because then
 * the field names cannot be trusted at all.
 */
export function sanitizeCampaignsSettings(raw: unknown): CampaignsSettings {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ...DEFAULT_CAMPAIGNS_SETTINGS };
  }
  const doc = raw as Partial<CampaignsSettings>;
  if (doc.version !== 1) return { ...DEFAULT_CAMPAIGNS_SETTINGS };

  const defaultSection =
    typeof doc.defaultSection === "string" && CAMPAIGNS_SECTION_IDS.includes(doc.defaultSection)
      ? doc.defaultSection
      : DEFAULT_CAMPAIGNS_SETTINGS.defaultSection;

  return {
    version: 1,
    defaultSection,
    showFiled:
      typeof doc.showFiled === "boolean" ? doc.showFiled : DEFAULT_CAMPAIGNS_SETTINGS.showFiled,
    trackByDefault:
      typeof doc.trackByDefault === "boolean"
        ? doc.trackByDefault
        : DEFAULT_CAMPAIGNS_SETTINGS.trackByDefault,
  };
}

export interface CampaignsSettingsStore {
  load(): CampaignsSettings;
  save(settings: CampaignsSettings): void;
}

export class LocalCampaignsSettingsStore implements CampaignsSettingsStore {
  /** `null` is "no storage" -- a private window and a full quota are normal
   *  cases, not failures. The default only replaces `undefined`. */
  constructor(
    private readonly storage:
      | Pick<Storage, "getItem" | "setItem">
      | null
      | undefined = globalThis.localStorage,
    private readonly key: string = CAMPAIGNS_SETTINGS_KEY,
  ) {}

  load(): CampaignsSettings {
    try {
      const raw = this.storage?.getItem(this.key);
      if (!raw) return { ...DEFAULT_CAMPAIGNS_SETTINGS };
      return sanitizeCampaignsSettings(JSON.parse(raw));
    } catch {
      // Unparseable JSON is the corrupt case sanitize exists for; it just
      // never gets that far, so the fallback is repeated here.
      return { ...DEFAULT_CAMPAIGNS_SETTINGS };
    }
  }

  save(settings: CampaignsSettings): void {
    try {
      this.storage?.setItem(this.key, JSON.stringify(settings));
    } catch {
      // Best-effort: a preference is not worth failing an interaction over.
    }
  }
}
