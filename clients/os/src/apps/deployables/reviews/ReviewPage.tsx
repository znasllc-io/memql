import { useState } from "react";

import { Button, Caption, ChoiceStack, Fact, Facts, Head, Notice, Panel, RecordListSkeleton, Subhead, TextArea, useNow } from "../../../kit";
import { ActionBar, type Act, type ActionBarTone } from "../../../kit/ActionBar";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import { formatMoment } from "../../../kit/format";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { deployedByLabel, type NameOf } from "../people";
import {
  ACT_LABEL,
  actionDone,
  reasonLabel,
  reasonsFor,
  reviewName,
  stateLabel,
  waitedFor,
  type ReviewAct,
  type ReviewDecision,
  type ReviewRow,
} from "./model";
import { useReviewDetail, type ReviewWrites } from "./useReviews";

// ONE REVIEW: the words as the writer sent them, who wrote them, every
// decision on them, and the acts this person may take on them now.
//
// THE ACTS ARE THE ENGINE'S (reviewsGet answers the ones it would accept), and
// the bar carries ONE button: the act that moves the review on -- Publish while
// it waits, Remove once it is out -- with every other act a text label beside
// it. An act that needs a reason opens one step in the page, the closed list
// of reasons and an optional note, and its button is there only once a reason
// is chosen: absent, never disabled.

/** The act the bar's one button carries, when the review offers it. */
const LEAD: readonly ReviewAct[] = ["publish", "remove"];

const tone = (act: ReviewAct): Act["tone"] => (act === "publish" ? "primary" : act === "decline" || act === "remove" || act === "erase" ? "danger" : "quiet");

/** What a step asks, in one line. */
function question(act: ReviewAct): string {
  switch (act) {
    case "decline":
      return "Which reason applies?";
    case "askToFix":
      return "What does the writer need to fix?";
    case "remove":
      return "Why is it coming down?";
    case "escalate":
      return "What is the concern?";
    case "erase":
      return "Erase the writer's data?";
    default:
      return "";
  }
}

/** What the step says before anything is sent. */
function stepNote(act: ReviewAct): string {
  switch (act) {
    case "decline":
      return "A low rating, a harsh tone or a bad experience is never a reason. The writer can change their review through their link while it is open, and it then waits again.";
    case "askToFix":
      return "The review stays waiting, and nothing is sent: contact the writer yourself and say what to change. Their link lets them change it while it is open.";
    case "remove":
      return "The review comes off the site. The decision is kept in the log with its reason.";
    case "escalate":
      return "Recorded in the decision log beside the review. It does not change whether the review is shown.";
    case "erase":
      return "Their name, email, headline and text are blanked in every saved version of this review, and the review is off the site for good. The log keeps that it was erased, by whom and when.";
    default:
      return "";
  }
}

export function ReviewPage({ reviewId, writes, nameOf, viewerUserId, trail, back }: {
  reviewId: string;
  writes: ReviewWrites;
  nameOf: NameOf;
  viewerUserId: string;
  /** The trail down to Reviews (or to the decision log): the review is added after it. */
  trail: readonly Breadcrumb[];
  back: { label: string; onSelect: () => void };
}) {
  const detail = useReviewDetail(reviewId, writes.revision);
  const now = useNow(60_000);
  const [step, setStep] = useState<ReviewAct | null>(null);
  const [reason, setReason] = useState("");
  const [note, setNote] = useState("");
  const who = (userId: string) => deployedByLabel(userId, viewerUserId, nameOf) || "someone";

  const value = detail.value;
  const review = value?.review ?? null;
  const name = review ? reviewName(review) : "Review";
  const head = <Head navigationKey={`review:${reviewId}`} title={name} breadcrumbs={[...trail, { label: name }]} back={back} />;

  const close = () => {
    setStep(null);
    setReason("");
    setNote("");
  };

  async function send(act: ReviewAct) {
    if (review === null) return;
    const outcome = act === "erase" ? await writes.erase(review, note) : await writes.decide(review, act, reason, note);
    if (outcome !== "refused") close();
  }

  function open(act: ReviewAct) {
    writes.clearRefusal();
    // PUBLISH NEEDS NO STEP: it names no reason, and the words are on screen.
    if (act === "publish") {
      void send("publish");
      return;
    }
    setReason("");
    setNote("");
    setStep(act);
  }

  if (review === null) {
    return (
      <section className="os-action-pane">
        <div className="os-action-body os-app-stack">
          {head}
          {detail.state === "failed" ? (
            <Notice tone="error" sentence={detail.error || "This review could not be read."}><Button onClick={detail.reread}>Try again</Button></Notice>
          ) : (
            <RecordListSkeleton label="Reading the review" rows={2} />
          )}
        </div>
        <ActionBar state="" tone="busy" />
      </section>
    );
  }

  const acts = value?.acts ?? [];
  const lead = LEAD.find((act) => acts.includes(act)) ?? null;
  const reasons = step === null ? [] : reasonsFor(step);
  const ready = step !== null && (reasons.length === 0 || reason !== "");
  const busy = writes.busy !== null;
  const barActs: Act[] = step !== null
    ? [
        { label: "Cancel", text: true, onAct: close },
        ...(ready ? [{ label: ACT_LABEL[step], tone: tone(step), busy, onAct: () => void send(step) }] : []),
      ]
    : [
        ...acts.filter((act) => act !== lead).map((act) => ({ label: ACT_LABEL[act], text: true, onAct: () => open(act) })),
        ...(lead !== null ? [{ label: ACT_LABEL[lead], tone: tone(lead), busy, onAct: () => open(lead) }] : []),
      ];
  const last = value?.decisions[0] ?? null;
  const barTone: ActionBarTone = busy ? "busy" : review.state === "published" ? "live" : review.state === "waiting" ? "paused" : "none";
  const barDetail = review.state === "waiting"
    ? `for ${waitedFor(review.createdAt, now)}`
    : last !== null ? `${formatMoment(last.at)} by ${who(last.decidedBy)}` : undefined;

  return (
    <section className="os-action-pane" data-os-page-context={JSON.stringify({ page: "Review", reviewId, state: review.state })}>
      <div className="os-action-body os-app-stack">
        {head}
        {writes.refusal ? <Notice tone="error" sentence={writes.refusal} /> : null}
        {value !== null && !value.moderates ? <Caption>You can read this review. The store&rsquo;s own people decide it.</Caption> : null}
        {step !== null ? (
          <Panel label={ACT_LABEL[step]}>
            {/* The words stay in view: a reason is chosen against them. */}
            <ReviewText review={review} />
            <Subhead>{question(step)}</Subhead>
            <Caption>{stepNote(step)}</Caption>
            {reasons.length > 0 ? (
              <ChoiceStack name={`review-reason-${step}`} label="Reason" voice="prose" value={reason} onChange={setReason}
                options={reasons.map((r) => ({ value: r.value, label: r.label, description: r.means }))} />
            ) : null}
            <TextArea id={`review-note-${reviewId}`} label="Note" rows={3} value={note} onChange={setNote}
              placeholder={step === "erase" ? "How the request arrived (optional)" : "A note for the log (optional)"} />
          </Panel>
        ) : (
          <>
            <Panel label="The review">
              <ReviewText review={review} />
              <Facts>
                <Fact label="Product" value={review.productHandle} mono />
                <Fact label="Rating" value={review.rating ? `${review.rating} of 5` : "None given"} />
                <Fact label="Writer" value={review.authorName || (review.state === "erased" ? "Erased" : "No name given")} />
                {review.authorEmail ? <Fact label="Email" value={review.authorEmail} copyValue={review.authorEmail} /> : null}
                <Fact label="Verified buyer" value={review.verifiedBuyer ? "Yes: written through their order's link" : "No"} />
                <Fact label="Sent" value={formatMoment(review.createdAt)} />
                <Fact label="Revision" value={review.revision > 1 ? `${review.revision}: edited after it was first sent` : "1"} />
              </Facts>
            </Panel>
            <Subhead meta={value?.decisions.length ? String(value.decisions.length) : undefined}>Decisions</Subhead>
            {value?.decisions.length ? (
              <RecordList as="ul" label="Decisions on this review">
                {value.decisions.map((d) => <DecisionRow key={d.id} decision={d} who={who} current={review.revision} />)}
              </RecordList>
            ) : (
              <Caption>No decision yet. It is not on the site until somebody publishes it.</Caption>
            )}
          </>
        )}
      </div>
      <ActionBar state={stateLabel(review.state)} detail={barDetail} tone={barTone} acts={barActs} scope={`review:${reviewId}:${step ?? ""}`} />
    </section>
  );
}

function ReviewText({ review }: { review: ReviewRow }) {
  if (review.state === "erased" && review.title === "" && review.body === "") {
    return <Caption>The writer&rsquo;s data was erased on request.</Caption>;
  }
  return (
    <blockquote className="os-review-text">
      {review.title ? <strong>{review.title}</strong> : null}
      {review.body ? <p>{review.body}</p> : null}
    </blockquote>
  );
}

function DecisionRow({ decision, who, current }: { decision: ReviewDecision; who: (userId: string) => string; current: number }) {
  const named = `${actionDone(decision.action)}${decision.reason ? `: ${reasonLabel(decision.reason)}` : ""}`;
  return (
    <RecordRow
      name={named}
      secondary={decision.note || undefined}
      state={formatMoment(decision.at)}
      tone="muted"
      dim={decision.revision !== current}>
      <span>by {who(decision.decidedBy)}</span>
      <span>{decision.revision !== current ? `on revision ${decision.revision}` : `revision ${decision.revision}`}</span>
    </RecordRow>
  );
}
