import { useMemo, useState } from "react";
import { ArrowLeft, UserRound } from "lucide-react";

import {
  Button,
  Fact,
  Facts,
  Head,
  Panel,
  RankMark,
  Row as ListRow,
  Subhead,
} from "../../kit";
import { ActionBar, type Act } from "../../kit/ActionBar";
import { accountName, type AccountRow } from "../accounts/rows";
import type { UsersActions } from "./actions";
import { Grid } from "./GridView";
import { heldPairs } from "./grid";
import { RefusalLine } from "./PersonPage";
import { personName, roleHolds, type PersonRow, type RoleRow } from "./rows";
import { ladderDescending, type RoleCatalog } from "./useRoles";

// ONE ROLE: where it sits, what it holds, and who holds it.

export function RolePage({
  role,
  catalog,
  people,
  accounts,
  actions,
  viewerRole,
  onBack,
  onOpenPerson,
}: {
  role: RoleRow;
  catalog: RoleCatalog;
  people: readonly PersonRow[];
  accounts: readonly AccountRow[];
  actions: UsersActions;
  viewerRole: string;
  onBack: () => void;
  onOpenPerson: (userId: string) => void;
}) {
  const ladder = useMemo(() => ladderDescending(catalog.roles), [catalog.roles]);
  const held = useMemo(() => heldPairs(catalog.grants, role.slug), [catalog.grants, role.slug]);
  const callerHolds = useMemo(() => heldPairs(catalog.grants, viewerRole), [catalog.grants, viewerRole]);
  const [draft, setDraft] = useState<string[] | null>(null);

  // EDITABLE means two things at once, and both are the server's rules said
  // here rather than guessed: a predefined role is immutable at runtime (the
  // engine's own base-role guard), and editing any role at all needs `update`
  // on `role`.
  const editable = !role.predefined && roleHolds(catalog.grants, viewerRole, "update", "role");
  const pairs = draft ?? held;

  const holders = useMemo(
    () => people.filter((p) => p.role === role.slug || role.aliases.includes(p.role)),
    [people, role],
  );

  const account = accounts.find((a) => a.id === role.accountId) ?? null;

  const acts: Act[] = [];
  if (draft !== null) {
    acts.push({ label: "Discard", onAct: () => setDraft(null) });
    acts.push({
      label: "Save permissions",
      tone: "primary",
      busy: actions.busyKey === role.slug,
      onAct: () => void actions.roleUpdate(role.slug, draft).then((ok) => ok && setDraft(null)),
    });
  } else if (!role.predefined && role.active && roleHolds(catalog.grants, viewerRole, "delete", "role")) {
    // DEACTIVATE IS ABSENT WHILE ANYONE HOLDS IT (rule 12). A role somebody
    // holds cannot be retired without deciding what those people become, and
    // that decision is not this button's to make.
    if (holders.length === 0) {
      acts.push({
        label: "Deactivate",
        tone: "danger",
        onAct: () => void actions.roleDeactivate(role.slug).then((ok) => ok && onBack()),
      });
    }
  }

  return (
    <div className="os-app-stack">
      <Head title={role.name} meta={role.slug}>
        <Button tone="quiet" onClick={onBack} ariaLabel="Back to Roles">
          <ArrowLeft size={13} aria-hidden /> Roles
        </Button>
      </Head>

      <Panel label={`Where ${role.name} sits`}>
        <Subhead>The ladder</Subhead>
        <ul className="os-role-ladder" aria-label="This cluster's roles, strongest first">
          {ladder.map((rung) => (
            <li
              key={rung.slug}
              className="os-role-rung"
              data-current={rung.slug === role.slug ? "" : undefined}
            >
              <span className="os-role-rung-line" data-static="true">
                <RankMark actorRole={viewerRole} ownerRole={rung.slug} />
                <span className="os-role-rung-name">{rung.name}</span>
                <span className="os-role-slug">{rung.slug}</span>
                {rung.slug === role.slug ? <span className="os-role-rung-held">this one</span> : null}
              </span>
            </li>
          ))}
        </ul>
        <Facts>
          <Fact label="Rank" value={String(role.rank)} mono />
          <Fact
            label="Scope"
            value={account === null ? (role.accountId === "" ? "Everywhere" : role.accountId) : accountName(account)}
          />
          <Fact label="Defined by" value={role.predefined ? "The cluster" : "Somebody here"} />
        </Facts>
        {role.description === "" ? null : <p className="os-caption">{role.description}</p>}
      </Panel>

      <Panel label={`What ${role.name} holds`}>
        <Subhead>Permissions</Subhead>
        <Grid
          held={pairs}
          callerHolds={callerHolds}
          editable={editable}
          label={`What ${role.name} holds`}
          onToggle={(pair, next) =>
            setDraft((current) => {
              const base = current ?? held;
              return next ? [...base, pair] : base.filter((p) => p !== pair);
            })
          }
        />
        {role.predefined ? (
          <p className="os-caption">
            A predefined role is the cluster's own and cannot be edited here. Make a role that
            starts from it instead.
          </p>
        ) : editable ? null : (
          <p className="os-caption">Editing a role needs a role that holds update on role.</p>
        )}
        <RefusalLine actions={actions} />
      </Panel>

      <Panel label={`Who holds ${role.name}`}>
        <Subhead>Holders</Subhead>
        {holders.length === 0 ? (
          <p className="os-caption">Nobody holds this role.</p>
        ) : (
          <ul className="os-holder-list" aria-label={`People who hold ${role.name}`}>
            {holders.map((person) => (
              <li key={person.id}>
                <ListRow
                  icon={<UserRound size={16} aria-hidden />}
                  name={personName(person)}
                  onOpen={() => onOpenPerson(person.id)}
                >
                  <span className="os-caption os-mono">{person.primaryEmail}</span>
                </ListRow>
              </li>
            ))}
          </ul>
        )}
        {holders.length === 0 || role.predefined ? null : (
          <p className="os-caption">
            Held by {holders.length === 1 ? "1 person" : `${holders.length} people`}; move them
            first.
          </p>
        )}
      </Panel>

      <ActionBar
        state={role.active ? "Active" : "Deactivated"}
        detail={role.active ? undefined : "People who held it keep it; nobody new gets it."}
        tone={role.active ? "live" : "paused"}
        acts={acts}
      />
    </div>
  );
}
