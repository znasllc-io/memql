import { useEffect, useState } from "react";
import type { ReactNode } from "react";

import { useAuth } from "../auth/AuthProvider";
import {
  Badge,
  Band,
  Button,
  ButtonLink,
  Callout,
  Checkbox,
  ConfirmDialog,
  ErrorNotice,
  Field,
  Panel,
  Select,
  Skeleton,
  TextInput,
} from "../ui";
import { ExternalLink } from "../ui/icons";
import { IDENTITY_EXPORT, IDENTITY_SETTINGS, identityPath } from "./urls";
import type { MeAccount, MePreferences } from "./useMe";
import type { MeState } from "./useMe";
import { COMPUTER_USE_GROUP, useMyPreferences } from "./useMyPreferences";

// /me/settings -- the user-level settings the portal owns (memql#4523).
//
// THE HOME FOR USER SETTINGS. Everything settings-shaped is viewed and changed
// here, or linked from here when another surface owns it. The identity service
// owns changing your email, exporting your data and deleting the account; those
// are named in the "Identity and data" band rather than left for a reader to
// go looking for, because a capability that moved and a capability that is gone
// look identical from a page that mentions neither.
//
// # Groups are curated, not generated from the schema
//
// `v1:identity:user.preferences` is a closed block of twelve keys, and a
// generated form would render them in declaration order with their field names
// as labels. What a person changes together is not what the schema declares
// together: the daily-space trio is one decision with three knobs, and the
// retention select only means anything beside the rollover action.
//
// # Two keys are deliberately NOT here
//
// PORTAL THEME is the header toggle, not `preferences.theme`. The toggle is
// per-browser BY DESIGN (src/app/theme.ts) -- a console read on a laptop and a
// wall display should not have to agree about dark mode. The server-side
// `preferences.theme` this page writes is the one product SPAs read, which is
// why it is offered here and why changing it does not repaint this page. That
// is confusing enough to be worth the hint the control carries.
//
// activeAssistantId is not rendered at all. It is an app-managed pointer
// (memql#406) that product SPAs stamp as a side effect of picking an assistant
// -- a value with no meaning to a person reading a settings page, and one they
// would break by editing.
//
// # The kill switch is not one of the saved fields
//
// Every other control here batches into its group's Save. The computer-use kill
// switch is IMMEDIATE and CONFIRMED, and it goes through its own mutation
// (toggleComputerUseEnabled) because updateMyPreferences cannot reach the key
// -- see useMyPreferences.ts. Disabling it suspends running plans, so it gets
// a confirm; re-enabling gets one too, more briefly, because turning a safety
// switch back on should also be deliberate.

const GROUP_LOCALE = "locale";
const GROUP_NOTIFICATIONS = "notifications";
const GROUP_DAILY_SPACE = "dailySpace";
const GROUP_SESSIONS = "sessions";

export function SettingsTab({ me }: { me: MeState }): ReactNode {
  const { config } = useAuth();
  const writes = useMyPreferences(me);
  const account = me.account;

  if (me.error !== "") {
    return (
      <ErrorNotice
        sentence="Could not read your settings."
        next="Reload the page to read them again."
        detail={me.error}
      />
    );
  }
  if (account === null) {
    return (
      <Panel>
        <Skeleton variant="kv" rows={6} />
      </Panel>
    );
  }

  return <SettingsForm account={account} writes={writes} identityUrl={config.identityUrl} />;
}

// SettingsForm exists so the draft state can be SEEDED from a resolved account
// rather than from null. A single component would need the hooks above the
// null check, and would then hold a draft built from defaults that the arriving
// row silently disagrees with.
function SettingsForm({
  account,
  writes,
  identityUrl,
}: {
  account: MeAccount;
  writes: ReturnType<typeof useMyPreferences>;
  identityUrl: string;
}): ReactNode {
  const prefs = account.preferences;
  const [draft, setDraft] = useState<MePreferences>(prefs);
  const [confirmingComputerUse, setConfirmingComputerUse] = useState(false);

  // Re-seed whenever the row changes. Every write ends in a re-read, so this is
  // what makes the form show the stored value after a save -- and, on a
  // refusal, what puts the rejected value back to what the cluster still holds
  // instead of leaving it on screen looking saved.
  useEffect(() => {
    setDraft(prefs);
  }, [prefs]);

  const set = (patch: Partial<MePreferences>): void => setDraft((d) => ({ ...d, ...patch }));
  const busy = (group: string): boolean => writes.busyGroup === group;
  const error = (group: string): string => writes.errors[group] ?? "";

  return (
    <div className="flex flex-col gap-6">
      <SettingsGroup
        title="Locale"
        meta="How dates and language are presented to you"
        group={GROUP_LOCALE}
        writes={writes}
        onSave={() =>
          writes.save(GROUP_LOCALE, { language: draft.language, timezone: draft.timezone })
        }
      >
        <Field
          label="Language"
          hint="A BCP 47 tag, for example en-US or es-MX. Leave empty for the cluster default."
        >
          <TextInput
            value={draft.language}
            onChange={(next) => set({ language: next })}
            placeholder="en-US"
            disabled={busy(GROUP_LOCALE)}
          />
        </Field>
        {/* Free text rather than a picker: the IANA database has ~600 zones and
            grows, so a curated list is a list that goes stale. The server
            validates the SHAPE and the daily-space cron reads the value. */}
        <Field
          label="Timezone"
          hint="An IANA name, for example America/Los_Angeles. Empty falls back to UTC. This is what decides when 'today' begins for your daily space."
        >
          <TextInput
            value={draft.timezone}
            onChange={(next) => set({ timezone: next })}
            placeholder="America/Los_Angeles"
            disabled={busy(GROUP_LOCALE)}
          />
        </Field>
        <Field
          label="Theme for product apps"
          hint="This console's own light/dark switch is in the header and is per-browser by design; it is not this setting. This one is the theme apps built on the cluster read."
        >
          <Select
            ariaLabel="Theme for product apps"
            value={draft.theme}
            onChange={(next) => set({ theme: next })}
            disabled={busy(GROUP_LOCALE)}
          >
            <option value="system">Match the system</option>
            <option value="light">Light</option>
            <option value="dark">Dark</option>
          </Select>
        </Field>
      </SettingsGroup>

      <SettingsGroup
        title="Notifications"
        group={GROUP_NOTIFICATIONS}
        writes={writes}
        onSave={() => writes.save(GROUP_NOTIFICATIONS, { notifications: draft.notifications })}
      >
        <Checkbox
          checked={draft.notifications}
          onChange={(next) => set({ notifications: next })}
          disabled={busy(GROUP_NOTIFICATIONS)}
          label="Send me notifications"
        />
      </SettingsGroup>

      <SettingsGroup
        title="Daily space"
        meta="A per-day space, provisioned and rolled over for you"
        group={GROUP_DAILY_SPACE}
        writes={writes}
        onSave={() =>
          writes.save(GROUP_DAILY_SPACE, {
            dailySpaceEnabled: draft.dailySpaceEnabled,
            dailySpaceRolloverAction: draft.dailySpaceRolloverAction,
            archiveRetentionDays: draft.archiveRetentionDays,
          })
        }
      >
        <Checkbox
          checked={draft.dailySpaceEnabled}
          onChange={(next) => set({ dailySpaceEnabled: next })}
          disabled={busy(GROUP_DAILY_SPACE)}
          label="Give me a daily space"
          hint="A space is provisioned for you each day, and yesterday's is rolled over."
        />
        <Field
          label="At rollover"
          hint="What happens to yesterday's space. Archived spaces age out; saved ones are kept indefinitely."
        >
          <Select
            ariaLabel="At rollover"
            value={draft.dailySpaceRolloverAction}
            onChange={(next) => set({ dailySpaceRolloverAction: next })}
            disabled={busy(GROUP_DAILY_SPACE) || !draft.dailySpaceEnabled}
          >
            <option value="archive">Archive it</option>
            <option value="save">Keep it</option>
          </Select>
        </Field>
        {/* A select, not a number box: 30 and 60 are the two documented
            choices, and the server refuses anything outside that window. An
            open number field would invite a value that comes back refused. */}
        <Field
          label="Keep archived spaces for"
          hint="The purge sweep reads your current value, so raising this rescues spaces that are already archived."
        >
          <Select
            ariaLabel="Keep archived spaces for"
            value={String(draft.archiveRetentionDays)}
            onChange={(next) => set({ archiveRetentionDays: Number(next) })}
            disabled={busy(GROUP_DAILY_SPACE) || draft.dailySpaceRolloverAction !== "archive"}
          >
            <option value="30">30 days</option>
            <option value="60">60 days</option>
          </Select>
        </Field>
      </SettingsGroup>

      <SettingsGroup
        title="Sessions and voice"
        meta="How an agent behaves while it is working with you"
        group={GROUP_SESSIONS}
        writes={writes}
        onSave={() =>
          writes.save(GROUP_SESSIONS, {
            voiceMode: draft.voiceMode,
            interactivePace: draft.interactivePace,
            takeoverMode: draft.takeoverMode,
            cursorTweenMs: draft.cursorTweenMs,
          })
        }
      >
        <Field
          label="Microphone"
          hint="Toggle: click on, click off. Continuous: the mic stays open and speech detection gates it."
        >
          <Select
            ariaLabel="Microphone"
            value={draft.voiceMode}
            onChange={(next) => set({ voiceMode: next })}
            disabled={busy(GROUP_SESSIONS)}
          >
            <option value="toggle">Click to talk</option>
            <option value="continuous">Always listening</option>
          </Select>
        </Field>
        <Field
          label="Pace"
          hint="Cursor speed in conversational sessions -- walkthroughs, demos, teaching flows."
        >
          <Select
            ariaLabel="Pace"
            value={draft.interactivePace}
            onChange={(next) => set({ interactivePace: next })}
            disabled={busy(GROUP_SESSIONS)}
          >
            <option value="quick">Quick</option>
            <option value="steady">Steady</option>
            <option value="deliberate">Deliberate</option>
          </Select>
        </Field>
        <Field
          label="During a takeover"
          hint="Clicks are blocked across the viewport either way; this only changes the visual cue."
        >
          <Select
            ariaLabel="During a takeover"
            value={draft.takeoverMode}
            onChange={(next) => set({ takeoverMode: next })}
            disabled={busy(GROUP_SESSIONS)}
          >
            <option value="clean">Leave the app visible</option>
            <option value="dim">Dim, with a spotlight</option>
          </Select>
        </Field>
        <Field
          label="Cursor travel time"
          hint="Milliseconds for the agent's cursor to move between targets, from 250 to 2500. Separate from Pace: this one is for transactional sessions."
        >
          <TextInput
            type="number"
            value={String(draft.cursorTweenMs)}
            onChange={(next) => set({ cursorTweenMs: Number(next) })}
            disabled={busy(GROUP_SESSIONS)}
          />
        </Field>
      </SettingsGroup>

      {/* The kill switch. Its own band, its own mutation, no Save button --
          this one takes effect on confirm. */}
      <Band title="Computer use">
        <div className="flex flex-col gap-3 rounded-lg border border-line bg-surface p-3">
          <p className="max-w-prose text-sm text-muted">
            {prefs.computerUseEnabled
              ? "Agents may act on your machine when you have granted them scope. Turning this off refuses every dispatch, whatever else is granted."
              : "Computer use is off for this account. Every dispatch is refused with kill_switch_engaged, and it stays off across sessions, devices and restarts until you turn it back on."}
          </p>
          <div className="flex flex-wrap items-center gap-3">
            {/* A COMMAND, not a toggle -- the same reasoning as the sign-in
                switch on the Security tab: the label says what pressing it
                does, and the Badge says what is true now, so aria-pressed on
                top of a changing label would announce a contradiction. */}
            <Button
              tone={prefs.computerUseEnabled ? "danger" : "primary"}
              busy={writes.busyGroup === COMPUTER_USE_GROUP}
              busyLabel="Saving"
              onClick={() => setConfirmingComputerUse(true)}
            >
              {prefs.computerUseEnabled ? "Turn computer use off" : "Turn computer use back on"}
            </Button>
            <Badge tone={prefs.computerUseEnabled ? "neutral" : "ok"}>
              {prefs.computerUseEnabled ? "Enabled" : "Kill switch engaged"}
            </Badge>
          </div>
          {error(COMPUTER_USE_GROUP) === "" ? null : (
            <Callout tone="danger" title="That change was refused">
              {error(COMPUTER_USE_GROUP)}
            </Callout>
          )}
          <p className="max-w-prose text-sm text-subtle">
            Only you can change this. There is no administrative override.
          </p>
        </div>
      </Band>

      {/* One door per destination (memql#4264). These live on identity's own
          pages and are named here so a reader knows the capability moved
          rather than concluding it is gone. Rendered only when an identity
          origin is configured: a link to nowhere is worse than an absent one. */}
      <IdentityAndData identityUrl={identityUrl} />

      <ConfirmDialog
        open={confirmingComputerUse}
        title={prefs.computerUseEnabled ? "Turn computer use off?" : "Turn computer use back on?"}
        confirmLabel={prefs.computerUseEnabled ? "Turn it off" : "Turn it on"}
        tone={prefs.computerUseEnabled ? "danger" : "primary"}
        busy={writes.busyGroup === COMPUTER_USE_GROUP}
        onCancel={() => setConfirmingComputerUse(false)}
        onConfirm={() => {
          setConfirmingComputerUse(false);
          writes.setComputerUseEnabled(!prefs.computerUseEnabled);
        }}
      >
        {prefs.computerUseEnabled ? (
          <p>
            Every worker dispatch will be refused with kill_switch_engaged, and any running plan
            with computer-use scope moves to awaiting feedback. This is sticky: it survives
            sessions, devices and restarts, and only you can re-enable it.
          </p>
        ) : (
          <p>
            Agents will be able to act on your machine again wherever you have already granted them
            scope. Plans that were suspended by the switch do not resume on their own.
          </p>
        )}
      </ConfirmDialog>
    </div>
  );
}

// SettingsGroup is the band + the explicit Save + the group's own error line.
// Explicit saves per group, the admin SettingsPage convention: a settings page
// that writes on every keystroke cannot tell a half-typed timezone from a
// finished one, and the server refuses the half-typed one.
function SettingsGroup({
  title,
  meta,
  group,
  writes,
  onSave,
  children,
}: {
  title: string;
  meta?: string;
  group: string;
  writes: ReturnType<typeof useMyPreferences>;
  onSave: () => void;
  children: ReactNode;
}): ReactNode {
  const message = writes.errors[group] ?? "";
  return (
    <Band title={title} {...(meta === undefined ? {} : { meta })}>
      <form
        className="flex flex-col gap-3 rounded-lg border border-line bg-surface p-3"
        onSubmit={(event) => {
          event.preventDefault();
          onSave();
        }}
      >
        {children}
        {message === "" ? null : (
          <Callout tone="danger" title="That change was refused">
            {message}
          </Callout>
        )}
        {/* A stacked form's submit, so a bare div rather than FormActions --
            that component exists to align a button with the fields BESIDE it
            in a FormRow, and its label-height spacer would be a stray gap
            here. Default size (sm) deliberately: `sm` IS the control line, and
            an xs button is the 26px-beside-a-36px-field shape memql#4504
            removed. Same as admin/SettingsPage's stacked save. */}
        <div>
          <Button type="submit" busy={writes.busyGroup === group} busyLabel="Saving…">
            Save
          </Button>
        </div>
      </form>
    </Band>
  );
}

function IdentityAndData({ identityUrl }: { identityUrl: string }): ReactNode {
  const settings = identityPath(identityUrl, IDENTITY_SETTINGS);
  const exportPath = identityPath(identityUrl, IDENTITY_EXPORT);
  if (settings === "" && exportPath === "") return null;

  return (
    <Band title="Identity and data" meta="Managed on the identity service">
      <div className="flex flex-col gap-3 rounded-lg border border-line bg-surface p-3">
        <p className="max-w-prose text-sm text-muted">
          Your email address, a copy of your data, and deleting the account are handled by the
          identity service, which owns them. This console reads them.
        </p>
        <div className="flex flex-wrap gap-2">
          {settings === "" ? null : (
            <ButtonLink href={settings} target="_blank" rel="noreferrer noopener">
              Email and account deletion
              <ExternalLink size={14} aria-hidden="true" />
            </ButtonLink>
          )}
          {exportPath === "" ? null : (
            <ButtonLink href={exportPath} target="_blank" rel="noreferrer noopener">
              Export your data
              <ExternalLink size={14} aria-hidden="true" />
            </ButtonLink>
          )}
        </div>
      </div>
    </Band>
  );
}
