import { useEffect, useRef, useState, type ReactNode } from "react";
import { ArrowRightLeft } from "lucide-react";

import { Caption, ContentSkeleton, Fact, Facts, Field, Notice, Subhead } from "../../kit";
import type { Act } from "../../kit/ActionBar";
import type { Breadcrumb } from "../../kit/Breadcrumbs";
import type { Stop } from "../../kit/Rail";
import { Wizard } from "../../kit/Wizard";
import { AccountPicker } from "./AccountPicker";
import { rowsPhrase } from "./inventory";
import { kindName, planMoves, refusedByKind, refusalSentence, type MovePlan, type RowRef } from "./moves";
import { accountIsArchived, accountName, type AccountRow } from "./rows";
import { useMoveCalls } from "./useAccounts";

// "MOVE ROWS": the kit wizard over one act, `accountRowsMove` (design record
// 2026-11-12-account-export, sections I and K).
//
//   Destination   the account picker, the account the rows are in absent
//   Review        the PREVIEW call: what moves, what follows, and what is
//                 refused, per kind and in words. Writes nothing.
//   Move          the cluster's turn: the same call, for real
//   Done          what moved. What hangs off it follows in a background pass
//                 after the call returns: the kinds are named, never counted,
//                 and not waited for.
//
// ONE QUESTION A STEP, and the only question is where: choosing the account
// answers Destination and opens Review (DESIGN.md, One wizard for adding).
//
// A REFUSAL STAYS ON SCREEN in the engine's words, with Try again on the
// floor, whichever call it came from. A failed move writes nothing the person
// has to undo, so the way out is Cancel either way.

export interface MoveRowsWizardProps {
  /** The rows chosen. */
  rows: readonly RowRef[];
  /** Where they are now; empty for rows in no account. Not offered as a destination. */
  fromAccountId: string;
  /** Every account the app holds, for the picker. */
  accounts: readonly AccountRow[];
  /** Accounts offered first, in this order: a conflict's candidates. */
  candidates?: readonly string[];
  /** The control's own words: "Move rows", "Place in an account". */
  title: string;
  /** What is being moved, in a few words: "3 rows of artifact", a row's id. */
  subject: string;
  breadcrumbs?: readonly Breadcrumb[];
  back: { label: string; onSelect: () => void };
  /** The move returned, with what it did: the pages behind this one read again. */
  onMoved: (plan: MovePlan) => void;
  /** Leave the wizard: Cancel before the move, Done after it. */
  onClose: () => void;
}

type StepId = "destination" | "review" | "move" | "done";
const ORDER: readonly StepId[] = ["destination", "review", "move", "done"];

const NAMES: Record<StepId, string> = { destination: "Destination", review: "Review", move: "Move", done: "Done" };
const SUBTITLES: Record<StepId, string> = {
  destination: "The account they go to",
  review: "What moves, and what follows",
  move: "The cluster moves them",
  done: "What moved",
};

interface Call {
  state: "idle" | "running" | "done" | "failed";
  plan: MovePlan | null;
  error: string;
}

const IDLE: Call = { state: "idle", plan: null, error: "" };

export function MoveRowsWizard({
  rows,
  fromAccountId,
  accounts,
  candidates = [],
  title,
  subject,
  breadcrumbs,
  back,
  onMoved,
  onClose,
}: MoveRowsWizardProps) {
  const calls = useMoveCalls();
  const [to, setTo] = useState("");
  const [step, setStep] = useState<StepId>("destination");
  const [preview, setPreview] = useState<Call>(IDLE);
  const [move, setMove] = useState<Call>(IDLE);
  const [previewAttempt, setPreviewAttempt] = useState(0);
  // A move is asked once at a time, whatever the bar draws.
  const moving = useRef(false);

  // THE DESTINATIONS: every account but the one the rows are in, archived
  // ones left out (nothing should be filed with a former client by accident),
  // and a conflict's candidates first, in the engine's order.
  const offered = accounts.filter((a) => a.id !== fromAccountId && !accountIsArchived(a));
  const first = candidates.map((id) => offered.find((a) => a.id === id)).filter((a): a is AccountRow => a !== undefined);
  const ordered = [...first, ...offered.filter((a) => !first.includes(a))];
  const destination = offered.find((a) => a.id === to) ?? null;
  const destinationName = destination ? accountName(destination) : "";

  // THE PREVIEW, asked whenever the destination changes or Try again is
  // pressed. It writes nothing, so asking again is always safe.
  useEffect(() => {
    if (calls === null || to === "") return;
    let alive = true;
    setPreview({ state: "running", plan: null, error: "" });
    calls.preview(rows, to).then(
      (plan) => { if (alive) setPreview({ state: "done", plan, error: "" }); },
      (err: unknown) => { if (alive) setPreview({ state: "failed", plan: null, error: err instanceof Error ? err.message : String(err) }); },
    );
    return () => { alive = false; };
  }, [calls, rows, to, previewAttempt]);

  function choose(id: string) {
    if (move.state === "running" || move.state === "done") return;
    setTo(id);
    if (id !== "") setStep("review");
  }

  async function runMove() {
    if (calls === null || to === "" || moving.current) return;
    moving.current = true;
    setStep("move");
    setMove({ state: "running", plan: null, error: "" });
    try {
      const plan = await calls.move(rows, to);
      setMove({ state: "done", plan, error: "" });
      setStep("done");
      onMoved(plan);
    } catch (err: unknown) {
      setMove({ state: "failed", plan: null, error: err instanceof Error ? err.message : String(err) });
    } finally {
      moving.current = false;
    }
  }

  const moved = move.state === "done";
  const movable = preview.plan !== null && planMoves(preview.plan) > 0;

  const reached: Record<StepId, boolean> = {
    destination: !moved,
    review: to !== "" && !moved,
    move: move.state !== "idle",
    done: moved,
  };
  const stateOf = (id: StepId): Stop["state"] => {
    if (id === step) return id === "move" && move.state === "running" ? "current" : "open";
    if (!reached[id]) return ORDER.indexOf(id) < ORDER.indexOf(step) ? "done" : "ahead";
    return ORDER.indexOf(id) < ORDER.indexOf(step) ? "done" : "waiting";
  };

  const bodies: Record<StepId, ReactNode> = {
    destination: (
      <div className="os-app-stack">
        <Caption>{subject}</Caption>
        <Field label="Account">
          <AccountPicker
            id="os-account-move-destination"
            label="Destination account"
            value={to}
            onChange={choose}
            accounts={ordered}
            required
            requiredLabel="Choose an account"
          />
        </Field>
        {first.length === 0 ? null : (
          <Caption>Suggested first: {first.map(accountName).join(", ")}.</Caption>
        )}
      </div>
    ),
    review:
      preview.state === "running" || (preview.state === "idle" && to !== "") ? (
        <ContentSkeleton kind="detail" label="Checking what moves" />
      ) : preview.plan === null ? null : (
        <PlanFacts plan={preview.plan} verb="would" />
      ),
    move: move.state === "failed" ? null : <Caption>Moving {subject} to {destinationName}.</Caption>,
    done:
      move.plan === null ? null : (
        <div className="os-app-stack">
          <PlanFacts plan={move.plan} verb="did" />
        </div>
      ),
  };

  const steps: Stop[] = ORDER.map((id) => ({
    id,
    name: NAMES[id],
    sentence: SUBTITLES[id],
    answer: id === "destination" ? destinationName : undefined,
    state: stateOf(id),
    body: bodies[id],
  }));

  let status: { word: string; detail?: string; tone?: "busy" | "none" };
  const acts: Act[] = [];
  const cancel: Act = { label: "Cancel", text: true, onAct: onClose };
  if (moved) {
    status = { word: "Moved" };
    acts.push({ label: "Done", tone: "primary", onAct: onClose });
  } else if (move.state === "running") {
    status = { word: "Moving rows", tone: "busy" };
  } else if (move.state === "failed") {
    status = { word: "Not moved", detail: "Nothing was moved." };
    acts.push(cancel, { label: "Try again", tone: "primary", onAct: () => void runMove() });
  } else if (step === "destination" || to === "") {
    status = { word: "Choose an account" };
    acts.push(cancel);
  } else if (preview.state === "failed") {
    status = { word: "Not checked" };
    acts.push(cancel, { label: "Try again", tone: "primary", onAct: () => setPreviewAttempt((n) => n + 1) });
  } else if (preview.state !== "done") {
    status = { word: "Checking what moves", tone: "busy" };
    acts.push(cancel);
  } else if (!movable) {
    status = { word: "Nothing can move", detail: "Choose another account, or leave these rows where they are." };
    acts.push(cancel);
  } else {
    status = { word: "Ready to move", detail: `${rowsPhrase(planMoves(preview.plan!))} to ${destinationName}` };
    acts.push(cancel, { label: "Move", tone: "primary", onAct: () => void runMove() });
  }

  // NOT LEFT WHILE THE WRITE IS IN FLIGHT. The floor offers no way out then,
  // and neither do the trail's crumbs or Back: leaving would hide a move that
  // still lands, and its answer would have nowhere to be read.
  const leave = () => {
    if (move.state !== "running") back.onSelect();
  };
  const guardedCrumbs = breadcrumbs?.map((crumb) =>
    crumb.onSelect === undefined ? crumb : { ...crumb, onSelect: () => { if (move.state !== "running") crumb.onSelect!(); } },
  );

  const notices =
    move.state === "failed" ? (
      <Notice tone="error" sentence="The rows were not moved." next="Nothing was moved." detail={move.error} />
    ) : step === "review" && preview.state === "failed" ? (
      <Notice tone="error" sentence="What would move was not checked." detail={preview.error} />
    ) : null;

  return (
    <Wizard
      icon={<ArrowRightLeft size={20} aria-hidden />}
      title={title}
      label={title}
      breadcrumbs={guardedCrumbs}
      back={{ label: back.label, onSelect: leave }}
      steps={steps}
      open={step}
      onOpen={(id) => {
        const next = id as StepId;
        if (reached[next] && move.state !== "running") setStep(next);
      }}
      status={status}
      acts={acts}
      notices={notices}
    />
  );
}

/**
 * What a move moves, what follows it and what it refuses, per kind, in words.
 * `verb` is the tense: a preview says what would happen, Done what did.
 */
function PlanFacts({ plan, verb }: { plan: MovePlan; verb: "would" | "did" }) {
  const refused = refusedByKind(plan);
  return (
    <div className="os-app-stack">
      {plan.moves.length === 0 ? (
        <Caption>{verb === "would" ? "None of these rows can move to this account." : "No rows moved."}</Caption>
      ) : (
        <>
          <Subhead>{verb === "would" ? "Moves" : "Moved"}</Subhead>
          <Facts>
            {plan.moves.map((m) => (
              <Fact key={m.concept} label={kindName(m.concept)} value={rowsPhrase(m.rows)} />
            ))}
          </Facts>
        </>
      )}
      {/* WHAT FOLLOWS IS NAMED, NEVER COUNTED: a background pass carries it
          after the move returns, so no number here would be true. */}
      {plan.follows.length === 0 ? null : (
        <>
          <Caption>
            {verb === "would"
              ? "What hangs off these rows follows in the background, and an export waits until it has."
              : "What hangs off the rows that moved follows in the background, and an export waits until it has."}
          </Caption>
          <Caption>Kinds that can follow: {[...new Set(plan.follows.map((f) => kindName(f.concept)))].join(", ")}</Caption>
        </>
      )}
      {refused.length === 0 ? null : (
        <>
          <Subhead>{verb === "would" ? "Stays where it is" : "Stayed where it was"}</Subhead>
          <Facts>
            {refused.map((r) => (
              <Fact
                key={`${r.concept} ${r.reason}`}
                label={kindName(r.concept)}
                value={`${rowsPhrase(r.rows)}. ${refusalSentence(r.reason)}`}
              />
            ))}
          </Facts>
        </>
      )}
    </div>
  );
}

/** "3 rows of artifact": the chosen rows, named by their kinds. */
export function rowsSubject(rows: readonly RowRef[]): string {
  const kinds = [...new Set(rows.map((r) => kindName(r.concept)))];
  return `${rowsPhrase(rows.length)} of ${kinds.join(", ")}`;
}
