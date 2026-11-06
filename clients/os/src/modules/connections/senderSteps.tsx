import type { ReactNode } from "react";

import { Field, Input } from "../../kit";
import type { Act } from "../../kit/ActionBar";
import type { Stop } from "../../kit/Rail";

// THE STEPS EVERY ADD A SENDER SHARES (DESIGN.md, "One wizard for adding").
//
//   [Organization] -> Address -> Display name -> Reply to (optional) -> Review
//
// Email's record-only wizard and an Azure domain's wizard ask the same
// questions in the same order with the same words. What differs is each
// wizard's own: whether there is an organization to choose, how the address is
// asked, what Review says beneath its sentence, and the act that adds. The rest
// -- the display name and reply steps, which steps can be opened, the floor's
// acts and the words on its left -- is here, once.
//
// One question a step. The forward act lives on the floor and nowhere else, and
// it is ABSENT until the step is answered: the words on the left say what is
// missing. No hooks: the wizards keep their own state and hand it in.

export type SenderStep = "organization" | "address" | "name" | "reply" | "review";

/** The sentence Review says, which is also what the sender will do. */
export function sendAs(fromName: string, address: string, replyTo: string): string {
  const base = `Send as ${fromName.trim()} <${address.trim()}>`;
  return replyTo.trim() === "" ? `${base}.` : `${base}, replies to ${replyTo.trim()}.`;
}

export interface SenderStepsInput {
  step: SenderStep;
  go: (step: SenderStep) => void;
  /** The organization step, when there is a choice; its state is worked out here. */
  organization?: Omit<Stop, "state"> | null;
  /** How the address is asked, the address it makes, and whether it is answered. */
  address: { body: ReactNode; value: string; answered: boolean; word: string };
  fromName: string;
  onFromName: (value: string) => void;
  replyTo: string;
  onReplyTo: (value: string) => void;
  replyPlaceholder: string;
  /** Review's line on what this step does, and what it says beneath its sentence. */
  reviewLine: string;
  review: ReactNode;
  /** The act that adds, at Review, when adding can be done now. */
  add: Act | null;
  /** The floor's words at Review. */
  reviewWord: string;
  onCancel: () => void;
  /** Keeps the fields' ids apart from another wizard's. */
  idPrefix: string;
}

export function senderSteps(input: SenderStepsInput): { steps: Stop[]; acts: Act[]; word: string; reached: (step: SenderStep) => boolean } {
  const { step, go, address, fromName, replyTo, idPrefix } = input;
  const named = fromName.trim() !== "";
  const reached = (candidate: SenderStep) =>
    candidate === "organization" ? !!input.organization
    : candidate === "address" ? true
    : candidate === "name" ? address.answered
    : address.answered && named;
  const order: SenderStep[] = ["organization", "address", "name", "reply", "review"];
  const stateOf = (candidate: SenderStep): Stop["state"] =>
    candidate === step ? "open" : reached(candidate) ? (order.indexOf(candidate) < order.indexOf(step) ? "done" : "waiting") : "ahead";

  const steps: Stop[] = input.organization ? [{ ...input.organization, state: stateOf("organization") }] : [];
  steps.push(
    {
      id: "address",
      name: "Address",
      state: stateOf("address"),
      sentence: "The address mail is sent from.",
      answer: address.answered ? address.value.trim() : undefined,
      body: address.body,
    },
    {
      id: "name",
      name: "Display name",
      state: stateOf("name"),
      sentence: "The name recipients see.",
      answer: named ? fromName.trim() : undefined,
      body: (
        <Field label="Display name">
          <Input id={`${idPrefix}-name`} label="Display name" value={fromName} placeholder="Acme News" onChange={input.onFromName} onEnter={() => { if (named) go("reply"); }} />
        </Field>
      ),
    },
    {
      id: "reply",
      name: "Reply to",
      state: stateOf("reply"),
      sentence: "Where replies go, if elsewhere.",
      answer: replyTo.trim() !== "" ? replyTo.trim() : step === "review" ? "Skipped" : undefined,
      body: (
        <Field label="Reply to">
          <Input id={`${idPrefix}-reply`} label="Reply to" value={replyTo} placeholder={input.replyPlaceholder} onChange={input.onReplyTo} onEnter={() => { if (replyTo.trim() !== "") go("review"); }} />
        </Field>
      ),
    },
    {
      id: "review",
      name: "Review",
      sentence: input.reviewLine,
      state: stateOf("review"),
      body: (
        <div className="os-app-stack">
          <p>{sendAs(fromName, address.value, replyTo)}</p>
          {input.review}
        </div>
      ),
    },
  );

  // THE WAY OUT FIRST AND QUIET, the forward act last. Nothing is written
  // before Review, so Cancel is simply going.
  const acts: Act[] = [{ label: "Cancel", text: true, onAct: input.onCancel }];
  if (step === "address" && address.answered) acts.push({ label: "Next", tone: "primary", onAct: () => go("name") });
  if (step === "name" && named) acts.push({ label: "Next", tone: "primary", onAct: () => go("reply") });
  if (step === "reply") {
    // Reply to is the one question that may go unanswered. Skip is the quiet
    // way past it and lets go of anything typed; Next keeps what was.
    acts.push(
      { label: "Skip", text: true, onAct: () => { input.onReplyTo(""); go("review"); } },
      { label: "Next", tone: "primary", onAct: () => go("review") },
    );
  }
  if (step === "review" && input.add) acts.push(input.add);

  const word =
    step === "organization" ? "Choose the organization"
    : step === "address" ? address.word
    : step === "name" ? "Enter the display name"
    : step === "reply" ? "Reply to is optional"
    : input.reviewWord;

  return { steps, acts, word, reached };
}
