import type { ReactNode } from "react";
import { Building2 } from "lucide-react";

import { Button, Caption, ChoiceRow, EmptyState, Notice, RecordListSkeleton, Refine, useNow, type RefineChip } from "../../../kit";
import { ActionBar } from "../../../kit/ActionBar";
import { LoadMore } from "../../../kit/LoadMore";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { ReviewsPane } from "../reviews/ReviewsPane";
import { countFor, dayWords, emptyWords, isNotSetUp, NOT_SET_UP, refineStages, stageWords, waitedWords, type WholesaleFilter, type WholesaleRow } from "./model";
import { useWholesaleList, type WholesaleScope, type WholesaleWrites } from "./useWholesale";

// THE STORE'S APPLICATIONS, one stage at a time. Ready to review is the
// default and is oldest first, because how long an application has waited is
// what the store's people answer to; every other stage is newest first. Both
// orders are the engine's (wholesaleList).
//
// THE STAGE IS A QUESTION, asked behind Refine beside the tabs (DESIGN.md rule
// 2), never a tab: every stage is the same kind of thing. A row says its stage
// only under All, the one view that mixes them.

export function ApplicationsTab({ scope, heading, writes, operator, termsRequired, stage, onStage, search, onSearch, onOpen }: {
  scope: WholesaleScope;
  /** What the list is asked, kept by the section so a visit to one application keeps it. */
  stage: WholesaleFilter;
  onStage: (next: WholesaleFilter) => void;
  search: string;
  onSearch: (next: string) => void;
  /** The Wholesale heading and its tabs, with Refine beside them. */
  heading: (actions: ReactNode) => ReactNode;
  writes: WholesaleWrites;
  /** One of the cluster's operators, who read the applications and decide none. */
  operator: boolean;
  /** Whether the store requires its terms, as its settings answer: only then does an application wait for them. */
  termsRequired: boolean;
  onOpen: (applicationId: string, name: string) => void;
}) {
  const list = useWholesaleList(scope, stage, search, writes.revision);
  const now = useNow(60_000);
  const page = list.value;
  const rows = page?.rows ?? [];
  // THE ENGINE COUNTS A STAGE, never a search: while one is asked, the bar
  // claims no count over rows that are not the stage's.
  const searching = search.trim() !== "";
  const count = page === null || searching ? undefined : countFor(page.counts, stage);
  const queue = stage === "readyToReview";

  const chips: RefineChip[] = stage === "readyToReview" ? [] : [{ id: "stage", label: stageWords(stage), onRemove: () => onStage("readyToReview") }];
  const refine = <Refine label="Refine applications" placeholder="Search applications" search={search} onSearch={onSearch} chips={chips}>
    <ChoiceRow label="Stage" value={stage} onChange={(next) => onStage(next as WholesaleFilter)}
      options={[...refineStages(termsRequired), ...(refineStages(termsRequired).includes(stage) ? [] : [stage])].map((s) => ({ value: s, label: stageWords(s) }))} />
  </Refine>;

  // A STORE THAT IS NOT SET UP IS A STATE, not a failed read: nothing here can
  // be tried again until the cluster's operators set it up.
  if (list.state === "failed" && isNotSetUp(list.error)) {
    return (
      <ReviewsPane heading={heading(null)} bar={<ActionBar state="Not set up" tone="none" />} context={{ page: "Wholesale", view: "Applications", siteId: scope.siteId, storeId: scope.storeId }}>
        <Notice sentence={NOT_SET_UP} />
      </ReviewsPane>
    );
  }

  const bar = <ActionBar
    state={count === undefined ? "" : count === 0 ? emptyWords(stage) : `${count} ${stage === "all" ? (count === 1 ? "application" : "applications") : stageWords(stage).toLowerCase()}`}
    detail={count ? (queue ? "oldest first" : "newest first") : undefined}
    tone={list.state === "reading" ? "busy" : !count ? "none" : queue ? "paused" : "live"} />;

  return (
    <ReviewsPane heading={heading(refine)} bar={bar} context={{ page: "Wholesale", view: "Applications", stage, siteId: scope.siteId, storeId: scope.storeId }}>
      {operator ? <Caption>You can read this store's applications. Its own people decide them.</Caption> : null}
      {/* A FAILED READ IS SAID, whether or not an earlier one left rows: those
          rows stand under it, as what could not be refreshed. */}
      {list.state === "failed" ? <Notice tone="error" sentence={list.error}><Button onClick={list.reread}>Try again</Button></Notice> : null}
      {page === null ? (
        list.state === "failed" ? null :
        <RecordListSkeleton label="Reading applications" rows={3} />
      ) : rows.length === 0 ? (
        <EmptyState title={search ? "No matching applications" : emptyWords(stage)} icon={Building2}>
          {/* While the store requires its terms, an application waits for them
              before it is ready: the empty queue says where they are. */}
          {queue && termsRequired && !search ? "Applications appear here once the applicant has accepted the terms." : null}
        </EmptyState>
      ) : (
        <>
          <RecordList as="ul" label="Applications">
            {rows.map((row) => <ApplicationRow key={row.id} row={row} now={now} mixed={stage === "all"} onOpen={() => onOpen(row.id, row.companyName)} />)}
          </RecordList>
          {/* THE ENGINE READ ONLY THE NEWEST THOUSAND, and says so: the ones
              left out are the oldest, which in the queue have waited longest. */}
          {page.truncated ? <Caption>This store has more applications than one read covers, so the oldest are not listed.</Caption> : null}
        </>
      )}
      <LoadMore feed={list} />
    </ReviewsPane>
  );
}

/** An application on the list: the company, who applied, the client's own two
 * facts as plain text, and how long it has waited -- or when it came in. */
function ApplicationRow({ row, now, mixed, onOpen }: { row: WholesaleRow; now: Date; mixed: boolean; onOpen: () => void }) {
  const submitted = row.stage === "readyToReview" || row.stage === "waitingForTerms";
  const when = submitted ? waitedWords(row.createdAt, now) : dayWords(row.createdAt);
  const who = [row.applicantName, row.applicantEmail].filter((s) => s !== "").join(" · ");
  return (
    <RecordRow
      icon={<Building2 size={18} aria-hidden />}
      name={row.companyName}
      // ONE LINE, NEVER BROKEN MID-WORD: the kit's excerpt ellipsizes a long
      // email and keeps the whole of it on `title`. It needs a grid of no
      // minimum width around it to shrink in, which is the row identity's own.
      secondary={who ? <span className="os-record-identity"><span className="os-record-excerpt" title={who}>{who}</span></span> : undefined}
      state={mixed ? stageWords(row.stage) : undefined}
      trailing={when ? <span className="os-caption">{when}</span> : undefined}
      onOpen={onOpen}
      label={`Open ${row.companyName}`}>
      {row.primary ? <span>{row.primary}</span> : null}
      {row.secondary ? <span>{row.secondary}</span> : null}
    </RecordRow>
  );
}
