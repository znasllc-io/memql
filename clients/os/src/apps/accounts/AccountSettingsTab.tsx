import { useEffect, useRef, useState } from "react";

import {
  Button,
  Check,
  Details,
  DetailsSection,
  Input,
  Notice,
  PeerRowReadOnly,
  SettingRow,
  TextArea,
} from "../../kit";
import { InPlaceAsk } from "../../kit/pressHold";
import { useSession } from "../../chrome/access";
import type { ArchiveAccountState, UpdateAccountState } from "./actions";
import { AccountDomainRail } from "./AccountDomainRail";
import { AccountDomainsPanel } from "./DomainSetup";
import type { FrontDoorRow } from "./frontDoor";
import { accountIsArchived, accountIsSelf, accountName, type AccountRow } from "./rows";

// THE SETTINGS TAB: every edit the account takes (design record
// 2026-11-12-account-export, section K), in the Fleet machine page's
// Settings shape -- named sections in one grid, the form on the page rather
// than behind an Edit button, because the facts it would have shown first are
// the Details tab's.
//
// In order: the profile, the assigned domains, the domain rail (its proof,
// joining and the MemQL address), and Archive. Each control is absent, not
// disabled, for somebody who may not use it.
//
// NO CREDENTIALS SECTION HERE, AND THAT IS THE POINT (memql#5013). A
// credential is minted against a `v1:identity:account` -- the paying account
// of the isolation model -- and this row is a `v1:accounts:account`: a CLIENT.
// The two share the word and nothing else. The panel lives in the app's
// Credentials section, under the billing account it can actually be issued
// against.

export interface AccountSettingsTabProps {
  account: AccountRow;
  update: UpdateAccountState;
  archive: ArchiveAccountState;
  onArchived: () => void;
  /** Opens the domain setup wizard. Absent, the Domains section is too. */
  onConfigureDomains?: () => void;
  /** Every front door this caller may read, live; held by the page. */
  doors: FrontDoorRow[];
  /** Whether this viewer can read front doors at all (cluster owner only). */
  canReadDoors: boolean;
}

export function AccountSettingsTab({
  account,
  update,
  archive,
  onArchived,
  onConfigureDomains,
  doors,
  canReadDoors,
}: AccountSettingsTabProps) {
  const { config, access } = useSession();
  const archived = accountIsArchived(account);
  const self = accountIsSelf(account);
  return (
    <Details>
      <ProfileSettings account={account} update={update} />
      {onConfigureDomains ? <AccountDomainsPanel account={account} onConfigure={onConfigureDomains} /> : null}
      {/* THE DOMAIN RAIL sits beneath the profile because that is where its
          first stop's value is edited: the domain is a profile fact, and what
          it BUYS -- proof, joining, a reserved name -- is what the rail
          answers. */}
      <AccountDomainRail
        account={account}
        update={update}
        clusterDomain={config.domain}
        profileEditable={profileIsWritable(account, access?.userId ?? "", access?.role ?? "")}
        doors={doors}
        canReadDoors={canReadDoors}
      />
      {/* THE SELF ACCOUNT IS NOT ARCHIVABLE (memql#4837). The engine refuses
          it (accounts_self_archive_guard.go, no escape, not even for a cluster
          owner); this is the courtesy half. archiveClientAccount takes the id
          as a caller argument, so hiding the button stops the button and
          nothing else.

          The REASON replaces the control rather than disabling it. A greyed
          Archive button on your own company is a question the surface refuses
          to answer, and the answer is short. */}
      {archived || self ? null : <ArchiveSection account={account} archive={archive} onArchived={onArchived} />}
      {self && !archived ? <SelfAccountSection /> : null}
    </Details>
  );
}

// ---------------------------------------------------------------------------
// The profile
// ---------------------------------------------------------------------------

interface ProfileDraft {
  name: string;
  domain: string;
  primaryContactName: string;
  primaryContactEmail: string;
  notes: string;
}

function toDraft(account: AccountRow): ProfileDraft {
  return {
    name: account.name,
    domain: account.domain,
    primaryContactName: account.primaryContactName,
    primaryContactEmail: account.primaryContactEmail,
    notes: account.notes,
  };
}

function sameDraft(a: ProfileDraft, b: ProfileDraft): boolean {
  return a.name === b.name && a.domain === b.domain && a.primaryContactName === b.primaryContactName
    && a.primaryContactEmail === b.primaryContactEmail && a.notes === b.notes;
}

/**
 * WHOSE ROW IS THIS, and can this viewer write it.
 *
 * Presentation only, as ever: the engine's write guard is the authority and
 * refuses the same write whatever this renders. What this decides is whether
 * somebody is offered a form that CANNOT SAVE -- which under rank-visible
 * reads is a real possibility, because a row you may not write is now a row
 * you can see (epic memql#4832, D2).
 *
 *  - the viewer's OWN row            writable
 *  - CLUSTER-OWNED (empty owner)     writable by a cluster OWNER only
 *  - anyone else's                   read-only
 *
 * The cluster-owned case is the `self` account. An empty owner is not
 * "nobody's, so everybody's": `sameRowAuthzOwner` refuses an empty owner
 * outright, so the clusterOwner branch is the only one that admits the
 * write, and that branch is `Role == RoleOwner` exactly -- one role, not a
 * rung, which is why this is not a rank floor.
 *
 * Shared with the domain rail, whose first stop points at this form only for
 * a viewer who is offered it.
 */
function profileIsWritable(account: AccountRow, viewerUserId: string, viewerRole: string): boolean {
  const ownerUserId = account.ownerUserId.trim();
  if (ownerUserId === "") return viewerRole === "owner";
  return viewerUserId !== "" && ownerUserId === viewerUserId;
}

function ProfileSettings({ account, update }: { account: AccountRow; update: UpdateAccountState }) {
  const { access } = useSession();
  const actorRole = access?.role ?? "";

  const readOnly = !profileIsWritable(account, access?.userId ?? "", actorRole);

  const [draft, setDraft] = useState(() => toDraft(account));
  const baseline = useRef(toDraft(account));
  // THE ROW WINS UNLESS SOMEBODY IS TYPING. The feed is live, so another
  // person's edit can land while this form stands open. A field nobody has
  // touched takes the new value; a draft that differs from what it started
  // from is kept, because overwriting what somebody typed is worse than
  // showing them a stale baseline. Keyed on the values, so an event that
  // changes nothing here (a domain walk restamp) leaves the form alone.
  useEffect(() => {
    const next = toDraft(account);
    const previous = baseline.current;
    baseline.current = next;
    setDraft((held) => (sameDraft(held, previous) ? next : held));
  }, [account.id, account.name, account.domain, account.primaryContactName, account.primaryContactEmail, account.notes]);

  const changed = !sameDraft(draft, toDraft(account));
  // THIS FORM'S OWN REFUSAL ONLY. The update state is shared with the domain
  // rail's Joining stop, and a refusal belongs beside the control that caused
  // it: a joining refusal drawn under the profile would say the profile did
  // not save when nobody asked it to.
  const [asked, setAsked] = useState(false);
  const set = (field: keyof ProfileDraft) => (value: string) => setDraft((held) => ({ ...held, [field]: value }));

  if (readOnly) {
    return (
      <DetailsSection title="Profile">
        {account.ownerUserId.trim() === "" ? (
          <PeerRowReadOnly actorRole={actorRole} ownerName="This cluster" ownerRole="owner" />
        ) : (
          <PeerRowReadOnly actorRole={actorRole} />
        )}
      </DetailsSection>
    );
  }

  return (
    <DetailsSection title="Profile">
      <form
        onSubmit={(event) => {
          event.preventDefault();
          if (!changed) return;
          // No local write-through: the saved value comes back on the live
          // feed, and the form then matches the row again on its own.
          setAsked(true);
          void update.update(account.id, draft).then((ok) => {
            if (ok) setAsked(false);
          });
        }}
      >
        <SettingRow label="Name">
          <Input id={`os-account-name-${account.id}`} label="Name" value={draft.name} onChange={set("name")} />
        </SettingRow>
        <SettingRow label="Domain">
          <Input id={`os-account-domain-${account.id}`} label="Domain" value={draft.domain} onChange={set("domain")} placeholder="acme.com" />
        </SettingRow>
        <SettingRow label="Primary contact">
          <Input id={`os-account-contact-${account.id}`} label="Primary contact" value={draft.primaryContactName} onChange={set("primaryContactName")} />
        </SettingRow>
        <SettingRow label="Contact email">
          <Input id={`os-account-email-${account.id}`} label="Contact email" value={draft.primaryContactEmail} onChange={set("primaryContactEmail")} />
        </SettingRow>
        <SettingRow label="Notes">
          <TextArea id={`os-account-notes-${account.id}`} label="Notes" value={draft.notes} onChange={set("notes")} rows={2} />
        </SettingRow>
        {!asked || update.error === "" ? null : (
          <Notice
            tone="error"
            sentence="This did not save."
            next="Nothing was written; what is above is still as you typed it."
            detail={update.error}
          />
        )}
        <div className="os-panel-actions">
          {changed && !update.busy ? (
            <Button
              onClick={() => {
                setDraft(toDraft(account));
                setAsked(false);
                update.reset();
              }}
            >
              Discard changes
            </Button>
          ) : null}
          <Button type="submit" tone="primary" disabled={!changed} busy={update.busy} busyLabel="Saving...">
            Save
          </Button>
        </div>
      </form>
    </DetailsSection>
  );
}

// ---------------------------------------------------------------------------
// Archiving
// ---------------------------------------------------------------------------

/**
 * File a client away (D8).
 *
 * AN IN-SURFACE CONFIRM, never a browser dialog. `window.confirm` blocks the
 * whole shell, and the OS suppresses the browser's own menus for the same
 * reason -- a modal the desktop did not draw is the loudest tell that this is
 * a tab.
 *
 * The confirm states what archiving does NOT do, because that is the part
 * somebody hesitating actually needs: every tie keeps resolving. Unfiling a
 * client must not rewrite the record of what was done for them.
 */
function ArchiveSection({
  account,
  archive,
  onArchived,
}: {
  account: AccountRow;
  archive: ArchiveAccountState;
  onArchived: () => void;
}) {
  const [asking, setAsking] = useState(false);
  const [understood, setUnderstood] = useState(false);

  async function run() {
    const ok = await archive.archive(account.id);
    if (ok) {
      setAsking(false);
      setUnderstood(false);
      onArchived();
    }
  }

  return (
    <DetailsSection title="Archive">
      {!asking ? (
        <div className="os-account-ledger-head">
          <p className="os-caption">
            Filing a client away takes them out of the list. Nothing they are tied to changes.
          </p>
          <Button onClick={() => setAsking(true)}>Archive this client</Button>
        </div>
      ) : (
        <InPlaceAsk className="os-account-confirm">
          <p className="os-account-confirm-line">
            Archive {accountName(account)}?
          </p>
          <p className="os-caption">
            They leave the default list and are marked archived under the Archived filter. Their
            sites keep serving, their files keep their labels, and their guests keep their invites
            -- an archived client is filed, not deleted.
          </p>
          <Check checked={understood} onChange={setUnderstood}>
            I want to file this client away
          </Check>

          {archive.error === "" ? null : (
            <Notice
              tone="error"
              sentence="This client was not archived."
              next="Nothing changed."
              detail={archive.error}
            />
          )}

          <div className="os-panel-actions">
            <Button
              onClick={() => {
                setAsking(false);
                setUnderstood(false);
                archive.reset();
              }}
            >
              Cancel
            </Button>
            <Button tone="danger" onClick={run} disabled={!understood || archive.busy}>
              {archive.busy ? "Archiving" : "Archive"}
            </Button>
          </div>
        </InPlaceAsk>
      )}
    </DetailsSection>
  );
}

/**
 * What stands where Archive would be, on the cluster's own account.
 *
 * It says the rule and the reason and offers nothing, because there is
 * nothing to offer -- this is not a permission a different person could
 * exercise, it is a row that has no exit. A disabled button would imply the
 * opposite.
 *
 * Everything ELSE about this account stays editable, including its name and
 * domain, and the section says so: there is no delete in this model, so a
 * first-run typo would otherwise be permanent. Nothing in the cluster derives
 * authority from these fields -- `name` is read by no gate, and `domain` feeds
 * custom-domain SUGGESTIONS only.
 */
function SelfAccountSection() {
  return (
    <DetailsSection title="Archive" label="This cluster">
      <p className="os-caption">
        This is your own company, not a client. It cannot be archived -- archive is
        the only exit here, and the cluster would be left without one. Its name,
        domain and contact stay editable above.
      </p>
    </DetailsSection>
  );
}
