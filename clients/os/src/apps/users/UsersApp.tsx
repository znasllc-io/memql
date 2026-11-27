import { AccountAccessEditor } from "../accounts/AccountAccessEditor";
import { AccountPicker } from "../accounts/AccountPicker";
import { RoleTestingSection } from "./RoleTestingSection";
import { listCount } from "../../kit/RecordRow";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Concepts } from "@znasllc-io/memql-sdk-core/client";

import { Check, ContentSkeleton, Head, Notice, Panel, SetupGroup, gateFor } from "../../kit";
import { useSession, useSessionIfPresent } from "../../chrome/access";
import { AppLogsSection } from "../../logs/AppLogsSection";
import { useOsIfPresent } from "../../chrome/state";
import type { OsAppProps } from "../../system/registry";
import { useAccountOptions, useAccountOptionsFeed } from "../accounts/tie";
import { useUsersActions } from "./actions";
import { GroupsSection } from "./GroupsSection";
import { PeopleSection } from "./PeopleSection";
import { RolesSection } from "./RolesSection";
import { AccessSection } from "../settings/AccessSection";
import { groupFromRow, invitationFromRow, personFromRow } from "./rows";
import {
  DEFAULT_USERS_SETTINGS,
  LocalUsersSettingsStore,
  USERS_REQUIRES,
  USERS_SECTIONS,
  USERS_WANTS,
  type UsersSettings,
  type UsersSettingsStore,
} from "./settings";
import { MEMBERSHIP_CONCEPT, useGroups } from "./useGroups";
import { useInvites } from "./useInvites";
import { usePeople } from "./usePeople";
import { useRoleCatalog } from "./useRoles";

// Users: the people of this cluster, the groups they belong to, and the roles
// they hold (epic memql#5167).
//
// Recent People and Groups use bounded, searched directory feeds. Full people
// and invitation catalogs are demanded only by membership/authority surfaces;
// memberships remain scoped to one opened group or person. Role definitions
// are a bounded catalog, not a resource browser.

/** The concepts this app owns, for its Logs section. */
const USERS_LOG_CONCEPTS = [
  Concepts.IDENTITY_USER,
  Concepts.IDENTITY_INVITATION,
  Concepts.IDENTITY_GROUP,
  // The membership concept has no generated constant in this tree yet -- epic
  // memql#5165's last task regenerates the SDKs -- so the id is named through
  // the app's own export rather than duplicated as a literal here.
  MEMBERSHIP_CONCEPT,
  Concepts.RBAC_ROLE,
  Concepts.RBAC_CAPABILITY,
] as const;

export function UsersApp(props: OsAppProps & { store?: UsersSettingsStore }) {
  const access = useSessionIfPresent()?.access;
  // Access keeps the original settings/access door and its own reads. A
  // moved-section-only grant must not seed People, Groups or role catalogs.
  if (props.sectionId === "access") return <AccessSection intent={props.intent} consumeIntent={props.consumeIntent} />;
  if (access?.everyAccount === false && !["owner", "developer", "admin"].includes(access.role)) return <AccountUsersApp {...props} />;
  return <UsersDirectoryApp {...props} />;
}

function UsersDirectoryApp({
  sectionId,
  navigate,
  askContext,
  intent,
  consumeIntent,
  store,
}: OsAppProps & { store?: UsersSettingsStore }) {
  // Injectable for tests, which is the whole reason the parameter exists --
  // nothing in the shell passes one.
  const settingsStore = useMemo(() => store ?? new LocalUsersSettingsStore(), [store]);
  const [settings, setSettings] = useState<UsersSettings>(() => settingsStore.load());
  const actions = useUsersActions();
  const { access, readiness } = useSession();
  const os = useOsIfPresent();

  // An UNRESOLVED session is not an owner. `roleAdmits` refuses an unrankable
  // role, so "" admits only ungated controls -- the right answer while access
  // is still resolving, and the safe one if it never does.
  const viewerRole = access?.role ?? "";
  const viewerUserId = access?.userId ?? "";

  // A scoped group manager reads people through groupPeople on the opened
  // group, never through the cluster directory or invitation roster.
  const operator = access?.everyAccount === true;
  const [catalogDemand, setCatalogDemand] = useState(false);
  const users = usePeople(operator && (sectionId === "roles" || catalogDemand));
  const invites = useInvites(operator && catalogDemand);
  const groups = useGroups(sectionId === "people");
  const accounts = useAccountOptions(catalogDemand || sectionId === "roles");
  const catalog = useRoleCatalog();

  const people = useMemo(
    () => users.snapshot.rows.map(personFromRow).filter((p) => p.id !== ""),
    [users.snapshot],
  );
  const invitations = useMemo(
    () => invites.snapshot.rows.map(invitationFromRow).filter((i) => i.id !== ""),
    [invites.snapshot],
  );
  const groupRows = useMemo(
    () => groups.snapshot.rows.map(groupFromRow).filter((g) => g.id !== ""),
    [groups.snapshot],
  );

  // ONLY SENDING AN INVITATION NEEDS EMAIL, so no section declares `wants` and
  // the People Head asks the question for itself: the roster reads fine with no
  // mailbox, and an app that hid Invite with no account of itself would read as
  // a missing feature.
  const emailGate = gateFor(readiness, [], ["email"]);
  const emailReady = emailGate.state === "ready";

  function update(patch: Partial<UsersSettings>) {
    const next = { ...settings, ...patch, version: 1 as const };
    setSettings(next);
    settingsStore.save(next);
  }

  // THE DEFAULT-SECTION PREFERENCE, APPLIED ONCE PER WINDOW. The shell opens an
  // app on its manifest's FIRST section, so an app-level "open me here" can
  // only be this component navigating itself on its first render.
  const applied = useRef(false);
  useEffect(() => {
    if (applied.current) return;
    applied.current = true;
    // ONLY when the window opened on the SHELL's default. A window opened on a
    // named section was opened by somebody who said where they wanted to be --
    // the Accounts page's People band, say -- and a preference that overrode
    // that would make the deep link silently not work.
    const shellDefault = USERS_SECTIONS[0]?.id ?? "";
    if (sectionId !== shellDefault) return;
    if (intent) return;
    if (settings.defaultSection && settings.defaultSection !== sectionId) {
      navigate(settings.defaultSection);
    }
    // ONCE PER MOUNT, WHICH IS ONCE PER WINDOW.
  }, []);

  // The intent another app handed this window, consumed BY ID so acting on a
  // stale render can never eat a newer instruction.
  const intentGroupId = typeof intent?.payload["groupId"] === "string" ? (intent.payload["groupId"] as string) : "";
  const intentUserId = typeof intent?.payload["userId"] === "string" ? (intent.payload["userId"] as string) : "";
  const consume = useCallback(() => {
    if (intent) consumeIntent?.(intent.id);
  }, [intent, consumeIntent]);

  const openAccount = useCallback(
    (accountId: string) => os?.actions.openApp("accounts", "accounts", { accountId }),
    [os],
  );
  // OPENING A PERSON FROM ANOTHER SECTION. The person page lives inside the
  // People section's view union (one Head per view, rule 11), so a group's
  // member row cannot render it: it asks the app to go there, and the People
  // section opens it from `openId`. The same seam the Accounts page's intent
  // arrives on, used from inside.
  const [pendingPersonId, setPendingPersonId] = useState("");
  const openPerson = useCallback(
    (userId: string) => {
      setPendingPersonId(userId);
      navigate("people");
    },
    [navigate],
  );

  if (sectionId === "testing") return <RoleTestingSection role={viewerRole} />;
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
  if (sectionId === "groups") {
    return (
      <GroupsSection
        onCatalogDemand={setCatalogDemand}
        people={people}
        peopleAvailable={listCount(users.snapshot) !== undefined}
        invitations={invitations}
        invitationsAvailable={listCount(invites.snapshot) !== undefined}
        accounts={accounts}
        catalog={catalog}
        actions={actions}
        viewerRole={viewerRole}
        showArchived={settings.showArchivedGroups}
        openId={intentGroupId}
        onOpened={consume}
        onOpenAccount={openAccount}
        onOpenPerson={openPerson}
      />
    );
  }
  if (sectionId === "roles") {
    return (
      <RolesSection
        createForAccountId={intent?.payload["createRole"] === true && typeof intent?.payload["accountId"] === "string" ? intent.payload["accountId"] : ""}
        onOpened={consume}
        catalog={catalog}
        people={people}
        peopleAvailable={listCount(users.snapshot) !== undefined}
        accounts={accounts}
        actions={actions}
        viewerRole={viewerRole}
        onOpenPerson={openPerson}
      />
    );
  }
  return (
    <PeopleSection
      enabled={operator}
      peopleCatalog={people}
      onCatalogDemand={setCatalogDemand}
      groups={groupRows}
      accounts={accounts}
      catalog={catalog}
      actions={actions}
      viewerUserId={viewerUserId}
      viewerRole={viewerRole}
      showDeactivated={settings.showDeactivated}
      sort={settings.sort}
      emailReady={emailReady}
      emailGateSentence="Sending an invitation needs a mailbox. Settings is where one is wired."
      askContext={askContext}
      openId={pendingPersonId || intentUserId}
      onOpened={() => {
        setPendingPersonId("");
        consume();
      }}
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
  const { readiness } = useSession();
  return (
    <div className="os-settings">
      <Head title="Users settings" />
      {/* THE SET UP GROUP sits above the preferences on purpose: it is the
          reason a person was sent here from an unconfigured surface, and the
          first thing they need is what to configure and where. Rule 4 puts
          micro-preferences in Settings; it never said they come first. */}
      <SetupGroup app="Users" requires={USERS_REQUIRES} wants={USERS_WANTS} readiness={readiness} />
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

        <fieldset className="os-field-group">
          <legend>Archived groups</legend>
          <Check
            checked={settings.showArchivedGroups}
            onChange={(next) => update({ showArchivedGroups: next })}
          >
            List archived groups
          </Check>
          <p className="os-caption">
            Off by default, for the same reason. An archived group grants nothing, so the standing
            question the list answers is which groups are placing people right now.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>Order people by</legend>
          <div className="os-choice-row" role="radiogroup" aria-label="Default order">
            {(
              [
                { id: "recent", name: "Recent" },
                { id: "name", name: "Name" },
                { id: "lastSeen", name: "Last seen" },
              ] as const
            ).map((option) => (
              <button
                key={option.id}
                type="button"
                role="radio"
                aria-checked={settings.sort === option.id}
                className="os-choice"
                onClick={() => update({ sort: option.id })}
              >
                {option.name}
              </button>
            ))}
          </div>
          <p className="os-caption">
            The list's own order control changes what you are looking at now; this is where it
            starts.
          </p>
        </fieldset>

        <p className="os-caption">
          These are kept in this browser, separately from your desktop, so an app learning a
          checkbox can never cost you your desks. The defaults are{" "}
          {DEFAULT_USERS_SETTINGS.defaultSection} with deactivated people and archived groups
          hidden.
        </p>
      </Panel>
    </div>
  );
}

function AccountUsersApp({ intent, consumeIntent }: OsAppProps) {
  const feed = useAccountOptionsFeed(true), accounts = feed.accounts;
  const [selected, setSelected] = useState("");
  const [group, setGroup] = useState("");
  const accountId = selected || accounts[0]?.id || "";
  useEffect(() => {
    const next = intent?.payload["groupId"];
    const account = intent?.payload["accountId"];
    if (typeof account === "string" && typeof next === "string") { setSelected(account); setGroup(next === `acct-${account}` ? "" : next); if (intent) consumeIntent?.(intent.id); }
    else if (typeof next === "string" && next.startsWith("acct-")) { setSelected(next.slice(5)); setGroup(""); if (intent) consumeIntent?.(intent.id); }
  }, [intent, consumeIntent]);
  return <div className="os-action-pane"><div className="os-action-body os-app-stack">
    <Head title="People and groups" />
    <AccountPicker id="users-account" label="Account" required accounts={accounts} value={accountId} onChange={value => { setSelected(value); setGroup(""); }} />
    {group ? <button className="os-button" onClick={() => setGroup("")}>All people</button> : null}
    {accountId ? <AccountAccessEditor key={`${accountId}:${group}`} accountId={accountId} groupId={group || undefined} onOpenGroup={setGroup} /> : feed.error ? <Notice tone="warn" sentence="Accounts could not be loaded." detail={feed.error} /> : feed.state === "seeding" ? <ContentSkeleton label="Loading accounts" /> : <p className="os-caption">No accounts are available to manage.</p>}
  </div></div>;
}
