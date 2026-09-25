import { listCount } from "../../kit/RecordRow";
import { holds } from "../../system/roles";
import { AddButton } from "../../kit/AddButton";
import { useEffect, useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { Users } from "lucide-react";

import {
  Button,
  ContentSkeleton,
  Head,
  LiveList,
  Notice,
  Refine,
  RecordRow,
  useLiveView,
  type RefineChip,
} from "../../kit";
import { AccountChip } from "../accounts/AccountPicker";
import { accountName, type AccountRow } from "../accounts/rows";
import type { UsersActions } from "./actions";
import { GroupPage } from "./GroupPage";
import { NewGroupForm } from "./NewGroupForm";
import { groupFromRow, type GroupRow, type InvitationRow, type PersonRow } from "./rows";
import type { RoleCatalog } from "./useRoles";
import type { GroupsView } from "./views";
import type { LiveCollectionHandle } from "../../live/useLiveCollection";

// The groups of this cluster: what a client's people are placed in, and what
// an operator makes to organize.
//
// THREE SIBLING VIEWS, one at a time, one Head each: the list, a group's page,
// and the New group form in place of the list.
//
// ===========================================================================
// THE ROW CARRIES NO MEMBER COUNT, AND THAT IS THE NO-MEMBERSHIP-FEED RULE
// ===========================================================================
// A count per row would mean one `membersOfGroup` read PER GROUP on every
// render of this list -- which is the cluster-wide membership feed the design
// forbids, arrived at one read at a time. The count belongs where the members
// already are: the group's own page, which reads exactly the one group
// somebody opened. The Accounts ledger's People band does fan out, and it is
// bounded by the groups of ONE client.

export function GroupsSection({
  groups,
  people,
  peopleAvailable = false,
  invitations,
  invitationsAvailable = false,
  accounts,
  catalog,
  actions,
  viewerRole,
  showArchived,
  openId,
  onOpened,
  onOpenAccount,
  onOpenPerson,
}: {
  groups: LiveCollectionHandle<Row>;
  people: readonly PersonRow[];
  peopleAvailable?: boolean;
  invitations: readonly InvitationRow[];
  invitationsAvailable?: boolean;
  accounts: readonly AccountRow[];
  catalog: RoleCatalog;
  actions: UsersActions;
  viewerRole: string;
  showArchived: boolean;
  /** A group to open on arrival -- the Accounts page's People band. */
  openId?: string;
  onOpened?: () => void;
  onOpenAccount: (accountId: string) => void;
  onOpenPerson: (userId: string) => void;
}) {
  const [view, setView] = useState<GroupsView>(() =>
    openId ? { kind: "group", groupId: openId } : { kind: "list" },
  );
  const [search, setSearch] = useState("");

  // IN AN EFFECT, not during render. Consuming the intent is a call INTO THE
  // PARENT, and a parent's setState during a child's render is the one thing
  // React will not do -- it warns and, in a StrictMode double render, can
  // consume an instruction twice. The consumption is still id-matched, so
  // acting on a stale render cannot eat a newer one (system/registry.ts).
  useEffect(() => {
    if (!openId) return;
    setView({ kind: "group", groupId: openId });
    onOpened?.();
  }, [openId, onOpened]);

  const all = useMemo(
    () => groups.snapshot.rows.map(groupFromRow).filter((g) => g.id !== ""),
    [groups.snapshot],
  );

  const source = useLiveView<Row, GroupRow>(
    groups.source,
    // Keyed on the question so flipping the archived setting RE-BASELINES the
    // arrival cues rather than announcing every newly-visible row as new.
    `groups:${showArchived}:${search}`,
    (rows) => {
      const needle = search.trim().toLowerCase();
      return rows
        .map(groupFromRow)
        .filter((g) => g.id !== "")
        .filter((g) => showArchived || g.status === "active")
        .filter((g) => needle === "" || g.name.toLowerCase().includes(needle))
        .sort((a, b) => a.name.localeCompare(b.name));
    },
  );

  if (view.kind === "group") {
    const group = all.find((g) => g.id === view.groupId) ?? null;
    if (group === null) {
      if (groups.snapshot.state === "seeding") return <ContentSkeleton label="Opening group" />;
      return (
        <div className="os-app-stack">
          <Head title="Group" back={{ label: "Groups", onSelect: () => setView({ kind: "list" }) }} />
          <Notice
            tone="warn"
            sentence="This group is not in the list this window holds."
            next="It may have been made since this window loaded, or archived."
          />
        </div>
      );
    }
    return (
      <GroupPage
        group={group}
        people={people}
        peopleAvailable={peopleAvailable}
        invitations={invitations}
        invitationsAvailable={invitationsAvailable}
        accounts={accounts}
        catalog={catalog}
        actions={actions}
        viewerRole={viewerRole}
        onBack={() => setView({ kind: "list" })}
        onOpenAccount={onOpenAccount}
        onOpenPerson={onOpenPerson}
      />
    );
  }

  if (view.kind === "new") {
    return (
      <div className="os-app-stack">
        <Head title="New group" back={{ label: "Groups", onSelect: () => setView({ kind: "list" }) }} />
        <NewGroupForm
          accounts={accounts}
          actions={actions}
          onCancel={() => setView({ kind: "list" })}
          onCreated={() => setView({ kind: "list" })}
        />
      </div>
    );
  }

  const chips: RefineChip[] = [];
  const count = listCount(source?.snapshot);

  return (
    <div className="os-app-stack">
      <Head title="Groups" meta={count}>
        {holds("create", "group") ? <AddButton onClick={() => setView({ kind: "new" })} label="New group" /> : null}
      </Head>

      {groups.snapshot.error ? (
        <Notice
          tone="error"
          sentence="This cluster did not return its groups."
          next="Your organization scope and granted group permissions determine which groups you can read."
        >
          <Button onClick={groups.reseed}>Try again</Button>
        </Notice>
      ) : null}

      {/* No filter chrome over no content (rule 2); see PeopleSection. */}
      {count === 0 && search.trim() === "" ? null : (
        <Refine search={search} onSearch={setSearch} chips={chips} label="Refine the groups list" />
      )}

      <LiveList<GroupRow>
        key={`groups:${showArchived}`}
        source={source}
        rowId={(group) => group.id}
        fingerprint={(group) => `${group.name}|${group.description}|${group.status}|${group.accountId}`}
        label="Groups in this cluster"
        emptyText={
          showArchived
            ? "No groups yet. New group is on the Head."
            : "No active groups. New group is on the Head -- or turn on archived groups in this app's settings if you are looking for one that was filed away."
        }
        renderRow={(group, tick) => (
          <GroupLine
            group={group}
            accounts={accounts}
            tick={tick}
            onOpen={() => setView({ kind: "group", groupId: group.id })}
          />
        )}
      />
    </div>
  );
}

function GroupLine({
  group,
  accounts,
  tick,
  onOpen,
}: {
  group: GroupRow;
  accounts: readonly AccountRow[];
  tick: "added" | "updated" | null;
  onOpen: () => void;
}) {
  const account = accounts.find((a) => a.id === group.accountId) ?? null;
  const archived = group.status === "archived";
  // The client's name only when it SAYS something the group's name does not:
  // an account-kind group is named after its client, so a chip beside it reads
  // "Acme Acme". The kind word carries the tie in that case.
  const clientName = account === null ? "" : accountName(account);
  return (
    <RecordRow
      icon={<Users size={16} aria-hidden />}
      name={group.name}
      secondary={group.description}
      state={archived ? "Archived" : "Active"}
      tone={archived ? "muted" : "accent"}
      current={!archived}
      dim={archived}
      onOpen={onOpen}
      stateExtra={
        <>
          {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
        </>
      }
    >
      {clientName === "" || clientName === group.name ? null : <AccountChip name={clientName} />}
      <span className="os-caption">{kindWord(group, account)}</span>
    </RecordRow>
  );
}

/**
 * What KIND of group this is, in a person's words.
 *
 * The self account's group is "your company" rather than the account's own
 * name, because on the cluster's own row the name is the operator's company
 * and repeating it beside the account chip says the same thing twice.
 */
function kindWord(group: GroupRow, account: AccountRow | null): string {
  if (group.accountId === "") return "no client";
  if (account === null) return "a client";
  return account.id === "self" ? "your company" : "a client";
}
