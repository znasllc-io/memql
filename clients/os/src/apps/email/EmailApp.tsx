import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Concepts } from "@znasllc-io/memql-sdk-core/client";

import { useAuthSource } from "../../auth/context";
import { EdgeUploadProvider } from "../../items/edgeUpload";
import type { UploadProvider } from "../../items/upload";
import { AppLogsSection } from "../../logs/AppLogsSection";
import { accessAdmits, type OsAppProps } from "../../system/registry";
import { AccountOptionsProvider } from "../accounts/tie";
import { AudiencesSection } from "./AudiencesSection";
import { CampaignsSection } from "./campaigns/CampaignsSection";
import { OverviewSection } from "./overview/OverviewSection";
import { RulesSection } from "./RulesSection";
import { TemplatesSection } from "./templates/TemplatesSection";
import { useCampaignWrites } from "./actions";
import {
  EmailNavigationContext,
  useEmailRequests,
  useOpenRecord,
  type OpenSection,
  type Requests,
} from "./openRequest";
import { InboxSection } from "./inbox/InboxSection";
import {
  EMAIL_OPENING_TABS,
  EMAIL_SECTIONS,
  EMAIL_SECTION_IDS,
  LocalEmailSettingsStore,
  type EmailSettings,
  type EmailSettingsStore } from "./settings";
import { SettingsSection } from "./settings/SettingsSection";
import { useEmailFeeds, useEmailRecordFeeds } from "./useEmail";
import "./email.css";

// What a section outside this directory reaches for to open a record or start
// an Add flow, and the type of the request a section is handed.
export { useOpenRecord, useOpenSettings } from "./openRequest";
export type { OpenRequest } from "./openRequest";

// Email: writing mail, sending it, and knowing what happened (epic memql#4827 /
// #4828 / #4830), and the capture inbox that shows what a test cluster did
// with it instead of delivering it. ONE APP, ONE RESOURCE FAMILY (`app:email`,
// `app:email/inbox`, `app:email/logs`) and ONE preferences store (memql#6033):
// a second app for the same mail would be a second set of grants, a second
// window and a second place to look for what happened to a message.
//
// ===========================================================================
// FIVE FEEDS AT THE ROOT, ONE PER CONCEPT -- AND THREE MORE, COMPOSED
// ===========================================================================
// The one-feed rule is per CONCEPT, not per app (the Packages rule): what must
// never happen is two subscriptions over the SAME concept, free to disagree
// about what the cluster holds. Five concepts cannot disagree with each other.
//
// They are retained HERE rather than per section because every one of them is
// needed in more than one place -- the campaign editor picks an audience, a
// template and a mailbox; the rules builder picks the same three; the template
// editor lists the campaigns that use it. A per-section feed would open a
// second subscription over one concept the moment somebody opened two
// sections, which is exactly the failure the rule names.
//
// A series, a testing audience and a signup form are read per id (the series
// OF this campaign), and used to be one collection per open page. Each is now
// ONE collection here, composed over the ids the five lists are showing
// (`useEmailRecordFeeds`), and the account list every picker reads is retained
// once under `AccountOptionsProvider` -- so with a campaign page, an audience
// page and Settings all open, no concept has two subscriptions.
//
// SECTIONS ARE RETAINED PANES (Fleet's pattern). A section stays mounted,
// hidden, once it has been opened, so a wizard's answers and an edit's unsaved
// changes survive a tab switch -- and a hidden pane's `AutoRefresh` stops, so
// parking a tab costs no reads.
//
// NO MANIFEST ROLE, and the reason is the concepts' tier rather than a
// judgment made here: every operator-facing campaigns concept is
// organization-tier, so every signed-in person has mail of their own to read
// and the engine decides how far each list reaches. Gating the app would be
// presentation pretending to be authorization. Two sections ARE gated, each on
// its own resource: the capture inbox, because what it holds (sign-in links
// for any test recipient) is not an organization's to read, and Logs, which
// carries the log store's floor.

/** The concepts this app owns, for its Logs section: everything a send is
 *  made of, plus the engine-owned job that carries it out, plus the messages
 *  a capture cluster keeps instead of delivering. */
const EMAIL_LOG_CONCEPTS = [
  Concepts.CAMPAIGNS_CAMPAIGN,
  Concepts.CAMPAIGNS_AUDIENCE,
  Concepts.CAMPAIGNS_TEMPLATE,
  Concepts.CAMPAIGNS_SENDER_IDENTITY,
  Concepts.CAMPAIGNS_EMAIL_RULE,
  Concepts.CAMPAIGNS_SEND_JOB,
  Concepts.CAMPAIGNS_DELIVERY,
  Concepts.EMAIL_CAPTURED_MESSAGE,
] as const;

/** The records search and Ask can name, and the tab that holds each. These are
 *  the `idField`s the manifest declares in `records`; a test pins the two. */
const RECORD_INTENTS: readonly { idField: string; section: OpenSection }[] = [
  { idField: "campaignId", section: "campaigns" },
  { idField: "audienceId", section: "audiences" },
  { idField: "templateId", section: "templates" },
];

type EmailAppProps = OsAppProps & { store?: EmailSettingsStore; uploads?: UploadProvider };

// OPENING A RECORD FROM ELSEWHERE is one request per tab, held HERE and handed
// to the tab by name (`openRequest.ts` carries the why). The window's intent
// (search, Ask) and the pages inside the app (a link to an audience, an Add
// that has to leave for another tab) both make one, through the same openers.
export function EmailApp(props: EmailAppProps) {
  const { requests, navigation } = useEmailRequests(props.navigate);
  return (
    <EmailNavigationContext.Provider value={navigation}>
      <EmailSections {...props} requests={requests} />
    </EmailNavigationContext.Provider>
  );
}

function EmailSections({
  sectionId,
  navigate,
  intent,
  consumeIntent,
  store,
  uploads,
  requests,
}: EmailAppProps & { requests: Requests }) {
  // Injectable for tests, which is the whole reason these parameters exist --
  // nothing in the shell passes either.
  const settingsStore = useMemo(() => store ?? new LocalEmailSettingsStore(), [store]);
  const [settings, setSettings] = useState<EmailSettings>(() => settingsStore.load());
  const authSource = useAuthSource();

  // THE SHELL'S ONE UPLOAD PATH. `items/edgeUpload.ts` is the only place in
  // clients/os that speaks the artifact upload wire (test/files/onePath.test.ts
  // fails the build on a second speaker), so the CSV import inherits chunking,
  // resume, retry, progress and verbatim refusals without learning any of them.
  const uploadProvider = useMemo(
    () => uploads ?? new EdgeUploadProvider(() => authSource.bearer()),
    [uploads, authSource],
  );

  const feeds = useEmailFeeds();
  // THE ORGANIZATIONS SETTINGS HAS BEEN ON. The composed feeds read the
  // organizations the lists show; an organization with no campaign or audience
  // is not among them, and Settings asks for the one it is on. They accumulate,
  // so choosing back an organization already visited reads nothing again.
  const [settingsOrganizations, setSettingsOrganizations] = useState<readonly string[]>([]);
  const noteOrganization = useCallback(
    (accountId: string) => setSettingsOrganizations((held) => (accountId === "" || held.includes(accountId) ? held : [...held, accountId])),
    [],
  );
  // THE CAMPAIGNS WHOSE PAGES HAVE OPENED. The series feed reads every draft's
  // series; a campaign that is no longer a draft may still have one running, and
  // its page is where it is paused. They accumulate, as the organizations do.
  const [pageCampaigns, setPageCampaigns] = useState<readonly string[]>([]);
  const noteCampaign = useCallback(
    (campaignId: string) => setPageCampaigns((held) => (campaignId === "" || held.includes(campaignId) ? held : [...held, campaignId])),
    [],
  );
  const recordFeeds = useEmailRecordFeeds(feeds, settingsOrganizations, pageCampaigns);
  const writes = useCampaignWrites();

  // THE OPEN INTENT (epic memql#4842, #4845): "show this campaign, this
  // audience, this template", from search and from Ask. Consumed by id, so
  // acting on a stale render can never eat a newer instruction, and a payload
  // naming nothing this app opens is consumed and ignored rather than left
  // standing to re-fire on every render. Logs' own payload (a subject to
  // narrow to) is taken by that section first: effects run child-first.
  const openRecord = useOpenRecord();
  const handledIntent = useRef("");
  useEffect(() => {
    if (!intent || intent.id === handledIntent.current) return;
    handledIntent.current = intent.id;
    for (const { idField, section } of RECORD_INTENTS) {
      const id = intent.payload[idField];
      if (typeof id === "string" && id !== "") {
        openRecord(section, { id });
        break;
      }
    }
    consumeIntent?.(intent.id);
  }, [intent, consumeIntent, openRecord]);

  function updateSettings(patch: Partial<EmailSettings>) {
    const next = { ...settings, ...patch, version: 1 as const };
    setSettings(next);
    settingsStore.save(next);
  }

  // THE DEFAULT-SECTION PREFERENCE, APPLIED ONCE PER WINDOW -- the pattern
  // every app since Fleet has used. The shell opens an app on its manifest's
  // FIRST section, so an app-level "open me here" can only be the app
  // navigating itself on first render.
  const applied = useRef(false);
  useEffect(() => {
    if (applied.current) return;
    applied.current = true;
    // ONLY when the window opened on the SHELL's default. A window opened on a
    // named section was opened by somebody who said where they wanted to be,
    // and a preference that overrode that would make a deep link silently not
    // work.
    const shellDefault = EMAIL_SECTIONS[0]?.id ?? "";
    if (sectionId !== shellDefault) return;
    // NOR WHEN AN INSTRUCTION OPENED IT: the intent above sends the window to
    // the tab that holds the record, and a preference navigating after it
    // would win.
    if (intent) return;
    // NOR TO A TAB THIS PERSON CANNOT OPEN. The preference is the browser's, and
    // a browser is shared: what an owner chose is not a tab a writer can reach,
    // and a window sent there would show nothing.
    const preferred = EMAIL_OPENING_TABS.find((tab) => tab.id === settings.defaultSection);
    if (preferred && accessAdmits(preferred.requires) && preferred.id !== sectionId) {
      navigate(preferred.id);
    }
    // ONCE PER MOUNT, WHICH IS ONCE PER WINDOW.
  }, []);

  // A section the manifest does not declare reads as the campaigns list, as
  // it always has.
  const current = EMAIL_SECTION_IDS.includes(sectionId) ? sectionId : "campaigns";
  const [visited, setVisited] = useState<string[]>([current]);
  useEffect(() => { setVisited(held => held.includes(current) ? held : [...held, current]); }, [current]);
  const panes = Array.from(new Set([...visited, current]));

  function section(id: string, active: boolean): ReactNode {
    switch (id) {
      case "overview":
        return <OverviewSection feeds={feeds} />;
      case "inbox":
        return <InboxSection />;
      case "settings":
        return (
          <SettingsSection
            feeds={feeds}
            recordFeeds={recordFeeds}
            writes={writes}
            settings={settings}
            update={updateSettings}
            request={requests.settings}
            onOrganization={noteOrganization}
          />
        );
      case "logs":
        return (
          <AppLogsSection
            app="email"
            subjectConcepts={EMAIL_LOG_CONCEPTS}
            // Only the pane in view takes an instruction: a parked Logs pane
            // would otherwise consume one meant for the section on screen.
            intent={active ? intent : undefined}
            consumeIntent={consumeIntent}
          />
        );
      case "audiences":
        return <AudiencesSection feeds={feeds} recordFeeds={recordFeeds} writes={writes} uploads={uploadProvider} includeArchived={settings.includeArchived} openRequest={requests.audiences} />;
      case "templates":
        return <TemplatesSection feeds={feeds} includeArchived={settings.includeArchived} openRequest={requests.templates} />;
      case "automations":
        return <RulesSection feeds={feeds} writes={writes} includeArchived={settings.includeArchived} openRequest={requests.automations} />;
      default:
        return (
          <CampaignsSection
            feeds={feeds}
            recordFeeds={recordFeeds}
            trackByDefault={settings.trackByDefault}
            openRequest={requests.campaigns}
            onCampaign={noteCampaign}
          />
        );
    }
  }

  return (
    <AccountOptionsProvider>
      {panes.map(id => {
        const declared = EMAIL_SECTIONS.find(candidate => candidate.id === id);
        // Re-checked on every render: a pane parked behind another tab goes
        // the moment the person's access to it does.
        if (!declared || !accessAdmits(declared.requires)) return null;
        const active = id === current;
        return <div className="email-pane" data-email-section={id} key={id} hidden={!active}>{section(id, active)}</div>;
      })}
    </AccountOptionsProvider>
  );
}
