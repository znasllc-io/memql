import { useEffect, useMemo, useRef, useState } from "react";
import { Concepts } from "@znasllc-io/memql-sdk-core/client";

import { Check, Head, Panel } from "../../kit";
import { AppLogsSection } from "../../logs/AppLogsSection";
import { useOsIfPresent } from "../../chrome/state";
import type { OsAppProps } from "../../system/registry";
import { AccountsSection } from "./AccountsSection";
import { FirstRunCard } from "./FirstRunCard";
import { useArchiveAccount, useCreateAccount, useUpdateAccount } from "./actions";
import { accountFromRow, needsFirstRun, SELF_ACCOUNT_ID, type AccountRow } from "./rows";
import { useAccountRecord } from "./useAccounts";
import {
  ACCOUNTS_SECTIONS,
  DEFAULT_ACCOUNTS_SETTINGS,
  LocalAccountsSettingsStore,
  type AccountsSettings,
  type AccountsSettingsStore,
} from "./settings";

// Accounts: the client registry (epic memql#4800).
//
// AN ORDINARY APP. Always-docked is the Bin's distinction (#4784) and this
// claims nothing special -- it opens from the launcher, lives in a window,
// navigates its own sections, and never opens another app's.
//
// NO MANIFEST ROLE, and the reason is the concept's tier rather than a
// judgment made here: `v1:accounts:account` declares the composite tier, so
// every signed-in person has accounts of their own to read and the engine
// decides how far the list reaches. Gating the app would be presentation
// pretending to be authorization.

/** The concept this app owns, for its Logs section. */
const ACCOUNTS_LOG_CONCEPTS = [Concepts.ACCOUNTS_ACCOUNT] as const;

export function AccountsApp({
  sectionId,
  navigate,
  intent,
  consumeIntent,
  store,
}: OsAppProps & { store?: AccountsSettingsStore }) {
  // Injectable for tests, which is the whole reason the parameter exists --
  // nothing in the shell passes one.
  const settingsStore = useMemo(() => store ?? new LocalAccountsSettingsStore(), [store]);
  const [settings, setSettings] = useState<AccountsSettings>(() => settingsStore.load());
  const create = useCreateAccount();
  const update = useUpdateAccount();
  const archive = useArchiveAccount();

  function updateSettings(patch: Partial<AccountsSettings>) {
    const next = { ...settings, ...patch, version: 1 as const };
    setSettings(next);
    settingsStore.save(next);
  }

  // THE DEFAULT-SECTION PREFERENCE, APPLIED ONCE PER WINDOW -- the pattern
  // every app since Fleet has used, and its reasoning holds unchanged. The
  // shell opens an app on its manifest's FIRST section, so an app-level "open
  // me here" can only be the app navigating itself on first render.
  const applied = useRef(false);
  useEffect(() => {
    if (applied.current) return;
    applied.current = true;
    // ONLY when the window opened on the SHELL's default. A window opened on
    // a named section was opened by somebody who said where they wanted to be
    // -- the Settings apps index deep-linking here, say -- and a preference
    // that overrode that would make the deep link silently not work.
    const shellDefault = ACCOUNTS_SECTIONS[0]?.id ?? "";
    if (sectionId !== shellDefault) return;
    if (settings.defaultSection && settings.defaultSection !== sectionId) {
      navigate(settings.defaultSection);
    }
    // ONCE PER MOUNT, WHICH IS ONCE PER WINDOW.
  }, []);

  if (sectionId === "settings") {
    return <AccountsSettingsSection settings={settings} update={updateSettings} />;
  }
  // CREDENTIALS IS ITS OWN SECTION AND SHARES NOTHING WITH THE REST OF THIS
  // APP (memql#5013). It reads `v1:identity:account` -- the paying account of the
  // isolation model -- while everything else in this app reads
  // `v1:accounts:account`, the client registry. The two share the word and
  // nothing else, so the section takes no props from here: not the feed, not
  // the write states, not the settings. A shared prop would be the first step
  // back towards the placement this fixed.
  if (sectionId === "logs") {
    return (
      <AppLogsSection
        app="accounts"
        subjectConcepts={ACCOUNTS_LOG_CONCEPTS}
        intent={intent}
        consumeIntent={consumeIntent}
      />
    );
  }

  return (
    <AccountsSurface
      settings={settings}
      update={update}
      create={create}
      archive={archive}
      openId={typeof intent?.payload.accountId === "string" ? intent.payload.accountId : ""}
      onOpened={() => { if (intent) consumeIntent?.(intent.id); }}
    />
  );
}

/** Setup reads the self record directly; it can be older than every list page. */
function AccountsSurface({
  settings,
  update,
  create,
  archive,
  openId,
  onOpened,
}: {
  openId: string;
  onOpened: () => void;
  settings: AccountsSettings;
  update: ReturnType<typeof useUpdateAccount>;
  create: ReturnType<typeof useCreateAccount>;
  archive: ReturnType<typeof useArchiveAccount>;
}) {
  const { snapshot } = useAccountRecord(SELF_ACCOUNT_ID);
  // `useOsIfPresent`, not `useOs`: a section rendered in a test with no shell
  // around it is not a bug, and "there is nowhere to hand off to" is exactly
  // what null means. The People band renders its count either way and offers
  // the handoff only when there is one.
  const os = useOsIfPresent();

  const self: AccountRow | null = useMemo(() => {
    const found = snapshot.rows.map(accountFromRow).find((a) => a.id === SELF_ACCOUNT_ID);
    return found ?? null;
  }, [snapshot]);

  const settled = snapshot.state === "live" || snapshot.state === "degraded";

  if (settled && needsFirstRun(self) && self !== null) {
    return (
      <FirstRunCard
        account={self}
        update={update}
        // NOTHING TO DO ON SAVE. `updateClientAccount` stamps `configuredAt`,
        // the update broadcasts, the feed folds it, and this component
        // re-renders with the gate false. The callback exists so the card can
        // clear its own error state; re-reading here would race the broadcast
        // and could put the card back for a frame.
        onSaved={() => update.reset()}
      />
    );
  }

  return (
    <AccountsSection
      showArchived={settings.showArchived}
      initialOpenId={openId}
      onOpened={onOpened}
      create={create}
      update={update}
      archive={archive}
      // OPENING THE PEOPLE BAND IS AN APP HANDOFF, not a panel: membership is
      // managed on the group's own page, and a members list here would be a
      // second place to manage it. The intent is consumed by id on the far
      // side, so acting on a stale render cannot eat a newer instruction.
      onOpenGroup={(groupId) => os?.actions.openApp("users", "groups", { groupId })}
    />
  );
}

function AccountsSettingsSection({
  settings,
  update,
}: {
  settings: AccountsSettings;
  update: (patch: Partial<AccountsSettings>) => void;
}) {
  return (
    <div className="os-settings">
      <Head title="Accounts settings" />
      <Panel label="Accounts settings">
        <fieldset className="os-field-group">
          <legend>Open Accounts on</legend>
          <div className="os-choice-row" role="radiogroup" aria-label="Default section">
            {ACCOUNTS_SECTIONS.map((section) => (
              <button
                key={section.id}
                type="button"
                role="radio"
                aria-checked={settings.defaultSection === section.id}
                className="os-choice"
                onClick={() => update({ defaultSection: section.id })}
              >
                {section.name}
              </button>
            ))}
          </div>
          <p className="os-caption">
            Applies the next time an Accounts window opens. It does not move the window you are
            looking at.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>Archived clients</legend>
          <Check
            checked={settings.showArchived}
            onChange={(next) => update({ showArchived: next })}
          >
            List clients you have filed away
          </Check>
          <p className="os-caption">
            Off by default. The standing question this list answers is who you are working for now;
            a list padded with former clients makes the current ones harder to see. Archived rows
            are marked and never read as current.
          </p>
        </fieldset>

        <p className="os-caption">
          These are kept in this browser, separately from your desktop, so an app learning a
          checkbox can never cost you your desks. The defaults are{" "}
          {DEFAULT_ACCOUNTS_SETTINGS.defaultSection} with archived clients hidden.
        </p>
      </Panel>
    </div>
  );
}
