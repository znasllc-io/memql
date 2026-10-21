import { UserPlus } from "lucide-react";
import { Wizard } from "../../kit/Wizard";
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
// Review offers "Send invitation" once the steps are answered. A wizard
// with a permanently-present primary
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
  const [open, setOpen] = useState(0);
  const [furthest, setFurthest] = useState(0);
  function go(index: number) { setOpen(index); setFurthest(held => Math.max(held, index)); }
  const [draft, setDraft] = useState<InviteDraft>(EMPTY_INVITE);
  const [issued, setIssued] = useState<UserInvitationResult | null>(null);
  const now = useNow(60_000);
  useEffect(() => {
    if (!actions.refusal) return;
    const step = stopForRefusal(actions.refusal.code ?? "", actions.refusal.detail);
    const index = step === "who" ? 0 : step === "groups" ? 1 : step === "role" ? 2 : -1;
    if (index >= 0) go(index);
  }, [actions.refusal]);

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
      sentence: "Where the invitation goes",
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
                : ` ${matchedGroup.name} is selected in Groups.`}
            </p>
          )}
          {refusalStop === "who" ? <RefusalLine actions={actions} /> : null}
        </>
      ),
    },
    {
      id: "groups",
      name: "Groups",
      state: draft.groupIds.length > 0 ? "done" : "waiting",
      sentence: "Groups they join on accepting",
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
    {
      id: "role",
      name: "Role",
      state: draft.role === "" ? "waiting" : "done",
      sentence: "What they can do here",
      answer: draft.role,
      body: (
        <>
          <RoleLadderPicker
            rungs={ladder}
            value={draft.role}
            onChange={(slug) => { setDraft((held) => ({ ...held, role: slug })); go(3); }}
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

  stops.push({ id: "review", name: "Review", sentence: "Check, then send", state: "ahead", body: <Facts><Fact label="Invite" value={draft.email.trim()} /><Fact label="Role" value={ladder.find(role => role.slug === draft.role)?.name || draft.role} /><Fact label="Groups" value={groups.filter(group => draft.groupIds.includes(group.id)).map(group => group.name).join(", ") || "None"} /></Facts> });
  const busy = actions.busyKey === `invite:${draft.email.trim()}`;
  const ready = open < 2 ? inviteIsAnswered({ ...draft, role: "chosen" }) : answered;
  const footer: Act[] = busy ? [] : [{ label: "Cancel", text: true, onAct: onBack }];
  if (open === 3) footer.push(...acts);
  else if (!busy && ready) footer.push({ label: "Next", tone: "primary", onAct: () => go(open + 1) });
  return <Wizard icon={<UserPlus size={20} aria-hidden />} title="Invite somebody" label="What this invitation says" back={{ label: "People", onSelect: onBack }} steps={stops.map((stop, index) => ({ ...stop, state: index === open ? "open" : index < open ? "done" : index <= furthest ? "waiting" : "ahead" }))} open={stops[open]!.id} onOpen={id => { const index = stops.findIndex(stop => stop.id === id); if (!busy && index >= 0 && index <= furthest) setOpen(index); }} status={{ word: busy ? "Sending invitation" : open === 3 ? "Ready to send" : stops[open]!.name, tone: busy ? "busy" : "none" }} acts={footer} notices={actions.refusal && refusalStop !== stops[open]?.id ? <RefusalLine actions={actions} /> : null} />;

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
        <Head navigationKey={"invitation-sent"} title={email || "Invited"} meta="Invited" back={{ label: "People", onSelect: onBack }} />

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
