import { useState, type ReactNode } from "react";
import { MessageSquareText } from "lucide-react";

import { Button, Caption, EmptyState, Head, Notice, RecordListSkeleton, useNow } from "../../../kit";
import { ActionBar } from "../../../kit/ActionBar";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import { formatMoment } from "../../../kit/format";
import { LocalTabs } from "../../../kit/LocalTabs";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import type { NameOf } from "../people";
import type { SiteRow } from "../rows";
import { DecisionLog } from "./DecisionLogPage";
import { emptyFilterSentence, ratingWords, reviewName, waitedFor, writerWords, type ReviewRow } from "./model";
import { ReviewPage } from "./ReviewPage";
import { ReviewSettingsView } from "./ReviewSettingsPage";
import { ReviewsPane } from "./ReviewsPane";
import { useReviewList, useReviewSettings, useReviewWrites, type ReviewsScope, type ReviewWrites } from "./useReviews";
import "./reviews.css";

// A STORE'S REVIEWS, as a section of its deployable's page. Its views are
// TABS, drawn the way Fleet draws a machine's (the owner's rule, 2026-11-04):
// the heading, then `LocalTabs`, then the view. Each tab is a different kind of
// thing -- what waits for a decision, what is on the site, the record of every
// decision, the settings -- and one review is a page of its own under the
// tab it was opened from, so the row's Back and the browser's Back both step
// out of it.
//
// WAITING IS OLDEST FIRST, because how long a review has waited is what the
// store's timeline is measured against; Published is newest first. Both
// orders are the engine's (reviewsList). A declined or removed review is
// reached through the decision log, which names every decision and opens the
// review it was made on.

type Tab = "waiting" | "published" | "log" | "settings";

const TABS: readonly (readonly [Tab, string])[] = [
  ["waiting", "Waiting"],
  ["published", "Published"],
  ["log", "Decision log"],
  ["settings", "Settings"],
];

const tabName = (tab: Tab): string => TABS.find(([id]) => id === tab)?.[1] ?? "";

export interface ReviewsSectionProps {
  site: SiteRow;
  storeId: string;
  /** The deployable's name, as its page's trail says it. */
  name: string;
  nameOf: NameOf;
  viewerUserId: string;
  /** The trail down to the deployable: Reviews is added after it. */
  trail: readonly Breadcrumb[];
  /** Back out of Reviews, to the deployable. */
  back: { label: string; onSelect: () => void };
}

export function ReviewsSection({ site, storeId, nameOf, viewerUserId, trail, back }: ReviewsSectionProps) {
  const scope: ReviewsScope = { siteId: site.id, storeId };
  const [tab, setTab] = useState<Tab>("waiting");
  const [openReview, setOpenReview] = useState<string | null>(null);
  const writes = useReviewWrites(scope);
  const settings = useReviewSettings(scope, writes.revision);
  // THE CLUSTER'S OPERATORS HOST THE REVIEWS AND DECIDE NONE: said once, on
  // the lists, rather than as acts drawn and then refused.
  const moderates = settings.value?.viewer.moderates ?? true;

  const show = (next: Tab) => {
    writes.clearRefusal();
    setOpenReview(null);
    setTab(next);
  };
  const toTab = () => {
    writes.clearRefusal();
    setOpenReview(null);
  };

  if (openReview !== null) {
    return <ReviewPage
      key={openReview}
      reviewId={openReview}
      writes={writes}
      nameOf={nameOf}
      viewerUserId={viewerUserId}
      trail={[...trail, { label: "Reviews", onSelect: () => show("waiting") }, { label: tabName(tab), onSelect: toTab }]}
      back={{ label: tabName(tab), onSelect: toTab }} />;
  }

  // The deployable's crumb leads back to it; the Reviews crumb leads to the
  // first view, and stays plain while that view is the page.
  const heading = <>
    <Head navigationKey={`reviews:${site.id}:${tab}`} title="Reviews"
      breadcrumbs={[...trail, { label: "Reviews", onSelect: tab === "waiting" ? undefined : () => show("waiting") }, { label: tabName(tab) }]}
      back={back} />
    <LocalTabs label="Review views" value={tab} onChange={show} options={TABS} />
  </>;

  switch (tab) {
    case "waiting":
    case "published":
      return <ReviewList key={tab} state={tab} scope={scope} heading={heading} writes={writes} moderates={moderates} onOpen={setOpenReview} />;
    case "log":
      return <DecisionLog scope={scope} heading={heading} revision={writes.revision} nameOf={nameOf} viewerUserId={viewerUserId} onOpenReview={setOpenReview} />;
    case "settings":
      return <ReviewSettingsView heading={heading} settings={settings} writes={writes} nameOf={nameOf} viewerUserId={viewerUserId} />;
  }
}

function ReviewList({ state, scope, heading, writes, moderates, onOpen }: {
  state: "waiting" | "published";
  scope: ReviewsScope;
  heading: ReactNode;
  writes: ReviewWrites;
  moderates: boolean;
  onOpen: (reviewId: string) => void;
}) {
  const [pages, setPages] = useState(1);
  const list = useReviewList(scope, state, pages, writes.revision);
  const now = useNow(60_000);
  const page = list.value;
  const rows = page?.reviews ?? [];
  const count = page === null ? undefined : (page.counts[state] ?? page.total);
  const waiting = state === "waiting";

  const bar = <ActionBar
    state={count === undefined ? "" : count === 0 ? (waiting ? "Nothing waiting" : "Nothing published") : `${count} ${waiting ? "waiting" : "published"}`}
    detail={count ? (waiting ? "oldest first" : "newest first") : undefined}
    tone={list.state === "reading" ? "busy" : waiting && count ? "paused" : "live"} />;

  return (
    <ReviewsPane heading={heading} bar={bar} context={{ page: "Reviews", view: tabName(state), siteId: scope.siteId, storeId: scope.storeId }}>
      {!moderates ? <Caption>You can read this store&rsquo;s reviews. Its own people publish and decline them.</Caption> : null}
      {writes.refusal ? <Notice tone="error" sentence={writes.refusal} /> : null}
      {list.state === "failed" && page === null ? (
        <Notice tone="error" sentence={list.error || "The reviews could not be read."}><Button onClick={list.reread}>Try again</Button></Notice>
      ) : page === null ? (
        <RecordListSkeleton label="Reading reviews" rows={3} />
      ) : rows.length === 0 ? (
        <EmptyState title={waiting ? "Nothing waiting" : "Nothing published"} icon={MessageSquareText}>{emptyFilterSentence(state)}</EmptyState>
      ) : (
        <RecordList as="ul" label={waiting ? "Waiting reviews" : "Published reviews"}>
          {rows.map((review) => <ReviewListRow key={review.id} review={review} now={now} onOpen={() => onOpen(review.id)} />)}
        </RecordList>
      )}
      {page !== null && page.next !== "" && pages < 4 ? (
        <div><Button tone="quiet" onClick={() => setPages((n) => n + 1)}>Show more</Button></div>
      ) : null}
    </ReviewsPane>
  );
}

/** A review on a list: its words' name, the product, the rating, the writer,
 * and when -- how long it has waited, or when it went up. The tab already says
 * which state every row is in, so no row says it again. */
function ReviewListRow({ review, now, onOpen }: { review: ReviewRow; now: Date; onOpen: () => void }) {
  const name = reviewName(review);
  const when = review.state === "waiting" ? waitedFor(review.createdAt, now) : formatMoment(review.lastDecision?.at || review.createdAt);
  return (
    <RecordRow
      icon={<MessageSquareText size={18} aria-hidden />}
      name={name}
      secondary={review.productHandle}
      trailing={<span className="os-caption">{when}</span>}
      onOpen={onOpen}
      label={`Open review ${name}`}>
      <span>{ratingWords(review.rating)}</span>
      <span>{writerWords(review)}</span>
    </RecordRow>
  );
}
