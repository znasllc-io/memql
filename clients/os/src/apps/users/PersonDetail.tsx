import { useCallback, useEffect, useState } from "react";

import {
  Button,
  Chip,
  Fact,
  Facts,
  FormRow,
  Notice,
  Panel,
  Select,
  Subhead,
  formatFreshness,
  formatMoment,
  roleAdmits,
  roleGrantSlug,
  roleLadder,
} from "../../kit";
import { useOsConnection } from "../../live/connection";
import type { UsersActions } from "./actions";
import { personFromRow, personName, type PersonRow } from "./rows";
import { rereadPerson } from "./usePeople";
import { useSessionsCount } from "./useSessions";

// One person, and the three things an operator does to an account.
//
// ===========================================================================
// IT RE-READS ON OPEN, AND THAT IS NOT A REFRESH BUTTON IN DISGUISE
// ===========================================================================
// There is no `graph.node.updated.v1:identity:user` broadcast rule and this
// epic must not add one (see usePeople.ts for the volume reasoning). So the
// list's copy of a person is only as fresh as the seed plus whatever creates
// have arrived. A panel is opened deliberately, once, about one row -- which
// makes it exactly the right place to pay for one authorized read, and the
// wrong place to poll.
//
// Every write below then hands its own result back into `local`, because the
// same missing broadcast means an accepted write produces no event either.

/**
 * Roles an operator may grant, weakest first -- read from the CLUSTER's
 * ladder rather than a literal (epic memql#4832, D1).
 *
 * The list used to be a five-item array in the shell whose ORDER disagreed
 * with the engine's. Reading it here means a cluster that defines a custom
 * role offers it in this picker with no client release, and that the order
 * the options appear in is the order the engine ranks them.
 */
function grantableRoles(): string[] {
  // roleGrantSlug, not rung.slug: the catalog's vocabulary and a user row's
  // are different sets, and setUserRole validates against the second. Offering
  // `viewer` is a write the engine refuses, and dropping `reader` means the
  // current role of every ordinary principal matches no option.
  return roleLadder().map(roleGrantSlug);
}

export function PersonDetail({
  person,
  actions,
  ownerRole,
  now,
}: {
  person: PersonRow;
  actions: UsersActions;
  ownerRole: string;
  now: Date;
}) {
  const connection = useOsConnection();
  // The panel's OWN copy, seeded from the list and replaced by the re-read.
  // The list's row stays authoritative for identity (`person.id`); this holds
  // the fields the panel edits.
  const [local, setLocal] = useState<PersonRow>(person);
  const [rereadFailed, setRereadFailed] = useState(false);
  // Shown ONCE. It exists nowhere else -- the server persisted only its
  // SHA-256 hash and no later call can retrieve it -- so it is held in
  // component state that dies with the panel, never in storage and never on a
  // row.
  const [enrolmentUrl, setEnrolmentUrl] = useState("");

  useEffect(() => {
    if (connection === null) return;
    const controller = new AbortController();
    let live = true;
    void (async () => {
      try {
        const row = await rereadPerson(connection.query, person.id, controller.signal);
        if (!live || row === null) return;
        setLocal(personFromRow(row));
      } catch {
        // BEST-EFFORT BY CONTRACT. The panel already has a row -- the one the
        // list is rendering -- so a failed re-read degrades to showing that
        // rather than to an empty panel. It says so, because silently
        // showing possibly-stale values is the failure this note exists to
        // prevent.
        if (live) setRereadFailed(true);
      }
    })();
    return () => {
      live = false;
      controller.abort();
    };
    // Once per person. `person` itself is deliberately not a dependency: the
    // list's row changes identity on every snapshot, which would re-read on
    // every event anywhere in the cluster.
  }, [connection, person.id]);

  const sessions = useSessionsCount(person.id);

  const applyRole = useCallback(
    async (role: string) => {
      if (role === local.role) return;
      const ok = await actions.setRole(local.id, role);
      // The reply carries no row, so the new value is the one we sent -- and
      // only after the server accepted it. Setting it optimistically would
      // leave a refused change on screen as though it had happened.
      if (ok) setLocal((held) => ({ ...held, role }));
    },
    [actions, local.id, local.role],
  );

  const applyReset = useCallback(async () => {
    const ok = await actions.resetSignInPolicy(local.id);
    if (ok) setLocal((held) => ({ ...held, signInPolicy: "any" }));
  }, [actions, local.id]);

  const mintLink = useCallback(async () => {
    const url = await actions.issueEnrolmentLink(local.id);
    if (url !== "") setEnrolmentUrl(url);
  }, [actions, local.id]);

  const busy = actions.busyKey === local.id;

  return (
    <Panel label={`Details for ${personName(local)}`}>
      <Subhead>{personName(local)}</Subhead>

      <Facts>
        <Fact label="Email" value={local.primaryEmail} mono />
        <Fact label="Role" value={local.role} mono />
        <Fact label="Sign-in policy" value={local.signInPolicy} mono />
        <Fact
          label="Last activity"
          value={formatFreshness(local.lastSeenAt, now)}
          title={local.lastSeenAt || undefined}
        />
        <Fact label="Recent sessions" value={sessions.label} />
        <Fact label="Joined" value={formatMoment(local.createdAt)} />
        {local.sharedMailbox ? (
          <Fact label="Mailbox" value={<Chip>shared</Chip>} />
        ) : null}
        {local.active ? null : <Fact label="Account" value={<Chip>deactivated</Chip>} />}
      </Facts>

      {rereadFailed ? (
        <Notice
          tone="warn"
          sentence="These are the values this window already had."
          next="Re-reading this person did not succeed, so anything changed since the list loaded is not shown here."
        />
      ) : null}

      {/* ---- role ---- */}
      <FormRow>
        <Select
          id={`role-${local.id}`}
          label={`Cluster role for ${personName(local)}`}
          value={local.role || "reader"}
          onChange={(next) => void applyRole(next)}
        >
          {grantableRoles().map((role) => (
            <option
              key={role}
              value={role}
              // PRESENTATION GATING (spec section E), MIRRORING THE SERVER
              // RULE EXACTLY: an option is disabled when it is STRICTLY ABOVE
              // the viewer's own rank, which is what the server refuses
              // (`role_above_inviter` -- an inviter cannot grant ABOVE their
              // own role). An admin granting admin is permitted and stays
              // enabled.
              //
              // The task text said to owner-gate every grant at or above
              // `admin`. That is a DIFFERENT rule from the server's, and the
              // wrong direction to differ in: hiding a control that would
              // have succeeded teaches an operator a restriction the cluster
              // does not have, and they have no way to discover otherwise.
              // Hiding is only honest for what always fails -- which is the
              // reasoning the SDK records for this whole surface.
              //
              // None of this is the control either way. `adminops.authorize`
              // is, against the role the stream interceptor verified, and its
              // refusal renders below.
              //
              // `disabled` rather than absent, so the CURRENT value of a
              // person ranked above the viewer still renders in the select
              // instead of the box showing somebody else's role.
              disabled={!roleAdmits(ownerRole, { min: role }) && role !== local.role}
            >
              {role}
            </option>
          ))}
        </Select>
      </FormRow>

      {/* ---- sign-in policy ---- */}
      {local.signInPolicy === "passkey_only" ? (
        <FormRow>
          <Button onClick={() => void applyReset()} busy={busy} busyLabel="Resetting...">
            Re-enable sign-in links
          </Button>
          <span className="os-caption">
            Turns sign-in links back on for somebody who chose passkey-only and then lost their
            passkey. There is no control here that turns them off for another person -- that is
            self-service and needs their own active passkey.
          </span>
        </FormRow>
      ) : (
        <p className="os-caption">
          Sign-in links are on for this account. Turning them off is self-service and requires the
          person's own active passkey, so it is not an action available here.
        </p>
      )}

      {/* ---- enrolment link ---- */}
      <FormRow>
        <Button onClick={() => void mintLink()} busy={busy} busyLabel="Minting...">
          Mint enrolment link
        </Button>
        <span className="os-caption">
          Single-use and short-lived. It authorizes exactly one action -- register a passkey as this
          person -- and is spent the moment that succeeds.
        </span>
      </FormRow>

      {enrolmentUrl === "" ? null : (
        <Notice
          tone="info"
          sentence="Here is the enrolment link. This is the only time it is shown."
          next="The cluster kept only its hash, so it cannot be retrieved again. Closing this panel discards it."
        >
          <FormRow>
            <code className="os-mono os-users-secret">{enrolmentUrl}</code>
            <Button
              onClick={() => void navigator.clipboard?.writeText(enrolmentUrl)}
              ariaLabel="Copy the enrolment link"
            >
              Copy
            </Button>
            <Button onClick={() => setEnrolmentUrl("")}>Done</Button>
          </FormRow>
        </Notice>
      )}

      {/* One refusal line for the whole panel, in the server's own words,
          beside the controls that produced it. Never a toast. */}
      {actions.refusal === null ? null : (
        <Notice
          tone="error"
          sentence={
            actions.refusal.denied
              ? "The cluster refused that -- your role does not carry it."
              : "That did not go through."
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
