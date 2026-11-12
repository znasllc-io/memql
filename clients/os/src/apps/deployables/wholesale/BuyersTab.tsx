import type { ReactNode } from "react";
import { Store } from "lucide-react";

import { Button, Caption, EmptyState, Notice, RecordListSkeleton } from "../../../kit";
import { ActionBar } from "../../../kit/ActionBar";
import { AddButton } from "../../../kit/AddButton";
import { LoadMore } from "../../../kit/LoadMore";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { ReviewsPane } from "../reviews/ReviewsPane";
import { buyerAccessWords, isNotSetUp, NOT_SET_UP, type WholesaleBuyer, type WholesaleSettings } from "./model";
import { useWholesaleBuyers, type WholesaleScope, type WholesaleWrites } from "./useWholesale";

// THE STORE'S BUYERS: every application somebody approved, newest first
// (wholesaleBuyers). A row says, in words on its own line, whether the
// buyer's trade prices are on -- and since when -- or why they are not yet. A
// row opens the buyer's application, where everything else about them is.
//
// A BUYER ADDED BY HAND is the Head's one creation control, drawn for the
// store's own people on a store that is set up: the engine adds nobody for
// the cluster's operators, and nobody on a store that is not set up. Who is an
// operator is the buyers' own reply (`operator`), so the control waits for it.

export function BuyersTab({ scope, heading, writes, settings, onOpen, onAdd }: {
  scope: WholesaleScope;
  /** The Wholesale heading and its tabs; the second slot is the Head's creation control. */
  heading: (actions?: ReactNode, add?: ReactNode) => ReactNode;
  writes: WholesaleWrites;
  /** The store's settings as last read: whether it is set up. */
  settings: WholesaleSettings | null;
  onOpen: (applicationId: string, name: string) => void;
  onAdd: () => void;
}) {
  const list = useWholesaleBuyers(scope, "", writes.revision);
  const context = { page: "Wholesale", view: "Buyers", siteId: scope.siteId, storeId: scope.storeId };

  if (list.state === "failed" && isNotSetUp(list.error)) {
    return (
      <ReviewsPane heading={heading()} bar={<ActionBar state="Not set up" tone="none" />} context={context}>
        <Notice sentence={NOT_SET_UP} />
      </ReviewsPane>
    );
  }

  const page = list.value;
  const rows = page?.rows ?? [];
  const add = settings?.exists === true && page !== null && !page.operator ? <AddButton label="Add buyer" onClick={onAdd} /> : undefined;
  // THE ENGINE COUNTS NO BUYERS: a count is said only once every one is on
  // the page, never of a page that has more after it.
  const bar = <ActionBar state={page === null || list.hasMore ? "" : rows.length === 0 ? "No buyers" : `${rows.length} ${rows.length === 1 ? "buyer" : "buyers"}`}
    detail={rows.length > 0 ? "newest first" : undefined}
    tone={list.state === "reading" ? "busy" : rows.length === 0 ? "none" : "live"} />;

  return (
    <ReviewsPane heading={heading(undefined, add)} bar={bar} context={context}>
      {list.state === "failed" ? <Notice tone="error" sentence={list.error}><Button onClick={list.reread}>Try again</Button></Notice> : null}
      {page === null ? (
        list.state === "failed" ? null : <RecordListSkeleton label="Reading buyers" rows={3} />
      ) : rows.length === 0 ? (
        <EmptyState title="No buyers yet." icon={Store}>{null}</EmptyState>
      ) : (
        <>
          <RecordList as="ul" label="Buyers">
            {rows.map((row) => <BuyerRow key={row.id} buyer={row} onOpen={() => onOpen(row.id, row.companyName)} />)}
          </RecordList>
          {page.truncated ? <Caption>This store has more applications than one read covers, so the buyers among the oldest are not listed.</Caption> : null}
        </>
      )}
      <LoadMore feed={list} />
    </ReviewsPane>
  );
}

/** A buyer on the list: the company, the buyer's address, and their access in words. */
function BuyerRow({ buyer, onOpen }: { buyer: WholesaleBuyer; onOpen: () => void }) {
  return (
    <RecordRow
      icon={<Store size={18} aria-hidden />}
      name={buyer.companyName}
      secondary={buyer.applicantEmail ? <span className="os-record-identity"><span className="os-record-excerpt" title={buyer.applicantEmail}>{buyer.applicantEmail}</span></span> : undefined}
      onOpen={onOpen}
      label={`Open ${buyer.companyName}`}>
      <span>{buyerAccessWords(buyer)}</span>
    </RecordRow>
  );
}
