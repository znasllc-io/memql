import { useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { UserRound } from "lucide-react";

import {
  Button,
  Chip,
  FormRow,
  Head,
  Input,
  LiveList,
  Notice,
  Panel,
  Row as ListRow,
  RoleTag,
  Subhead,
  roleRungOf,
} from "../../kit";
import { ActionBar, type Act } from "../../kit/ActionBar";
import { useLiveView } from "../../live/liveView";
import { AccountChip } from "../accounts/AccountPicker";
import { accountIsArchived, accountName, type AccountRow } from "../accounts/rows";
import type { UsersActions } from "./actions";
import { PeoplePicker } from "./pickers";
import { RefusalLine } from "./PersonPage";
import {
  membershipFromRow,
  originSentence,
  personName,
  type GroupRow,
  type InvitationRow,
  type MembershipRow,
  type PersonRow,
} from "./rows";
import { useMembersOfGroup } from "./useGroups";
import type { RoleCatalog } from "./useRoles";

// ONE GROUP: who is in it, who is standing in it by rule, and what it grants.

export function GroupPage({
  group,
  people,
  invitations,
  accounts,
  catalog,
  actions,
  viewerRole,
  onBack,
  onOpenAccount,
  onOpenPerson,
}: {
  group: GroupRow;
  people: readonly PersonRow[];
  invitations: readonly InvitationRow[];
  accounts: readonly AccountRow[];
  catalog: RoleCatalog;
  actions: UsersActions;
  viewerRole: string;
  onBack: () => void;
  onOpenAccount: (accountId: string) => void;
  onOpenPerson: (userId: string) => void;
}) {
  const [adding, setAdding] = useState(false);
  const [renaming, setRenaming] = useState(false);
  const [name, setName] = useState(group.name);
  const [description, setDescription] = useState(group.description);

  const members = useMembersOfGroup(group.id);
  const memberRows = useLiveView<Row, MembershipRow>(
    members.source,
    `members:${group.id}`,
    (rows) => rows.map(membershipFromRow).filter((m) => m.id !== "" && m.status === "active"),
  );
  const memberIds = useMemo(
    () => members.snapshot.rows.map((row) => membershipFromRow(row).userId),
    [members.snapshot],
  );

  const account = accounts.find((a) => a.id === group.accountId) ?? null;

  // THE STANDING STAFF (epic memql#5165, D6) ARE A RULE, NOT ROWS.
  // Developer rank and above are members of every account-kind group by rule,
  // and no query returns those memberships -- so this band is drawn from the
  // ROSTER this window already holds, and it carries no controls: there is
  // nothing to remove, because there is no row to remove.
  const developerRank = catalog.roles.find((r) => r.slug === "developer")?.rank ?? Number.MAX_SAFE_INTEGER;
  const standing = useMemo(
    () =>
      group.kind !== "account"
        ? []
        : people.filter((person) => (roleRungOf(person.role)?.rank ?? -1) >= developerRank),
    [people, group.kind, developerRank],
  );

  // People invited into this group who have not arrived. They come from the
  // invitations feed, because there is no membership row until they accept.
  const invited = useMemo(
    () => invitations.filter((invite) => invite.groupIds.includes(group.id)),
    [invitations, group.id],
  );

  const archived = group.status === "archived";
  const accountStillActive = account !== null && !accountIsArchived(account);

  const acts: Act[] = [];
  if (!archived) {
    acts.push({ label: renaming ? "Cancel" : "Rename", onAct: () => setRenaming((held) => !held) });
    // ARCHIVE IS ABSENT, NOT DISABLED, for an account-kind group whose client
    // is still active (rule 12, and the engine's own `group_account_active`).
    // That group IS the client's; archiving it alone would leave them with no
    // way for their people to reach their work while still looking configured.
    if (!(group.kind === "account" && accountStillActive)) {
      acts.push({ label: "Archive", tone: "danger", onAct: () => void actions.groupArchive(group.id).then((ok) => ok && onBack()) });
    }
  }

  return (
    <div className="os-action-pane">
      <div className="os-action-body os-app-stack">
        <Head title={group.name} meta={group.kind === "account" ? "A client's group" : undefined} back={{ label: "Groups", onSelect: onBack }}>

          {account === null ? null : <AccountChip name={accountName(account)} />}
          {archived ? null : (
            <Button tone="primary" onClick={() => setAdding((held) => !held)}>
              {adding ? "Done" : "Add people"}
            </Button>
          )}
        </Head>

        {adding ? (
          <Panel label="Add people to this group">
            <PeoplePicker
              people={people}
              exclude={memberIds}
              onPick={(userId) => void actions.groupMemberAdd(group.id, userId)}
              label={`Somebody to add to ${group.name}`}
              busyId={actions.busyKey}
            />
            <RefusalLine actions={actions} />
          </Panel>
        ) : null}

        {renaming ? (
          <Panel label="Rename this group">
            <Input id="group-rename" label="Name" value={name} onChange={setName} />
            <Input id="group-redescribe" label="What it is for" value={description} onChange={setDescription} />
            <Button
              tone="primary"
              busy={actions.busyKey === group.id}
              busyLabel="Saving..."
              onClick={() =>
                void actions.groupUpdate(group.id, name.trim(), description.trim()).then((ok) => {
                  if (ok) setRenaming(false);
                })
              }
            >
              Save
            </Button>
            <RefusalLine actions={actions} />
          </Panel>
        ) : null}

        <Panel label={`Members of ${group.name}`}>
          <Subhead>Members</Subhead>
          <LiveList<MembershipRow>
            source={memberRows}
            rowId={(m) => m.id}
            fingerprint={(m) => `${m.userId}|${m.status}|${m.origin}`}
            label={`The people in ${group.name}`}
            emptyText="Nobody in this group yet. Add people from the Head."
            renderRow={(membership) => {
              const person = people.find((p) => p.id === membership.userId) ?? null;
              const addedBy = people.find((p) => p.id === membership.addedBy) ?? null;
              return (
                <ListRow
                  icon={<UserRound size={16} aria-hidden />}
                  name={person ? personName(person) : membership.userId}
                  onOpen={person ? () => onOpenPerson(person.id) : undefined}
                  state={
                    archived ? null : (
                      <Button
                        onClick={() => void actions.groupMemberRemove(group.id, membership.userId)}
                        busy={actions.busyKey === membership.userId}
                        busyLabel="Removing..."
                        ariaLabel={`Remove ${person ? personName(person) : "this person"} from ${group.name}`}
                      >
                        Remove
                      </Button>
                    )
                  }
                >
                  {person === null ? null : <RoleTag role={person.role} actorRole={viewerRole} />}
                  <span className="os-caption">
                    {originSentence(membership, {
                      addedByName: addedBy ? personName(addedBy) : undefined,
                      domain: account?.domain,
                    })}
                  </span>
                </ListRow>
              );
            }}
          />

          {invited.length === 0 ? null : (
            <ul className="os-invited-list" aria-label={`People invited into ${group.name}`}>
              {invited.map((invite) => (
                <li key={invite.id} className="os-invited-row">
                  <span>{invite.inviteeEmail}</span>
                  <Chip title="They join this group when they accept">Invited</Chip>
                </li>
              ))}
            </ul>
          )}
        </Panel>

        {standing.length === 0 ? null : (
          <Panel label={`Managed by, for ${group.name}`}>
            <Subhead>Managed by</Subhead>
            <p className="os-caption">Everyone at developer and above, standing.</p>
            <ul className="os-standing-list" aria-label="Standing members">
              {standing.map((person) => (
                <li key={person.id} className="os-standing-row">
                  <span>{personName(person)}</span>
                  <RoleTag role={person.role} actorRole={viewerRole} />
                </li>
              ))}
            </ul>
          </Panel>
        )}

        {account === null ? null : (
          <Panel label={`The domain of ${accountName(account)}`}>
            <Subhead>Domain</Subhead>
            {/* READ FROM THE ACCOUNTS FEED. The group holds no domain state: the
                rule lives on the client's row, and a second copy here would be a
                second answer to "does joining apply". */}
            <p className="os-caption">
              {account.joinOnDomain && account.domainStatus === "verified"
                ? `Joins on @${account.domain}.`
                : "Joining is off."}
            </p>
            <FormRow>
              <Button onClick={() => onOpenAccount(account.id)}>Open in Accounts</Button>
            </FormRow>
          </Panel>
        )}

        {members.snapshot.error ? (
          <Notice
            tone="error"
            sentence="This cluster did not return this group's members."
            detail={members.snapshot.error}
          />
        ) : null}

        {/* THE ABSENT ACT IS EXPLAINED WHERE THE ACTS ARE (rule 12). Archive is
            missing from this bar for an account-kind group whose client is still
            active, and the sentence that says why belongs beside the acts rather
            than floating under the Head -- a person looking for the control is
            looking here. */}
      </div>
      <ActionBar
        state={archived ? "Archived" : "Active"}
        detail={
          group.kind === "account" && accountStillActive
            ? "Archive the account to archive this group."
            : undefined
        }
        tone={archived ? "paused" : "live"}
        acts={acts}
      />
    </div>
  );
}
