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

/**
 * The tabs a window can OPEN ON: every section but Logs and Settings. Those two
 * are the header's icons, reached by their own control in every app, and a
 * window that opened on one would open on the place a person goes to leave the
 * work. Which of these a given person may open is their access's to say
 * (`accessAdmits`), so the preference lists these and the picker narrows them.
 */
export const EMAIL_OPENING_TABS: OsAppSection[] = EMAIL_SECTIONS.filter((s) => s.id !== "logs" && s.id !== "settings");
const EMAIL_OPENING_TAB_IDS = EMAIL_OPENING_TABS.map((s) => s.id);

export interface EmailSettings {
  version: 1;
  /** The tab a new window opens on (one of `EMAIL_OPENING_TABS`). */
  defaultSection: string;
  /**
   * Whether archived things are listed: archived audiences and templates,
   * archived senders and, for now, paused rules. OFF by default. Finished
   * campaigns are always listed; which of them a list shows is a question asked
   * through its Refine (Ruling 5).
   *
   * ONE TOGGLE FOR EVERY LIST, deliberately. The standing question all of them
   * answer is what you are working with now, and somebody who wants to see an
   * archived audience is almost always looking for the campaign that used
   * it. Separate switches would be separate places to be in a different state,
   * and a list that is empty because of a switch on another screen is the
   * worst kind of empty.
   *
   * It is a FILTER over rows already here, not a second read. Every seed asks
   * for everything and the fold narrows -- seeding filtered would make
   * flipping the toggle re-run the read and re-baseline every arrival cue, so
   * revealing rows the browser already had would announce them as new.
   */
  includeArchived: boolean;
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
  includeArchived: false,
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

  // A tab a window can open on. A section the app does not declare, one it
  // used to, and the two header icons all fall back to the first.
  const defaultSection =
    typeof doc.defaultSection === "string" && EMAIL_OPENING_TAB_IDS.includes(doc.defaultSection)
      ? doc.defaultSection
      : DEFAULT_EMAIL_SETTINGS.defaultSection;

  return {
    version: 1,
    defaultSection,
    includeArchived:
      typeof doc.includeArchived === "boolean" ? doc.includeArchived : DEFAULT_EMAIL_SETTINGS.includeArchived,
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
