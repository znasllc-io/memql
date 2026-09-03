import { useEffect, useMemo, useRef, useState } from "react";
import { Concepts } from "@znasllc-io/memql-sdk-core/client";

import { Check, Head, Panel } from "../../kit";
import { useSession } from "../../chrome/access";
import { AppLogsSection } from "../../logs/AppLogsSection";
import type { OsAppProps } from "../../system/registry";
import { useUsersActions } from "./actions";
import { InvitesSection } from "./InvitesSection";
import { PeopleSection } from "./PeopleSection";
import {
  DEFAULT_USERS_SETTINGS,
  LocalUsersSettingsStore,
  USERS_SECTIONS,
  type UsersSettings,
  type UsersSettingsStore,
} from "./settings";

// Users: the people of this cluster, the invitations outstanding, and the
// three admin actions the identity service exposes (epic memql#4733).
//
// The manifest declares `roles: { min: "admin" }`, which under the cluster's
// one ladder is rank >= 200 = {admin, developer, owner}. Developer is in that
// set because it holds the ADMISSION capability -- it may invite people and
// mint enrolment links -- and NOT because it can manage them (memql#4917).
//
// All of it is PRESENTATION (spec section E): `searchUsers` and
// `pendingUserInvitations` carry `requiresDeveloperOrAbove` in their own
// filters, `adminops` gates every write behind one of its TWO gates (the
// admission four, the owner/admin ten), and row admission gates the
// subscriptions. Hiding a control here is a courtesy to the person reading,
// never the boundary.
//
// Sections are the app's own navigation. It never opens a window.

/** The concepts this app owns, for its Logs section. */
const USERS_LOG_CONCEPTS = [Concepts.IDENTITY_USER, Concepts.IDENTITY_INVITATION] as const;

export function UsersApp({
  sectionId,
  navigate,
  intent,
  consumeIntent,
  store,
}: OsAppProps & { store?: UsersSettingsStore }) {
  // Injectable for tests, which is the whole reason the parameter exists --
  // nothing in the shell passes one.
  const settingsStore = useMemo(() => store ?? new LocalUsersSettingsStore(), [store]);
  const [settings, setSettings] = useState<UsersSettings>(() => settingsStore.load());
  const actions = useUsersActions();
  const { access } = useSession();
  // An UNRESOLVED session is not an owner. `roleAdmits` refuses an unrankable
  // role, so "" admits only ungated controls -- which is the right answer
  // while access is still resolving, and the safe one if it never does.
  const ownerRole = access?.clusterRole ?? "";

  function update(patch: Partial<UsersSettings>) {
    const next = { ...settings, ...patch, version: 1 as const };
    setSettings(next);
    settingsStore.save(next);
  }

  // THE DEFAULT-SECTION PREFERENCE, APPLIED ONCE PER WINDOW -- the Fleet's
  // pattern, and its reasoning holds unchanged. The shell opens an app on its
  // manifest's FIRST section, so an app-level "open me here" can only be the
  // app navigating itself on the first render of this component instance.
  const applied = useRef(false);
  useEffect(() => {
    if (applied.current) return;
    applied.current = true;
    // ONLY when the window opened on the SHELL's default. A window opened on
    // a named section was opened by somebody who said where they wanted to
    // be -- the Settings apps index deep-linking to this app's own settings,
    // say -- and a preference that overrode that would make the deep link
    // silently not work (memql#4743).
    const shellDefault = USERS_SECTIONS[0]?.id ?? "";
    if (sectionId !== shellDefault) return;
    if (settings.defaultSection && settings.defaultSection !== sectionId) {
      navigate(settings.defaultSection);
    }
    // ONCE PER MOUNT, WHICH IS ONCE PER WINDOW. Re-running on a section change
    // would drag somebody back to their default the moment they navigated
    // away.
  }, []);

  if (sectionId === "settings") {
    return <UsersSettingsSection settings={settings} update={update} />;
  }
  if (sectionId === "logs") {
    return (
      <AppLogsSection
        app="users"
        subjectConcepts={USERS_LOG_CONCEPTS}
        intent={intent}
        consumeIntent={consumeIntent}
      />
    );
  }
  if (sectionId === "invites") {
    return <InvitesSection actions={actions} ownerRole={ownerRole} />;
  }
  return (
    <PeopleSection
      showDeactivated={settings.showDeactivated}
      actions={actions}
      ownerRole={ownerRole}
    />
  );
}

function UsersSettingsSection({
  settings,
  update,
}: {
  settings: UsersSettings;
  update: (patch: Partial<UsersSettings>) => void;
}) {
  return (
    <div className="os-settings">
      <Head title="Users settings" />
      <Panel label="Users settings">
        <fieldset className="os-field-group">
          <legend>Open Users on</legend>
          <div className="os-choice-row" role="radiogroup" aria-label="Default section">
            {USERS_SECTIONS.map((section) => (
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
            Applies the next time a Users window opens. It does not move the window you are looking
            at.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>Deactivated people</legend>
          <Check
            checked={settings.showDeactivated}
            onChange={(next) => update({ showDeactivated: next })}
          >
            List deactivated and suspended accounts
          </Check>
          <p className="os-caption">
            Off by default. The standing question the People list answers is who can currently reach
            this cluster; a list padded with retired accounts makes the live ones harder to see.
            Deactivated rows are marked and never read as current.
          </p>
        </fieldset>

        <p className="os-caption">
          These are kept in this browser, separately from your desktop, so an app learning a
          checkbox can never cost you your desks. The defaults are{" "}
          {DEFAULT_USERS_SETTINGS.defaultSection} with deactivated people hidden.
        </p>
      </Panel>
    </div>
  );
}
