import { useState } from "react";
import { Boxes } from "lucide-react";

import {
  Button,
  Caption,
  ContentSkeleton,
  DetailsSection,
  EmptyState,
  Fact,
  Facts,
  Notice,
  RecordList,
  RecordListSkeleton,
  RecordRow,
  Subhead,
  formatBytes,
  formatCount,
} from "../../kit";
import type { Reading } from "../../cluster/reading";
import {
  assignmentPhase,
  conceptFinished,
  inventoryAreas,
  inventoryIsEmpty,
  rowsPhrase,
  type AssignmentStatus,
  type Inventory,
} from "./inventory";
import { accountNameFrom, type AccountRow } from "./rows";

// The account's inventory, and the assignment of existing rows (account row
// ownership, section H).
//
// BOTH ARE DRAWN ONLY FOR A HOLDER of `execute app:accounts/inventory`, and the
// caller decides that: these components assume the part is held. For anyone
// else they are absent rather than disabled (DESIGN.md rule 12) -- there is no
// version of either a non-holder could act on.
//
// Built from the Fleet machine page's parts (`DetailsSection`, `Facts`,
// `RecordList`), with the state of the read said in the kit's words: a
// skeleton while reading, the server's sentence and a retry on a refusal, and
// an empty state only after a read that came back empty.

/**
 * The inventory, one line per area.
 *
 * AN AREA OPENS IN PLACE to list its concepts. Opening goes no further than
 * that: the area is a count, and the app that owns its rows is where somebody
 * works with them.
 */
export function InventorySection({ reading }: { reading: Reading<Inventory> }) {
  const [open, setOpen] = useState<ReadonlySet<string>>(() => new Set());
  const toggle = (key: string) =>
    setOpen((held) => {
      const next = new Set(held);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });

  let body;
  if (reading.state === "failed") {
    body = (
      <Notice tone="error" sentence="The inventory was not read." next="Nothing below is current." detail={reading.error}>
        <Button onClick={reading.reread}>Try again</Button>
      </Notice>
    );
  } else if (reading.value === null) {
    body =
      reading.state === "unread" ? (
        <Caption>Not connected to the cluster.</Caption>
      ) : (
        <RecordListSkeleton label="Loading inventory" rows={3} />
      );
  } else if (inventoryIsEmpty(reading.value)) {
    body = (
      <EmptyState icon={Boxes} title="Nothing belongs to this account yet">
        Rows recorded under this account are counted here.
      </EmptyState>
    );
  } else {
    const areas = inventoryAreas(reading.value);
    body = (
      <RecordList as="ul" label="Inventory by area" density="compact">
        {areas.map((area) => {
          const isOpen = open.has(area.key);
          const summary = area.bytes === null ? rowsPhrase(area.rows) : `${rowsPhrase(area.rows)}, ${formatBytes(area.bytes)}`;
          return (
            <li key={area.key}>
              <RecordRow name={area.label} open={isOpen} onOpen={() => toggle(area.key)} label={`${area.label}, ${summary}`}>
                <span className="os-caption">{summary}</span>
              </RecordRow>
              {isOpen ? (
                <Facts>
                  {area.concepts.map((concept) => (
                    <Fact
                      key={concept.concept}
                      label={concept.name}
                      value={rowsPhrase(concept.rows)}
                      title={concept.concept}
                    />
                  ))}
                </Facts>
              ) : null}
            </li>
          );
        })}
      </RecordList>
    );
  }

  return <DetailsSection title="Inventory">{body}</DetailsSection>;
}

/**
 * How far this cluster has got assigning its existing rows to accounts.
 *
 * ONE STATE AT A TIME, in the order `assignmentPhase` decides, and then the
 * rows that need a person, whatever the state. Drawn on the primary account's
 * page only: the assignment is the cluster's, not any one client's.
 */
export function ExistingDataSection({
  reading,
  accounts,
}: {
  reading: Reading<AssignmentStatus>;
  /** The accounts the page already holds, to name an account by. */
  accounts: readonly AccountRow[];
}) {
  let body;
  if (reading.state === "failed") {
    body = (
      <Notice tone="error" sentence="The assignment was not read." next="Nothing below is current." detail={reading.error}>
        <Button onClick={reading.reread}>Try again</Button>
      </Notice>
    );
  } else if (reading.value === null) {
    body =
      reading.state === "unread" ? (
        <Caption>Not connected to the cluster.</Caption>
      ) : (
        <ContentSkeleton kind="detail" label="Loading existing data" />
      );
  } else {
    body = (
      <>
        <AssignmentState status={reading.value} accounts={accounts} />
        <Conflicts status={reading.value} />
        <div className="os-account-ledger-head">
          {reading.at === null ? <span /> : <Caption>Read at {reading.at.toLocaleTimeString()}.</Caption>}
          <Button onClick={reading.reread}>Re-read</Button>
        </div>
      </>
    );
  }
  return <DetailsSection title="Existing data">{body}</DetailsSection>;
}

function AssignmentState({ status, accounts }: { status: AssignmentStatus; accounts: readonly AccountRow[] }) {
  switch (assignmentPhase(status)) {
    case "held":
      return (
        <Notice
          tone="warn"
          sentence="Recording accounts on rows is held on this cluster."
          next="New rows are written without an account and existing rows are not assigned until an operator lifts the hold."
        />
      );
    case "waiting":
      return (
        <Caption>
          Waiting for every node to run this release. Accounts are recorded on rows once they all do.
        </Caption>
      );
    case "running": {
      // The per-concept counts are those of each concept's CURRENT pass and
      // start again with every pass, so they do not add up to a running
      // total. The totals below, per account, are the figure to read.
      const finished = status.progress.filter(conceptFinished).length;
      return (
        <>
          <Caption>Assigning existing rows to accounts.</Caption>
          <Facts>
            <Fact label="Concepts finished" value={`${formatCount(finished)} of ${formatCount(status.progress.length)}`} />
          </Facts>
          <AccountTotals status={status} accounts={accounts} />
        </>
      );
    }
    case "finished":
      return (
        <>
          <Caption>Existing rows have been assigned.</Caption>
          <AccountTotals status={status} accounts={accounts} />
        </>
      );
  }
}

/**
 * Rows per account, while the switch is on.
 *
 * ABSENT ACCOUNT ROWS ARE NOT "NOTHING ASSIGNED". The server sends them only
 * while the switch is enabled, and skips them when counting takes too long,
 * so their absence says only that the totals were not counted this time.
 */
function AccountTotals({ status, accounts }: { status: AssignmentStatus; accounts: readonly AccountRow[] }) {
  if (status.accounts.length === 0) return <Caption>Totals are not available right now.</Caption>;
  return (
    <Facts>
      {status.accounts.map((a) => (
        <Fact key={a.accountId} label={accountNameFrom(accounts, a.accountId) || a.accountId} value={rowsPhrase(a.rows)} />
      ))}
    </Facts>
  );
}

/** The rows no rule could decide. Absent when there are none. */
function Conflicts({ status }: { status: AssignmentStatus }) {
  if (status.conflicts.length === 0) return null;
  return (
    <>
      <Subhead meta={status.conflicts.length}>Needs a decision</Subhead>
      <RecordList as="ul" label="Rows that need a decision" density="compact">
        {status.conflicts.map((c) => (
          <li key={`${c.concept} ${c.rowId} ${c.conflict}`}>
            <RecordRow name={c.rowId} secondary={c.concept}>
              <span className="os-caption">{c.detail || c.conflict}</span>
            </RecordRow>
          </li>
        ))}
      </RecordList>
    </>
  );
}
