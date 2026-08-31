import { useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { MailPlus } from "lucide-react";

import {
  Button,
  Chip,
  Head,
  Input,
  LiveList,
  Notice,
  Panel,
  Row as ListRow,
  Select,
  formatMoment,
  roleAdmits,
  ROLE_LADDER,
  useLiveView,
  useNow,
} from "../../kit";
import type { UsersActions } from "./actions";
import {
  invitationFromRow,
  invitationHasExpired,
  type InvitationRow,
} from "./rows";
import { useInvites } from "./useInvites";

// Who has been asked to join, and has not yet.
//
// The exemplar closes here: an acceptance takes a row OFF this list and puts
// one ON the People list, on the same connection, with no refetch on either
// side. This half works because v1:identity:invitation carries BOTH a
// `created` and an `updated` broadcast rule; the other half works because
// v1:identity:user carries a `created` one.

export function InvitesSection({
  actions,
  ownerRole,
}: {
  actions: UsersActions;
  /** The viewer's own cluster role, for presentation gating only. */
  ownerRole: string;
}) {
  const { source: collection, snapshot, reseed } = useInvites();
  const now = useNow(60_000);

  const source = useLiveView<Row, InvitationRow>(collection, "invites", (rows) =>
    rows.map(invitationFromRow).filter((i) => i.id !== ""),
  );

  return (
    <div className="os-app-stack">
      <Head title="Invites" />

      <IssueInvitation actions={actions} ownerRole={ownerRole} />

      {snapshot.error ? (
        <Notice
          tone="error"
          sentence="This cluster did not return its outstanding invitations."
          next="Reading them is owner and admin only; the engine decides that, not this window."
        >
          <Button onClick={reseed}>Try again</Button>
        </Notice>
      ) : null}

      <LiveList<InvitationRow>
        source={source}
        rowId={(i) => i.id}
        // What a person would call a change to an invitation: it was accepted
        // or revoked, or the mail finally went out (or failed). `expiresAt`
        // is set once at issue and never moves, so it is not news; nothing
        // here churns on a timer, which is why this list has no heartbeat
        // problem to avoid.
        fingerprint={(i) => `${i.status}|${i.active}|${i.deliveryState}|${i.deliveryError}`}
        label="Outstanding invitations"
        emptyText="Nobody is waiting on an invitation."
        renderRow={(invite, tick) => (
          <InviteLine invite={invite} tick={tick} now={now} actions={actions} />
        )}
      />
    </div>
  );
}

function IssueInvitation({
  actions,
  ownerRole,
}: {
  actions: UsersActions;
  ownerRole: string;
}) {
  const [email, setEmail] = useState("");
  const [role, setRole] = useState("");
  // The link is shown ONCE and only when it has to be. See `submit`.
  const [issued, setIssued] = useState<{ url: string; note: string; tone: "info" | "warn" } | null>(
    null,
  );

  const trimmed = email.trim();

  async function submit() {
    // PRESENCE ONLY, CLIENT-SIDE. There is deliberately no address-shape check
    // here: the cluster's registration mode is what decides whether an address
    // may be invited (`domain_restricted` refuses one outside the allowlist),
    // that decision is server-side, and a second opinion in the browser would
    // refuse addresses the cluster would have accepted while teaching the
    // operator a rule that is not the real one.
    if (trimmed === "") return;
    const result = await actions.issueInvitation(trimmed, role);
    if (result === null) return;
    setEmail("");
    // THE THREE DELIVERY STATES (memql#4584), and why the link is not always
    // rendered. `emailSent` true means the invitation is on its way and the
    // link is a credential with no reason to be on screen. `false` with no
    // error means no mail is wired on this cluster, so the link is the ONLY
    // delivery mechanism and withholding it would leave an invitation nobody
    // can act on. `false` WITH an error is an incident, and the link is the
    // way to rescue it by hand.
    setIssued(
      result.emailSent
        ? { url: "", note: `Invitation emailed to ${trimmed}.`, tone: "info" }
        : result.emailError === ""
          ? {
              url: result.url,
              note: "No mail is configured on this cluster, so nothing was sent. This link is the only way to deliver the invitation.",
              tone: "info",
            }
          : {
              url: result.url,
              note: `Sending the invitation email failed: ${result.emailError}. The invitation exists; this link delivers it by hand.`,
              tone: "warn",
            },
    );
  }

  const busy = actions.busyKey === `invite:${trimmed}`;

  return (
    <Panel label="Invite somebody">
      <form
        className="os-form-row"
        onSubmit={(e) => {
          e.preventDefault();
          void submit();
        }}
      >
        <Input
          id="invite-email"
          label="Email address to invite"
          placeholder="colleague@example.com"
          value={email}
          onChange={setEmail}
        />
        <Select
          id="invite-role"
          label="Role to grant"
          value={role}
          onChange={setRole}
        >
          <option value="">cluster default</option>
          {ROLE_LADDER.map((r) => (
            <option
              key={r}
              value={r}
              // Presentation gating only. The server rank-caps this itself --
              // an inviter cannot grant above their own role -- and its
              // refusal renders below rather than being pre-empted here.
              disabled={!roleAdmits(ownerRole, { min: r })}
            >
              {r}
            </option>
          ))}
        </Select>
        <Button type="submit" tone="primary" disabled={trimmed === ""} busy={busy} busyLabel="Inviting...">
          Invite
        </Button>
      </form>

      {issued === null ? null : (
        <Notice
          tone={issued.tone}
          sentence={issued.note}
          next={
            issued.url === ""
              ? undefined
              : "This is the only time the link is shown -- the cluster kept only its hash."
          }
        >
          {issued.url === "" ? null : (
            <div className="os-form-row">
              <code className="os-mono os-users-secret">{issued.url}</code>
              <Button
                onClick={() => void navigator.clipboard?.writeText(issued.url)}
                ariaLabel="Copy the invitation link"
              >
                Copy
              </Button>
            </div>
          )}
          <Button onClick={() => setIssued(null)}>Done</Button>
        </Notice>
      )}

      {actions.refusal === null ? null : (
        <Notice
          tone="error"
          sentence={
            actions.refusal.denied
              ? "The cluster refused that -- your role does not carry it."
              : "That invitation was not issued."
          }
          next={
            actions.refusal.auditEventId === ""
              ? undefined
              : `Audited as ${actions.refusal.auditEventId}.`
          }
          detail={actions.refusal.detail}
        />
      )}
    </Panel>
  );
}

function InviteLine({
  invite,
  tick,
  now,
  actions,
}: {
  invite: InvitationRow;
  tick: "added" | "updated" | null;
  now: Date;
  actions: UsersActions;
}) {
  const expired = useMemo(() => invitationHasExpired(invite, now), [invite, now]);
  const busy = actions.busyKey === invite.id;

  return (
    <ListRow
      icon={<MailPlus size={16} aria-hidden />}
      name={invite.inviteeEmail || invite.inviteeName || invite.id}
      current={!expired}
      dim={expired}
      state={
        <>
          <Button
            onClick={() =>
              void actions.resendInvitation(invite.id, invite.inviteeEmail, invite.inviteeRole)
            }
            busy={busy}
            busyLabel="Re-sending..."
            ariaLabel={`Re-send the invitation to ${invite.inviteeEmail}`}
          >
            Re-send
          </Button>
          <Button
            tone="danger"
            onClick={() => void actions.revokeInvitation(invite.id)}
            busy={busy}
            busyLabel="Cancelling..."
            ariaLabel={`Cancel the invitation to ${invite.inviteeEmail}`}
          >
            Cancel
          </Button>
          {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
        </>
      }
    >
      {invite.inviteeRole === "" ? null : <Chip tone="muted">{invite.inviteeRole}</Chip>}
      <DeliveryChip invite={invite} />
      <span className="os-caption" title={invite.expiresAt || undefined}>
        {invite.expiresAt === ""
          ? "no expiry"
          : expired
            ? `expired ${formatMoment(invite.expiresAt)}`
            : `expires ${formatMoment(invite.expiresAt)}`}
      </span>
    </ListRow>
  );
}

/**
 * Whether the mail went out.
 *
 * THREE STATES, NOT TWO (memql#4587). `not_attempted` is a configuration
 * statement -- no mail is wired on this cluster -- and `failed` is an
 * incident. Rendering both as "not sent" is what let an invitation look
 * delivered when nothing had been sent at all, so they get different words and
 * different tones, and `failed` carries the reason.
 */
function DeliveryChip({ invite }: { invite: InvitationRow }) {
  if (invite.deliveryState === "sent") {
    return <Chip tone="muted">emailed</Chip>;
  }
  if (invite.deliveryState === "failed") {
    return (
      <Chip tone="accent" title={invite.deliveryError || "The send failed; no reason was recorded."}>
        email failed
      </Chip>
    );
  }
  return (
    <Chip tone="muted" title="No mail is configured on this cluster, so the link is the only way to deliver this.">
      not emailed
    </Chip>
  );
}
