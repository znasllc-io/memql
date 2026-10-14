import { useEffect, useMemo, useState } from "react";

import { useSession } from "../../../chrome/access";
import { ContentSkeleton, Field, Head, SetupGroup, canConfigure } from "../../../kit";
import { OrganizationSendingDomain } from "../../../modules/connections/AzureEmailConnections";
import { CampaignPackageConfig } from "../../../modules/connections/CampaignPackageConfig";
import { AccountPicker } from "../../accounts/AccountPicker";
import { useDefaultOrganization } from "../../accounts/organization";
import { useAccountOptionsFeed } from "../../accounts/tie";
import type { CampaignWrites } from "../actions";
import { organizationName, selectableOrganizations } from "../organizations";
import { recordNamed, useOpenSettingsRequest, type SettingsRequest } from "../openRequest";
import { senderIdentityFromRow } from "../rows";
import { EMAIL_REQUIRES, EMAIL_WANTS, type EmailSettings } from "../settings";
import type { EmailFeeds, EmailRecordFeeds } from "../useEmail";
import { AddSenderWizard } from "./AddSenderWizard";
import { PreferencesPanel } from "./PreferencesPanel";
import { SenderPage } from "./SenderPage";
import { SendersPanel } from "./SendersPanel";
import { TestingPanel } from "./TestingPanel";

// SETTINGS: how Email is set up to send, and how this browser opens it.
//
// ONE PAGE, GROUPED BY WHOSE IT IS (memql#6033, Ruling 4). The Set up group
// leads, because it is where the gear's "not set up" mark points. Then the
// organization -- asked ONCE, and only when there is a choice -- scopes
// everything an organization owns: its sending domain, its senders, its testing
// audience and its package. Preferences come last because they belong to this
// browser, not to an organization. The page used to ask for the organization
// three times, once per subject, and a person could be looking at three
// different organizations at once.
//
// THE PAGE GIVES WAY. A sender's page, the Add a sender wizard and the sending
// domain's wizard and page each take the pane, as every list's page does: the
// window draws its one trail row for the FIRST heading on the pane, so a page
// drawn beneath Settings' own heading would have no way back. What Settings
// drew stays MOUNTED behind them, parked, so an unsaved choice survives a visit.

type Page = { kind: "list" } | { kind: "add" } | { kind: "sender"; id: string };

export function SettingsSection({ feeds, recordFeeds, writes, settings, update, request, onOrganization }: {
  feeds: EmailFeeds;
  recordFeeds: EmailRecordFeeds;
  writes: CampaignWrites;
  settings: EmailSettings;
  update: (patch: Partial<EmailSettings>) => void;
  /** Open Settings on one organization (`useOpenSettings`); acted on once per revision. */
  request?: SettingsRequest;
  /**
   * Told which organization the page is on. The app root reads an organization's
   * testing audience for the organizations its lists show; an organization with
   * no campaign or audience is not among them, and this is how the page asks.
   */
  onOrganization: (accountId: string) => void;
}) {
  const { access, readiness } = useSession();
  const { accounts, state: accountsState } = useAccountOptionsFeed();
  const choices = useMemo(() => selectableOrganizations(accounts), [accounts]);
  const fallback = useDefaultOrganization(accounts);
  const [chosen, setChosen] = useState("");
  const [page, setPage] = useState<Page>({ kind: "list" });
  const [domainOpen, setDomainOpen] = useState(false);
  // Bumped when a package is imported, which prepares an organization's domain
  // and so makes the domain read itself again.
  const [imported, setImported] = useState(0);

  useOpenSettingsRequest(request, (accountId) => {
    setChosen(accountId);
    setPage({ kind: "list" });
  });

  // THE ORGANIZATION: what was chosen or asked for, if it can be chosen; else
  // the only one there is; else where this person starts (the operator's own,
  // or their one membership). With several and no start, nothing until they choose.
  const accountId = choices.some((choice) => choice.id === chosen)
    ? chosen
    : choices.length === 1
      ? choices[0]!.id
      : choices.some((choice) => choice.id === fallback)
        ? fallback
        : "";
  useEffect(() => { onOrganization(accountId); }, [accountId, onOrganization]);
  const organization = organizationName(accounts, accountId);
  const mayConfigure = canConfigure(access?.role ?? "");

  // A sender's page opens whichever sender its id names in the app's WHOLE
  // feed, however the list is filtered; one the feed has not delivered yet
  // leaves the list on screen and the page opens when it does.
  const sender = page.kind === "sender" ? recordNamed(feeds.senders.snapshot.rows, senderIdentityFromRow, page.id) : null;
  const adding = page.kind === "add";
  const away = adding || sender !== null;

  return (
    <div className="os-settings os-settings-wide os-app-stack email-settings">
      <div className="email-pane" hidden={away}>
        <div className="email-pane" hidden={domainOpen}>
          <Head title="Settings" />
          <SetupGroup app="Email" requires={EMAIL_REQUIRES} wants={EMAIL_WANTS} readiness={readiness} />
          {choices.length > 1 ? (
            <div className="email-field">
              <Field label="Organization">
                <AccountPicker
                  id="email-settings-organization"
                  label="Organization"
                  required
                  accounts={choices}
                  value={accountId}
                  onChange={setChosen}
                />
              </Field>
            </div>
          ) : null}
          {accountId === "" && choices.length === 0 && accountsState === "seeding" ? (
            <ContentSkeleton kind="form" label="Loading organizations" />
          ) : null}
        </div>

        {accountId === "" ? null : (
          <OrganizationSendingDomain
            key={`${accountId}:${imported}`}
            accountId={accountId}
            name={organization}
            backLabel="Settings"
            onPane={setDomainOpen}
          />
        )}

        <div className="email-pane" hidden={domainOpen}>
          {accountId === "" ? null : (
            <>
              <SendersPanel
                feeds={feeds}
                accountId={accountId}
                includeArchived={settings.includeArchived}
                onOpen={(id) => setPage({ kind: "sender", id })}
                onAdd={() => setPage({ kind: "add" })}
              />
              <TestingPanel key={`testing:${accountId}`} accountId={accountId} audiences={feeds.audiences} testSettings={recordFeeds.testSettings} />
              {/* A package is read and written by an owner or developer, and by
                  nobody else: for the rest it is absent rather than refused. */}
              {mayConfigure ? (
                <CampaignPackageConfig
                  key={`package:${accountId}`}
                  accountId={accountId}
                  organizationName={organization}
                  onImported={() => setImported((count) => count + 1)}
                />
              ) : null}
            </>
          )}
          <PreferencesPanel settings={settings} update={update} />
        </div>
      </div>

      {adding ? (
        <AddSenderWizard
          organizations={choices}
          initialAccountId={accountId}
          writes={writes}
          onBack={() => setPage({ kind: "list" })}
          onCreated={(id) => setPage({ kind: "sender", id })}
        />
      ) : null}
      {sender === null ? null : (
        <SenderPage
          key={sender.id}
          sender={sender}
          organization={organizationName(accounts, sender.accountId)}
          writes={writes}
          onBack={() => setPage({ kind: "list" })}
        />
      )}
    </div>
  );
}
