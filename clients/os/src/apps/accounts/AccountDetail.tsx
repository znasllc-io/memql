import { AccountDomainsPanel } from "./DomainSetup";
import { ContentSkeleton } from "../../kit/ContentSkeleton";
import { RecordList, RecordRow } from "../../kit/RecordRow";
import { InPlaceAsk } from "../../kit/pressHold";
import { useEffect, useMemo, useState, useSyncExternalStore } from "react";
import { UserPlus, UserRound } from "lucide-react";

import { Button, Caption, Check, Details, Fact, Facts, Input, Notice, Panel, Subhead,
  PeerRowReadOnly,
} from "../../kit";
import { rowString } from "@znasllc-io/memql-sdk-core/client";
import { flatten } from "../../kit/rows";
import type { ArchiveAccountState, UpdateAccountState } from "./actions";
import { accountIsArchived, accountIsSelf, accountName, type AccountRow } from "./rows";
import { useSession } from "../../chrome/access";
import { holds } from "../../system/roles";
import { AccountDomainRail } from "./AccountDomainRail";
import { ExistingDataSection, InventorySection } from "./AccountInventory";
import { frontDoorFromRow, type FrontDoorRow } from "./frontDoor";
import { useAccountFrontDoors } from "./useAccountFrontDoors";
import { useLiveView } from "../../live/liveView";
import {
  useAccountAssignmentStatus,
  useAccountGuests,
  useAccountInventory,
  useAccountPeople,
  type Rollup,
} from "./useAccounts";

// One client: their facts, and everything of this cluster's that is theirs.
//
// ===========================================================================
// THE LEDGER IS THIS APP'S ONE IDEA
// ===========================================================================
// Every other app in the OS lists things the cluster OWNS -- machines, sites,
// files, people, domains. This one lists the people the cluster works FOR, and
// the question that only it can answer is the second half: what of all that is
// theirs. The ledger below is that question, answered in one place, and it is
// the only surface in the OS that crosses app boundaries.
//
// TWO HALVES (account row ownership, section H). The People and Guests bands
// say WHO: the people in the client's groups and the guests invited to it.
// The inventory says WHAT: every concept holding rows recorded under the
// account, by area, from one builtin (`accountInventory`). It replaced four
// bands that each listed one population from its own query; their counts are
// the inventory's now, and the rows they named are listed in their own apps.
//
// THE INVENTORY IS THE ACCOUNT'S, NOT THE VIEWER'S. It counts rows by the
// account recorded on them, which is why it is drawn only for a holder of
// `execute app:accounts/inventory` (owner and developer by seed): it reports
// rows a viewer may not be able to read one by one. The two bands remain
// presentation over the caller's own reads, so two people opening the same
// client can still see different numbers there.

// A stable empty array: useSyncExternalStore compares snapshots by identity,
// and a fresh `[]` on every read is an infinite render loop.
const EMPTY_DOORS: FrontDoorRow[] = [];

export function AccountDetail({
  account,
  accounts = [],
  update,
  archive,
  onArchived,
  onOpenGroup,
  onConfigureDomains,
}: {
  onConfigureDomains?: () => void;
  account: AccountRow;
  /** The accounts the app already holds, to name an account by. */
  accounts?: readonly AccountRow[];
  update: UpdateAccountState;
  archive: ArchiveAccountState;
  onArchived: () => void;
  /** Opens the Users app on one of this client's groups. */
  onOpenGroup?: (groupId: string) => void;
}) {
  const archived = accountIsArchived(account);
  const self = accountIsSelf(account);
  // WHO MAY SEE THE INVENTORY AND THE ASSIGNMENT. Both builtins carry the
  // part; this is the courtesy half, so a non-holder is offered nothing that
  // would only be refused (DESIGN.md rule 12: absent, not disabled).
  // `accessEpoch` is the reactivity signal: the held set is read out of band,
  // and a grant written in this browser bumps it.
  const { accessEpoch } = useSession();
  const canInventory = useMemo(() => holds("execute", "app:accounts/inventory"), [accessEpoch]);
  const assignment = useAccountAssignmentStatus(canInventory && self);

  // The front doors, live (epic memql#5168). RETAINED here rather than inside
  // the rail: a collection does nothing until retain() (clients/os/README.md),
  // and the rail is one of several stops that unmounts as somebody clicks
  // between them -- a subscription torn down and re-seeded on every click is
  // the arrival cue claiming news that did not arrive.
  const { source: doorSource } = useAccountFrontDoors();
  const doorView = useLiveView<Record<string, unknown>, FrontDoorRow>(
    doorSource,
    // KEYED ON NOTHING THAT CHANGES. `accountFrontDoorsOpen` takes no
    // arguments, so selecting a different account changes no read -- folding
    // the account id in would tear the subscription down and re-seed from
    // empty on every click, and the arrival cue would announce rows nobody
    // sent (clients/os/README.md).
    "accounts:frontDoors",
    (rows) => rows.map(frontDoorFromRow).filter((d) => d.id !== ""),
  );
  const doorSnapshot = useSyncExternalStore(
    useMemo(
      () => (doorView ? doorView.subscribe.bind(doorView) : () => () => {}),
      [doorView],
    ),
    () => doorView?.snapshot.rows ?? EMPTY_DOORS,
  );
  const doors = doorSnapshot as FrontDoorRow[];

  const { access: doorAccess, config: doorConfig } = useSession();
  // `role === "owner"` rather than a rank floor, the ProfilePanel's reasoning
  // forty lines down: v1:platform:accountFrontDoor is clusterOwner tier, which
  // mirrors auth.IsClusterOwner -- one role, not a rung. An admin reads zero
  // rows and no error, so the stop has to be told which kind of empty it is
  // looking at rather than rendering "not held" about a serving door.
  const canReadDoors = (doorAccess?.role ?? "") === "owner";

  return (
    <div className="os-account-detail">
      <ProfilePanel account={account} update={update} />
      {onConfigureDomains ? <AccountDomainsPanel account={account} onConfigure={onConfigureDomains} /> : null}
      {/* THE DOMAIN RAIL sits beneath the profile because that is where its
          first stop's value is edited: the domain is a profile fact, and what
          it BUYS -- proof, joining, a reserved name -- is what the rail
          answers. */}
      <AccountDomainRail
        account={account}
        update={update}
        clusterDomain={doorConfig.domain}
        doors={doors}
        canReadDoors={canReadDoors}
      />
      <Ledger account={account} canInventory={canInventory} onOpenGroup={onOpenGroup} />
      {/* EXISTING DATA, ON THE PRIMARY ACCOUNT ONLY. Assigning rows written
          before accounts were recorded is the cluster's work, not any one
          client's, and the primary account is the cluster's own page. */}
      {self && canInventory ? (
        <Details>
          <ExistingDataSection reading={assignment} accounts={accounts} />
        </Details>
      ) : null}
      {/* NO CREDENTIALS PANEL HERE, AND THAT IS THE POINT (memql#5013).
          A credential is minted against a `v1:identity:account` -- the paying
          account of the isolation model -- and `mintAccountToken` gates on
          `query accountById`, which binds that concept. This row is a
          `v1:accounts:account`: a CLIENT. The two share the word and nothing
          else, there is no field linking them, and handing a client id to the
          mint resolves zero rows -- which IS the refusal. The panel lives in
          the app's Credentials section, under the billing account it can
          actually be issued against. */}
      {/* THE SELF ACCOUNT IS NOT ARCHIVABLE (memql#4837).
          The panel rendered for it like any other client, so the owner could
          file away their own company -- and archive is the ONLY exit in this
          model, so it was a one-way trip. The engine refuses it now
          (accounts_self_archive_guard.go, no escape, not even for a cluster
          owner); this is the courtesy half, and the two are not
          interchangeable: archiveClientAccount takes the id as a caller
          argument, so hiding the button stops the button and nothing else.

          The REASON replaces the control rather than disabling it. A greyed
          Archive button on your own company is a question the surface refuses
          to answer, and the answer is short. */}
      {archived || self ? null : (
        <ArchivePanel account={account} archive={archive} onArchived={onArchived} />
      )}
      {self && !archived ? <SelfAccountPanel /> : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// The facts, and editing them
// ---------------------------------------------------------------------------

function ProfilePanel({
  account,
  update,
}: {
  account: AccountRow;
  update: UpdateAccountState;
}) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(() => toDraft(account));
  const { access } = useSession();
  const actorRole = access?.role ?? "";

  // WHOSE ROW IS THIS, and can this viewer write it.
  //
  // Presentation only, as ever: the engine's write guard is the authority and
  // refuses the same write whatever this renders. What this decides is whether
  // somebody is offered a form that CANNOT SAVE -- which under rank-visible
  // reads is a new possibility, because a row you may not write is now a row
  // you can see (epic memql#4832, D2).
  //
  // Three cases, and the middle one is the one that is easy to get wrong:
  //
  //  - the viewer's OWN row            writable
  //  - CLUSTER-OWNED (empty owner)     writable by a cluster OWNER only
  //  - anyone else's                   read-only
  //
  // The cluster-owned case is the `self` account. It is tempting to read an
  // empty owner as "nobody's, so everybody's" -- and the engine says the
  // opposite: `sameRowAuthzOwner` refuses an empty owner outright, so the
  // clusterOwner branch is the only one that admits the write, and that branch
  // is `Role == RoleOwner` exactly. Offering an admin the Edit button here
  // would hand them a form the engine refuses on save, which is precisely the
  // shape this panel now exists to avoid.
  //
  // `role === "owner"` rather than roleAdmits, because this is not a
  // rank floor: it mirrors auth.IsClusterOwner, which is one role and not a
  // rung. Settings' ClusterSection reads it the same way and for the same
  // reason.
  const ownerUserId = account.ownerUserId.trim();
  const viewerOwnsIt = ownerUserId !== "" && ownerUserId === (access?.userId ?? "\u0000");
  const clusterOwned = ownerUserId === "";
  const readOnly = clusterOwned ? actorRole !== "owner" : !viewerOwnsIt;


  // RESET THE DRAFT WHEN THE ROW CHANGES UNDER THE FORM. The feed is live, so
  // somebody else's edit can land while this panel is open. Keyed on the row's
  // own values rather than on a version counter: what matters is whether what
  // is on screen still describes the row, and re-seeding a form somebody is
  // typing into is worse than showing them a stale baseline, so this only runs
  // while the form is CLOSED.
  useEffect(() => {
    if (editing) return;
    setDraft(toDraft(account));
  }, [account.id, account.name, account.domain, account.primaryContactName, account.primaryContactEmail, account.notes, editing]);

  async function save() {
    const ok = await update.update(account.id, draft);
    if (ok) setEditing(false);
  }

  if (!editing) {
    return (
      <Panel label="Profile">
        <div className="os-account-profile-head">
          <Subhead>Profile</Subhead>
          {/* NO EDIT BUTTON ON SOMEBODY ELSE'S ACCOUNT (epic memql#4832, D2).
              This is a state the app could not previously be in: before
              rank-visible reads, an account you could not edit was one you
              could not SEE, so an Edit button that opened a form the engine
              then refused to save had never been reachable. It is now -- a
              developer reads an admin's clients -- and the honest surface is
              to say why rather than to offer a save that cannot land. */}
          {readOnly ? null : <Button onClick={() => setEditing(true)}>Edit</Button>}
        </div>
        {readOnly ? (
          clusterOwned ? (
            <PeerRowReadOnly actorRole={actorRole} ownerName="This cluster" ownerRole="owner" />
          ) : (
            <PeerRowReadOnly actorRole={actorRole} />
          )
        ) : null}
        <Facts>
          <Fact label="Name" value={account.name} />
          <Fact label="Domain" value={account.domain} mono />
          <Fact label="Contact" value={account.primaryContactName} />
          <Fact label="Email" value={account.primaryContactEmail} mono />
          <Fact label="Notes" value={account.notes} />
        </Facts>
        {accountIsSelf(account) ? (
          <Caption>
            This is your own company -- the one this instance belongs to. It was created when the
            cluster first started.
          </Caption>
        ) : null}
      </Panel>
    );
  }

  return (
    <Panel label="Edit profile">
      <Subhead>Profile</Subhead>
      <div className="os-account-form">
        <Field id="os-account-name" label="Name" value={draft.name} onChange={(v) => setDraft({ ...draft, name: v })} />
        <Field id="os-account-domain" label="Domain" value={draft.domain} onChange={(v) => setDraft({ ...draft, domain: v })} placeholder="acme.com" />
        <Field id="os-account-contact" label="Primary contact" value={draft.primaryContactName} onChange={(v) => setDraft({ ...draft, primaryContactName: v })} />
        <Field id="os-account-email" label="Contact email" value={draft.primaryContactEmail} onChange={(v) => setDraft({ ...draft, primaryContactEmail: v })} />
        <Field id="os-account-notes" label="Notes" value={draft.notes} onChange={(v) => setDraft({ ...draft, notes: v })} />
      </div>

      {update.error === "" ? null : (
        <Notice
          tone="error"
          sentence="This did not save."
          next="Nothing was written; what is below is still as you typed it."
          detail={update.error}
        />
      )}

      <div className="os-account-form-actions">
        <Button tone="primary" onClick={save} disabled={update.busy}>
          {update.busy ? "Saving" : "Save"}
        </Button>
        <Button
          onClick={() => {
            setDraft(toDraft(account));
            update.reset();
            setEditing(false);
          }}
        >
          Cancel
        </Button>
      </div>
    </Panel>
  );
}

function toDraft(account: AccountRow) {
  return {
    name: account.name,
    domain: account.domain,
    primaryContactName: account.primaryContactName,
    primaryContactEmail: account.primaryContactEmail,
    notes: account.notes,
  };
}

function Field({
  id,
  label,
  value,
  onChange,
  placeholder,
}: {
  id: string;
  label: string;
  value: string;
  onChange: (next: string) => void;
  placeholder?: string;
}) {
  return (
    <div className="os-form-field">
      <label className="os-form-field-label" htmlFor={id}>
        {label}
      </label>
      <Input id={id} label={label} value={value} onChange={onChange} placeholder={placeholder} />
    </div>
  );
}

// ---------------------------------------------------------------------------
// The ledger
// ---------------------------------------------------------------------------

function Ledger({
  account,
  canInventory,
  onOpenGroup,
}: {
  account: AccountRow;
  canInventory: boolean;
  onOpenGroup?: (groupId: string) => void;
}) {
  // THE PEOPLE BAND'S OWN READ: two numbers over two reads -- distinct
  // people, across the groups that grant this client.
  const people = useAccountPeople(account.id);
  const guests = useAccountGuests(account.id);
  // Asked only of a holder: for anyone else the builtin would refuse, and the
  // page draws no inventory at all.
  const inventory = useAccountInventory(account.id, canInventory);

  // ONE READ TIME AND ONE RE-READ for the whole ledger, so the page says once
  // that none of it is live.
  const readAt = inventory.at?.toISOString() ?? (people.readAt || guests.readAt);

  return (
    <section className="os-account-ledger" aria-label={`What belongs to ${accountName(account)}`}>
      <div className="os-account-ledger-head">
        <Subhead>What is theirs</Subhead>
        <Button
          onClick={() => {
            people.reload();
            guests.reload();
            inventory.reread();
          }}
        >
          Re-read
        </Button>
      </div>

      <div className="os-account-bands">
        {/* FIRST, because they are who everything in the inventory is for. */}
        <PeopleBand people={people} onOpenGroup={onOpenGroup} />
        <GuestsBand rollup={guests} />
      </div>

      {canInventory ? (
        <Details>
          <InventorySection reading={inventory} />
        </Details>
      ) : null}

      {/* A READ THAT SAYS WHEN IT HAPPENED. These are on-demand reads, not
          live feeds (see useAccounts.ts for why), so the honest thing is to
          print the time rather than let a static number read as a live one. */}
      {readAt === "" ? null : (
        <Caption>
          Read at {new Date(readAt).toLocaleTimeString()}. These are not live -- re-read to see
          changes made since.
        </Caption>
      )}
    </section>
  );
}

/**
 * How many people reach this client's work, and through which groups.
 *
 * OPENING IT GOES TO USERS, on the group, by intent -- rather than listing
 * members here. A members list in this ledger would be a second place to
 * manage membership, and the one that manages it is the group's own page.
 */
function PeopleBand({
  people,
  onOpenGroup,
}: {
  people: ReturnType<typeof useAccountPeople>;
  onOpenGroup?: (groupId: string) => void;
}) {
  if (people.state === "error") {
    // A REFUSAL IS NOT A ZERO. The reads carry `@requiresRank("admin")`, so
    // below that the engine refuses -- and rendering that as "0 people" would
    // be this window inventing a fact about a client.
    return (
      <article className="os-account-band" data-state="refused">
        <header className="os-account-band-head">
          <UserRound size={15} aria-hidden />
          <h4 className="os-account-band-title">People</h4>
        </header>
        <p className="os-account-band-refused">Not yours to read</p>
        <p className="os-account-band-detail os-mono">{people.error}</p>
      </article>
    );
  }

  if (people.state !== "ready") {
    return (
      <article className="os-account-band" data-state="loading">
        <header className="os-account-band-head">
          <UserRound size={15} aria-hidden />
          <h4 className="os-account-band-title">People</h4>
        </header>
        <p className="os-account-band-count" aria-hidden>
          --
        </p>
        <ContentSkeleton kind="detail" label="Loading account details" />
      </article>
    );
  }

  return (
    <article className="os-account-band" data-state={people.people === 0 ? "empty" : "ready"}>
      <header className="os-account-band-head">
        <UserRound size={15} aria-hidden />
        <h4 className="os-account-band-title">People</h4>
      </header>
      <p className="os-account-band-count">{people.people}</p>
      <p className="os-account-band-note">
        {people.groups.length === 0
          ? "in no groups yet"
          : people.groups.length === 1
            ? "in 1 group"
            : `in ${people.groups.length} groups`}
      </p>
      {people.groups.length === 0 ? (
        <p className="os-account-band-owner">Users is where these are added.</p>
      ) : (
        <RecordList density="compact"><ul className="os-account-band-rows" aria-label="Groups for this client">
          {people.groups.map((group) => (
            <li key={group.id}><RecordRow name={group.name} onOpen={onOpenGroup ? () => onOpenGroup(group.id) : undefined} /></li>
          ))}
        </ul></RecordList>
      )}
      <p className="os-caption">Open a group to manage membership, organization roles and app access.</p>
    </article>
  );
}

/**
 * Who has been invited to this client and has not joined yet.
 *
 * The inventory counts invitations under People; this band says WHO, which
 * the count cannot, and which the People band (members only) never shows.
 */
function GuestsBand({ rollup }: { rollup: Rollup }) {
  // A REFUSAL IS NOT A ZERO. The guest-invitation read carries
  // `@requiresRank("developer")`, so below that floor the engine refuses the
  // read -- and rendering that as "0 invitations" would be this window
  // inventing a fact about a client. The server's own sentence goes on screen.
  if (rollup.state === "error") {
    return (
      <article className="os-account-band" data-state="refused">
        <header className="os-account-band-head">
          <UserPlus size={15} aria-hidden />
          <h4 className="os-account-band-title">Guests</h4>
        </header>
        <p className="os-account-band-refused">Not yours to read</p>
        <p className="os-account-band-detail os-mono">{rollup.error}</p>
      </article>
    );
  }

  if (rollup.state === "loading" || rollup.state === "idle") {
    return (
      <article className="os-account-band" data-state="loading">
        <header className="os-account-band-head">
          <UserPlus size={15} aria-hidden />
          <h4 className="os-account-band-title">Guests</h4>
        </header>
        <p className="os-account-band-count" aria-hidden>
          --
        </p>
        <ContentSkeleton kind="detail" label="Loading account details" />
      </article>
    );
  }

  const count = rollup.rows.length;
  return (
    <article className="os-account-band" data-state={count === 0 ? "empty" : "ready"}>
      <header className="os-account-band-head">
        <UserPlus size={15} aria-hidden />
        <h4 className="os-account-band-title">Guests</h4>
      </header>
      <p className="os-account-band-count">{count}</p>
      <p className="os-account-band-note">
        {count === 0 ? "No invitations yet" : count === 1 ? "invitation" : "invitations"}
      </p>
      {count === 0 ? (
        <p className="os-account-band-owner">Users is where these are added.</p>
      ) : (
        <RecordList density="compact"><ul className="os-account-band-rows" aria-label="Guests for this client">
          {rollup.rows.slice(0, LEDGER_ROWS).map((row, i) => (
            <li key={rowString(flatten(row), "id") || String(i)}><RecordRow name={rowString(flatten(row), "inviteeEmail") || "--"} /></li>
          ))}
          {count > LEDGER_ROWS ? (
            <li className="os-account-band-more">and {count - LEDGER_ROWS} more, in Users</li>
          ) : null}
        </ul></RecordList>
      )}
    </article>
  );
}

/**
 * How many guests the band names before it stops.
 *
 * A band is a COUNT with evidence, not a table -- Users is where somebody
 * goes to work with invitations. Five is enough to recognize who they are.
 */
const LEDGER_ROWS = 5;

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
function ArchivePanel({
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
    <Panel label="Archive">
      <Subhead>Archive</Subhead>
      {!asking ? (
        <>
          <p className="os-caption">
            Filing a client away takes them out of the list. Nothing they are tied to changes.
          </p>
          <Button onClick={() => setAsking(true)}>Archive this client</Button>
        </>
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

          <div className="os-account-form-actions">
            <Button tone="danger" onClick={run} disabled={!understood || archive.busy}>
              {archive.busy ? "Archiving" : "Archive"}
            </Button>
            <Button
              onClick={() => {
                setAsking(false);
                setUnderstood(false);
                archive.reset();
              }}
            >
              Cancel
            </Button>
          </div>
        </InPlaceAsk>
      )}
    </Panel>
  );
}


/**
 * What stands where the Archive panel would be, on the cluster's own account.
 *
 * It says the rule and the reason in two lines and offers nothing, because
 * there is nothing to offer -- this is not a permission a different person
 * could exercise, it is a row that has no exit. A disabled button would imply
 * the opposite.
 *
 * Everything ELSE about this account stays editable, including its name and
 * domain, and the panel says so: there is no delete in this model, so a
 * first-run typo would otherwise be permanent. Nothing in the cluster derives
 * authority from these fields -- `name` is read by no gate, and `domain` feeds
 * custom-domain SUGGESTIONS only.
 */
function SelfAccountPanel() {
  return (
    <Panel label="This cluster">
      <Subhead>This cluster</Subhead>
      <p className="os-caption">
        This is your own company, not a client. It cannot be archived -- archive is
        the only exit here, and the cluster would be left without one. Its name,
        domain and contact stay editable above.
      </p>
    </Panel>
  );
}
