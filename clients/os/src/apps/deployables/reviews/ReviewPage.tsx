import { useState } from "react";
import { History } from "lucide-react";

import { Button, Caption, ChoiceStack, ContentSkeleton, Details, DetailsSection, Fact, Facts, Head, Notice, Subhead, TextArea, useNow } from "../../../kit";
import { ActionBar, type Act, type ActionBarTone } from "../../../kit/ActionBar";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import { formatMoment } from "../../../kit/format";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { deployedByLabel, type NameOf } from "../people";
import {
  ACT_LABEL,
  actionDone,
  ratingWords,
  reasonLabel,
  reasonsFor,
  reviewName,
  stateLabel,
  waitedFor,
  type ReviewAct,
  type ReviewDecision,
  type ReviewRow,
} from "./model";
import { ReviewsPane } from "./ReviewsPane";
import { useReviewDetail, type ReviewWrites } from "./useReviews";

// ONE REVIEW: the words as the writer sent them, who wrote them, every
// decision on them, and the acts this person may take on them now -- open,
// named sections the way Fleet draws a machine's Details, with no box around
// any of them.
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
      return "The same standards apply to every star rating: a low rating, a harsh tone or a bad experience is never a reason, and criticism of taste, results, packaging, shipping, price or service is publishable. When in doubt, publish. The writer can change their review through their link while it is open, and it then waits again.";
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
  /** The trail down to the view the review was opened from: the review is added after it. */
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
  const context = { page: "Review", reviewId, state: review?.state ?? "" };

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
      <ReviewsPane heading={head} bar={<ActionBar state="" tone={detail.state === "failed" ? "none" : "busy"} />} context={context}>
        {detail.state === "failed" ? (
          <Notice tone="error" sentence={detail.error || "This review could not be read."}><Button onClick={detail.reread}>Try again</Button></Notice>
        ) : (
          <ContentSkeleton label="Reading the review" kind="detail" />
        )}
      </ReviewsPane>
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
  const bar = <ActionBar state={stateLabel(review.state)} detail={barDetail} tone={barTone} acts={barActs} scope={`review:${reviewId}:${step ?? ""}`} />;

  return (
    <ReviewsPane heading={head} bar={bar} context={context}>
      {writes.refusal ? <Notice tone="error" sentence={writes.refusal} /> : null}
      {value !== null && !value.moderates ? <Caption>You can read this review. The store&rsquo;s own people decide it.</Caption> : null}
      {/* THE WORDS STAY IN VIEW, a step open or not: a reason is chosen
          against them. The headline is the page's title, so it is not said
          again above the text. */}
      <ReviewText review={review} />
      {step !== null ? (
        <div className="os-review-step" aria-label={ACT_LABEL[step]} role="group">
          <Subhead>{question(step)}</Subhead>
          <Caption>{stepNote(step)}</Caption>
          {reasons.length > 0 ? (
            <ChoiceStack name={`review-reason-${step}`} label="Reason" voice="prose" value={reason} onChange={setReason}
              options={reasons.map((r) => ({ value: r.value, label: r.label, description: r.means }))} />
          ) : null}
          <TextArea id={`review-note-${reviewId}`} label="Note" rows={3} value={note} onChange={setNote}
            placeholder={step === "erase" ? "How the request arrived (optional)" : "A note for the log (optional)"} />
        </div>
      ) : (
        <Details>
          <DetailsSection title="Review">
            <Facts>
              <Fact label="Product" value={review.productHandle} copyValue={review.productHandle} />
              <Fact label="Rating" value={ratingWords(review.rating)} />
              <Fact label="Sent" value={formatMoment(review.createdAt)} />
              <Fact label="Revision" value={review.revision > 1 ? `${review.revision}, edited after it was first sent` : "1"} />
            </Facts>
          </DetailsSection>
          <DetailsSection title="Writer">
            <Facts>
              <Fact label="Name" value={review.authorName || (review.state === "erased" ? "Erased" : "No name given")} />
              {review.authorEmail ? <Fact label="Email" value={review.authorEmail} copyValue={review.authorEmail} /> : null}
              <Fact label="Verified buyer" value={review.verifiedBuyer ? "Yes, written through their order's link" : "No"} />
            </Facts>
          </DetailsSection>
          <DetailsSection title="Decisions">
            {value?.decisions.length ? (
              <RecordList as="ul" label="Decisions on this review">
                {value.decisions.map((d) => <DecisionRow key={d.id} decision={d} who={who} current={review.revision} />)}
              </RecordList>
            ) : (
              <Caption>No decision yet. It is not on the site until somebody publishes it.</Caption>
            )}
          </DetailsSection>
        </Details>
      )}
    </ReviewsPane>
  );
}

function ReviewText({ review }: { review: ReviewRow }) {
  if (review.state === "erased" && review.title === "" && review.body === "") {
    return <Caption>The writer&rsquo;s data was erased on request.</Caption>;
  }
  return review.body ? <blockquote className="os-review-text"><p>{review.body}</p></blockquote> : null;
}

function DecisionRow({ decision, who, current }: { decision: ReviewDecision; who: (userId: string) => string; current: number }) {
  const named = `${actionDone(decision.action)}${decision.reason ? `: ${reasonLabel(decision.reason)}` : ""}`;
  const earlier = decision.revision !== current;
  return (
    <RecordRow
      icon={<History size={18} aria-hidden />}
      name={named}
      secondary={decision.note || undefined}
      trailing={<span className="os-caption">{formatMoment(decision.at)}</span>}
      dim={earlier}>
      <span>by {who(decision.decidedBy)}</span>
      <span>{earlier ? `on revision ${decision.revision}` : `revision ${decision.revision}`}</span>
    </RecordRow>
  );
}
