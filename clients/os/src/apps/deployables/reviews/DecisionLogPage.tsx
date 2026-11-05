import { useState, type ReactNode } from "react";
import { ScrollText } from "lucide-react";

import { Button, EmptyState, Notice, RecordListSkeleton } from "../../../kit";
import { ActionBar } from "../../../kit/ActionBar";
import { formatMoment } from "../../../kit/format";
import { PageHelp } from "../../../kit/pageHelp";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { deployedByLabel, type NameOf } from "../people";
import { actionDone, reasonLabel, type LogEntry } from "./model";
import { ReviewsPane } from "./ReviewsPane";
import { useDecisionLog, type ReviewsScope } from "./useReviews";

// THE STORE'S DECISION LOG, a tab of its Reviews: every decision on every one
// of its reviews, newest first -- what was decided and why, on which review,
// by whom and when. A row opens its review, which is where its state is said.
//
// It names the product and the headline, never the writer: the log is the
// record of decisions, and a writer's name belongs to their review.

export function DecisionLog({ scope, heading, revision, nameOf, viewerUserId, onOpenReview }: {
  scope: ReviewsScope;
  /** The Reviews heading and its tabs. */
  heading: ReactNode;
  revision: number;
  nameOf: NameOf;
  viewerUserId: string;
  onOpenReview: (reviewId: string) => void;
}) {
  const [pages, setPages] = useState(1);
  const log = useDecisionLog(scope, pages, revision);
  const page = log.value;
  const who = (userId: string) => deployedByLabel(userId, viewerUserId, nameOf) || "someone";

  const bar = <ActionBar
    state={page === null ? "" : page.total === 0 ? "No decisions yet" : page.total === 1 ? "1 decision" : `${page.total} decisions`}
    detail={page?.total ? "newest first" : undefined}
    tone={log.state === "reading" ? "busy" : "none"} />;

  return (
    <ReviewsPane heading={heading} bar={bar} context={{ page: "Decision log", siteId: scope.siteId, storeId: scope.storeId }}>
      <PageHelp title="Decision log">
        <p>Every decision on this store&rsquo;s reviews, newest first: what was decided and why, on which review, by whom and when. Select a decision to open its review.</p>
      </PageHelp>
      {log.state === "failed" && page === null ? (
        <Notice tone="error" sentence={log.error || "The decision log could not be read."}><Button onClick={log.reread}>Try again</Button></Notice>
      ) : page === null ? (
        <RecordListSkeleton label="Reading the decision log" rows={3} />
      ) : page.entries.length === 0 ? (
        <EmptyState title="No decisions yet" icon={ScrollText}>Each publish, decline and removal is recorded here as it is made.</EmptyState>
      ) : (
        <RecordList as="ul" label="Decision log">
          {page.entries.map((entry) => <LogRow key={entry.id} entry={entry} who={who} onOpen={() => onOpenReview(entry.reviewId)} />)}
        </RecordList>
      )}
      {page !== null && page.next !== "" && pages < 4 ? (
        <div><Button tone="quiet" onClick={() => setPages((n) => n + 1)}>Show more</Button></div>
      ) : null}
    </ReviewsPane>
  );
}

function LogRow({ entry, who, onOpen }: { entry: LogEntry; who: (userId: string) => string; onOpen: () => void }) {
  const named = `${actionDone(entry.action)}${entry.reason ? `: ${reasonLabel(entry.reason)}` : ""}`;
  // WHICH REVIEW, in the words a moderator knows it by: its headline, on its product.
  const about = entry.title !== "" ? `“${entry.title}” on ${entry.productHandle}` : entry.productHandle;
  return (
    <RecordRow
      icon={<ScrollText size={18} aria-hidden />}
      name={named}
      secondary={about || undefined}
      trailing={<span className="os-caption">{formatMoment(entry.at)}</span>}
      onOpen={onOpen}
      label={`Open the review: ${named}`}>
      <span>by {who(entry.decidedBy)}</span>
      {entry.note ? <span className="os-record-excerpt">{entry.note}</span> : null}
    </RecordRow>
  );
}
