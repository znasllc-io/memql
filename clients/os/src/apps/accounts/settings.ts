// The Accounts app's own settings, and the store that keeps them.
//
// Same discipline as the Users app's (apps/users/settings.ts) and a separate
// key for the same reason: the SHELL's DesktopDocument rejects a version it
// does not know, so folding per-app preferences into it would mean a person
// loses their desks because an app learned a checkbox.

import type { OsAppSection } from "../../system/registry";

/**
 * The sections this app declares, in manifest order.
 *
 * Exported because the manifest and the settings picker must offer the SAME
 * set. A second literal is one that can disagree, and a preference naming a
 * section the manifest does not declare leaves the window on the first
 * section with the nav highlighting nothing.
 */
export const ACCOUNTS_SECTIONS: OsAppSection[] = [
  { id: "accounts", name: "Accounts" },
  // The credentials an operator has issued on behalf of a BILLING account
  // (memql#5013). ITS OWN SECTION, not a panel inside a client's detail, and
  // the reason is that the two are different concepts: a credential is minted
  // against `v1:identity:account` and the Accounts section lists
  // `v1:accounts:account`. They share the word and nothing else -- no field,
  // no reference -- so a panel hung on a client row was refused on every mint,
  // with the engine's zero-rows permission error as the only clue.
  //
  // NO ROLE FLOOR, for the reason this app declares none: `v1:identity:account`
  // is the plain owned tier (`@rowAuthz(owner="ownerUserId")`, no cluster-owner
  // escape), so the engine already answers "which of these are yours" and a
  // floor written here would be presentation pretending to be authorization.
  { id: "credentials", name: "Credentials" },
  // The app's slice of the cluster's logs (epic memql#4895): the lines it
  // tagged and the lines about the things it owns. Admin-floored because
  // every read on the log store is (spec L3), and this is the ONE section
  // whose floor is not this app's to choose.
  { id: "logs", name: "Logs", roles: { min: "admin" } },
  { id: "settings", name: "Settings" },
];

export const ACCOUNTS_SECTION_IDS = ACCOUNTS_SECTIONS.map((s) => s.id);

export interface AccountsSettings {
  version: 1;
  /** The section the app navigates to when its window opens. */
  defaultSection: string;
  /**
   * Whether archived clients are listed. OFF by default (D8).
   *
   * It is a FILTER over rows already here, not a second read.
   * `clientAccountsAll` takes an `includeArchived` argument and this toggle
   * deliberately does NOT pass it: seeding filtered would make flipping the
   * toggle re-run the read and re-baseline every arrival cue, so revealing
   * rows the browser already had would announce them as new. The seed asks
   * for everything and the fold narrows -- the same shape the Files browse
   * uses for the same reason.
   */
  showArchived: boolean;
}

export const ACCOUNTS_SETTINGS_KEY = "memql-os-accounts-v1";

export const DEFAULT_ACCOUNTS_SETTINGS: AccountsSettings = {
  version: 1,
  // Accounts first, per the manifest. Named here as well because this value
  // is what a corrupt or absent document falls back to, and falling back to
  // "whatever is first in an array" would move with an unrelated edit.
  defaultSection: "accounts",
  showArchived: false,
};

/**
 * Bring a parsed document back to invariants.
 *
 * Every field is repaired INDEPENDENTLY rather than the document being
 * rejected on the first bad value: a garbage `defaultSection` must not cost
 * somebody their show-archived choice. A wrong `version` IS wholesale,
 * because then the field names cannot be trusted at all.
 */
export function sanitizeAccountsSettings(raw: unknown): AccountsSettings {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ...DEFAULT_ACCOUNTS_SETTINGS };
  }
  const doc = raw as Partial<AccountsSettings>;
  if (doc.version !== 1) return { ...DEFAULT_ACCOUNTS_SETTINGS };

  const defaultSection =
    typeof doc.defaultSection === "string" && ACCOUNTS_SECTION_IDS.includes(doc.defaultSection)
      ? doc.defaultSection
      : DEFAULT_ACCOUNTS_SETTINGS.defaultSection;

  return {
    version: 1,
    defaultSection,
    showArchived:
      typeof doc.showArchived === "boolean"
        ? doc.showArchived
        : DEFAULT_ACCOUNTS_SETTINGS.showArchived,
  };
}

export interface AccountsSettingsStore {
  load(): AccountsSettings;
  save(settings: AccountsSettings): void;
}

export class LocalAccountsSettingsStore implements AccountsSettingsStore {
  /** `null` is "no storage" -- a private window and a full quota are normal
   *  cases, not failures. The default only replaces `undefined`. */
  constructor(
    private readonly storage:
      | Pick<Storage, "getItem" | "setItem">
      | null
      | undefined = globalThis.localStorage,
    private readonly key: string = ACCOUNTS_SETTINGS_KEY,
  ) {}

  load(): AccountsSettings {
    try {
      const raw = this.storage?.getItem(this.key);
      if (!raw) return { ...DEFAULT_ACCOUNTS_SETTINGS };
      return sanitizeAccountsSettings(JSON.parse(raw));
    } catch {
      // Unparseable JSON is the corrupt case sanitize exists for; it just
      // never gets that far, so the fallback is repeated here.
      return { ...DEFAULT_ACCOUNTS_SETTINGS };
    }
  }

  save(settings: AccountsSettings): void {
    try {
      this.storage?.setItem(this.key, JSON.stringify(settings));
    } catch {
      // Best-effort: a preference is not worth failing an interaction over.
    }
  }
}
