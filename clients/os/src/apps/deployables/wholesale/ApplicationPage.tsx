import { useEffect, useState, type ReactNode } from "react";
import { History } from "lucide-react";

import { Button, Caption, ContentSkeleton, Details, DetailsSection, Fact, Facts, Head, Notice, TextArea, useNow } from "../../../kit";
import { ActionBar, type Act, type ActionBarTone } from "../../../kit/ActionBar";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import { formatMoment } from "../../../kit/format";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { ReviewsPane } from "../reviews/ReviewsPane";
import { ExtensionDetails } from "./ExtensionDetails";
import { accessWords, actLabel, dayWords, emailLines, emailWords, fingerprint, resendKind, stageWords, termsWords, transitionWords, waitedWords, type WholesaleAcceptance, type WholesaleApplication, type WholesaleDecision } from "./model";
import { orNotRecorded } from "./notRecorded";
import { useWholesaleApplication, type WholesaleWrites } from "./useWholesale";

// ONE APPLICATION: who applied, the client's own fields, where the buyer's
// access stands, and every decision on it -- open, named sections the way
// Fleet draws a machine's Details, with no box around any of them.
//
// THE ACTS ARE THE ENGINE'S (wholesaleGet's `acts`): the page draws those it
// has a word for and nothing else, an act from a newer engine included. THE
// BAR IS THE DECISION: its one button is the last decision act, the primary
// one; every other is a text label beside it. Reject and Remove ask in place,
// in the bar, with an optional note in the page; Approve and Retry access are
// sent at once. Remove is its own act (wholesaleRemoveBuyer). THE FOUR RESENDS
// ARE THE EMAILS SECTION'S, drawn there exactly when the engine offers them,
// never the bar's button. After any write the page reads again, and a refusal
// stands where its act was taken, in the engine's words.
//
// THE TERMS AND THE EMAILS ARE THE ENGINE'S RECORD: who accepted which
// versions (eight characters of each fingerprint), or when the link expires;
// and every attempt at each email, one line each, in the ruled words with the
// engine's sentence wherever it gives one, never a reason this page wrote.

/** The ask an act opens, when it opens one. */
type Ask = "reject" | "remove";

const ASKS: Readonly<Record<Ask, { question: string; yes: string; no: string }>> = {
  reject: { question: "Reject this application?", yes: "Reject it", no: "Keep it" },
  remove: { question: "Remove this buyer's trade access?", yes: "Remove them", no: "Keep them" },
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
  // WHERE THE LAST ACT WAS TAKEN, so its refusal stands there: the bar, or the Emails section.
  const [actedIn, setActedIn] = useState<"bar" | "emails">("bar");

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

  function resend(act: string) {
    const kind = resendKind(act);
    if (kind === null) return;
    writes.clearRefusal();
    setActedIn("emails");
    void writes.resendEmail(applicationId, kind);
  }

  async function answer(open: Ask) {
    const outcome = open === "reject" ? await writes.decide(applicationId, "reject", note) : await writes.removeBuyer(applicationId, note);
    if (outcome !== "refused") close();
  }

  function press(act: string) {
    writes.clearRefusal();
    setActedIn("bar");
    if (act === "reject" || act === "remove") {
      setNote("");
      setAsk(act);
      return;
    }
    void send(act);
  }

  const busy = writes.busy !== null;
  const known = value.acts.filter((act) => actLabel(act) !== null && resendKind(act) === null);
  const resends = value.acts.filter((act) => resendKind(act) !== null);
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
      <ApplicationDetails value={value} emails={
        <EmailsSection value={value} resends={resends} busy={writes.busy === "resend"} onResend={resend}
          refusal={actedIn === "emails" ? writes.refusal : ""} />
      } />
      {/* THE ENGINE'S REFUSAL, standing on the bar it answers. */}
      {writes.refusal && actedIn === "bar" ? <Notice tone="error" sentence={writes.refusal} /> : null}
    </ReviewsPane>
  );
}

function ApplicationDetails({ value, emails }: { value: WholesaleApplication; emails: ReactNode }) {
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
      <ExtensionDetails details={details} />
      <TermsSection acceptance={value.acceptance} link={value.termsLink} />
      <DetailsSection title="Access">
        <Facts>
          <Fact label="State" value={accessWords(value.entitlement)} />
        </Facts>
      </DetailsSection>
      {emails}
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

/**
 * The terms: who accepted which versions, and from where -- or, before that,
 * when the link the applicant was sent expires. Absent while there is neither
 * (a store that requires no terms, a link not sent yet).
 */
function TermsSection({ acceptance, link }: { acceptance: WholesaleAcceptance | null; link: { expiresAt: string; expired: boolean } | null }) {
  const said = termsWords(acceptance, link);
  if (said === "") return null;
  return (
    <DetailsSection title="Terms">
      <p className="os-wholesale-line">{said}</p>
      {acceptance !== null ? (
        <Facts>
          <Fact label="Wholesale terms" value={<Signed label={acceptance.termsLabel} sha256={acceptance.termsSha256} />} copyValue={acceptance.termsSha256 || undefined} />
          <Fact label="MAP policy" value={<Signed label={acceptance.mapLabel} sha256={acceptance.mapSha256} />} copyValue={acceptance.mapSha256 || undefined} />
          <Fact label="IP address" value={orNotRecorded(acceptance.clientAddress)} copyValue={acceptance.clientAddress || undefined} />
          <Fact label="Browser" value={orNotRecorded(acceptance.userAgent)} copyValue={acceptance.userAgent || undefined} />
        </Facts>
      ) : null}
    </DetailsSection>
  );
}

/** A version an applicant accepted: its label, and eight characters of its fingerprint. */
function Signed({ label, sha256 }: { label: string; sha256: string }) {
  return <><span>{label}</span><span className="os-mono os-wholesale-fingerprint">{orNotRecorded(sha256, fingerprint(sha256))}</span></>;
}

/**
 * Every email the applicant was sent, one line per attempt, and the resends
 * the engine offers on this application. Absent while there is neither.
 */
function EmailsSection({ value, resends, busy, onResend, refusal }: {
  value: WholesaleApplication;
  /** The engine's resend acts on this application, as it answered them. */
  resends: readonly string[];
  busy: boolean;
  onResend: (act: string) => void;
  /** The engine's refusal of a resend, standing where it was asked. */
  refusal: string;
}) {
  const lines = emailLines(value.emails);
  if (lines.length === 0 && resends.length === 0) return null;
  return (
    <DetailsSection title="Emails">
      {lines.length > 0 ? (
        <Facts>
          {lines.map(({ kind, name, email }) => <Fact key={`${kind}:${email.attempt}`} label={name} value={emailWords(email)} />)}
        </Facts>
      ) : null}
      {resends.length > 0 ? (
        <div className="os-panel-actions os-wholesale-resends">
          {resends.map((act) => <Button key={act} tone="quiet" busy={busy} onClick={() => onResend(act)}>{actLabel(act)}</Button>)}
        </div>
      ) : null}
      {refusal ? <Notice tone="error" sentence={refusal} /> : null}
    </DetailsSection>
  );
}
