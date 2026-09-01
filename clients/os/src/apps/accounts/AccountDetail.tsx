import { useEffect, useMemo, useState, type ReactNode } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { FileText, GraduationCap, Rocket, Send, UserPlus } from "lucide-react";

import { Button, Caption, Check, Fact, Facts, Input, Notice, Panel, Subhead,
  PeerRowReadOnly,
} from "../../kit";
import { rowString } from "@znasllc-io/memql-sdk-core/client";
import { flatten } from "../../kit/rows";
import { useAccountCampaignsRollup } from "../campaigns/useCampaigns";
import type { ArchiveAccountState, UpdateAccountState } from "./actions";
import { accountIsArchived, accountIsSelf, accountName, type AccountRow } from "./rows";
import { useSession } from "../../chrome/access";
import { useAccountRollups, type Rollup } from "./useAccounts";

// One client: their facts, and everything of this cluster's that is theirs.
//
// ===========================================================================
// THE LEDGER IS THIS APP'S ONE IDEA
// ===========================================================================
// Every other app in the OS lists things the cluster OWNS -- machines, sites,
// files, people, domains. This one lists the people the cluster works FOR, and
// the question that only it can answer is the second half: what of all that is
// theirs. The four bands below are that question, answered in one place, and
// they are the only surface in the OS that crosses app boundaries.
//
// They are drawn as one object rather than four panels because the thing they
// add up to is a single fact -- this client's footprint on this instance --
// and four separate cards would read as four unrelated lists that happen to
// share a heading.
//
// EACH BAND IS PRESENTATION OVER ENGINE TRUTH (D1). The counts are of rows the
// caller could already read; a band is not narrowed by the account and the
// account narrows nothing. Two people opening the same client can therefore
// see different numbers, and that is correct: the client is shared, the rows
// are not.

export function AccountDetail({
  account,
  update,
  archive,
  onArchived,
}: {
  account: AccountRow;
  update: UpdateAccountState;
  archive: ArchiveAccountState;
  onArchived: () => void;
}) {
  const rollups = useAccountRollups(account.id);
  const archived = accountIsArchived(account);
  const self = accountIsSelf(account);

  return (
    <div className="os-account-detail">
      <ProfilePanel account={account} update={update} />
      <Ledger account={account} rollups={rollups} />
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
  const actorRole = access?.clusterRole ?? "";

  // WHOSE ROW IS THIS. `ownerUserId` empty means CLUSTER-OWNED -- the self
  // account -- which is everyone's to edit who got this far, so it is not a
  // peer row. Anything else that is not the viewer's own is.
  //
  // Presentation only, as ever: the engine's write guard is the authority, and
  // it refuses the same write whatever this renders. What this decides is
  // whether somebody is offered a form that cannot save.
  const ownerUserId = account.ownerUserId.trim();
  const readOnly = ownerUserId !== "" && ownerUserId !== (access?.userId ?? "\u0000");


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
          <PeerRowReadOnly actorRole={actorRole} />
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

interface BandSpec {
  key: string;
  title: string;
  /** What one row IS, in the reader's words. Used for the empty line. */
  noun: string;
  icon: ReactNode;
  rollup: Rollup;
  /** The app that owns this population, named so the reader knows where to go. */
  owner: string;
  line: (row: Row) => string;
}

function Ledger({
  account,
  rollups,
}: {
  account: AccountRow;
  rollups: ReturnType<typeof useAccountRollups>;
}) {
  // THE FIFTH BAND, AND ITS READ LIVES IN THE CAMPAIGNS APP. `tie.tsx` states
  // the rule for the other direction -- a tie surface belongs to the domain
  // that owns the concept -- and this is the same rule read the other way
  // round: the ledger renders the band, and the read that fills it is the
  // campaigns app's to own. It answers in this ledger's own `Rollup`
  // vocabulary so five bands can settle independently and print one read time
  // between them.
  //
  // IT IS ON-DEMAND EVEN THOUGH `v1:campaigns:campaign` DOES BROADCAST, and
  // that is the ledger's stated consistency reason rather than a liveness one.
  // `v1:knowledge:knowledgeDomain` carries no rule and nothing broadcasts it,
  // so the Knowledge band could never be live -- and a ledger where four bands
  // move and the fifth silently does not is worse than one where none do,
  // because the reader has no way to tell which kind of band they are looking
  // at. Consistency across a composite surface beat liveness on part of it.
  const campaigns = useAccountCampaignsRollup(account.id);

  const bands: BandSpec[] = useMemo(
    () => [
      {
        key: "sites",
        title: "Deployables",
        noun: "site",
        icon: <Rocket size={15} aria-hidden />,
        rollup: rollups.sites,
        owner: "Deployables",
        line: (row) => rowString(flatten(row), "hostname") || rowString(flatten(row), "title"),
      },
      {
        key: "files",
        title: "Files",
        noun: "file",
        icon: <FileText size={15} aria-hidden />,
        rollup: rollups.files,
        owner: "Files",
        line: (row) => rowString(flatten(row), "title"),
      },
      {
        key: "domains",
        title: "Knowledge",
        noun: "domain",
        icon: <GraduationCap size={15} aria-hidden />,
        rollup: rollups.domains,
        owner: "Training",
        line: (row) => rowString(flatten(row), "name") || rowString(flatten(row), "id"),
      },
      {
        key: "invites",
        title: "Guests",
        noun: "invitation",
        icon: <UserPlus size={15} aria-hidden />,
        rollup: rollups.invites,
        owner: "Users",
        line: (row) => rowString(flatten(row), "inviteeEmail"),
      },
      {
        key: "campaigns",
        title: "Campaigns",
        noun: "campaign",
        icon: <Send size={15} aria-hidden />,
        // Shaped into this ledger's vocabulary. The campaigns reader answers
        // with the same {value, state, error, readAt} triple the four above
        // use, so the refusal case renders identically -- A REFUSAL IS NOT A
        // ZERO holds for this band exactly as it does for the guests one.
        rollup: {
          rows: campaigns.value,
          state: campaigns.state,
          error: campaigns.error,
          readAt: campaigns.readAt,
        },
        owner: "Campaigns",
        // It counts CAMPAIGNS and not sends: a band reading "4,182" would be
        // counting delivery rows, which is a number about one busy week rather
        // than about the client.
        line: (row) => rowString(flatten(row), "name"),
      },
    ],
    [rollups, campaigns],
  );

  const readAt = bands.map((b) => b.rollup.readAt).filter((t) => t !== "")[0] ?? "";

  return (
    <section className="os-account-ledger" aria-label={`What belongs to ${accountName(account)}`}>
      <div className="os-account-ledger-head">
        <Subhead>What is theirs</Subhead>
        <Button
          onClick={() => {
            rollups.reload();
            campaigns.reload();
          }}
        >
          Re-read
        </Button>
      </div>

      <div className="os-account-bands">
        {bands.map((band) => (
          <Band key={band.key} band={band} />
        ))}
      </div>

      {/* A READ THAT SAYS WHEN IT HAPPENED. These four are on-demand reads,
          not live feeds (see useAccounts.ts for why), so the honest thing is
          to print the time rather than let a static number read as a live
          one. The Training app's knowledge surfaces set this precedent for
          the same reason. */}
      {readAt === "" ? null : (
        <Caption>
          Read at {new Date(readAt).toLocaleTimeString()}. These are not live -- re-read to see
          changes made since.
        </Caption>
      )}
    </section>
  );
}

function Band({ band }: { band: BandSpec }) {
  const { rollup } = band;

  // A REFUSAL IS NOT A ZERO. The guest-invitation rollup carries
  // `requiresOwnerOrAdmin`, so below that floor the engine refuses the read --
  // and rendering that as "0 invitations" would be this window inventing a
  // fact about a client. The server's own sentence goes on screen instead.
  if (rollup.state === "error") {
    return (
      <article className="os-account-band" data-state="refused">
        <header className="os-account-band-head">
          {band.icon}
          <h4 className="os-account-band-title">{band.title}</h4>
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
          {band.icon}
          <h4 className="os-account-band-title">{band.title}</h4>
        </header>
        <p className="os-account-band-count" aria-hidden>
          --
        </p>
        <p className="os-account-band-note">Reading</p>
      </article>
    );
  }

  const count = rollup.rows.length;
  return (
    <article className="os-account-band" data-state={count === 0 ? "empty" : "ready"}>
      <header className="os-account-band-head">
        {band.icon}
        <h4 className="os-account-band-title">{band.title}</h4>
      </header>
      {/* The count in the shell's display face -- the same one the desk
          numeral uses. It is the only place in APP content where that face
          appears, and it earns the exception: this number IS the thing the
          band is for, and the echo ties the ledger to the shell's own way of
          saying "how many". */}
      <p className="os-account-band-count">{count}</p>
      <p className="os-account-band-note">
        {count === 0 ? `No ${band.noun}s yet` : count === 1 ? band.noun : `${band.noun}s`}
      </p>
      {count === 0 ? (
        <p className="os-account-band-owner">{band.owner} is where these are added.</p>
      ) : (
        <ul className="os-account-band-rows" aria-label={`${band.title} for this client`}>
          {rollup.rows.slice(0, LEDGER_ROWS).map((row, i) => (
            <li key={rowString(flatten(row), "id") || String(i)}>{band.line(row) || "--"}</li>
          ))}
          {count > LEDGER_ROWS ? (
            <li className="os-account-band-more">
              and {count - LEDGER_ROWS} more, in {band.owner}
            </li>
          ) : null}
        </ul>
      )}
    </article>
  );
}

/**
 * How many rows a band lists before it stops naming them.
 *
 * A band is a COUNT with evidence, not a table -- the app that owns each
 * population is where somebody goes to work with it, and four unbounded lists
 * in one panel is four scroll regions competing with the profile above them.
 * Five is enough to recognize what the count is made of.
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
        <div className="os-account-confirm">
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
        </div>
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
