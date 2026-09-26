import { AddButton } from "../../kit/AddButton";
import { useEffect, useMemo, useState } from "react";
import { Shield } from "lucide-react";

import { Button, Chip, Head, Notice, RankMark, RecordRow, RecordList } from "../../kit";
import { AccountChip } from "../accounts/AccountPicker";
import { accountName, type AccountRow } from "../accounts/rows";
import type { UsersActions } from "./actions";
import { NewRolePage } from "./NewRolePage";
import { RolePage } from "./RolePage";
import { roleHolds, type PersonRow, type RoleRow } from "./rows";
import { holderCount, ladderDescending, type RoleCatalog } from "./useRoles";
import type { RolesView } from "./views";

// The cluster's roles, drawn as the ladder they are.
//
// NOT A LIVE LIST, and this is the one section of this app that is not. The
// catalog is two small registries consumed whole (`activeRoles` and
// `activeCapabilities` both declare `@unbounded` on that ground), the grid is a
// function of BOTH, and there is no arrival worth announcing: nobody watches a
// role list for a role to appear. It re-reads when either concept broadcasts --
// see useRoles.ts.

export function RolesSection({
  catalog,
  people,
  peopleAvailable = false,
  accounts,
  actions,
  viewerRole,
  onOpenPerson,
  createForAccountId = "",
  onOpened,
}: {
  createForAccountId?: string;
  onOpened?: () => void;
  catalog: RoleCatalog;
  people: readonly PersonRow[];
  peopleAvailable?: boolean;
  accounts: readonly AccountRow[];
  actions: UsersActions;
  viewerRole: string;
  onOpenPerson: (userId: string) => void;
}) {
  const [view, setView] = useState<RolesView>({ kind: "list" });
  const [initialAccountId, setInitialAccountId] = useState("");
  useEffect(() => {
    if (!createForAccountId) return;
    setInitialAccountId(createForAccountId);
    setView({ kind: "new" });
    onOpened?.();
  }, [createForAccountId, onOpened]);
  const ladder = useMemo(() => ladderDescending(catalog.roles), [catalog.roles]);

  const mayCreate = roleHolds(catalog.grants, viewerRole, "create", "role");

  if (view.kind === "role") {
    const role = ladder.find((r) => r.slug === view.slug) ?? null;
    if (role === null) {
      return (
        <div className="os-app-stack">
          <Head title="Role" back={{ label: "Roles", onSelect: () => setView({ kind: "list" }) }} />
          <Notice tone="warn" sentence="This role is not in the catalog this window holds." />
        </div>
      );
    }
    return (
      <RolePage
        role={role}
        catalog={catalog}
        people={people}
        peopleAvailable={peopleAvailable}
        accounts={accounts}
        actions={actions}
        viewerRole={viewerRole}
        onBack={() => setView({ kind: "list" })}
        onOpenPerson={onOpenPerson}
      />
    );
  }

  if (view.kind === "new") {
    return (
      <NewRolePage
        key={initialAccountId}
        initialAccountId={initialAccountId}
        catalog={catalog}
        accounts={accounts}
        actions={actions}
        viewerRole={viewerRole}
        onBack={() => setView({ kind: "list" })}
        onCreated={(slug) => setView({ kind: "role", slug })}
      />
    );
  }

  return (
    <div className="os-app-stack">
      <Head title="Roles" meta={catalog.state === "ready" && !catalog.error ? ladder.length : undefined}>
        {/* OFFERED ONLY WHERE IT WOULD WORK. `create` on `role` is the grant
            roleCreate checks, and a New role button for somebody who does not
            hold it is a form whose every submission is refused. */}
        {mayCreate ? (
          <AddButton onClick={() => { setInitialAccountId(""); setView({ kind: "new" }); }} label="New role" />
        ) : null}
      </Head>

      {catalog.state === "error" ? (
        <Notice
          tone="error"
          sentence="This cluster did not return its roles."
          next="What is drawn below is the last answer this window had."
          detail={catalog.error}
        >
          <Button onClick={catalog.reload}>Try again</Button>
        </Notice>
      ) : null}

      <RecordList as="ul" label="This cluster's roles, strongest first">
        {ladder.map((role) => (
          <RoleLine
              key={role.slug}
              role={role}
              accounts={accounts}
              holders={peopleAvailable ? holderCount(people, role) : undefined}
              viewerRole={viewerRole}
              onOpen={() => setView({ kind: "role", slug: role.slug })}
          />
        ))}
      </RecordList>

      {ladder.length === 0 && catalog.state === "ready" ? (
        <p className="os-caption">
          No roles in the catalog. That is a cluster whose seeds have not run; roles are seeded on
          every start.
        </p>
      ) : null}
    </div>
  );
}

function RoleLine({
  role,
  accounts,
  holders,
  viewerRole,
  onOpen,
}: {
  role: RoleRow;
  accounts: readonly AccountRow[];
  holders: number | undefined;
  viewerRole: string;
  onOpen: () => void;
}) {
  const account = accounts.find((a) => a.id === role.accountId) ?? null;
  return (
    <RecordRow
      icon={<Shield size={16} aria-hidden />}
      name={role.name}
      secondary={role.slug}
      state={role.active ? "Active" : "Retired"}
      tone={role.active ? "accent" : "muted"}
      current={role.active}
      dim={!role.active}
      onOpen={onOpen}
      stateExtra={holders === undefined ? null :
        <span className="os-caption">
          {holders === 0 ? "nobody" : holders === 1 ? "1 person" : `${holders} people`}
        </span>
      }
    >
      <RankMark actorRole={viewerRole} ownerRole={role.slug} />
      {role.predefined ? (
        <span className="os-caption">predefined</span>
      ) : account !== null ? (
        <AccountChip name={accountName(account)} />
      ) : role.accountId !== "" ? (
        <Chip>scoped</Chip>
      ) : null}
    </RecordRow>
  );
}
