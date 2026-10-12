// The Email app's own settings, and the store that keeps them.
//
// Same discipline as the Accounts app's (apps/accounts/settings.ts) and a
// separate key for the same reason: the SHELL's DesktopDocument rejects a
// version it does not know, so folding per-app preferences into it would mean
// a person loses their desks because an app learned a checkbox.

import type { ModuleId } from "../../system/modules";
import type { OsAppSection } from "../../system/registry";

/**
 * The sections this app declares, in manifest order.
 *
 * Exported because the manifest and the settings picker must offer the SAME
 * set. A second literal is one that can disagree, and a preference naming a
 * section the manifest does not declare leaves the window on the first
 * section with the nav highlighting nothing.
 *
 * Overview opens with the current state and setup direction. Resource
 * lists and guided authoring keep their own destinations.
 *
 * THE TABS ARE THE APP'S NOUNS (DESIGN.md, "A tab is a different kind of
 * thing"): campaigns, automations, audiences, templates. A sender is not one
 * of them -- it is how the app is set up to send, so it lives in Settings --
 * and a rule is one kind of automation, so it is listed under Automations
 * rather than beside it.
 */
export const EMAIL_SECTIONS: OsAppSection[] = [
  { id: "overview", name: "Overview" },
  { id: "campaigns", name: "Campaigns" },
  { id: "automations", name: "Automations", needs: ["email", "campaigns"] },
  { id: "audiences", name: "Audiences" },
  { id: "templates", name: "Templates" },
  // The capture inbox: what a test cluster kept instead of delivering. Owner
  // and developer only, because a captured message can hold a sign-in link
  // for any test recipient -- the same two roles the seeds admit to
  // `app:email/inbox`.
  { id: "inbox", name: "Inbox", requires: "app:email/inbox" },
  // The app's slice of the cluster's logs (epic memql#4895): the lines it
  // tagged and the lines about the things it owns. Admin-floored because
  // every read on the log store is (spec L3), and this is the ONE section
  // whose floor is not this app's to choose.
  { id: "logs", name: "Logs", requires: "app:email/logs" },
  { id: "settings", name: "Settings" },
];

export const EMAIL_SECTION_IDS = EMAIL_SECTIONS.map((s) => s.id);

export interface EmailSettings {
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

export const EMAIL_SETTINGS_KEY = "memql-os-email-v1";

export const DEFAULT_EMAIL_SETTINGS: EmailSettings = {
  version: 1,
  // Named here as well as in the section list, because this value is what a
  // corrupt or absent document falls back to -- and falling back to "whatever
  // is first in an array" would move with an unrelated edit.
  defaultSection: "overview",
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
export function sanitizeEmailSettings(raw: unknown): EmailSettings {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ...DEFAULT_EMAIL_SETTINGS };
  }
  const doc = raw as Partial<EmailSettings>;
  if (doc.version !== 1) return { ...DEFAULT_EMAIL_SETTINGS };

  const defaultSection =
    typeof doc.defaultSection === "string" && EMAIL_SECTION_IDS.includes(doc.defaultSection)
      ? doc.defaultSection
      : DEFAULT_EMAIL_SETTINGS.defaultSection;

  return {
    version: 1,
    defaultSection,
    showFiled:
      typeof doc.showFiled === "boolean" ? doc.showFiled : DEFAULT_EMAIL_SETTINGS.showFiled,
    trackByDefault:
      typeof doc.trackByDefault === "boolean"
        ? doc.trackByDefault
        : DEFAULT_EMAIL_SETTINGS.trackByDefault,
  };
}

export interface EmailSettingsStore {
  load(): EmailSettings;
  save(settings: EmailSettings): void;
}

export class LocalEmailSettingsStore implements EmailSettingsStore {
  /** `null` is "no storage" -- a private window and a full quota are normal
   *  cases, not failures. The default only replaces `undefined`. */
  constructor(
    private readonly storage:
      | Pick<Storage, "getItem" | "setItem">
      | null
      | undefined = globalThis.localStorage,
    private readonly key: string = EMAIL_SETTINGS_KEY,
  ) {}

  load(): EmailSettings {
    try {
      const raw = this.storage?.getItem(this.key);
      if (!raw) return { ...DEFAULT_EMAIL_SETTINGS };
      return sanitizeEmailSettings(JSON.parse(raw));
    } catch {
      // Unparseable JSON is the corrupt case sanitize exists for; it just
      // never gets that far, so the fallback is repeated here.
      return { ...DEFAULT_EMAIL_SETTINGS };
    }
  }

  save(settings: EmailSettings): void {
    try {
      this.storage?.setItem(this.key, JSON.stringify(settings));
    } catch {
      // Best-effort: a preference is not worth failing an interaction over.
    }
  }
}

/** Draft preparation and setup remain available before email is configured.
 * Actual send controls separately require fresh affirmative readiness. */
export const EMAIL_REQUIRES: readonly ModuleId[] = [];
export const EMAIL_WANTS: readonly ModuleId[] = ["email", "campaigns"];
