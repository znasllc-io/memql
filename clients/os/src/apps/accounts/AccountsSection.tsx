import { AddAccountWizard } from "./AddAccountWizard";
import { DomainSetup } from "./DomainSetup";
import { AddButton } from "../../kit/AddButton";
import { useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { Building2, } from "lucide-react";

import { Button, Chip, Head, LiveList, Notice, RecordRow, listCount } from "../../kit";
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
  feed,
  showArchived,
  create,
  update,
  archive,
  onOpenGroup,
}: {
  /** The app root's ONE feed. Not retained here -- see AccountsApp for why a
   *  second collection over this concept is the failure to avoid. */
  feed: ReturnType<typeof useAccounts>;
  showArchived: boolean;
  create: CreateAccountState;
  update: UpdateAccountState;
  archive: ArchiveAccountState;
  /** Opens the Users app on one of this client's groups, by intent. */
  onOpenGroup?: (groupId: string) => void;
}) {
  const { source: collection, snapshot, reseed } = feed;
  const [openId, setOpenId] = useState("");
  const [adding, setAdding] = useState(false);
  const [domains, setDomains] = useState(false);

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

  if (adding) return <AddAccountWizard create={create} onBack={() => setAdding(false)} onDone={id => { setAdding(false); setOpenId(id); reseed(); }} />;
  if (open && domains) return <DomainSetup account={open} onBack={() => setDomains(false)} onSaved={() => { setDomains(false); reseed(); }} />;
  if (open) return <div className="os-app-stack"><Head title={accountName(open)} back={{ label: "Accounts", onSelect: () => setOpenId("") }} /><AccountDetail account={open} update={update} archive={archive} onArchived={() => setOpenId("")} onOpenGroup={onOpenGroup} onConfigureDomains={() => setDomains(true)} /></div>;

  return (
    <div className="os-app-stack">
      <Head title="Accounts" meta={listCount(source?.snapshot)}>
        <AddButton onClick={() => setAdding((v) => !v)} label="Add a client" />
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
            : "No active clients. Add one above -- or turn on archived clients in this app's settings if you are looking for one you filed away."
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
    <RecordRow
      icon={<Building2 size={16} aria-hidden />}
      name={accountName(account)}
      // `current` is the row's own liveness, and for a client that is simply
      // whether they are still current work. An archived client keeps its
      // facts and loses its ink.
      current={!archived}
      dim={archived}
      open={open}
      onOpen={onToggle}
      secondary={account.domain || account.primaryContactName || undefined}
      state={archived ? "archived" : "active"}
      tone={archived ? "muted" : "accent"}
      stateExtra={
        <>
          {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
        </>
      }
    >
      {account.primaryContactName === "" || account.domain === "" ? null : (
        <span className="os-caption">{account.primaryContactName}</span>
      )}
      {accountIsSelf(account) ? (
        <Chip tone="accent" title="The company this instance belongs to">
          you
        </Chip>
      ) : null}
    </RecordRow>
  );
}
