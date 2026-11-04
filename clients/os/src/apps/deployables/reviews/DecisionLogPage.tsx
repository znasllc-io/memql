import { useState } from "react";
import { ScrollText } from "lucide-react";

import { Button, Caption, EmptyState, Head, Notice, RecordListSkeleton } from "../../../kit";
import { ActionBar } from "../../../kit/ActionBar";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import { formatMoment } from "../../../kit/format";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { deployedByLabel, type NameOf } from "../people";
import { actionDone, reasonLabel, stateLabel, toneFor } from "./model";
import { useDecisionLog, type ReviewsScope } from "./useReviews";

// THE STORE'S DECISION LOG: every decision on every one of its reviews,
// newest first -- who decided, when, on which words, and why -- with the
// state each review is in now. A row opens its review.
//
// It names the product and the headline, never the writer: the log is the
// record of decisions, and a writer's name belongs to their review.

export function DecisionLogPage({ scope, revision, nameOf, viewerUserId, trail, back, onOpenReview }: {
  scope: ReviewsScope;
  revision: number;
  nameOf: NameOf;
  viewerUserId: string;
  trail: readonly Breadcrumb[];
  back: { label: string; onSelect: () => void };
  onOpenReview: (reviewId: string) => void;
}) {
  const [pages, setPages] = useState(1);
  const log = useDecisionLog(scope, pages, revision);
  const page = log.value;
  const who = (userId: string) => deployedByLabel(userId, viewerUserId, nameOf) || "someone";

  return (
    <section className="os-action-pane" data-os-page-context={JSON.stringify({ page: "Decision log", siteId: scope.siteId, storeId: scope.storeId })}>
      <div className="os-action-body os-app-stack">
        <Head navigationKey={`review-log:${scope.siteId}`} title="Decision log" breadcrumbs={[...trail, { label: "Decision log" }]} back={back} />
        <Caption>Every decision on this store&rsquo;s reviews, newest first: who decided, when, and why.</Caption>
        {log.state === "failed" && page === null ? (
          <Notice tone="error" sentence={log.error || "The decision log could not be read."}><Button onClick={log.reread}>Try again</Button></Notice>
        ) : page === null ? (
          <RecordListSkeleton label="Reading the decision log" rows={3} />
        ) : page.entries.length === 0 ? (
          <EmptyState title="No decisions yet" icon={ScrollText}>Each publish, decline and removal is recorded here as it is made.</EmptyState>
        ) : (
          <RecordList as="ul" label="Decision log">
            {page.entries.map((entry) => {
              const named = `${actionDone(entry.action)}${entry.reason ? `: ${reasonLabel(entry.reason)}` : ""}`;
              const about = [entry.productHandle, entry.title].filter((part) => part !== "").join(" · ");
              return (
                <RecordRow
                  key={entry.id}
                  name={named}
                  secondary={about || undefined}
                  state={stateLabel(entry.reviewState) || undefined}
                  tone={toneFor(entry.reviewState)}
                  trailing={<span className="os-caption">{formatMoment(entry.at)}</span>}
                  onOpen={() => onOpenReview(entry.reviewId)}
                  label={`Open the review: ${named}`}>
                  <span>by {who(entry.decidedBy)}</span>
                  {entry.note ? <span>{entry.note}</span> : null}
                </RecordRow>
              );
            })}
          </RecordList>
        )}
        {page !== null && page.next !== "" && pages < 4 ? (
          <div><Button tone="quiet" onClick={() => setPages((n) => n + 1)}>Show more</Button></div>
        ) : null}
      </div>
      <ActionBar state={page === null ? "" : page.total === 1 ? "1 decision" : `${page.total} decisions`} tone={log.state === "reading" ? "busy" : "none"} />
    </section>
  );
}
