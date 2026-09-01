import { useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { Building2, Plus } from "lucide-react";

import { Button, Caption, Check, Chip, Head, Input, LiveList, Notice, Panel, Row as ListRow } from "../../kit";
import { useLiveView } from "../../live/liveView";
import { AccountDetail } from "./AccountDetail";
import {
  accountFingerprint,
  accountFromRow,
  accountIsArchived,
  accountIsSelf,
  accountName,
  type AccountRow,
} from "./rows";
import { useAccounts } from "./useAccounts";
import type { ArchiveAccountState, CreateAccountState, UpdateAccountState } from "./actions";

// The registry: every client, live.
//
// The seed asks for EVERYTHING (`includeArchived: true`) and the archived
// filter is a fold over rows already here -- see settings.ts. Seeding
// filtered would make flipping the toggle re-run the read and re-baseline
// every arrival cue, so revealing rows the browser already had would announce
// them as new. Revealing rows is not the cluster sending them.

export function AccountsSection({
  showArchived,
  onToggleArchived,
  create,
  update,
  archive,
}: {
  showArchived: boolean;
  onToggleArchived: (next: boolean) => void;
  create: CreateAccountState;
  update: UpdateAccountState;
  archive: ArchiveAccountState;
}) {
  const { source: collection, snapshot, reseed } = useAccounts();
  const [openId, setOpenId] = useState("");
  const [adding, setAdding] = useState(false);

  // PROJECT, then narrow, in one pass -- the collection holds RAW wire rows
  // (the fold upserts an event payload with no projection hook), so every
  // predicate below runs on an accountFromRow result.
  const source = useLiveView<Row, AccountRow>(collection, `archived:${showArchived}`, (rows) => {
    const accounts = rows.map(accountFromRow).filter((a) => a.id !== "");
    return showArchived ? accounts : accounts.filter((a) => !accountIsArchived(a));
  });

  const open = useMemo(
    () => source?.snapshot.rows.find((a) => a.id === openId) ?? null,
    [source, source?.snapshot, openId],
  );

  return (
    <div className="os-app-stack">
      <Head title="Accounts">
        <Button onClick={() => setAdding((v) => !v)}>
          <Plus size={14} aria-hidden /> Add a client
        </Button>
      </Head>

      {snapshot.error ? (
        <Notice
          tone="error"
          sentence="This cluster did not return its clients."
          next="Nothing below is current."
        >
          <Button onClick={reseed}>Try again</Button>
        </Notice>
      ) : null}

      {adding ? (
        <CreateAccountForm
          create={create}
          onDone={(id) => {
            setAdding(false);
            if (id !== "") setOpenId(id);
          }}
        />
      ) : null}

      <div className="os-account-filters">
        <Check checked={showArchived} onChange={onToggleArchived}>
          Show archived clients
        </Check>
      </div>

      {/* Keyed on the filter so flipping it RE-BASELINES the arrival cues.
          Without it, revealing archived rows makes them flash "new" on the
          next event -- claiming the cluster just sent them, when all that
          happened is that this browser started showing rows it already had. */}
      <LiveList<AccountRow>
        key={`accounts:${showArchived}`}
        source={source}
        rowId={(a) => a.id}
        // EVERY RENDERED FIELD, honestly. `v1:accounts:account` carries no
        // field the engine churns -- no lastSeenAt, no freshness -- so unlike
        // the machines and people lists there is nothing here that would turn
        // the cue into a strobe. `configuredAt` is the one deliberate
        // omission: it moves on every save, so naming it would fire the cue
        // twice for one edit.
        fingerprint={accountFingerprint}
        label="Clients in this registry"
        emptyText={
          showArchived
            ? "No clients yet. Add the first one above."
            : "No active clients. Add one above -- or show archived clients if you are looking for one you filed away."
        }
        renderRow={(account, tick) => (
          <AccountLine
            account={account}
            tick={tick}
            open={openId === account.id}
            onToggle={() => setOpenId((held) => (held === account.id ? "" : account.id))}
          />
        )}
      />

      {open === null ? null : (
        <AccountDetail
          key={open.id}
          account={open}
          update={update}
          archive={archive}
          onArchived={() => setOpenId("")}
        />
      )}
    </div>
  );
}

function AccountLine({
  account,
  tick,
  open,
  onToggle,
}: {
  account: AccountRow;
  tick: "added" | "updated" | null;
  open: boolean;
  onToggle: () => void;
}) {
  const archived = accountIsArchived(account);
  return (
    <ListRow
      icon={<Building2 size={16} aria-hidden />}
      name={accountName(account)}
      // `current` is the row's own liveness, and for a client that is simply
      // whether they are still current work. An archived client keeps its
      // facts and loses its ink.
      current={!archived}
      dim={archived}
      open={open}
      onOpen={onToggle}
      state={
        <>
          {archived ? <Chip tone="muted">archived</Chip> : null}
          {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
        </>
      }
    >
      {account.domain === "" ? null : (
        <span className="os-caption os-mono">{account.domain}</span>
      )}
      {account.primaryContactName === "" ? null : (
        <span className="os-caption">{account.primaryContactName}</span>
      )}
      {accountIsSelf(account) ? (
        <Chip tone="accent" title="The company this instance belongs to">
          you
        </Chip>
      ) : null}
    </ListRow>
  );
}

/**
 * Add a client.
 *
 * `name` is the only required field, matching the concept and matching the
 * first-run card: everything else is correctable from the profile, and a form
 * that insists on five fields is one people fill with placeholders.
 */
function CreateAccountForm({
  create,
  onDone,
}: {
  create: CreateAccountState;
  onDone: (createdId: string) => void;
}) {
  const [name, setName] = useState("");
  const [domain, setDomain] = useState("");
  const [contactName, setContactName] = useState("");
  const [contactEmail, setContactEmail] = useState("");

  async function submit() {
    const id = await create.create({
      name,
      domain,
      primaryContactName: contactName,
      primaryContactEmail: contactEmail,
      notes: "",
    });
    if (id !== "") onDone(id);
  }

  return (
    <Panel label="Add a client">
      <div className="os-account-form">
        <div className="os-form-field">
          <label className="os-form-field-label" htmlFor="os-account-new-name">
            Name
          </label>
          <Input
            id="os-account-new-name"
            label="Client name"
            value={name}
            onChange={setName}
            placeholder="Acme Consulting"
            onEnter={submit}
          />
        </div>
        <div className="os-form-field">
          <label className="os-form-field-label" htmlFor="os-account-new-domain">
            Domain
          </label>
          <Input
            id="os-account-new-domain"
            label="Client domain"
            value={domain}
            onChange={setDomain}
            placeholder="acme.com"
          />
        </div>
        <div className="os-form-field">
          <label className="os-form-field-label" htmlFor="os-account-new-contact">
            Primary contact
          </label>
          <Input
            id="os-account-new-contact"
            label="Primary contact name"
            value={contactName}
            onChange={setContactName}
          />
        </div>
        <div className="os-form-field">
          <label className="os-form-field-label" htmlFor="os-account-new-email">
            Contact email
          </label>
          <Input
            id="os-account-new-email"
            label="Primary contact email"
            value={contactEmail}
            onChange={setContactEmail}
          />
        </div>
      </div>

      {create.error === "" ? null : (
        <Notice
          tone="error"
          sentence="This client was not added."
          next="Nothing was written; what is above is still as you typed it."
          detail={create.error}
        />
      )}

      <div className="os-account-form-actions">
        <Button tone="primary" onClick={submit} disabled={create.busy || name.trim() === ""}>
          {create.busy ? "Adding" : "Add client"}
        </Button>
        <Button onClick={() => onDone("")}>Cancel</Button>
      </div>
      <Caption>
        Only the name is required. Everything else can be filled in later from the client's profile.
      </Caption>
    </Panel>
  );
}
