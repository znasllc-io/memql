import { useEffect, useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  Caption,
  ChoiceStack,
  Chip,
  CopyValue,
  Fact,
  Facts,
  Head,
  Input,
  LiveList,
  Notice,
  Panel,
  Refine,
  RecordRow as KitRow,
  listCount,
  Subhead,
  formatFreshness,
  formatMoment,
  useLiveView,
  useNow,
  type LiveListSource,
} from "../../kit";
import { ActionBar, type Act } from "../../kit/ActionBar";
import type { DecideApprovalState } from "./actions";
import {
  appWord,
  procedureTitle,
  promotionSubject,
  promotionTargetWords,
  PROCEDURE_PROMOTION,
  type ProcedureRow,
  type PromotionSubject,
} from "./ladder";
import { BindingCounts } from "./ProcedurePage";
import {
  approvalFingerprint,
  approvalFromRow,
  approvalSubjectLine,
  idTail,
  runTitle,
  type ApprovalRow,
  type RunRow,
} from "./rows";
import {
  approvalDecidedNext,
  approvalKindMeaning,
  approvalKindWord,
  approvalParksARun,
  approvalRejectMeaning,
  decisionWord,
} from "./words";

// APPROVALS: the inbox, and the reason this app exists at all.
//
// ===========================================================================
// A PARKED RUN IS STUCK UNTIL SOMEBODY ACTS HERE
// ===========================================================================
// Every human gate in the work spine is one `v1:work:approval` row -- a side
// effect, a scope elevation, a budget ceiling, a skill mint, a question, a
// proposed repair -- and the run that raised it does not move until it is
// decided. The design record is explicit about what the old planner got wrong:
// its human gates were canvas cards on a cognition space, and an engine-only
// cluster registers no canvas concept, so those approvals were ALREADY
// invisible. This surface is the fix, and it is the most important thing in
// this app.
//
// ===========================================================================
// READ, THEN DECIDE -- WHICH IS WHY IT IS NOT A ROW OF APPROVE BUTTONS
// ===========================================================================
// A one-click approve in a list is the obvious design and it is wrong here.
// An approval is a decision about a SPECIFIC artifact -- `artifactHash` is
// over the exact command, patch, message or draft -- and the builtin refuses a
// decision whose artifact moved since it was raised. So the inbox is a triage
// list (kind, what it wants, which run, how long it has waited) and the
// decision is made beside the evidence, on one bar (rules 11 and 12).
//
// The classifier's evidence -- tier, reason, rule id, source -- is rendered in
// the DATA VOICE and never paraphrased. It is the only account of WHY this was
// asked rather than allowed, and a friendlier sentence would drop the rule id
// that tells somebody where to change the policy.

export interface ApprovalsSectionProps {
  approvals: LiveListSource<Row> | null;
  runs: readonly RunRow[];
  decide: DecideApprovalState;
  selectedApprovalId: string;
  onSelectApproval: (approvalId: string) => void;
  onOpenRun: (runId: string) => void;
  /**
   * The learned procedures, so a promotion can name the procedure it would
   * move -- and its parameters by the goal inputs that bind them. Empty until
   * the catalog is read; the card then says what the approval itself carries.
   */
  procedures?: readonly ProcedureRow[];
  onOpenProcedure?: (constructId: string) => void;
}

export function ApprovalsSection({
  approvals,
  runs,
  decide,
  selectedApprovalId,
  onSelectApproval,
  onOpenRun,
  procedures = [],
  onOpenProcedure,
}: ApprovalsSectionProps) {
  const [search, setSearch] = useState("");
  const [choice, setChoice] = useState("");
  const [freeText, setFreeText] = useState("");
  const now = useNow(15_000);

  const viewKey = `approvals:${search.trim().toLowerCase()}`;
  const view = useLiveView<Row, ApprovalRow>(approvals, viewKey, (rows) => {
    const needle = search.trim().toLowerCase();
    const projected = rows
      .map(approvalFromRow)
      .filter((approval) => approval.id !== "")
      .filter((approval) =>
        needle === ""
          ? true
          : [approval.kind, approval.question, approval.evidenceReason, approval.stepKey]
              .join(" ")
              .toLowerCase()
              .includes(needle),
      );
    // OLDEST FIRST, WHICH IS THE OPPOSITE OF EVERY OTHER LIST IN THIS APP.
    // A queue is answered from the front: the approval that has waited longest
    // is the run that has been stopped longest, and burying it under this
    // morning's arrivals is how a run waits a week. There is deliberately no
    // sort control -- a queue with a newest-first option is a queue somebody
    // can accidentally read backwards.
    projected.sort((a, b) => (a.requestedAt || a.createdAt).localeCompare(b.requestedAt || b.createdAt));
    return projected;
  });

  const rows = view?.snapshot.rows ?? [];
  const selected = rows.find((a) => idTail(a.id) === idTail(selectedApprovalId)) ?? null;
  const runsById = useMemo(() => {
    const byId = new Map<string, RunRow>();
    for (const run of runs) byId.set(idTail(run.id), run);
    return byId;
  }, [runs]);

  // A different approval is a different question. Clearing the draft answer on
  // selection is what stops an option picked for one question being sent as
  // the answer to another -- which the artifact hash would not catch, because
  // both artifacts are intact.
  useEffect(() => {
    setChoice("");
    setFreeText("");
    decide.reset();
    // Deliberately keyed on the SELECTION alone.
  }, [selectedApprovalId]);

  const isFeedback = selected?.kind === "feedback";
  const hasOptions = (selected?.options.length ?? 0) > 0;
  const answerReady = isFeedback
    ? hasOptions
      ? choice !== ""
      : freeText.trim() !== ""
    : true;

  const acts: Act[] = [];
  if (selected !== null) {
    if (isFeedback) {
      if (answerReady) {
        acts.push({
          label: "Send answer",
          tone: "primary",
          busy: decide.deciding === idTail(selected.id),
          onAct: () => {
            void decide.decide(selected.id, "answered", answerPayload(selected, choice, freeText));
          },
        });
      }
    } else {
      const labels = decisionLabels(selected);
      // A DECIDED PROMOTION MOVES THE LADDER ON ITS OWN: the procedure is a
      // feed, so its page and its row follow the answer without a re-read.
      const decideIt = (decision: "approved" | "rejected") => void decide.decide(selected.id, decision);
      acts.push({
        label: labels.reject,
        busy: decide.deciding === idTail(selected.id),
        ariaLabel: `${labels.reject === "Reject" ? "Reject this" : labels.reject}: ${approvalRejectMeaning(selected.kind)}`,
        onAct: () => decideIt("rejected"),
      });
      acts.push({
        label: labels.approve,
        tone: "primary",
        busy: decide.deciding === idTail(selected.id),
        onAct: () => decideIt("approved"),
      });
    }
  }

  return (
    <div className="os-nexus-approvals">
      <Head
        title="Approvals"
        meta={listCount(view?.snapshot)}
      >
        <Refine
          search={search}
          onSearch={setSearch}
          placeholder="Kind, question or reason"
          label="Search your approvals"
        />
      </Head>

      <div className="os-nexus-scope">
        <Caption>Longest wait first -- a queue is answered from the front.</Caption>
      </div>

      <div className="os-nexus-split">
        <div className="os-nexus-column">
          <LiveList<ApprovalRow>
            source={view}
            label="Approvals waiting for you"
            rowId={(approval) => approval.id}
            fingerprint={approvalFingerprint}
            emptyText={
              search.trim() === ""
                ? "Nothing is waiting for you. A run that needs a decision puts it here and stops until you make it."
                : "No approval matches that."
            }
            renderRow={(approval) => (
              <ApprovalLine
                approval={approval}
                run={runsById.get(idTail(approval.runId)) ?? null}
                now={now}
                selected={idTail(approval.id) === idTail(selectedApprovalId)}
                onOpen={() => onSelectApproval(approval.id)}
              />
            )}
          />
        </div>

        {/* THE ASIDE IS ABSENT WHEN THERE IS NOTHING IN IT (rule 9). An
            empty panel saying "pick one" reserved half the window to say what
            a clickable row already says, and pushed the queue -- the thing
            this section is for -- into a column. */}
        {selected === null ? null : (
          <div className="os-nexus-column os-nexus-aside">
            <ApprovalDetail
              approval={selected}
              run={runsById.get(idTail(selected.runId)) ?? null}
              choice={choice}
              onChoice={setChoice}
              freeText={freeText}
              onFreeText={setFreeText}
              onOpenRun={onOpenRun}
              procedures={procedures}
              onOpenProcedure={onOpenProcedure}
            />
          </div>
        )}
      </div>

      {selected === null ? null : (
        <ActionBar
          state={decisionWord(selected.decision)}
          // A BAR WITH A STATE AND NO ACTS HAS TO SAY WHY. Send is absent
          // until an answer exists, which is right -- an act that cannot
          // succeed should not be offered -- but an empty bar with no
          // account of itself reads as something nobody built.
          detail={
            isFeedback && !answerReady
              ? hasOptions
                ? "pick an answer above to send it"
                : "write an answer above to send it"
              : selected.kind === PROCEDURE_PROMOTION
                ? // SAID ONCE: a promotion's question already says what it does.
                  undefined
                : approvalKindMeaning(selected.kind)
          }
          tone={selected.decision === "" ? "paused" : "none"}
          acts={acts}
        >
          {decide.error === "" ? null : (
            <span className="os-nexus-act-error os-mono" role="alert">
              {decide.error}
            </span>
          )}
        </ActionBar>
      )}
    </div>
  );
}

/**
 * The `answer` object a feedback decision carries.
 *
 * `answer` is declared `object` on both the concept and the builtin, and epic
 * A2 owns the executor that reads it -- so its INNER shape is not pinned by
 * anything this window can read. A chosen option sends `{value, label}`,
 * which is exactly the member the approval itself offered, so the engine is
 * handed back its own vocabulary rather than one invented here. A free-text
 * answer sends `{text}`, which is the only honest name for what somebody
 * typed into a question with no options.
 *
 * This is the one place in the app that guesses at a contract, and it is
 * written down rather than buried: if A2 names those keys differently, this
 * function is the single edit.
 */
export function answerPayload(
  approval: ApprovalRow,
  choice: string,
  freeText: string,
): Record<string, unknown> {
  const option = approval.options.find((o) => o.value === choice);
  if (option !== undefined) return { value: option.value, label: option.label };
  // A CHOICE WHOSE OPTION IS GONE STILL SENDS THE CHOICE. The row can change
  // under an open panel -- a live feed is what this app is built on -- and
  // falling through to the free-text branch would send `{text: ""}`, which
  // the engine accepts (it only checks the map is non-empty) and records as a
  // blank answer to a question somebody did answer.
  if (choice !== "") return { value: choice };
  return { text: freeText.trim() };
}

function ApprovalLine({
  approval,
  run,
  now,
  selected,
  onOpen,
}: {
  approval: ApprovalRow;
  run: RunRow | null;
  now: Date;
  selected: boolean;
  onOpen: () => void;
}) {
  const waited = approval.requestedAt || approval.createdAt;
  const lapsed = approval.expiresAt !== "" && Date.parse(approval.expiresAt) < now.getTime();
  return (
    <KitRow
      name={<span className="os-nexus-approval-subject">{approvalSubjectLine(approval)}</span>}
      onOpen={onOpen}
      open={selected}
      current={approval.decision === "" && !lapsed}
      dim={approval.decision !== "" || lapsed}
      state={approvalKindWord(approval.kind)}
      stateTitle={approvalKindMeaning(approval.kind)}
      secondary={approvalContext(approval, run)}
      stateExtra={
        <>
          {lapsed ? <Chip tone="muted">lapsed</Chip> : null}
          <span className="os-caption" title={formatMoment(waited)}>
            {formatFreshness(waited, now)}
          </span>
        </>
      }
    />
  );
}

function ApprovalDetail({
  approval,
  run,
  choice,
  onChoice,
  freeText,
  onFreeText,
  onOpenRun,
  procedures,
  onOpenProcedure,
}: {
  approval: ApprovalRow;
  run: RunRow | null;
  choice: string;
  onChoice: (next: string) => void;
  freeText: string;
  onFreeText: (next: string) => void;
  onOpenRun: (runId: string) => void;
  procedures: readonly ProcedureRow[];
  onOpenProcedure?: (constructId: string) => void;
}) {
  const isFeedback = approval.kind === "feedback";
  const shutDoors = doorsFromSubject(approval);
  const promotion = promotionSubject(approval);
  // The procedure a promotion names, once the catalog holds it -- which is
  // what lets the card call its parameters by their goal inputs and compare
  // the version proposed with the version in the catalog now.
  const promoted =
    promotion === null || promotion.constructId === ""
      ? null
      : (procedures.find((p) => idTail(p.id) === idTail(promotion.constructId)) ?? null);
  // The door report has its OWN panel below, so it is kept out of the generic
  // key/value dump -- where it would render as one line of JSON.stringify and
  // tell a reader nothing they could act on. A generic renderer over a
  // structured subject is how a human gate becomes invisible while remaining
  // technically present. A promotion's subject is the same case: it has a
  // panel of its own, so none of it is dumped here.
  const subjectEntries =
    promotion !== null
      ? []
      : Object.entries(approval.subject ?? {}).filter(
          ([key]) => shutDoors === null || (key !== "doors" && key !== "code"),
        );

  return (
    <>
      <Panel label="What is being asked">
        <p className="os-nexus-approval-ask">{approvalSubjectLine(approval)}</p>
        {/* SAID ONCE: a promotion's question already says what promoting does. */}
        {approval.kind === PROCEDURE_PROMOTION ? null : <Caption>{approvalKindMeaning(approval.kind)}</Caption>}

        {isFeedback ? (
          approval.options.length > 0 ? (
            <ChoiceStack
              name="work-approval-answer"
              label="Your answer"
              value={choice}
              onChange={onChoice}
              // PROSE, not the data voice: these are answers to a question
              // somebody is being asked, not enum members they will type
              // elsewhere. `ChoiceStack` grew the prop for exactly this.
              voice="prose"
              options={approval.options.map((option) => ({
                value: option.value,
                label: option.label,
              }))}
            />
          ) : (
            <>
              <Subhead>Your answer</Subhead>
              <Input
                id="work-approval-freetext"
                label="Your answer to this question"
                placeholder="Type your answer"
                value={freeText}
                onChange={onFreeText}
              />
              <Caption>
                This question came with no options, so it takes whatever you write.
              </Caption>
            </>
          )
        ) : null}
      </Panel>

      {promotion === null ? null : (
        <PromotionPanel
          approval={approval}
          subject={promotion}
          procedure={promoted}
          onOpenProcedure={onOpenProcedure}
        />
      )}

      {shutDoors === null ? null : (
        <Panel label="Which doors were shut">
          <Caption>
            The router tries these in order: your own hardware, then a subscription you already pay
            for, then anybody's money. It stopped because every one it tried was shut.
          </Caption>
          <ul className="os-nexus-doors">
            {shutDoors.map((door, i) => (
              <li className="os-nexus-door" key={`${door.name}:${i}`}>
                <div className="os-nexus-door-head">
                  <span className="os-mono">{door.name === "" ? "(unnamed)" : door.name}</span>
                  {door.door === "" ? null : <Chip tone="muted">{door.door}</Chip>}
                </div>
                <p className="os-caption">{door.reason}</p>
                {/* THE MACHINE-LEVEL DETAIL IS THE HALF SOMEBODY CAN ACT ON.
                    "Your fleet is unavailable" sends a person nowhere;
                    "laptop: offline" names the laptop to open. */}
                {door.considered.length === 0 ? null : (
                  <ul className="os-nexus-door-considered">
                    {door.considered.map((one) => (
                      <li key={one.subject}>
                        <span className="os-mono">{one.subject}</span>
                        <span className="os-caption">{one.reason}</span>
                      </li>
                    ))}
                  </ul>
                )}
              </li>
            ))}
          </ul>
          {shutDoors.length === 0 ? (
            <Caption>
              No door list was recorded with this one -- the refusal reached the run as text
              rather than as a report. The evidence below still says which condition it was.
            </Caption>
          ) : null}
        </Panel>
      )}

      {/* THE EVIDENCE, VERBATIM AND IN THE DATA VOICE. This is the classifier's
          own account of why the run stopped rather than carrying on, and the
          rule id is where somebody goes to change the policy. A paraphrase
          would be this window's opinion about a decision the engine made.
          SAID ONCE on a promotion: "What you are promoting" carries its
          evidence -- the matches and the bindings the ladder counted -- and
          the generic panel would repeat the same reason in the data voice. */}
      {approval.kind === PROCEDURE_PROMOTION ? null : (
        <Panel label="Why you were asked">
          <Subhead>The classifier's evidence</Subhead>
          <Facts>
            <Fact label="Tier" value={approval.evidenceTier} mono />
            <Fact label="Reason" value={approval.evidenceReason} />
            <Fact label="Rule" value={approval.evidenceRuleId} mono />
            <Fact label="Source" value={approval.evidenceSource} mono />
          </Facts>
          {approval.evidenceTier === "" &&
          approval.evidenceReason === "" &&
          approval.evidenceRuleId === "" ? (
            <Caption>
              No evidence was recorded with this one. That is a fact about the row rather than about
              the decision -- it does not mean the gate fired for no reason.
            </Caption>
          ) : null}
        </Panel>
      )}

      <Panel label="What it is attached to">
        <Facts>
          {/* A RUN AND A STEP ONLY WHERE THERE ARE ONES. A routing review is
              raised by a sweep and names neither; a promotion names the
              FINISHED shadow run whose comparison met the threshold, and no
              step of it. An em dash beside "Step" on those says nothing. */}
          {approval.runId === "" ? null : (
            <Fact
              label={approval.kind === PROCEDURE_PROMOTION ? "Last comparison" : "Run"}
              value={
                run === null ? (
                  approval.runId
                ) : (
                  <button type="button" className="os-nexus-link" onClick={() => onOpenRun(run.id)}>
                    {runTitle(run)}
                  </button>
                )
              }
            />
          )}
          {approval.stepKey === "" ? null : <Fact label="Step" value={approval.stepKey} mono />}
          <Fact
            label="Raised"
            value={
              approval.requestedAt === "" ? "" : formatMoment(approval.requestedAt)
            }
            title={approval.requestedAt}
          />
          {/* TWO FACTS THAT ONLY EXIST ONCE THEY DO. An em dash beside
              "Decided by" on the queue's whole point -- an undecided approval
              -- is a line a reader takes in to learn nothing. */}
          {approval.expiresAt === "" ? null : (
            <Fact
              label="Lapses"
              value={formatMoment(approval.expiresAt)}
              title={approval.expiresAt}
            />
          )}
          {approval.decidedBy === "" ? null : (
            <Fact label="Decided by" value={approval.decidedBy} mono />
          )}
        </Facts>

        {/* THE HASH IS SHOWN BECAUSE IT IS THE PROMISE. Deciding this approves
            THIS artifact and no other: if it changes before the run resumes,
            the decision is refused rather than carried across. Somebody
            comparing two approvals of the same command needs the value. A
            promotion carries it in its own panel, beside the version it pins,
            so it is said there once. */}
        {promotion !== null ? null : (
          <>
            <Subhead>The exact thing you are deciding</Subhead>
            <CopyValue value={approval.artifactHash} label="artifact hash" />
            <Caption>
              {approvalParksARun(approval.kind)
                ? "Your decision is about this artifact only. If it changes before the run resumes, the decision is refused rather than carried over to the new one."
                : "Your decision is about this artifact only. If it changes before the decision is applied, the decision is refused rather than carried over to the new one."}
            </Caption>
          </>
        )}

        {subjectEntries.length === 0 ? null : (
          <>
            <Subhead>What it says</Subhead>
            <Facts>
              {subjectEntries.map(([key, value]) => (
                <Fact
                  key={key}
                  label={key}
                  value={
                    typeof value === "string" || typeof value === "number"
                      ? String(value)
                      : JSON.stringify(value)
                  }
                  mono
                />
              ))}
            </Facts>
          </>
        )}
      </Panel>

      {approval.decision === "" ? null : (
        <Notice
          tone="info"
          sentence={`${decisionWord(approval.decision)}${
            approval.decidedAt === "" ? "" : ` on ${formatMoment(approval.decidedAt)}`
          }.`}
          next={approvalDecidedNext(approval.kind, approval.decision)}
        />
      )}
    </>
  );
}

/**
 * What an approval is attached to, in the row's quiet line.
 *
 * "a run" was the fallback when the run was not in the feed, and it is wrong
 * for the two kinds that park none: a routing review comes from the nightly
 * fold, and a promotion is about a PROCEDURE -- the finished shadow run it
 * names is where the last comparison happened, not what is being decided.
 */
function approvalContext(approval: ApprovalRow, run: RunRow | null): string {
  if (approval.kind === "routingReview") return "the nightly routing review";
  // SAID ONCE: a promotion's question names the goal the procedure serves, so
  // the row does not name it a second time beside it.
  if (approval.kind === PROCEDURE_PROMOTION) return "";
  return run === null ? "a run" : runTitle(run);
}

/**
 * The two acts' names.
 *
 * A PROMOTION NAMES ITS OWN OUTCOMES. The ladder writes the two options it
 * offers -- "Promote to canary", "Keep it in shadow" -- and an act that says
 * exactly what happens beats "Approve" beside a question about a rung. They
 * are the approval's own words, not ones invented here; a row without them
 * falls back to the words every other kind uses.
 */
function decisionLabels(approval: ApprovalRow): { approve: string; reject: string } {
  if (approval.kind !== PROCEDURE_PROMOTION) return { approve: "Approve", reject: "Reject" };
  const offered = (value: string) => approval.options.find((o) => o.value === value)?.label.trim() ?? "";
  const approve = offered("approved");
  const reject = offered("rejected");
  return { approve: approve || "Approve", reject: reject || "Reject" };
}

/**
 * What a promotion would move, and the evidence it rests on.
 *
 * BOTH HASHES ARE ON THE CARD (#5412). The construct version is the one the
 * ladder proposed; the artifact hash is the one the decision is checked
 * against. They are the same value when all is well -- the approval pins the
 * version -- and showing both is what lets a person see that for themselves.
 * When the catalog holds a NEWER version than the one proposed, the card says
 * so before the decision is refused for it.
 */
function PromotionPanel({
  approval,
  subject,
  procedure,
  onOpenProcedure,
}: {
  approval: ApprovalRow;
  subject: PromotionSubject;
  procedure: ProcedureRow | null;
  onOpenProcedure?: (constructId: string) => void;
}) {
  const title =
    procedure !== null
      ? procedureTitle(procedure)
      : subject.title.trim() || subject.constructName.trim() || "The learned procedure";
  const changed =
    procedure !== null &&
    procedure.procedureHash !== "" &&
    approval.artifactHash !== "" &&
    procedure.procedureHash !== approval.artifactHash;
  const from = [
    appWord(subject.recordedFrom.app),
    subject.recordedFrom.model,
    subject.recordedFrom.effort === "" ? "" : `${subject.recordedFrom.effort} effort`,
  ].filter((part) => part.trim() !== "");
  // WHERE IT WOULD RUN, and whether its matches were dry: two additive keys.
  // An engine that sends neither gets exactly the card it had -- an absent key
  // is not drawn as a dash, because "not said" is not a fact about the run.
  const where = promotionTargetWords(subject.target);
  return (
    <Panel label="What you are promoting">
      <Subhead>What you are promoting</Subhead>
      {changed ? (
        <Notice
          tone="warn"
          sentence="This procedure has changed since it was proposed."
          next="Deciding will be refused: the version below is not the one in your catalog now, and a changed procedure earns its own proposal."
        />
      ) : null}
      <Facts>
        <Fact
          label="Procedure"
          value={
            onOpenProcedure !== undefined && subject.constructId !== "" ? (
              <button
                type="button"
                className="os-nexus-link"
                onClick={() => onOpenProcedure(subject.constructId)}
              >
                {title}
              </button>
            ) : (
              title
            )
          }
        />
        <Fact label="Construct version" value={<CopyValue value={subject.procedureHash} label="construct version" />} />
        <Fact label="Artifact hash" value={<CopyValue value={approval.artifactHash} label="artifact hash" />} />
        <Fact
          label="Matches beside the app"
          value={subject.shadowMatches === null ? "" : `${subject.shadowMatches} in a row`}
        />
        <Fact
          label="Distinct bindings"
          value={
            subject.distinctBindings.length === 0 ? (
              "no parameter to vary"
            ) : (
              <BindingCounts counts={subject.distinctBindings} inputMap={procedure?.inputMap ?? {}} />
            )
          }
        />
        <Fact
          label="Recorded from"
          value={
            // Each part whole: a model id broken at its own hyphen reads as two.
            from.length === 0 ? (
              ""
            ) : (
              <>
                {from.map((part, i) => (
                  <span key={part}>
                    {i === 0 ? null : ", "}
                    <span className="os-nexus-procedure-nowrap">{part}</span>
                  </span>
                ))}
              </>
            )
          }
        />
        {where === "" ? null : <Fact label="Where it would run" value={where} />}
      </Facts>
      {/* DRY MATCHES ARE SAID ONCE, IN PLAIN WORDS. The count above is real,
          but what it counted was a comparison: promoting is the first time
          this procedure would act by itself. */}
      {subject.dryEvidence ? (
        <Caption>
          Its matches compared the commands it would run with the app&apos;s own; it has not run by itself yet.
        </Caption>
      ) : null}
    </Panel>
  );
}

/** One door the router tried, as the approval's subject records it. */
interface ShutDoor {
  door: string;
  name: string;
  reason: string;
  considered: Array<{ subject: string; reason: string }>;
}

/**
 * The door report off an `inferenceUnavailable` approval, or null for every
 * other kind.
 *
 * It returns an EMPTY ARRAY rather than null for an inference approval whose
 * subject carries no doors -- the refusal reached the run as text rather than
 * as a report, which happens on a resumed run -- so the panel still appears
 * and says so. Collapsing that into "not an inference approval" would hide the
 * one kind of park a person most needs to recognise.
 */
function doorsFromSubject(approval: ApprovalRow): ShutDoor[] | null {
  if (approval.kind !== "inferenceUnavailable") return null;
  const raw = approval.subject?.["doors"];
  if (!Array.isArray(raw)) return [];
  const out: ShutDoor[] = [];
  for (const entry of raw) {
    if (entry === null || typeof entry !== "object") continue;
    const record = entry as Record<string, unknown>;
    const considered: Array<{ subject: string; reason: string }> = [];
    const rawConsidered = record["considered"];
    if (Array.isArray(rawConsidered)) {
      for (const one of rawConsidered) {
        if (one === null || typeof one !== "object") continue;
        const c = one as Record<string, unknown>;
        considered.push({
          subject: typeof c["subject"] === "string" ? c["subject"] : "",
          reason: typeof c["reason"] === "string" ? c["reason"] : "",
        });
      }
    }
    out.push({
      door: typeof record["door"] === "string" ? record["door"] : "",
      name: typeof record["name"] === "string" ? record["name"] : "",
      reason: typeof record["reason"] === "string" ? record["reason"] : "",
      considered,
    });
  }
  return out;
}
