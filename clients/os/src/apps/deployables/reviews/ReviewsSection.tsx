import { useState } from "react";
import { Download, MessageSquareText, ScrollText, Settings2 } from "lucide-react";

import { Button, Caption, ChoiceRow, EmptyState, Head, Notice, RecordListSkeleton, useNow } from "../../../kit";
import { ActionBar } from "../../../kit/ActionBar";
import { formatMoment } from "../../../kit/format";
import { IconButton } from "../../../kit/IconButton";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import type { NameOf } from "../people";
import type { SiteRow } from "../rows";
import { DecisionLogPage } from "./DecisionLogPage";
import { FILTERS, emptyFilterSentence, reviewName, stateLabel, toneFor, waitedFor, type ReviewFilter, type ReviewRow } from "./model";
import { ReviewPage } from "./ReviewPage";
import { ReviewSettingsPage } from "./ReviewSettingsPage";
import { useReviewList, useReviewSettings, useReviewWrites, type ReviewsScope } from "./useReviews";
import "./reviews.css";

// A STORE'S REVIEWS, as a section of its deployable's page (the owner's
// design, 2026-11-03): the queue first, one review with its decisions and the
// acts the engine would take from this person, the store's decision log, and
// the settings. Each is a page of its own under the deployable's trail, so
// the row's Back and the browser's Back both step out of it.
//
// THE QUEUE IS OLDEST FIRST, because how long a review has waited is what the
// store's timeline is measured against; every other filter is newest first.
// Both orders are the engine's (reviewsList).

type View =
  | { kind: "queue" }
  | { kind: "review"; id: string; from: "queue" | "log" }
  | { kind: "log" }
  | { kind: "settings" };

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
  const [view, setView] = useState<View>({ kind: "queue" });
  const [filter, setFilter] = useState<ReviewFilter>("waiting");
  const [pages, setPages] = useState(1);
  const writes = useReviewWrites(scope);
  const list = useReviewList(scope, filter, pages, writes.revision);
  const settings = useReviewSettings(scope, writes.revision);
  const now = useNow(60_000);

  const toQueue = () => setView({ kind: "queue" });
  const reviewsCrumb: Breadcrumb = { label: "Reviews", onSelect: toQueue };

  if (view.kind === "review") {
    const fromLog = view.from === "log";
    const toLog = () => setView({ kind: "log" });
    return <ReviewPage
      key={view.id}
      reviewId={view.id}
      writes={writes}
      nameOf={nameOf}
      viewerUserId={viewerUserId}
      trail={fromLog ? [...trail, reviewsCrumb, { label: "Decision log", onSelect: toLog }] : [...trail, reviewsCrumb]}
      back={fromLog ? { label: "Decision log", onSelect: toLog } : { label: "Reviews", onSelect: toQueue }} />;
  }
  if (view.kind === "log") {
    return <DecisionLogPage
      scope={scope}
      revision={writes.revision}
      nameOf={nameOf}
      viewerUserId={viewerUserId}
      trail={[...trail, reviewsCrumb]}
      back={{ label: "Reviews", onSelect: toQueue }}
      onOpenReview={(id) => setView({ kind: "review", id, from: "log" })} />;
  }
  if (view.kind === "settings") {
    return <ReviewSettingsPage
      settings={settings}
      writes={writes}
      nameOf={nameOf}
      viewerUserId={viewerUserId}
      trail={[...trail, reviewsCrumb]}
      back={{ label: "Reviews", onSelect: toQueue }} />;
  }

  const page = list.value;
  const rows = page?.reviews ?? [];
  const counts = page?.counts ?? {};
  const moderates = settings.value?.viewer.moderates ?? true;
  const waiting = counts.waiting;
  const exporting = writes.busy === "export";

  async function exportReviews() {
    const file = await writes.exportStore();
    if (file !== null) download(file.fileName, file.json);
  }

  return (
    <section className="os-action-pane" data-os-page-context={JSON.stringify({ page: "Reviews", siteId: site.id, storeId, filter })}>
      <div className="os-action-body os-app-stack">
        <Head navigationKey={`reviews:${site.id}`} title="Reviews" breadcrumbs={[...trail, { label: "Reviews" }]} back={back}>
          <IconButton label="Decision log" onClick={() => setView({ kind: "log" })}><ScrollText size={16} aria-hidden /></IconButton>
          <IconButton label="Review settings" onClick={() => setView({ kind: "settings" })}><Settings2 size={16} aria-hidden /></IconButton>
          <IconButton label="Export reviews" disabled={exporting} onClick={() => void exportReviews()}><Download size={16} aria-hidden /></IconButton>
        </Head>
        {/* THE CLUSTER'S OPERATORS HOST THE REVIEWS AND DECIDE NONE: said once,
            here, rather than as acts drawn and then refused. */}
        {!moderates ? <Caption>You can read this store&rsquo;s reviews. Its own people publish and decline them.</Caption> : null}
        {writes.refusal ? <Notice tone="error" sentence={writes.refusal} /> : null}
        <ChoiceRow
          label="Show reviews"
          value={filter}
          onChange={(next) => { setFilter(next as ReviewFilter); setPages(1); }}
          options={FILTERS.map((f) => {
            const n = counts[f.value];
            return { value: f.value, label: n === undefined ? f.label : `${f.label} ${n}`, ariaLabel: n === undefined ? f.label : `${f.label}, ${n}` };
          })} />
        {list.state === "failed" && page === null ? (
          <Notice tone="error" sentence={list.error || "The reviews could not be read."}><Button onClick={list.reread}>Try again</Button></Notice>
        ) : page === null ? (
          <RecordListSkeleton label="Reading reviews" rows={3} />
        ) : rows.length === 0 ? (
          <EmptyState title={filter === "waiting" ? "Nothing waiting" : "No reviews"} icon={MessageSquareText}>{emptyFilterSentence(filter)}</EmptyState>
        ) : (
          <RecordList as="ul" label="Reviews">
            {rows.map((review) => <QueueRow key={review.id} review={review} now={now} onOpen={() => setView({ kind: "review", id: review.id, from: "queue" })} />)}
          </RecordList>
        )}
        {page !== null && page.next !== "" && pages < 4 ? (
          <div><Button tone="quiet" onClick={() => setPages((n) => n + 1)}>Show more</Button></div>
        ) : null}
      </div>
      <ActionBar
        state={waiting === undefined ? "" : waiting === 0 ? "Nothing waiting" : `${waiting} waiting`}
        detail={waiting ? "oldest first" : undefined}
        tone={list.state === "reading" || exporting ? "busy" : waiting ? "paused" : "live"} />
    </section>
  );
}

function QueueRow({ review, now, onOpen }: { review: ReviewRow; now: Date; onOpen: () => void }) {
  const name = reviewName(review);
  // The state already says Waiting: beside it, only for how long.
  const when = review.state === "waiting"
    ? waitedFor(review.createdAt, now)
    : review.lastDecision ? formatMoment(review.lastDecision.at) : formatMoment(review.createdAt);
  return (
    <RecordRow
      icon={<MessageSquareText size={18} aria-hidden />}
      name={name}
      secondary={`${review.productHandle}${review.verifiedBuyer ? " · verified buyer" : ""}`}
      state={stateLabel(review.state)}
      tone={toneFor(review.state)}
      trailing={<span className="os-caption">{when}</span>}
      onOpen={onOpen}
      label={`Open review ${name}`}>
      <span>{review.rating ? `${review.rating} of 5` : "No rating"}</span>
      <span>{review.authorName || "No name given"}</span>
    </RecordRow>
  );
}

/** Hands the browser a file, the way a download link would. */
function download(fileName: string, json: string) {
  const url = URL.createObjectURL(new Blob([json], { type: "application/json" }));
  const link = document.createElement("a");
  link.href = url;
  link.download = fileName;
  document.body.appendChild(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
