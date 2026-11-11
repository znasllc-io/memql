import { useEffect, useState } from "react";
import { History } from "lucide-react";

import { Button, Caption, ContentSkeleton, Details, DetailsSection, Fact, Facts, Head, Notice, TextArea, useNow } from "../../../kit";
import { ActionBar, type Act, type ActionBarTone } from "../../../kit/ActionBar";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import { formatMoment } from "../../../kit/format";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { ReviewsPane } from "../reviews/ReviewsPane";
import { accessWords, actLabel, dayWords, stageWords, transitionWords, waitedWords, type WholesaleApplication, type WholesaleDecision } from "./model";
import { useWholesaleApplication, type WholesaleWrites } from "./useWholesale";

// ONE APPLICATION: who applied, the client's own fields, where the buyer's
// access stands, and every decision on it -- open, named sections the way
// Fleet draws a machine's Details, with no box around any of them.
//
// THE ACTS ARE THE ENGINE'S (wholesaleGet's `acts`): the bar draws those it
// has a word for and nothing else, an act from a newer engine included. Its
// one button is the last act, the primary one; every other act is a text label
// beside it. Reject and Remove ask in place, in the bar, with an optional note
// in the page; Approve and Retry access are sent at once. After any write the
// page reads again, and a refusal stands above the bar in the engine's words.

/** The ask an act opens, when it opens one. */
type Ask = "reject" | "remove";

const ASKS: Readonly<Record<Ask, { question: string; yes: string; no: string; transition: "reject" | "revoke" }>> = {
  reject: { question: "Reject this application?", yes: "Reject it", no: "Keep it", transition: "reject" },
  remove: { question: "Remove this buyer's trade access?", yes: "Remove them", no: "Keep them", transition: "revoke" },
};

/** The acts that move an application on are the bar's button; the rest are labels. */
const tone = (act: string): Act["tone"] => (act === "reject" || act === "remove" ? "danger" : "primary");

export function ApplicationPage({ applicationId, listedName, writes, trail, back }: {
  /** The id as the list gave it, whole. */
  applicationId: string;
  /** The company, as the list named it: the title until the application is read. */
  listedName: string;
  writes: WholesaleWrites;
  /** The trail down to the tab the application was opened from: the application is added after it. */
  trail: readonly Breadcrumb[];
  back: { label: string; onSelect: () => void };
}) {
  const reading = useWholesaleApplication(applicationId, writes.revision);
  const now = useNow(60_000);
  const [ask, setAsk] = useState<Ask | null>(null);
  const [note, setNote] = useState("");

  const value = reading.value;
  // AN ASK LIVES ONLY AS LONG AS THE ACT THAT OPENED IT: when the page reads
  // again (after a refusal, say, because somebody decided first) and the
  // engine no longer offers that act, the ask closes. Offered still -- a
  // refusal for another reason -- it stays, with the note as typed.
  const offered = ask !== null && value !== null && value.acts.includes(ask);
  useEffect(() => {
    if (ask !== null && value !== null && !value.acts.includes(ask)) {
      setAsk(null);
      setNote("");
    }
  }, [ask, value]);

  const name = value?.application.companyName || listedName || "Application";
  const head = <Head navigationKey={`wholesale-application:${applicationId}`} title={name} breadcrumbs={[...trail, { label: name }]} back={back} />;
  const context = { page: "Wholesale application", applicationId, stage: value?.stage ?? "" };

  if (value === null) {
    return (
      <ReviewsPane heading={head} bar={<ActionBar state="" tone={reading.state === "failed" ? "none" : "busy"} />} context={context}>
        {reading.state === "failed" ? (
          <Notice tone="error" sentence={reading.error}><Button onClick={reading.reread}>Try again</Button></Notice>
        ) : (
          <ContentSkeleton label="Reading the application" kind="detail" />
        )}
      </ReviewsPane>
    );
  }

  const close = () => {
    setAsk(null);
    setNote("");
  };

  async function send(act: string) {
    if (act === "approve") await writes.decide(applicationId, "approve", "");
    else if (act === "retryAccess") await writes.retryAccess(applicationId);
  }

  async function answer(open: Ask) {
    const outcome = await writes.decide(applicationId, ASKS[open].transition, note);
    if (outcome !== "refused") close();
  }

  function press(act: string) {
    writes.clearRefusal();
    if (act === "reject" || act === "remove") {
      setNote("");
      setAsk(act);
      return;
    }
    void send(act);
  }

  const busy = writes.busy !== null;
  const known = value.acts.filter((act) => actLabel(act) !== null);
  const lead = [...known].reverse().find((act) => tone(act) === "primary") ?? known[known.length - 1] ?? null;
  const open = offered ? ask : null;
  const acts: Act[] = open !== null
    ? [
        { label: ASKS[open].no, text: true, onAct: close },
        { label: ASKS[open].yes, tone: "danger", busy, onAct: () => void answer(open) },
      ]
    : [
        ...known.filter((act) => act !== lead).map((act) => ({ label: actLabel(act) ?? act, text: true, onAct: () => press(act) })),
        ...(lead !== null ? [{ label: actLabel(lead) ?? lead, tone: tone(lead), busy, onAct: () => press(lead) }] : []),
      ];
  const submitted = value.stage === "readyToReview" || value.stage === "waitingForTerms";
  const barTone: ActionBarTone = busy ? "busy" : value.stage === "readyToReview" ? "paused" : value.stage === "approved" ? "live" : "none";
  const bar = (
    <ActionBar state={stageWords(value.stage)} meta={submitted ? waitedWords(value.application.createdAt, now) : undefined} tone={barTone} acts={acts} scope={`wholesale:${applicationId}:${open ?? ""}`}>
      {open !== null ? <span className="os-actbar-detail">{ASKS[open].question}</span> : null}
    </ActionBar>
  );

  return (
    <ReviewsPane heading={head} bar={bar} context={context}>
      {open !== null ? (
        <TextArea id={`wholesale-note-${open}`} label="Note" rows={3} value={note} onChange={setNote} placeholder="A note for the decision log (optional)" />
      ) : null}
      <ApplicationDetails value={value} />
      {/* THE ENGINE'S REFUSAL, standing on the bar it answers. */}
      {writes.refusal ? <Notice tone="error" sentence={writes.refusal} /> : null}
    </ReviewsPane>
  );
}

function ApplicationDetails({ value }: { value: WholesaleApplication }) {
  const { application, details } = value;
  return (
    <Details>
      <DetailsSection title="Application">
        <Facts>
          <Fact label="Company" value={application.companyName} copyValue={application.companyName} />
          <Fact label="Applicant" value={application.applicantName} copyValue={application.applicantName} />
          <Fact label="Email" value={application.applicantEmail} copyValue={application.applicantEmail} />
          <Fact label="Applied" value={dayWords(application.createdAt)} copyValue={application.createdAt} />
        </Facts>
      </DetailsSection>
      <DetailsSection title="Their details">
        {details.state === "unreadable" ? (
          <Caption>These details are not readable to you</Caption>
        ) : details.fields.length === 0 ? (
          <Caption>No further details</Caption>
        ) : (
          <Facts>
            {details.fields.map((f, i) => <Fact key={`${f.label}:${i}`} label={f.label} value={f.value} copyValue={f.value || undefined} />)}
          </Facts>
        )}
      </DetailsSection>
      <DetailsSection title="Access">
        <Facts>
          <Fact label="State" value={accessWords(value.entitlement)} />
        </Facts>
      </DetailsSection>
      <DetailsSection title="Decisions">
        {value.decisions.length > 0 ? (
          <RecordList as="ul" label="Decisions on this application">
            {value.decisions.map((d, i) => <DecisionRow key={`${d.createdAt}:${i}`} decision={d} />)}
          </RecordList>
        ) : (
          <Caption>No decision yet</Caption>
        )}
      </DetailsSection>
    </Details>
  );
}

function DecisionRow({ decision }: { decision: WholesaleDecision }) {
  return (
    <RecordRow
      icon={<History size={18} aria-hidden />}
      name={transitionWords(decision.transition)}
      secondary={decision.note || undefined}
      trailing={<span className="os-caption">{formatMoment(decision.createdAt)}</span>}>
      {decision.decidedByName ? <span>by {decision.decidedByName}</span> : null}
    </RecordRow>
  );
}
