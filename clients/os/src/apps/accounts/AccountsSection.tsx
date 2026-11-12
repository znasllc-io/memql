import { AddAccountWizard } from "./AddAccountWizard";
import { DomainSetup } from "./DomainSetup";
import { AddButton } from "../../kit/AddButton";
import { useEffect, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { Building2, } from "lucide-react";

import { Button, Chip, ContentSkeleton, Head, LiveList, Notice, RecordRow, Refine, listCount } from "../../kit";
import { useLiveView } from "../../live/liveView";
import { AccountPage, type AccountTab } from "./AccountPage";
import {
  accountFingerprint,
  accountFromRow,
  accountIsArchived,
  accountIsSelf,
  accountName,
  SELF_ACCOUNT_ID,
  type AccountRow,
} from "./rows";
import { useAccounts, useAccountRecord } from "./useAccounts";
import { LoadMore } from "../../kit/LoadMore";
import { useDebouncedValue, TEXT_DEBOUNCE_MS } from "../../logs/useDebouncedValue";
import type { ArchiveAccountState, CreateAccountState, UpdateAccountState } from "./actions";

export function AccountsSection({
  initialOpenId = "",
  onOpened,
  showArchived,
  create,
  update,
  archive,
  onOpenGroup,
}: {
  initialOpenId?: string;
  onOpened?: () => void;
  showArchived: boolean;
  create: CreateAccountState;
  update: UpdateAccountState;
  archive: ArchiveAccountState;
  /** Opens the Users app on one of this client's groups, by intent. */
  onOpenGroup?: (groupId: string) => void;
}) {
  const [searchText, setSearchText] = useState("");
  const search = useDebouncedValue(searchText.trim(), TEXT_DEBOUNCE_MS);
  const feed = useAccounts(search, showArchived);
  const { source: collection, snapshot, reseed } = feed;
  const [openId, setOpenId] = useState(initialOpenId);
  const record = useAccountRecord(openId);
  useEffect(() => { if (initialOpenId) { setOpenId(initialOpenId); onOpened?.(); } }, [initialOpenId, onOpened]);
  const [adding, setAdding] = useState(false);
  const [domains, setDomains] = useState(false);
  // THE TAB IS HELD HERE, beside the open account, rather than inside the
  // page: the domain wizard replaces the page while it runs, and coming back
  // from it lands on the tab it was opened from. It is the tab OF ONE
  // ACCOUNT, so opening another starts on that account's first tab.
  const [view, setView] = useState<{ accountId: string; tab: AccountTab } | null>(null);

  const source = useLiveView<Row, AccountRow>(collection, `accounts:${showArchived}:${search}`, rows => rows.map(accountFromRow).filter(account => account.id).sort((a, b) => b.createdAt.localeCompare(a.createdAt) || a.id.localeCompare(b.id)));
  const open = record.snapshot.rows[0] ? accountFromRow(record.snapshot.rows[0]) : source?.snapshot.rows.find(account => account.id === openId);

  if (adding) return <AddAccountWizard create={create} onBack={() => setAdding(false)} onDone={id => { setAdding(false); setOpenId(id); reseed(); }} />;
  if (open && domains) return <DomainSetup account={open} onBack={() => setDomains(false)} onSaved={() => { setDomains(false); reseed(); }} />;
  if (open) return <AccountPage key={open.id} account={open} accounts={source?.snapshot.rows ?? []}
    tab={view?.accountId === open.id ? view.tab : undefined} onTab={tab => setView({ accountId: open.id, tab })}
    onBack={() => setOpenId("")} update={update} archive={archive} onArchived={() => setOpenId("")}
    onOpenGroup={onOpenGroup} onConfigureDomains={() => setDomains(true)}
    onOpenPrimaryData={() => { setOpenId(SELF_ACCOUNT_ID); setView({ accountId: SELF_ACCOUNT_ID, tab: "data" }); }} />;

  if (openId && record.snapshot.state === "seeding") return <ContentSkeleton label="Opening account" />;

  return (
    <div className="os-app-stack">
      <Head title="Accounts" meta={listCount(source?.snapshot) === undefined || search !== searchText.trim() ? undefined : `${source?.snapshot.rows.length ?? 0} shown`}>
        {!searchText ? <span className="os-caption">Recent</span> : null}
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

      <Refine search={searchText} onSearch={setSearchText} label="Find accounts" placeholder="Search accounts" />

      {/* Each question owns its session pages and arrival baseline. */}
      <LiveList<AccountRow>
        key={`accounts:${showArchived}:${search}`}
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
      <LoadMore feed={feed} />

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
