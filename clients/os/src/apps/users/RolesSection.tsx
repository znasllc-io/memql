import { AddButton } from "../../kit/AddButton";
import { useMemo, useState } from "react";
import { Shield } from "lucide-react";

import { Button, Chip, Head, Notice, RankMark, Row as ListRow } from "../../kit";
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
  accounts,
  actions,
  viewerRole,
  onOpenPerson,
}: {
  catalog: RoleCatalog;
  people: readonly PersonRow[];
  accounts: readonly AccountRow[];
  actions: UsersActions;
  viewerRole: string;
  onOpenPerson: (userId: string) => void;
}) {
  const [view, setView] = useState<RolesView>({ kind: "list" });
  const ladder = useMemo(() => ladderDescending(catalog.roles), [catalog.roles]);

  const mayCreate = roleHolds(catalog.grants, viewerRole, "create", "role");

  if (view.kind === "role") {
    const role = ladder.find((r) => r.slug === view.slug) ?? null;
    if (role === null) {
      return (
        <div className="os-app-stack">
          <Head title="Role">
            <Button tone="quiet" onClick={() => setView({ kind: "list" })}>
              Roles
            </Button>
          </Head>
          <Notice tone="warn" sentence="This role is not in the catalog this window holds." />
        </div>
      );
    }
    return (
      <RolePage
        role={role}
        catalog={catalog}
        people={people}
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
      <Head title="Roles" meta={ladder.length === 0 ? undefined : `${ladder.length}`}>
        {/* OFFERED ONLY WHERE IT WOULD WORK. `create` on `role` is the grant
            roleCreate checks, and a New role button for somebody who does not
            hold it is a form whose every submission is refused. */}
        {mayCreate ? (
          <AddButton onClick={() => setView({ kind: "new" })} label="New role" />
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

      <ul className="os-role-list" aria-label="This cluster's roles, strongest first">
        {ladder.map((role) => (
          <li key={role.slug}>
            <RoleLine
              role={role}
              accounts={accounts}
              holders={holderCount(people, role)}
              viewerRole={viewerRole}
              onOpen={() => setView({ kind: "role", slug: role.slug })}
            />
          </li>
        ))}
      </ul>

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
  holders: number;
  viewerRole: string;
  onOpen: () => void;
}) {
  const account = accounts.find((a) => a.id === role.accountId) ?? null;
  return (
    <ListRow
      icon={<Shield size={16} aria-hidden />}
      name={role.name}
      current={role.active}
      dim={!role.active}
      onOpen={onOpen}
      state={
        <span className="os-caption">
          {holders === 0 ? "nobody" : holders === 1 ? "1 person" : `${holders} people`}
        </span>
      }
    >
      <RankMark actorRole={viewerRole} ownerRole={role.slug} />
      <span className="os-role-slug">{role.slug}</span>
      {role.predefined ? (
        <span className="os-caption">predefined</span>
      ) : account !== null ? (
        <AccountChip name={accountName(account)} />
      ) : role.accountId !== "" ? (
        <Chip>scoped</Chip>
      ) : null}
      {role.active ? null : <span className="os-users-inactive-tag">retired</span>}
    </ListRow>
  );
}
