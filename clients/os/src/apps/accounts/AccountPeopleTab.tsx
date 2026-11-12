import { UserPlus, UserRound } from "lucide-react";
import { rowString } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, ContentSkeleton, RecordList, RecordRow } from "../../kit";
import { flatten } from "../../kit/rows";
import type { AccountRow } from "./rows";
import { useAccountGuests, useAccountPeople, type Rollup } from "./useAccounts";

// THE PEOPLE TAB: who the account's work is for (account row ownership,
// section H; the account page's tabs, design record 2026-11-12-account-export,
// section K). The People band and the Guests band, as they stood on the one
// column page.
//
// BOTH ARE PRESENTATION OVER THE CALLER'S OWN READS, so two people opening the
// same client can see different numbers here. They are on-demand reads, not
// live feeds (useAccounts.ts says why), so the tab says when it read and
// re-reads when asked.

export interface AccountPeopleTabProps {
  account: AccountRow;
  /** Opens the Users app on one of this client's groups. */
  onOpenGroup?: (groupId: string) => void;
}

export function AccountPeopleTab({ account, onOpenGroup }: AccountPeopleTabProps) {
  // THE PEOPLE BAND'S OWN READ: two numbers over two reads -- distinct
  // people, across the groups that grant this client.
  const people = useAccountPeople(account.id);
  const guests = useAccountGuests(account.id);
  const readAt = people.readAt || guests.readAt;

  return (
    <>
      <div className="os-account-bands">
        {/* FIRST, because they are who everything in the inventory is for. */}
        <PeopleBand people={people} onOpenGroup={onOpenGroup} />
        <GuestsBand rollup={guests} />
      </div>
      {/* A READ THAT SAYS WHEN IT HAPPENED, so a static number does not read
          as a live one. */}
      <div className="os-account-ledger-head">
        {readAt === "" ? <span /> : (
          <Caption>
            Read at {new Date(readAt).toLocaleTimeString()}. These are not live -- re-read to see
            changes made since.
          </Caption>
        )}
        <Button
          onClick={() => {
            people.reload();
            guests.reload();
          }}
        >
          Re-read
        </Button>
      </div>
    </>
  );
}

/**
 * How many people reach this client's work, and through which groups.
 *
 * OPENING IT GOES TO USERS, on the group, by intent -- rather than listing
 * members here. A members list on this page would be a second place to
 * manage membership, and the one that manages it is the group's own page.
 */
function PeopleBand({
  people,
  onOpenGroup,
}: {
  people: ReturnType<typeof useAccountPeople>;
  onOpenGroup?: (groupId: string) => void;
}) {
  if (people.state === "error") {
    // A REFUSAL IS NOT A ZERO. The reads carry `@requiresRank("admin")`, so
    // below that the engine refuses -- and rendering that as "0 people" would
    // be this window inventing a fact about a client.
    return (
      <article className="os-account-band" data-state="refused">
        <header className="os-account-band-head">
          <UserRound size={15} aria-hidden />
          <h4 className="os-account-band-title">People</h4>
        </header>
        <p className="os-account-band-refused">Not yours to read</p>
        <p className="os-account-band-detail os-mono">{people.error}</p>
      </article>
    );
  }

  if (people.state !== "ready") {
    return (
      <article className="os-account-band" data-state="loading">
        <header className="os-account-band-head">
          <UserRound size={15} aria-hidden />
          <h4 className="os-account-band-title">People</h4>
        </header>
        <p className="os-account-band-count" aria-hidden>
          --
        </p>
        <ContentSkeleton kind="detail" label="Loading account details" />
      </article>
    );
  }

  return (
    <article className="os-account-band" data-state={people.people === 0 ? "empty" : "ready"}>
      <header className="os-account-band-head">
        <UserRound size={15} aria-hidden />
        <h4 className="os-account-band-title">People</h4>
      </header>
      <p className="os-account-band-count">{people.people}</p>
      <p className="os-account-band-note">
        {people.groups.length === 0
          ? "in no groups yet"
          : people.groups.length === 1
            ? "in 1 group"
            : `in ${people.groups.length} groups`}
      </p>
      {people.groups.length === 0 ? (
        <p className="os-account-band-owner">Users is where these are added.</p>
      ) : (
        <RecordList density="compact"><ul className="os-account-band-rows" aria-label="Groups for this client">
          {people.groups.map((group) => (
            <li key={group.id}><RecordRow name={group.name} onOpen={onOpenGroup ? () => onOpenGroup(group.id) : undefined} /></li>
          ))}
        </ul></RecordList>
      )}
      <p className="os-caption">Open a group to manage membership, organization roles and app access.</p>
    </article>
  );
}

/**
 * Who has been invited to this client and has not joined yet.
 *
 * The inventory counts invitations under People; this band says WHO, which
 * the count cannot, and which the People band (members only) never shows.
 */
function GuestsBand({ rollup }: { rollup: Rollup }) {
  // A REFUSAL IS NOT A ZERO. The guest-invitation read carries
  // `@requiresRank("developer")`, so below that floor the engine refuses the
  // read -- and rendering that as "0 invitations" would be this window
  // inventing a fact about a client. The server's own sentence goes on screen.
  if (rollup.state === "error") {
    return (
      <article className="os-account-band" data-state="refused">
        <header className="os-account-band-head">
          <UserPlus size={15} aria-hidden />
          <h4 className="os-account-band-title">Guests</h4>
        </header>
        <p className="os-account-band-refused">Not yours to read</p>
        <p className="os-account-band-detail os-mono">{rollup.error}</p>
      </article>
    );
  }

  if (rollup.state === "loading" || rollup.state === "idle") {
    return (
      <article className="os-account-band" data-state="loading">
        <header className="os-account-band-head">
          <UserPlus size={15} aria-hidden />
          <h4 className="os-account-band-title">Guests</h4>
        </header>
        <p className="os-account-band-count" aria-hidden>
          --
        </p>
        <ContentSkeleton kind="detail" label="Loading account details" />
      </article>
    );
  }

  const count = rollup.rows.length;
  return (
    <article className="os-account-band" data-state={count === 0 ? "empty" : "ready"}>
      <header className="os-account-band-head">
        <UserPlus size={15} aria-hidden />
        <h4 className="os-account-band-title">Guests</h4>
      </header>
      <p className="os-account-band-count">{count}</p>
      <p className="os-account-band-note">
        {count === 0 ? "No invitations yet" : count === 1 ? "invitation" : "invitations"}
      </p>
      {count === 0 ? (
        <p className="os-account-band-owner">Users is where these are added.</p>
      ) : (
        <RecordList density="compact"><ul className="os-account-band-rows" aria-label="Guests for this client">
          {rollup.rows.slice(0, LEDGER_ROWS).map((row, i) => (
            <li key={rowString(flatten(row), "id") || String(i)}><RecordRow name={rowString(flatten(row), "inviteeEmail") || "--"} /></li>
          ))}
          {count > LEDGER_ROWS ? (
            <li className="os-account-band-more">and {count - LEDGER_ROWS} more, in Users</li>
          ) : null}
        </ul></RecordList>
      )}
    </article>
  );
}

/**
 * How many guests the band names before it stops.
 *
 * A band is a COUNT with evidence, not a table -- Users is where somebody
 * goes to work with invitations. Five is enough to recognize who they are.
 */
const LEDGER_ROWS = 5;
