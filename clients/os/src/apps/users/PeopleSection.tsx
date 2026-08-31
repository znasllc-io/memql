import { useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { UserRound } from "lucide-react";

import { Button, Chip, Head, LiveList, Notice, ProvenanceDot, Row as ListRow } from "../../kit";
import { useLiveView } from "../../live/liveView";
import { useNow } from "../../kit/useNow";
import { formatFreshness } from "../../kit/format";
import { PersonDetail } from "./PersonDetail";
import { personFromRow, personIsDim, personName, type PersonRow } from "./rows";
import { usePeople } from "./usePeople";
import type { UsersActions } from "./actions";

// The people of this cluster, live.
//
// The exemplar the epic exists for is here: somebody accepts an invitation
// while an admin is watching, the `graph.node.created.v1:identity:user`
// broadcast reaches this subscription, and the row rises in with the decaying
// tick. Nothing polls and nothing refetches.

export function PeopleSection({
  showDeactivated,
  actions,
  ownerRole,
}: {
  showDeactivated: boolean;
  actions: UsersActions;
  /** The viewer's own cluster role, for presentation gating only. */
  ownerRole: string;
}) {
  const { source: collection, snapshot, reseed } = usePeople();
  const [openId, setOpenId] = useState("");
  // ONE clock for the section, so two rows cannot disagree about what "now"
  // is. Slow on purpose: nothing here changes on a 15-second cadence the way a
  // machine's heartbeat does, and "last activity" is read in minutes.
  const now = useNow(60_000);

  // PROJECT, then narrow, in one pass -- the collection holds RAW wire rows
  // (the fold upserts an event payload as the row type with no projection
  // hook), so every predicate below has to run on a personFromRow result.
  const source = useLiveView<Row, PersonRow>(
    collection,
    `deactivated:${showDeactivated}`,
    (rows) => {
      const people = rows.map(personFromRow).filter((p) => p.id !== "");
      return showDeactivated ? people : people.filter((p) => !personIsDim(p));
    },
  );

  const open = useMemo(
    () => source?.snapshot.rows.find((p) => p.id === openId) ?? null,
    [source, source?.snapshot, openId],
  );

  return (
    <div className="os-app-stack">
      <Head title="People" />

      {/* A read this surface is not allowed to make comes back as a refusal
          on the feed, not as an empty list. It renders here, in surface --
          somebody who reached this section out-of-band should read WHY rather
          than conclude the cluster has no people in it.

          NO `detail`, deliberately, and this is the one place in the app where
          a Notice omits it: LiveList already prints `snapshot.error` verbatim
          directly beneath the list it belongs to. Passing it here as well puts
          the server's sentence on screen twice, a few lines apart, which reads
          as two different failures. The framing goes here and the words stay
          where the feed put them. */}
      {snapshot.error ? (
        <Notice
          tone="error"
          sentence="This cluster did not return its people."
          next="Reading the directory is owner and admin only; the engine decides that, not this window."
        >
          <Button onClick={reseed}>Try again</Button>
        </Notice>
      ) : null}

      {/* Keyed on the filter so flipping the toggle RE-BASELINES the arrival
          cues. Without it, revealing deactivated rows makes them flash "new"
          on the next event -- claiming the cluster just sent them, when all
          that happened is that this browser started showing rows it already
          had. */}
      <LiveList<PersonRow>
        key={`people:${showDeactivated}`}
        source={source}
        rowId={(p) => p.id}
        // A HEARTBEAT IS NOT NEWS, and this line decides it. `lastSeenAt`
        // moves for every person forever, so naming it would turn the list
        // into a strobe on a timer -- the standing badge this cue exists not
        // to be. What is left is what a person would call a change: a rename,
        // a role change, a sign-in policy flip, an account being deactivated.
        fingerprint={(p) =>
          `${p.displayName}|${p.primaryEmail}|${p.role}|${p.signInPolicy}|${p.active}|${p.suspendedAt}`
        }
        label="People in this cluster"
        emptyText={
          showDeactivated
            ? "Nobody yet. Invite someone from the Invites section."
            : "Nobody active. Invite someone from the Invites section -- or turn on deactivated people in this app's settings if you are looking for an account that was retired."
        }
        renderRow={(person, tick) => (
          <PersonLine
            person={person}
            tick={tick}
            now={now}
            open={openId === person.id}
            onToggle={() => setOpenId((held) => (held === person.id ? "" : person.id))}
          />
        )}
      />

      {open === null ? null : (
        <PersonDetail
          key={open.id}
          person={open}
          actions={actions}
          ownerRole={ownerRole}
          now={now}
        />
      )}
    </div>
  );
}

function PersonLine({
  person,
  tick,
  now,
  open,
  onToggle,
}: {
  person: PersonRow;
  tick: "added" | "updated" | null;
  now: Date;
  open: boolean;
  onToggle: () => void;
}) {
  const dim = personIsDim(person);
  return (
    <ListRow
      icon={<UserRound size={16} aria-hidden />}
      name={personName(person)}
      current={!dim}
      dim={dim}
      open={open}
      onOpen={onToggle}
      state={
        <>
          <span className="os-caption">{formatFreshness(person.lastSeenAt, now)}</span>
          {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
        </>
      }
    >
      {person.primaryEmail === "" ? null : (
        <span className="os-caption os-mono">{person.primaryEmail}</span>
      )}
      <Chip tone={person.role === "owner" ? "accent" : "muted"}>{person.role || "reader"}</Chip>
      <SignInPolicyChip person={person} />
      {dim ? <span className="os-users-inactive-tag">inactive</span> : null}
    </ListRow>
  );
}

/**
 * The sign-in policy, as a column.
 *
 * `passkey_only` is the one worth a chip: it is a choice somebody made about
 * their own account and it is the state an admin may have to rescue them out
 * of. `any` is the default and gets no chip -- a badge on every row is not a
 * column, it is noise.
 *
 * `sharedMailbox` rides here as a quiet hint rather than a column of its own,
 * because what it changes is what a sign-in LINK means for this address, which
 * is the thing the policy beside it is about.
 */
function SignInPolicyChip({ person }: { person: PersonRow }) {
  if (person.signInPolicy !== "passkey_only" && !person.sharedMailbox) return null;
  return (
    <>
      {person.signInPolicy === "passkey_only" ? (
        <Chip title="Sign-in links are disabled on this account; a passkey is the only way in.">
          passkey only
        </Chip>
      ) : null}
      {person.sharedMailbox ? (
        <ProvenanceDot tone="unreachable" label="Shared mailbox -- more than one person reads it" />
      ) : null}
    </>
  );
}
