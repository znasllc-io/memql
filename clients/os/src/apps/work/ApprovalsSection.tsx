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
  Row as KitRow,
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
  approvalFingerprint,
  approvalFromRow,
  approvalSubjectLine,
  idTail,
  runTitle,
  type ApprovalRow,
  type RunRow,
} from "./rows";
import { approvalKindMeaning, approvalKindWord, decisionWord } from "./words";

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
}

export function ApprovalsSection({
  approvals,
  runs,
  decide,
  selectedApprovalId,
  onSelectApproval,
  onOpenRun,
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
      acts.push({
        label: "Reject",
        busy: decide.deciding === idTail(selected.id),
        ariaLabel: "Reject this: the step fails and the run does not do it",
        onAct: () => void decide.decide(selected.id, "rejected"),
      });
      acts.push({
        label: "Approve",
        tone: "primary",
        busy: decide.deciding === idTail(selected.id),
        onAct: () => void decide.decide(selected.id, "approved"),
      });
    }
  }

  return (
    <div className="os-work-approvals">
      <Head
        title="Approvals"
        meta={rows.length === 0 ? "nothing waiting" : `${rows.length} waiting for you`}
      >
        <Refine
          search={search}
          onSearch={setSearch}
          placeholder="Kind, question or reason"
          label="Search your approvals"
        />
      </Head>

      <div className="os-work-scope">
        <Caption>Longest wait first -- a queue is answered from the front.</Caption>
      </div>

      <div className="os-work-split">
        <div className="os-work-column">
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
          <div className="os-work-column os-work-aside">
            <ApprovalDetail
              approval={selected}
              run={runsById.get(idTail(selected.runId)) ?? null}
              choice={choice}
              onChoice={setChoice}
              freeText={freeText}
              onFreeText={setFreeText}
              onOpenRun={onOpenRun}
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
              : approvalKindMeaning(selected.kind)
          }
          tone={selected.decision === "" ? "paused" : "none"}
          acts={acts}
        >
          {decide.error === "" ? null : (
            <span className="os-work-act-error os-mono" role="alert">
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
      name={<span className="os-work-approval-subject">{approvalSubjectLine(approval)}</span>}
      onOpen={onOpen}
      open={selected}
      current={approval.decision === "" && !lapsed}
      dim={approval.decision !== "" || lapsed}
      state={
        <>
          {lapsed ? <Chip tone="muted">lapsed</Chip> : null}
          <span className="os-caption" title={formatMoment(waited)}>
            {formatFreshness(waited, now)}
          </span>
        </>
      }
    >
      <Chip tone="neutral" title={approvalKindMeaning(approval.kind)}>
        {approvalKindWord(approval.kind)}
      </Chip>
      <span className="os-work-approval-run">{run === null ? "a run" : runTitle(run)}</span>
    </KitRow>
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
}: {
  approval: ApprovalRow;
  run: RunRow | null;
  choice: string;
  onChoice: (next: string) => void;
  freeText: string;
  onFreeText: (next: string) => void;
  onOpenRun: (runId: string) => void;
}) {
  const isFeedback = approval.kind === "feedback";
  const subjectEntries = Object.entries(approval.subject ?? {});

  return (
    <>
      <Panel label="What is being asked">
        <p className="os-work-approval-ask">{approvalSubjectLine(approval)}</p>
        <Caption>{approvalKindMeaning(approval.kind)}</Caption>

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

      {/* THE EVIDENCE, VERBATIM AND IN THE DATA VOICE. This is the classifier's
          own account of why the run stopped rather than carrying on, and the
          rule id is where somebody goes to change the policy. A paraphrase
          would be this window's opinion about a decision the engine made. */}
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

      <Panel label="What it is attached to">
        <Facts>
          <Fact
            label="Run"
            value={
              run === null ? (
                approval.runId
              ) : (
                <button type="button" className="os-work-link" onClick={() => onOpenRun(run.id)}>
                  {runTitle(run)}
                </button>
              )
            }
          />
          <Fact label="Step" value={approval.stepKey} mono />
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
            comparing two approvals of the same command needs the value. */}
        <Subhead>The exact thing you are deciding</Subhead>
        <CopyValue value={approval.artifactHash} label="artifact hash" />
        <Caption>
          Your decision is about this artifact only. If it changes before the run resumes, the
          decision is refused rather than carried over to the new one.
        </Caption>

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
          next="The run was told, and picked up from where it parked."
        />
      )}
    </>
  );
}
