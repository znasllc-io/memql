import { NeedsConfiguration } from "./NeedsConfiguration";
import { CampaignsOverview } from "./CampaignsOverview";
import { useEffect, useMemo, useRef, useState } from "react";
import { Concepts } from "@znasllc-io/memql-sdk-core/client";

import { useAuthSource } from "../../auth/context";
import { EdgeUploadProvider } from "../../items/edgeUpload";
import type { UploadProvider } from "../../items/upload";
import { Check, Head, Panel, SetupGroup } from "../../kit";
import { AppLogsSection } from "../../logs/AppLogsSection";
import type { OsAppProps } from "../../system/registry";
import { AudiencesSection } from "./AudiencesSection";
import { CampaignsSection } from "./CampaignsSection";
import { RulesSection } from "./RulesSection";
import { SendersSection } from "./SendersSection";
import { TemplatesSection } from "./TemplatesSection";
import { useCampaignWrites } from "./actions";
import {
  CAMPAIGNS_SECTIONS,
  DEFAULT_CAMPAIGNS_SETTINGS,
  LocalCampaignsSettingsStore,
  type CampaignsSettings,
  type CampaignsSettingsStore, CAMPAIGNS_REQUIRES, CAMPAIGNS_WANTS } from "./settings";
import { useCampaignFeeds, useEmailReadiness } from "./useCampaigns";
import { useSession } from "../../chrome/access";

// Campaigns: writing mail, sending it, and knowing what happened (epic
// memql#4827 / #4828 / #4830).
//
// ===========================================================================
// FIVE FEEDS AT THE ROOT, ONE PER CONCEPT
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
// NO MANIFEST ROLE, and the reason is the concepts' tier rather than a
// judgment made here: every operator-facing campaigns concept declares the
// composite tier, so every signed-in person has campaigns of their own to read
// and the engine decides how far each list reaches. Gating the app would be
// presentation pretending to be authorization.

/** The concepts this app owns, for its Logs section: everything a send is
 *  made of, plus the engine-owned job that carries it out. */
const CAMPAIGNS_LOG_CONCEPTS = [
  Concepts.CAMPAIGNS_CAMPAIGN,
  Concepts.CAMPAIGNS_AUDIENCE,
  Concepts.CAMPAIGNS_TEMPLATE,
  Concepts.CAMPAIGNS_SENDER_IDENTITY,
  Concepts.CAMPAIGNS_EMAIL_RULE,
  Concepts.CAMPAIGNS_SEND_JOB,
  Concepts.CAMPAIGNS_DELIVERY,
] as const;

export function CampaignsApp({
  sectionId,
  navigate,
  intent,
  consumeIntent,
  store,
  uploads,
}: OsAppProps & { store?: CampaignsSettingsStore; uploads?: UploadProvider }) {
  // Injectable for tests, which is the whole reason these parameters exist --
  // nothing in the shell passes either.
  const settingsStore = useMemo(() => store ?? new LocalCampaignsSettingsStore(), [store]);
  const [settings, setSettings] = useState<CampaignsSettings>(() => settingsStore.load());
  const authSource = useAuthSource();

  // THE SHELL'S ONE UPLOAD PATH. `items/edgeUpload.ts` is the only place in
  // clients/os that speaks the artifact upload wire (test/files/onePath.test.ts
  // fails the build on a second speaker), so the CSV import inherits chunking,
  // resume, retry, progress and verbatim refusals without learning any of them.
  const uploadProvider = useMemo(
    () => uploads ?? new EdgeUploadProvider(() => authSource.bearer()),
    [uploads, authSource],
  );

  const feeds = useCampaignFeeds();
  const writes = useCampaignWrites();
  const email = useEmailReadiness();

  function updateSettings(patch: Partial<CampaignsSettings>) {
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
    const shellDefault = CAMPAIGNS_SECTIONS[0]?.id ?? "";
    if (sectionId !== shellDefault) return;
    if (settings.defaultSection && settings.defaultSection !== sectionId) {
      navigate(settings.defaultSection);
    }
    // ONCE PER MOUNT, WHICH IS ONCE PER WINDOW.
  }, []);

  if (sectionId === "overview") return <CampaignsOverview feeds={feeds} navigate={navigate} />;

  if (sectionId === "settings") {
    return <CampaignsSettingsSection settings={settings} update={updateSettings} />;
  }
  if (sectionId === "logs") {
    return (
      <AppLogsSection
        app="campaigns"
        subjectConcepts={CAMPAIGNS_LOG_CONCEPTS}
        intent={intent}
        consumeIntent={consumeIntent}
      />
    );
  }

  return (
    <>
      {sectionId !== "campaigns" ? <NeedsConfiguration email={email} /> : null}
      {sectionId === "audiences" ? (
        <AudiencesSection
          feeds={feeds}
          writes={writes}
          uploads={uploadProvider}
          showFiled={settings.showFiled}
        />
      ) : sectionId === "templates" ? (
        <TemplatesSection
          feeds={feeds}
          writes={writes}
          showFiled={settings.showFiled}
        />
      ) : sectionId === "senders" ? (
        <SendersSection
          feeds={feeds}
          writes={writes}
          showFiled={settings.showFiled}
        />
      ) : sectionId === "rules" ? (
        <RulesSection
          feeds={feeds}
          writes={writes}
          showFiled={settings.showFiled}
        />
      ) : (
        <CampaignsSection
          feeds={feeds}
          writes={writes}
          showFiled={settings.showFiled}
          trackByDefault={settings.trackByDefault}
          uploads={uploadProvider}
          email={email}
        />
      )}
    </>
  );
}

function CampaignsSettingsSection({
  settings,
  update,
}: {
  settings: CampaignsSettings;
  update: (patch: Partial<CampaignsSettings>) => void;
}) {
  const { readiness } = useSession();
  return (
    <div className="os-settings">
      <Head title="Campaigns settings" />
      {/* THE SET UP GROUP sits above the preferences on purpose: it is the
          reason a person was sent here from an unconfigured surface, and the
          first thing they need is what to configure and where. Rule 4 puts
          micro-preferences in Settings; it never said they come first. */}
      <SetupGroup
        app="Campaigns"
        requires={CAMPAIGNS_REQUIRES}
        wants={CAMPAIGNS_WANTS}
        readiness={readiness}
      />
      <Panel label="Campaigns settings">
        <fieldset className="os-field-group">
          <legend>Open Campaigns on</legend>
          <div className="os-choice-row" role="radiogroup" aria-label="Default section">
            {CAMPAIGNS_SECTIONS.map((section) => (
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
            Applies the next time a Campaigns window opens. It does not move the window you are
            looking at.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>Finished and filed-away things</legend>
          <Check checked={settings.showFiled} onChange={(showFiled) => update({ showFiled })}>
            List finished campaigns, archived audiences and templates, and retired mailboxes
          </Check>
          <p className="os-caption">
            Off by default. The standing question these lists answer is what you are working with
            now; a list padded with last quarter&apos;s sends makes the current ones harder to find.
            Nothing is ever deleted -- past sends keep naming what they used.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>New campaigns</legend>
          <Check
            checked={settings.trackByDefault}
            onChange={(trackByDefault) => update({ trackByDefault })}
          >
            Start with open and click tracking switched on
          </Check>
          <p className="os-caption">
            This decides what the boxes are set to on the new-campaign form and nothing else. Each
            campaign keeps its own answer from the moment it is created, so changing this never
            reaches one that already exists.
          </p>
        </fieldset>

        <p className="os-caption">
          These are kept in this browser, separately from your desktop, so an app learning a checkbox
          can never cost you your desks. The defaults are{" "}
          {DEFAULT_CAMPAIGNS_SETTINGS.defaultSection} with finished things hidden and tracking on.
        </p>
      </Panel>
    </div>
  );
}
