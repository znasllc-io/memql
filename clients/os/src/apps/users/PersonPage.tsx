import { useCallback, useEffect, useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { ActionBar, type Act } from "../../kit/ActionBar";
import {
  Button,
  Chip,
  CopyValue,
  Fact,
  Facts,
  FormRow,
  Head,
  LiveList,
  Notice,
  Panel,
  PeerRowReadOnly,
  Subhead,
  formatFreshness,
  formatMoment,
  roleRungOf,
  useNow,
} from "../../kit";
import { useLiveView } from "../../live/liveView";
import { useOsConnection } from "../../live/connection";
import type { AccountRow } from "../accounts/rows";
import { accountName } from "../accounts/rows";
import type { AssignContext } from "./assign";
import { ladderSentence, rungRefusal } from "./assign";
import type { UsersActions } from "./actions";
import { GroupPicker, RoleLadderPicker } from "./pickers";
import {
  membershipFromRow,
  originSentence,
  personFromRow,
  personName,
  type GroupRow,
  type MembershipRow,
  type PersonRow,
} from "./rows";
import { ladderDescending, type RoleCatalog } from "./useRoles";
import { useGroupsForUser } from "./useGroups";
import { rereadPerson } from "./usePeople";
import { useSessions } from "./useSessions";

// ONE PERSON: what they are, where they belong, and how they get in.
//
// ===========================================================================
// IT RE-READS ON OPEN, AND EVERY WRITE HANDS ITS VALUE BACK
// ===========================================================================
// `v1:identity:user` broadcasts CREATES ONLY, and this epic must not add an
// update rule (usePeople.ts carries the volume reasoning: a user row churns on
// `lastSeenAt`, so an update rule is an event per person per heartbeat across
// the mesh forever). So the list's copy of a person is only as fresh as the
// seed, an accepted write produces no event, and this page pays for one
// authorized read when it opens and keeps every accepted value it is handed.
//
// The GROUPS panel is the exception, and it is live: memberships DO broadcast
// (component/node/routing.go), so somebody being added in another window
// arrives here on its own.

export function PersonPage({
  userId,
  seed,
  people,
  groups,
  accounts,
  catalog,
  actions,
  viewerUserId,
  viewerRole,
  onBack,
}: {
  userId: string;
  /** The list's row, rendered until the page's own read lands. */
  seed: PersonRow | null;
  /** The roster, for naming whoever added somebody to a group. */
  people: readonly PersonRow[];
  groups: readonly GroupRow[];
  accounts: readonly AccountRow[];
  catalog: RoleCatalog;
  actions: UsersActions;
  viewerUserId: string;
  viewerRole: string;
  askContext: (tag: string) => void;
  onBack: () => void;
}) {
  const connection = useOsConnection();
  const [local, setLocal] = useState<PersonRow | null>(seed);
  const [rereadFailed, setRereadFailed] = useState(false);
  const [adding, setAdding] = useState(false);
  const now = useNow(60_000);

  useEffect(() => {
    if (connection === null || userId === "") return;
    const controller = new AbortController();
    let alive = true;
    void (async () => {
      try {
        const row = await rereadPerson(connection.query, userId, controller.signal);
        if (!alive || row === null) return;
        setLocal(personFromRow(row));
      } catch {
        // BEST-EFFORT BY CONTRACT: the page already has the list's row, so a
        // failed re-read degrades to showing that. It says so, because
        // silently showing possibly-stale values is the failure this note
        // exists to prevent.
        if (alive) setRereadFailed(true);
      }
    })();
    return () => {
      alive = false;
      controller.abort();
    };
  }, [connection, userId]);

  const memberships = useGroupsForUser(userId);
  const membershipRows = useLiveView<Row, MembershipRow>(
    memberships.source,
    `memberships:${userId}`,
    (rows) => rows.map(membershipFromRow).filter((m) => m.id !== ""),
  );
  const held = useMemo(
    () => memberships.snapshot.rows.map(membershipFromRow),
    [memberships.snapshot],
  );
  const sessions = useSessions(userId);

  const person = local;
  const ladder = useMemo(() => ladderDescending(catalog.roles), [catalog.roles]);
  const viewerRung = roleRungOf(viewerRole);
  const targetRung = person ? roleRungOf(person.role) : null;

  // The accounts this person reaches, through the groups they are in. It is
  // what decides whether an account-scoped role is offered (assign.ts, rule 6).
  const targetAccountIds = useMemo(() => {
    const byId = new Map(groups.map((g) => [g.id, g] as const));
    return held
      .filter((m) => m.status === "active")
      .map((m) => byId.get(m.groupId)?.accountId ?? "")
      .filter((id) => id !== "");
  }, [held, groups]);

  const assignContext: AssignContext = {
    kind: "reRole",
    callerRole: viewerRole,
    callerRank: viewerRung?.rank ?? 0,
    callerIsOwner: (viewerRung?.slug ?? "") === "owner",
    grants: catalog.grants,
    targetRole: person?.role ?? "",
    targetRank: targetRung?.rank ?? 0,
    targetIsOwner: (targetRung?.slug ?? "") === "owner",
    targetAccountIds,
  };

  // WHETHER THIS PERSON IS THE VIEWER'S TO GOVERN AT ALL. A peer -- somebody
  // at or above the viewer's rank -- is READ-ONLY, and that state had no
  // vocabulary in this shell before rank-visible reads: a row you could not
  // edit used to be a row you could not see.
  const governable =
    person !== null &&
    person.id !== viewerUserId &&
    ((viewerRung?.slug ?? "") === "owner" || (targetRung?.rank ?? 0) < (viewerRung?.rank ?? 0));

  const applyRole = useCallback(
    async (slug: string) => {
      if (person === null || slug === person.role) return;
      const ok = await actions.setRole(person.id, slug);
      // The reply carries no row, so the new value is the one we sent -- and
      // only after the server accepted it. Setting it optimistically would
      // leave a refused change on screen as though it had happened.
      if (ok) setLocal((row) => (row === null ? row : { ...row, role: slug }));
    },
    [actions, person],
  );

  const applyReset = useCallback(async () => {
    if (person === null) return;
    const ok = await actions.resetSignInPolicy(person.id);
    if (ok) setLocal((row) => (row === null ? row : { ...row, signInPolicy: "any" }));
  }, [actions, person]);

  const applySuspended = useCallback(
    async (suspended: boolean) => {
      if (person === null) return;
      const ok = await actions.setSuspended(person.id, suspended);
      if (ok) {
        setLocal((row) =>
          row === null ? row : { ...row, active: !suspended, suspendedAt: suspended ? now.toISOString() : "" },
        );
      }
    },
    [actions, person, now],
  );

  if (person === null) {
    return (
      <div className="os-app-stack">
        <Head title="Person" back={{ label: "People", onSelect: onBack }} />
        <Notice
          tone="warn"
          sentence="This person is not in the list this window holds."
          next="They may have been added since it loaded, or they may not be yours to read."
        />
      </div>
    );
  }

  const name = personName(person);
  const refusals = ladder.map((rung) => rungRefusal(rung, assignContext));
  const sentence = ladderSentence(refusals);

  const acts: Act[] = [];
  if (governable) {
    if (person.active && person.suspendedAt === "") {
      if (person.signInPolicy === "passkey_only") {
        acts.push({ label: "Reset sign-in policy", onAct: () => void applyReset() });
      }
      acts.push({ label: "Deactivate", tone: "danger", onAct: () => void applySuspended(true) });
    } else {
      acts.push({ label: "Reactivate", tone: "primary", onAct: () => void applySuspended(false) });
    }
  }

  return (
    <div className="os-action-pane" data-os-page-context={JSON.stringify({ page: "Person", personId: person.id, name })}>
      <div className="os-action-body os-app-stack os-person-page">
        <Head title={name} meta={person.primaryEmail} back={{ label: "People", onSelect: onBack }} />

        {rereadFailed ? (
          <Notice
            tone="warn"
            sentence="These are the values this window already had."
            next="Re-reading this person did not succeed, so anything changed since the list loaded is not shown here."
          />
        ) : null}

        {governable ? null : (
          <PeerRowReadOnly actorRole={viewerRole} ownerRole={person.role} ownerName={name} />
        )}

        {/* ---- role ---- */}
        <Panel label={`Role for ${name}`}>
          <Subhead>Role</Subhead>
          <RoleLadderPicker
            rungs={ladder}
            value={person.role}
            onChange={(slug) => void applyRole(slug)}
            context={assignContext}
            actorRole={viewerRole}
            label={`The cluster's roles, for ${name}`}
            busy={actions.busyKey === person.id}
          />
          {sentence === "" ? null : <p className="os-caption">{sentence}</p>}
          <RefusalLine actions={actions} />
        </Panel>

        {/* ---- groups ---- */}
        <Panel label={`Groups for ${name}`}>
          <Subhead>Groups</Subhead>
          {isStaff(person, catalog) ? (
            // THE STANDING STAFF RULE IS A RULE, NOT ROWS (epic memql#5165, D6).
            // Developer rank and above are members of every account's group by
            // rule, and no query returns those memberships -- so a list here
            // would be either empty (wrong) or invented (worse). One sentence
            // says the true thing.
            <p className="os-caption">In every account's group, standing.</p>
          ) : (
            <>
              <LiveList<MembershipRow>
                source={membershipRows}
                rowId={(m) => m.id}
                fingerprint={(m) => `${m.groupId}|${m.status}|${m.origin}`}
                label={`The groups ${name} is in`}
                emptyText="In no groups yet. Adding somebody to a group is what lets them reach a client's work."
                renderRow={(membership) => (
                  <MembershipLine
                    membership={membership}
                    groups={groups}
                    accounts={accounts}
                    people={people}
                    onRemove={() => void actions.groupMemberRemove(membership.groupId, person.id)}
                    busy={actions.busyKey === person.id}
                    removable={governable}
                  />
                )}
              />
              {governable ? (
                adding ? (
                  <FormRow>
                    <GroupPicker
                      groups={groups.filter(
                        (g) => !held.some((m) => m.groupId === g.id && m.status === "active"),
                      )}
                      selected={[]}
                      onChange={(next) => {
                        const groupId = next[0];
                        if (groupId === undefined) return;
                        void actions.groupMemberAdd(groupId, person.id).then((ok) => {
                          if (ok) setAdding(false);
                        });
                      }}
                      label={`Groups to add ${name} to`}
                      accountNameOf={(accountId) => accountLabel(accounts, accountId)}
                      single
                    />
                    <Button onClick={() => setAdding(false)}>Cancel</Button>
                  </FormRow>
                ) : (
                  // A `FormRow` around it, so the control HUGS its label rather
                  // than stretching the panel's whole width: a Panel is a flex
                  // column, so a bare button in one becomes a full-width bar
                  // that reads as a banner rather than as an act.
                  <FormRow>
                    <Button onClick={() => setAdding(true)}>Add to a group</Button>
                  </FormRow>
                )
              ) : null}
            </>
          )}
        </Panel>

        {/* ---- sign-in ---- */}
        <Panel label={`Sign-in for ${name}`}>
          <Subhead>Sign-in</Subhead>
          <Facts>
            <Fact label="Signs in with" value={person.signInPolicy === "passkey_only" ? "A passkey only" : "A link or a passkey"} />
            <Fact label="Last seen" value={formatFreshness(person.lastSeenAt, now)} title={person.lastSeenAt || undefined} />
            <Fact label="Joined" value={formatMoment(person.createdAt)} />
            {person.sharedMailbox ? <Fact label="Mailbox" value={<Chip>shared</Chip>} /> : null}
          </Facts>

          {sessions.unknown ? (
            <p className="os-caption">
              This window could not read this person's sessions, so it is not saying how many are
              open. That read is owner and admin only.
            </p>
          ) : sessions.live.length === 0 ? (
            <p className="os-caption">No sessions open.</p>
          ) : (
            <ul className="os-session-list" aria-label={`Sessions open for ${name}`}>
              {sessions.live.map((session) => (
                <li key={session.id} className="os-session-row">
                  <span className="os-session-where">{session.clientLabel || session.source || "A session"}</span>
                  <span className="os-caption">{formatFreshness(session.lastActivityAt, now)}</span>
                  {governable ? (
                    <Button
                      onClick={() =>
                        void actions.endSession(session.id).then((ok) => {
                          if (ok) sessions.reload();
                        })
                      }
                      busy={actions.busyKey === session.id}
                      busyLabel="Ending..."
                      ariaLabel={`End this session for ${name}`}
                    >
                      End
                    </Button>
                  ) : null}
                </li>
              ))}
            </ul>
          )}
        </Panel>

      </div>
      <ActionBar
        state={person.active && person.suspendedAt === "" ? "Active" : "Deactivated"}
        detail={
          person.suspendedReason === ""
            ? undefined
            : `Deactivated: ${person.suspendedReason}`
        }
        tone={person.active && person.suspendedAt === "" ? "live" : "paused"}
        acts={acts}
      />
    </div>
  );
}

/**
 * Whether the standing-staff rule covers this person.
 *
 * Developer (300) and above are members of every account's group by rule, and
 * developer OUTRANKS admin here -- the ladder's own ordering, which the shell
 * reads from the cluster rather than carrying its own copy of.
 */
function isStaff(person: PersonRow, catalog: RoleCatalog): boolean {
  const rung = roleRungOf(person.role);
  const developer = catalog.roles.find((r) => r.slug === "developer");
  if (rung === null || developer === undefined) return false;
  return rung.rank >= developer.rank;
}

function accountLabel(accounts: readonly AccountRow[], accountId: string): string {
  if (accountId === "") return "";
  const found = accounts.find((a) => a.id === accountId);
  return found ? accountName(found) : "";
}

function MembershipLine({
  membership,
  groups,
  accounts,
  people,
  onRemove,
  busy,
  removable,
}: {
  membership: MembershipRow;
  groups: readonly GroupRow[];
  accounts: readonly AccountRow[];
  people: readonly PersonRow[];
  onRemove: () => void;
  busy: boolean;
  removable: boolean;
}) {
  const group = groups.find((g) => g.id === membership.groupId) ?? null;
  const account = group === null ? null : accounts.find((a) => a.id === group.accountId) ?? null;
  const addedBy = people.find((p) => p.id === membership.addedBy) ?? null;
  // The client's name only when it says something the group's name does not:
  // an account-kind group is NAMED after its client, so a chip beside it reads
  // "Acme Acme".
  const clientName = account === null ? "" : accountName(account);
  return (
    <div className="os-membership-row">
      <span className="os-membership-name">{group?.name ?? membership.groupId}</span>
      {clientName === "" || clientName === group?.name ? null : (
        <Chip tone="accent">{clientName}</Chip>
      )}
      <span className="os-caption">
        {originSentence(membership, {
          addedByName: addedBy ? personName(addedBy) : undefined,
          domain: account?.domain,
        })}
      </span>
      {/* The act sits at the row's trailing edge, where every other row in
          this shell puts one, rather than beside the sentence it is not
          about. */}
      <span className="os-membership-gap" />
      {removable ? (
        <Button onClick={onRemove} busy={busy} busyLabel="Removing..." ariaLabel={`Remove from ${group?.name ?? "this group"}`}>
          Remove
        </Button>
      ) : null}
    </div>
  );
}

/**
 * The one refusal line, in the server's own words, beside the controls that
 * produced it. Never a toast: a `role_above_caller` refusal is the most useful
 * thing this surface can say, and a toast moves it somewhere else on a timer.
 */
export function RefusalLine({ actions }: { actions: UsersActions }) {
  const refusal = actions.refusal;
  if (refusal === null) return null;
  return (
    <Notice
      tone="error"
      sentence={
        refusal.title !== undefined && refusal.title !== ""
          ? refusal.title
          : refusal.denied
            ? "The cluster refused that -- your role does not carry it."
            : "That did not go through."
      }
      next={
        refusal.next !== undefined && refusal.next !== ""
          ? refusal.next
          : refusal.auditEventId === ""
            ? undefined
            : `Audited as ${refusal.auditEventId}.`
      }
      detail={refusal.detail}
    >
      {refusal.auditEventId === "" ? null : <CopyValue value={refusal.auditEventId} label="Audit id" />}
    </Notice>
  );
}
