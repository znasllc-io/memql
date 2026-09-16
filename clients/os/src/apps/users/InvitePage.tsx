import { useEffect, useMemo, useState } from "react";
import type { UserInvitationResult } from "@znasllc-io/memql-sdk-core/identityadmin";

import {
  Chip,
  CopyValue,
  Fact,
  Facts,
  Head,
  Input,
  Notice,
  Panel,
  Rail,
  Subhead,
  formatMoment,
  roleRungOf,
  useNow,
  type Stop,
} from "../../kit";
import { ActionBar, type Act } from "../../kit/ActionBar";
import { accountName, type AccountRow } from "../accounts/rows";
import type { UsersActions } from "./actions";
import { EMPTY_INVITE, accountGroupOf, domainMatch, inviteIsAnswered, stopForRefusal, type InviteDraft } from "./invite";
import { GroupPicker, RoleLadderPicker } from "./pickers";
import { RefusalLine } from "./PersonPage";
import { daysLeft } from "./roster";
import type { GroupRow, InvitationRow } from "./rows";
import type { AssignContext } from "./assign";
import { ladderSentence, rungRefusal } from "./assign";
import { ladderDescending, type RoleCatalog } from "./useRoles";

// INVITING SOMEBODY, as a rail, and the page it becomes.
//
// ===========================================================================
// THREE STOPS, IN THE ORDER THE ANSWERS DEPEND ON EACH OTHER
// ===========================================================================
// Who (the address) decides which client this person is joining, because the
// domain is matched against the clients that have PROVEN theirs and asked for
// arrivals to be placed. That answers Groups. And a role scoped to one client
// is holdable only by a member of it, so the Role stop's ladder depends on
// what Groups holds.
//
// The Head's one action follows `nextOpen`: absent until the stops are
// answered, then "Send invitation". A rail with a permanently-present primary
// action is a form with a decoration down its left edge.

export function InvitePage({
  invitationId,
  invitation,
  groups,
  accounts,
  catalog,
  actions,
  viewerRole,
  onBack,
  onIssued,
}: {
  /** Non-empty when this is the invited person's page rather than the rail. */
  invitationId: string;
  /** The row, when the list opened one. Absent right after issuing. */
  invitation?: InvitationRow | null;
  groups: readonly GroupRow[];
  accounts: readonly AccountRow[];
  catalog: RoleCatalog;
  actions: UsersActions;
  viewerRole: string;
  onBack: () => void;
  onIssued: (invitationId: string) => void;
}) {
  const [draft, setDraft] = useState<InviteDraft>(EMPTY_INVITE);
  const [issued, setIssued] = useState<UserInvitationResult | null>(null);
  const now = useNow(60_000);

  const match = useMemo(() => domainMatch(draft.email, accounts), [draft.email, accounts]);
  const matchedGroup = useMemo(
    () => (match === null ? null : accountGroupOf(match.id, groups)),
    [match, groups],
  );

  // THE MATCH ANSWERS THE GROUPS STOP, ONCE. It prefills rather than pins: a
  // person who takes the group off has said something, and a prefill that
  // reapplied itself would argue with them.
  useEffect(() => {
    if (matchedGroup === null || draft.groupsTouched) return;
    setDraft((held) => ({ ...held, groupIds: [matchedGroup.id] }));
  }, [matchedGroup, draft.groupsTouched]);

  const ladder = useMemo(() => ladderDescending(catalog.roles), [catalog.roles]);
  const viewerRung = roleRungOf(viewerRole);

  // The accounts this invitation would place the person in, which is what
  // decides whether a scoped role is offered (assign.ts, rule 6).
  const draftAccountIds = useMemo(() => {
    const byId = new Map(groups.map((g) => [g.id, g] as const));
    return draft.groupIds.map((id) => byId.get(id)?.accountId ?? "").filter((id) => id !== "");
  }, [draft.groupIds, groups]);

  const assignContext: AssignContext = {
    kind: "invitation",
    callerRole: viewerRole,
    callerRank: viewerRung?.rank ?? 0,
    callerIsOwner: (viewerRung?.slug ?? "") === "owner",
    grants: catalog.grants,
    // EMPTY, and that is the invitation's whole difference: there is no
    // principal yet, so there is no current rung to outrank -- and the
    // capability asked for is create-on-admission rather than
    // update-on-principal, which is what keeps invitations with developers.
    targetRole: "",
    targetRank: 0,
    targetIsOwner: false,
    targetAccountIds: draftAccountIds,
  };

  // ---- the invited person's page -----------------------------------------
  if (issued !== null || invitationId !== "") {
    return (
      <InvitedPersonPage
        result={issued}
        invitation={invitation ?? null}
        groups={groups}
        actions={actions}
        now={now}
        onBack={onBack}
      />
    );
  }

  // ---- the rail ------------------------------------------------------------
  const refusalStop =
    actions.refusal === null
      ? null
      : stopForRefusal(actions.refusal.code ?? "", actions.refusal.detail);

  const stops: Stop[] = [
    {
      id: "who",
      name: "Who",
      state: draft.email.trim() === "" ? "open" : "done",
      sentence: "The address the invitation goes to.",
      answer: draft.email.trim(),
      body: (
        <>
          <Input
            id="invite-email"
            label="Email address"
            value={draft.email}
            onChange={(next) => setDraft((held) => ({ ...held, email: next }))}
            placeholder="name@example.com"
          />
          {match === null ? null : (
            <p className="os-caption">
              That address is on {accountName(match)}'s domain, and they take people on it.
              {matchedGroup === null
                ? " Their group is not in this window yet."
                : ` ${matchedGroup.name} is filled in below.`}
            </p>
          )}
          {refusalStop === "who" ? <RefusalLine actions={actions} /> : null}
        </>
      ),
    },
    {
      id: "role",
      name: "Role",
      // `waiting`, never `pending`: on a COMPOSE rail every stop renders its
      // body, and `pending` means NOT REACHABLE -- it dims the stop, so a form
      // somebody may legitimately fill in reads as one they may not. Nothing
      // here depends on the address having been typed first; the ORDER is
      // advice, not a gate.
      state: draft.role === "" ? "waiting" : "done",
      sentence: "What they can do once they are here.",
      answer: draft.role,
      body: (
        <>
          <RoleLadderPicker
            rungs={ladder}
            value={draft.role}
            onChange={(slug) => setDraft((held) => ({ ...held, role: slug }))}
            context={assignContext}
            actorRole={viewerRole}
            label="The role this invitation grants"
          />
          {ladderSentence(ladder.map((rung) => rungRefusal(rung, assignContext))) === "" ? null : (
            <p className="os-caption">
              {ladderSentence(ladder.map((rung) => rungRefusal(rung, assignContext)))}
            </p>
          )}
          {refusalStop === "role" ? <RefusalLine actions={actions} /> : null}
        </>
      ),
    },
    {
      id: "groups",
      name: "Groups",
      state: draft.groupIds.length > 0 ? "done" : "waiting",
      sentence: "The groups they join the moment they accept. None is the ordinary case.",
      answer:
        draft.groupIds.length === 0
          ? ""
          : groups
              .filter((g) => draft.groupIds.includes(g.id))
              .map((g) => g.name)
              .join(", "),
      body: (
        <>
          <GroupPicker
            groups={groups}
            selected={draft.groupIds}
            onChange={(next) => setDraft((held) => ({ ...held, groupIds: next, groupsTouched: true }))}
            label="Groups to join on acceptance"
            accountNameOf={(accountId) => {
              const found = accounts.find((a) => a.id === accountId);
              return found ? accountName(found) : "";
            }}
          />
          {refusalStop === "groups" ? <RefusalLine actions={actions} /> : null}
        </>
      ),
    },
  ];

  const answered = inviteIsAnswered(draft);
  const acts: Act[] = answered
    ? [
        {
          label: "Send invitation",
          tone: "primary",
          busy: actions.busyKey === `invite:${draft.email.trim()}`,
          onAct: () => {
            void actions
              .issueInvitation(draft.email.trim(), draft.role, draft.groupIds)
              .then((result) => {
                if (result === null) return;
                setIssued(result);
                onIssued(result.auditEventId === "" ? draft.email.trim() : draft.email.trim());
              });
          },
        },
      ]
    : [];

  return (
    <div className="os-action-pane">
      <div className="os-action-body os-app-stack">
        <Head title="Invite somebody" back={{ label: "People", onSelect: onBack }} />

        <Panel label="The invitation">
          {/* NO `openStop`, and that is the COMPOSE reading of this control:
              a rail with none is not a disclosure at all and every stop renders
              its body (kit/Rail.tsx states it). Here the rail IS the form -- the
              stops are what the person fills in, in the order the answers depend
              on each other -- and collapsing the one being typed into would take
              the field away at the first keystroke. The marks still carry which
              stops are settled, which is the whole reason it is a rail rather
              than three panels. */}
          <Rail stops={stops} label="What this invitation says" />
        </Panel>

      </div>
      <ActionBar
        state={answered ? "Ready to send" : "Not answered yet"}
        detail={
          answered
            ? undefined
            : "An address and a role are what an invitation needs; groups are optional."
        }
        tone={answered ? "live" : "none"}
        acts={acts}
      />
    </div>
  );
}

/**
 * The person who has been invited and has not arrived.
 *
 * The rail BECOMES this on success, with the link in a Notice: the link is a
 * CREDENTIAL and this is the only time it exists -- the cluster kept only its
 * hash -- so it is shown once, copyable, and held in state that dies with the
 * view.
 */
function InvitedPersonPage({
  result,
  invitation,
  groups,
  actions,
  now,
  onBack,
}: {
  result: UserInvitationResult | null;
  invitation: InvitationRow | null;
  groups: readonly GroupRow[];
  actions: UsersActions;
  now: Date;
  onBack: () => void;
}) {
  const email = invitation?.inviteeEmail ?? "";
  const role = invitation?.inviteeRole ?? "";
  const joined = groups.filter((g) => invitation?.groupIds.includes(g.id));
  const acts: Act[] = invitation
    ? [
        {
          label: "Revoke",
          tone: "danger",
          onAct: () => void actions.revokeInvitation(invitation.id).then((ok) => ok && onBack()),
        },
        {
          label: "Resend",
          onAct: () =>
            void actions.resendInvitation(
              invitation.id,
              invitation.inviteeEmail,
              invitation.inviteeRole,
              invitation.groupIds,
            ),
        },
      ]
    : [];

  return (
    <div className="os-action-pane">
      <div className="os-action-body os-app-stack">
        <Head title={email || "Invited"} meta="Invited" back={{ label: "People", onSelect: onBack }} />

        {result === null ? null : (
          <Notice
            tone={result.emailSent ? "info" : "warn"}
            sentence={
              result.emailSent
                ? "The invitation is on its way."
                : result.emailError === ""
                  ? "The invitation exists, and no mail is wired on this cluster."
                  : "The invitation exists, and the email did not go out."
            }
            next="This link is shown once. The cluster kept only its hash, so it cannot be retrieved again."
            detail={result.emailError === "" ? undefined : result.emailError}
          >
            <CopyValue value={result.url} label="Invitation link" />
          </Notice>
        )}

        <Panel label="What this invitation says">
          <Subhead>Invitation</Subhead>
          <Facts>
            <Fact label="Address" value={email} mono />
            <Fact label="Role" value={role} mono />
            <Fact label="Expires" value={invitation ? daysLeft(invitation, now) : ""} title={invitation?.expiresAt} />
            <Fact label="Invited by" value={invitation?.inviterName ?? ""} />
            <Fact label="Sent" value={invitation ? formatMoment(invitation.createdAt) : ""} />
          </Facts>
          {joined.length === 0 ? (
            <p className="os-caption">They join no groups on acceptance.</p>
          ) : (
            <p className="os-caption">
              They join{" "}
              {joined.map((group) => (
                <Chip key={group.id}>{group.name}</Chip>
              ))}{" "}
              when they accept.
            </p>
          )}
          <RefusalLine actions={actions} />
        </Panel>

      </div>
      <ActionBar state="Invited" tone="paused" acts={acts} />
    </div>
  );
}
