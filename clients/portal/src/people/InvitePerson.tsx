import { useState, type ReactNode } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { Button, ConfirmDialog, DataText, Field, Select, TextInput } from "../ui";
import { useAdminWrites, useClusterSettings, type WriteState } from "../admin/useAdminConsole";

// Inviting somebody into the cluster (memql#4270, memql#4272).
//
// # What the copy has to get right
//
// "Invite" means four different things depending on how the cluster is
// configured, and an operator who does not know which one they are doing will
// misread the result:
//
//   invite_only        the link IS the way in. Nothing else admits anybody.
//   domain_restricted  the server REFUSES an address outside the allowlist --
//                      a link the recipient cannot redeem is worse than a
//                      refusal, because they find out after clicking.
//   waitlist           people can ask; an invitation is how one is admitted.
//   open               anyone can already register. The link is a convenience,
//                      not a gate, and saying so is the difference between a
//                      link the operator understands and one they think is
//                      protecting something.
//
// The mode is read here to WORD the dialog. It is never the check: the gate in
// component/identity/adminops applies the policy and refuses independently, and
// a client deciding its own authorization is not a check.
//
// # The minted link
//
// A credential, held in component state and nowhere else -- same discipline as
// the enrolment link beside it. Only its SHA-256 hash reached the cluster, so
// it cannot be shown twice.

const INVITE_TTLS: readonly { seconds: number; label: string }[] = [
  { seconds: 0, label: "7 days (default)" },
  { seconds: 24 * 3600, label: "1 day" },
  { seconds: 3 * 24 * 3600, label: "3 days" },
  { seconds: 14 * 24 * 3600, label: "14 days" },
  { seconds: 30 * 24 * 3600, label: "30 days" },
];

// Descending power, matching the server's own ordering. The server refuses a
// role above the caller's own; this list is the courtesy half.
const ROLES: readonly string[] = ["reader", "writer", "developer", "admin", "owner"];

export function modeStatement(mode: string, domains: string): string {
  switch (mode) {
    case "invite_only":
      return "This cluster is invite-only. An invitation is the only way in.";
    case "domain_restricted":
      return domains === ""
        ? "This cluster only admits addresses at an allowed domain."
        : `This cluster only admits addresses at ${domains}. An invitation for any other address is refused.`;
    case "waitlist":
      return "Users can request access to this cluster. An invitation admits somebody directly, without waiting.";
    case "open":
      return "Anyone with an email can register on this cluster, so an invitation here is a convenience rather than a gate.";
    default:
      return "";
  }
}

export function InvitePerson({ onInvited }: { onInvited?: () => void }): ReactNode {
  const writes = useAdminWrites();
  const settings = useClusterSettings(true);
  const [open, setOpen] = useState(false);
  const [email, setEmail] = useState("");
  const [role, setRole] = useState("");
  const [seconds, setSeconds] = useState(0);
  const [minted, setMinted] = useState("");
  const [mintedMode, setMintedMode] = useState("");

  const row = settings.data;
  const mode = text(row, "registrationMode") || "open";
  const domains = text(row, "registrationDomains");

  function close(): void {
    setOpen(false);
    setEmail("");
    setRole("");
    setSeconds(0);
  }

  return (
    <>
      <Button size="xs" onClick={() => setOpen(true)}>
        Invite a user
      </Button>

      <ConfirmDialog
        open={open}
        title="Invite a user"
        confirmLabel="Send the invitation"
        busy={writes.busy}
        confirmDisabled={email.trim() === ""}
        onConfirm={() => {
          const address = email.trim();
          setMinted("");
          writes.run(
            async (client) => {
              const result = await client.issueUserInvitation(address, role, seconds);
              setMinted(result.url);
              setMintedMode(result.registrationMode);
              return result;
            },
            () => {
              close();
              onInvited?.();
            },
          );
        }}
        onCancel={close}
      >
        <p>{modeStatement(mode, domains)}</p>
        <div className="mt-3 flex flex-col gap-3">
          <Field label="Email address">
            <TextInput
              type="email"
              value={email}
              onChange={setEmail}
              placeholder="user@example.com"
            />
          </Field>
          <Field
            label="Role on arrival"
            hint="Empty takes the cluster's default. You cannot invite somebody above your own role."
          >
            <Select ariaLabel="Role on arrival" value={role} onChange={setRole}>
              <option value="">Cluster default</option>
              {ROLES.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="Valid for">
            <Select
              ariaLabel="Invitation lifetime"
              value={String(seconds)}
              onChange={(next) => setSeconds(Number(next))}
            >
              {INVITE_TTLS.map((option) => (
                <option key={option.seconds} value={String(option.seconds)}>
                  {option.label}
                </option>
              ))}
            </Select>
          </Field>
        </div>
      </ConfirmDialog>

      {minted === "" ? null : (
        <MintedInvitation
          url={minted}
          mode={mintedMode}
          onDismiss={() => {
            setMinted("");
            setMintedMode("");
          }}
        />
      )}
    </>
  );
}

// The one-time link. Same treatment as the enrolment link: shown once, in
// component state, never stored.
function MintedInvitation({
  url,
  mode,
  onDismiss,
}: {
  url: string;
  mode: string;
  onDismiss: () => void;
}): ReactNode {
  return (
    <div className="mt-3 flex flex-col gap-2 rounded border border-accent bg-accent-subtle p-3">
      <h4 className="text-sm font-semibold">Copy this link now</h4>
      <p className="text-xs">
        You will not see it again. Only its hash was stored, so it cannot be shown a second time —
        if you lose it, issue another. Anyone holding it can register the address it names.
      </p>
      {mode !== "open" ? null : (
        <p className="text-xs">
          This cluster allows open sign-up, so the link saves the recipient a step rather than
          granting them something they could not have had.
        </p>
      )}
      <code className="block rounded border border-line bg-bg px-2 py-1.5 font-mono text-xs break-all select-all">
        {url}
      </code>
      <div>
        <Button onClick={onDismiss}>I have copied it</Button>
      </div>
    </div>
  );
}

// Pending invitations, and the undo for one sent to the wrong address.
export function PendingInvitations({
  rows,
  writes,
  onChanged,
}: {
  rows: readonly Row[];
  writes: WriteState;
  onChanged: () => void;
}): ReactNode {
  if (rows.length === 0) {
    return (
      <p className="text-sm text-muted">
        Nobody is waiting on an invitation. One issued here appears in this list until it is
        accepted, revoked, or expires.
      </p>
    );
  }
  return (
    <div className="flex flex-col gap-1.5">
      {rows.map((row) => (
        <InvitationRow
          key={rowIdOf(row)}
          row={row}
          writes={writes}
          onChanged={onChanged}
        />
      ))}
    </div>
  );
}

function InvitationRow({
  row,
  writes,
  onChanged,
}: {
  row: Row;
  writes: WriteState;
  onChanged: () => void;
}): ReactNode {
  const [confirming, setConfirming] = useState(false);
  const id = rowIdOf(row);
  return (
    <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1 rounded border border-line bg-surface px-3 py-1.5">
      <span className="text-sm text-fg">{text(row, "inviteeEmail")}</span>
      <span className="text-xs text-muted">{text(row, "inviteeRole") || "cluster default"}</span>
      <span className="min-w-0 flex-1 text-xs text-subtle">
        invited by {text(row, "inviterName") || text(row, "inviterId")}
      </span>
      <DataText kind="time">{text(row, "expiresAt")}</DataText>
      <Button size="xs" tone="danger" onClick={() => setConfirming(true)} disabled={writes.busy}>
        Revoke
      </Button>
      <ConfirmDialog
        open={confirming}
        title="Revoke this invitation?"
        confirmLabel="Revoke"
        tone="danger"
        busy={writes.busy}
        onConfirm={() => {
          writes.run((client) => client.revokeUserInvitation(id), onChanged);
          setConfirming(false);
        }}
        onCancel={() => setConfirming(false)}
      >
        <p>
          The link stops working now. The record stays as history — revoking does not make whoever
          holds the link forget it, so the invitation is kept and marked cancelled rather than
          deleted.
        </p>
      </ConfirmDialog>
    </div>
  );
}

function rowIdOf(row: Row): string {
  return typeof row["id"] === "string" ? row["id"] : "";
}

function text(row: Row | null, key: string): string {
  if (row === null) return "";
  const v = row[key];
  return typeof v === "string" ? v : "";
}
