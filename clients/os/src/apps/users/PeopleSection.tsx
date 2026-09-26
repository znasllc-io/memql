import { listCount } from "../../kit/RecordRow";
import { AddButton } from "../../kit/AddButton";
import { useEffect, useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { Mail, UserRound } from "lucide-react";

import {
  Button,
  Chip,
  Head,
  LiveList,
  Notice,
  Refine,
  RecordRow,
  RoleTag,
  Select,
  SortControl,
  useNow,
  useTwoFeedView,
  type RefineChip,
} from "../../kit";
import { formatFreshness } from "../../kit/format";
import type { UsersActions } from "./actions";
import { InvitePage } from "./InvitePage";
import { PersonPage } from "./PersonPage";
import {
  DEFAULT_ROSTER_FILTER,
  daysLeft,
  filterIsNarrowing,
  foldRoster,
  rosterEmail,
  rosterFingerprint,
  rosterName,
  type RosterFilter,
  type RosterRow,
} from "./roster";
import {
  invitationFromRow,
  membershipFromRow,
  personFromRow,
  personIsDim,
  type GroupRow,
  type PersonRow,
} from "./rows";
import { useMembersOfGroup } from "./useGroups";
import type { RoleCatalog } from "./useRoles";
import type { PeopleView } from "./views";
import type { LiveCollectionHandle } from "../../live/useLiveCollection";
import type { AccountRow } from "../accounts/rows";

// The people of this cluster, and the people on their way to it.
//
// ===========================================================================
// THREE SIBLING VIEWS, ONE AT A TIME, ONE HEAD EACH (DESIGN.md rule 11)
// ===========================================================================
// The list, a person's page, and the Invite rail. This section used to render
// `PersonDetail` as a SIBLING of the list, so selecting a row appended a panel
// beneath the list it was selected from -- two Heads in one scroller, which is
// the tell rule 11 names. `DeployablesSection` is the form that fixed it and
// this is the same form.
//
//     People --open a person--> Person
//        |
//        '--Invite-----------> Invite (a rail), which becomes the invited
//                              person's page on success

export function PeopleSection({
  users,
  invites,
  groups,
  accounts,
  catalog,
  actions,
  viewerUserId,
  viewerRole,
  showDeactivated,
  sort,
  emailReady,
  emailGateSentence,
  askContext,
  openId,
  onOpened,
}: {
  users: LiveCollectionHandle<Row>;
  invites: LiveCollectionHandle<Row>;
  groups: readonly GroupRow[];
  accounts: readonly AccountRow[];
  catalog: RoleCatalog;
  actions: UsersActions;
  viewerUserId: string;
  viewerRole: string;
  showDeactivated: boolean;
  sort: "name" | "lastSeen";
  /** Whether the email module is ready. Only SENDING an invitation needs it. */
  emailReady: boolean;
  /** The gate's own sentence, rendered where Invite would be. */
  emailGateSentence: string;
  askContext: (tag: string) => void;
  /** A person to open on arrival -- an intent from another app. */
  openId?: string;
  onOpened?: () => void;
}) {
  const [view, setView] = useState<PeopleView>({ kind: "list" });
  const [filter, setFilter] = useState<RosterFilter>({
    ...DEFAULT_ROSTER_FILTER,
    showDeactivated,
    sort,
  });
  const now = useNow(60_000);

  // The settings are the DEFAULT and the Refine line is the question being
  // asked right now, so a change in Settings lands here without wiping a
  // narrowing somebody is in the middle of.
  useEffect(() => {
    setFilter((held) => ({ ...held, showDeactivated, sort }));
  }, [showDeactivated, sort]);

  useEffect(() => {
    if (!openId) return;
    setView({ kind: "person", userId: openId });
    onOpened?.();
  }, [openId, onOpened]);

  // THE GROUP FACET'S ONE READ. Memberships are read per group, never as a
  // cluster-wide feed (design record, D5), so picking a group here costs
  // exactly one `membersOfGroup` -- and picking none costs nothing.
  const members = useMembersOfGroup(filter.group);
  const memberIds = useMemo(() => {
    if (filter.group === "") return undefined;
    return new Set(
      members.snapshot.rows.map((row) => membershipFromRow(row).userId).filter((id) => id !== ""),
    );
  }, [filter.group, members.snapshot]);

  const source = useTwoFeedView<Row, Row, RosterRow>(
    users.source,
    invites.source,
    // The view key is what re-baselines the arrival cue when the QUESTION
    // changes: without it, revealing deactivated rows would make them flash
    // "new" on the next event, claiming the cluster just sent them.
    `${filter.search}|${filter.role}|${filter.group}|${filter.state}|${filter.showDeactivated}|${filter.sort}|${memberIds?.size ?? -1}`,
    (userRows, inviteRows) =>
      foldRoster(
        userRows.map(personFromRow),
        inviteRows.map(invitationFromRow),
        filter,
        now,
        memberIds,
      ),
  );

  const people = useMemo(
    () => users.snapshot.rows.map(personFromRow).filter((p) => p.id !== ""),
    [users.snapshot],
  );

  if (view.kind === "invite" || view.kind === "invited") {
    return (
      <InvitePage
        invitationId={view.kind === "invited" ? view.invitationId : ""}
        groups={groups}
        accounts={accounts}
        catalog={catalog}
        actions={actions}
        viewerRole={viewerRole}
        onBack={() => setView({ kind: "list" })}
        onIssued={(invitationId: string) => setView({ kind: "invited", invitationId })}
      />
    );
  }

  if (view.kind === "person") {
    const person = people.find((p) => p.id === view.userId) ?? null;
    return (
      <PersonPage
        userId={view.userId}
        seed={person}
        people={people}
        groups={groups}
        accounts={accounts}
        catalog={catalog}
        actions={actions}
        viewerUserId={viewerUserId}
        viewerRole={viewerRole}
        askContext={askContext}
        onBack={() => setView({ kind: "list" })}
      />
    );
  }

  const chips: RefineChip[] = [];
  if (filter.role !== "") {
    chips.push({ id: "role", label: `role ${filter.role}`, onRemove: () => setFilter((f) => ({ ...f, role: "" })) });
  }
  if (filter.group !== "") {
    const named = groups.find((g) => g.id === filter.group)?.name ?? filter.group;
    chips.push({ id: "group", label: named, onRemove: () => setFilter((f) => ({ ...f, group: "" })) });
  }
  if (filter.state !== "") {
    chips.push({ id: "state", label: filter.state, onRemove: () => setFilter((f) => ({ ...f, state: "" })) });
  }

  const count = filter.group !== "" && listCount(members.snapshot) === undefined
    ? undefined
    : listCount(source?.snapshot);

  return (
    <div className="os-app-stack">
      <Head title="People" meta={count}>
        {emailReady ? (
          <AddButton onClick={() => setView({ kind: "invite" })} label="Invite" />
        ) : (
          // THE GATE'S OWN SENTENCE, IN THE ACTION'S PLACE. An app that hides
          // Invite with no account of itself reads as a missing feature, and
          // somebody looking for it has nowhere to find out why.
          <span className="os-caption">{emailGateSentence}</span>
        )}
      </Head>

      {users.snapshot.error ? (
        <Notice
          tone="error"
          sentence="This cluster did not return its people."
          next="Reading the directory is admin and above; the engine decides that, not this window."
        >
          <Button onClick={users.reseed}>Try again</Button>
        </Notice>
      ) : null}

      {/* NO FILTER CHROME OVER NO CONTENT (rule 2). A cluster with nobody in
          it gets its empty state and nothing else -- except when a REFINEMENT
          is why it looks empty, where the controls have to stay or there is no
          way to clear what is hiding everything. */}
      {count === 0 && !filterIsNarrowing(filter) ? null : (
        <>
      <Refine
        search={filter.search}
        onSearch={(next) => setFilter((f) => ({ ...f, search: next }))}
        chips={chips}
        label="Refine the people list"
      >
        <Select
          id="people-role"
          label="Role"
          value={filter.role}
          onChange={(next) => setFilter((f) => ({ ...f, role: next }))}
        >
          <option value="">Any role</option>
          {catalog.roles.map((role) => (
            <option key={role.slug} value={role.slug}>
              {role.name}
            </option>
          ))}
        </Select>
        <Select
          id="people-group"
          label="Group"
          value={filter.group}
          onChange={(next) => setFilter((f) => ({ ...f, group: next }))}
        >
          <option value="">Any group</option>
          {groups
            .filter((g) => g.status === "active")
            .map((group) => (
              <option key={group.id} value={group.id}>
                {group.name}
              </option>
            ))}
        </Select>
        <Select
          id="people-state"
          label="State"
          value={filter.state}
          onChange={(next) => setFilter((f) => ({ ...f, state: next as RosterFilter["state"] }))}
        >
          <option value="">Any state</option>
          <option value="active">Active</option>
          <option value="invited">Invited</option>
          <option value="deactivated">Deactivated</option>
        </Select>
      </Refine>

      <div className="os-list-scope">
        <SortControl
          ascending={filter.sort === "name"}
          onToggle={() =>
            setFilter((f) => ({ ...f, sort: f.sort === "name" ? "lastSeen" : "name" }))
          }
          ascLabel="By name"
          descLabel="By last seen"
        />
      </div>
        </>
      )}

      <LiveList<RosterRow>
        key={`people:${filter.showDeactivated}:${filter.group}`}
        source={source}
        rowId={(row) => row.id}
        fingerprint={rosterFingerprint}
        label="People in this cluster"
        emptyText={emptyTextFor(filter)}
        renderRow={(row, tick) => (
          <RosterLine
            row={row}
            tick={tick}
            now={now}
            viewerRole={viewerRole}
            groups={groups}
            onOpen={() =>
              setView(
                row.kind === "person"
                  ? { kind: "person", userId: row.person.id }
                  : { kind: "invited", invitationId: row.invite.id },
              )
            }
          />
        )}
      />
    </div>
  );
}

/**
 * What an empty list says.
 *
 * IT POINTS AT THE SETTING WHEN HIDING IS WHY IT IS EMPTY (rule 4). A list
 * emptied by a preference and a cluster with nobody in it look identical, and
 * only one of them has a next step.
 */
function emptyTextFor(filter: RosterFilter): string {
  if (filterIsNarrowing(filter)) return "Nobody matches. Clear the refinements to see everyone.";
  if (!filter.showDeactivated) {
    return "Nobody active. Invite someone from the Head -- or turn on deactivated people in this app's settings if you are looking for an account that was retired.";
  }
  return "Nobody yet. Invite someone from the Head.";
}

function RosterLine({
  row,
  tick,
  now,
  viewerRole,
  groups,
  onOpen,
}: {
  row: RosterRow;
  tick: "added" | "updated" | null;
  now: Date;
  viewerRole: string;
  groups: readonly GroupRow[];
  onOpen: () => void;
}) {
  if (row.kind === "invited") {
    const invite = row.invite;
    const named = groups.filter((g) => invite.groupIds.includes(g.id));
    return (
      <RecordRow
        icon={<Mail size={16} aria-hidden />}
        name={rosterName(row)}
        state="Invited"
        onOpen={onOpen}
        stateExtra={
          <>
            <span className="os-caption">{daysLeft(invite, now)}</span>
            {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
          </>
        }
      >
        <RoleTag role={invite.inviteeRole || ""} actorRole={viewerRole} />
        {named.map((group) => (
          <Chip key={group.id} title="They join this group when they accept">
            {group.name}
          </Chip>
        ))}
      </RecordRow>
    );
  }

  const person = row.person;
  const dim = personIsDim(person);
  return (
    <RecordRow
      icon={<UserRound size={16} aria-hidden />}
      name={rosterName(row)}
      secondary={rosterEmail(row)}
      state={dim ? "Inactive" : "Active"}
      tone={dim ? "muted" : "accent"}
      current={!dim}
      dim={dim}
      onOpen={onOpen}
      stateExtra={
        <>
          <span className="os-caption">{formatFreshness(person.lastSeenAt, now)}</span>
          {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
        </>
      }
    >
      <RoleTag role={person.role || ""} actorRole={viewerRole} />
      <SignInPolicyChip person={person} />
    </RecordRow>
  );
}

/**
 * The sign-in policy, as a column.
 *
 * `passkey_only` is the one worth a chip: it is a choice somebody made about
 * their own account and the state an admin may have to rescue them out of.
 * `any` is the default and gets no chip -- a badge on every row is not a
 * column, it is noise.
 */
function SignInPolicyChip({ person }: { person: PersonRow }) {
  if (person.signInPolicy !== "passkey_only") return null;
  return (
    <Chip title="Sign-in links are disabled on this account; a passkey is the only way in.">
      passkey only
    </Chip>
  );
}
