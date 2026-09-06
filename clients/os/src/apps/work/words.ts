// EVERY STATE WORD THIS APP SAYS COMES FROM HERE.
//
// The Deployables app's `words.ts` is the precedent and its reason holds
// exactly: the same run status is read by the list row, the run page's bar,
// the goal's run strip and the approval's "what this unblocks" line, and four
// copies of a vocabulary are four chances to call one thing two names. An
// owner walking the app found "Deployed", "Published" and "It is not serving
// yet" on one screen meaning one state, which is what produced that file.
//
// ===========================================================================
// THE THREE WORDS THAT ARE NOT THE ENUM'S OWN
// ===========================================================================
// `cancelled` reads "Stopped" and `abandoned` reads "Lost", and neither is a
// flavour of "Failed" -- the distinction is WHO DECIDED. Abandoned is a loss
// the cluster observed (its node died); cancelled is a choice somebody made;
// failed is work that broke. Collapsing the first two into "failed" reports a
// person's own click back to them as a fault, and sends somebody to debug a
// step that was fine. That reading is the deploy timeline's, adopted whole.
//
// `compiling` reads "Working it out", which is the one place this vocabulary
// leaves the enum entirely -- and it is the sentence the whole product is
// about. The system works a goal out ONCE and replays it afterwards, and
// "compiling" describes the mechanism to somebody who already knows it.

/** The goal's coarse lifecycle. */
export function goalStatusWord(status: string): string {
  switch (status) {
    case "open":
      return "Open";
    case "active":
      return "Working";
    case "closed":
      return "Closed";
    default:
      // NOT "Open". A status this build has no name for is a status this
      // build does not know, and guessing the commonest one puts a confident
      // word on a row nothing here understands.
      return status === "" ? "--" : status;
  }
}

export function goalStatusDetail(status: string): string {
  switch (status) {
    case "open":
      return "accepted, and no run has finished yet";
    case "active":
      return "a run is in flight";
    case "closed":
      return "you or the system closed it";
    default:
      return "";
  }
}

/** The run's own state. */
export function runStatusWord(status: string): string {
  switch (status) {
    case "compiling":
      return "Working it out";
    case "running":
      return "Running";
    case "waiting":
      return "Waiting";
    case "succeeded":
      return "Done";
    case "failed":
      return "Failed";
    case "cancelled":
      return "Stopped";
    case "abandoned":
      return "Lost";
    default:
      return status === "" ? "--" : status;
  }
}

/**
 * What that state MEANS, in one clause -- the ActionBar's `detail` slot.
 *
 * "Lost" is the one that has to say more than its word: the natural reading of
 * a stopped run is that it broke, and this one did not. Nothing failed and
 * nothing was published, exactly as the deploy timeline says of its own
 * abandoned runs.
 */
export function runStatusDetail(status: string): string {
  switch (status) {
    case "compiling":
      return "working out the steps -- it does this once, then replays them";
    case "running":
      return "";
    case "waiting":
      return "";
    case "succeeded":
      return "";
    case "failed":
      return "";
    case "cancelled":
      return "somebody asked it to stop; nothing was abandoned mid-step";
    case "abandoned":
      return "the node running it went away -- nothing failed, and it can be resumed";
    default:
      return "";
  }
}

/** What a waiting run is waiting on, in the person's terms. */
export function waitingWord(kind: string): string {
  switch (kind) {
    case "approval":
      return "Waiting for you to approve something";
    case "feedback":
      return "Waiting for you to answer a question";
    case "timer":
      return "Waiting for a time to come round";
    case "external":
      return "Waiting for something outside the cluster";
    case "subrun":
      return "Waiting for a run it started";
    default:
      return "Waiting";
  }
}

/** True when the thing this run waits on is a PERSON -- the app's one urgency. */
export function waitsOnAPerson(kind: string): boolean {
  return kind === "approval" || kind === "feedback";
}

/** How a run served its model calls. */
export function runModeWord(mode: string): string {
  switch (mode) {
    case "live":
      return "Live";
    case "replay":
      return "Replay";
    case "fork":
      return "Fork";
    default:
      return mode === "" ? "--" : mode;
  }
}

export function runModeDetail(mode: string): string {
  switch (mode) {
    case "live":
      return "reasoning steps called a model; deterministic steps did not";
    case "replay":
      return "every model call was served from the journal -- no provider was reached";
    case "fork":
      return "the shared prefix came from the journal; everything from the fork step ran live";
    default:
      return "";
  }
}

// ===========================================================================
// KIND -- the distinction this whole app exists to draw
// ===========================================================================
// The LABEL is the enum member, because that is what somebody greps for and
// what every other surface in this shell does with an enum. The MEANING is a
// separate sentence, because "deterministic" describes the mechanism and
// "ran without calling a model" describes the bill.
//
// "" IS ITS OWN ANSWER AND IS NEVER FOLDED INTO deterministic. Epic A1
// derives the kind for every step type except `function`, which stays empty
// until the A2 loader rule lands. A blank read as deterministic would put
// "no model was called" on a step that may well have called one -- which is
// the exact claim this surface is here to make, made without evidence.

export const STEP_KINDS = [
  "deterministic",
  "reasoning",
  "decision",
  "human",
  "loop",
  "subrun",
  "",
] as const;

export type StepKind = (typeof STEP_KINDS)[number];

export function stepKindWord(kind: string): string {
  switch (kind) {
    case "deterministic":
      return "Deterministic";
    case "reasoning":
      return "Reasoning";
    case "decision":
      return "Decision";
    case "human":
      return "Human";
    case "loop":
      return "Loop";
    case "subrun":
      return "Subrun";
    case "":
      return "Unclassified";
    default:
      return kind;
  }
}

export function stepKindMeaning(kind: string): string {
  switch (kind) {
    case "deterministic":
      return "ran without calling a model";
    case "reasoning":
      return "called a model";
    case "decision":
      return "a spec answered it";
    case "human":
      return "it stopped and asked you";
    case "loop":
      return "a bounded loop; its inner calls are in the journal";
    case "subrun":
      return "it opened a run of its own and waited";
    case "":
      return "this build cannot say whether a model was called";
    default:
      return "";
  }
}

/** Whether a model was called. Unclassified answers NEITHER, deliberately. */
export function kindCalledAModel(kind: string): boolean | null {
  if (kind === "reasoning" || kind === "loop") return true;
  if (kind === "deterministic" || kind === "decision" || kind === "subrun") return false;
  return null;
}

export function stepStatusWord(status: string): string {
  switch (status) {
    case "pending":
      return "Pending";
    case "ready":
      return "Ready";
    case "running":
      return "Running";
    case "waiting":
      return "Waiting";
    case "done":
      return "Done";
    case "failed":
      return "Failed";
    case "skipped":
      return "Skipped";
    case "cancelled":
      return "Stopped";
    default:
      return status === "" ? "--" : status;
  }
}

/**
 * The classifier's five answers, in the owner's own three questions: is this
 * temporary, does it need fixing, or does it need a person.
 */
export function symptomWord(symptom: string): string {
  switch (symptom) {
    case "transient":
      return "Temporary";
    case "environment":
      return "The environment moved";
    case "contract":
      return "A contract was broken";
    case "plan":
      return "The plan was wrong";
    case "human":
      return "Needs a person";
    default:
      return "";
  }
}

export function symptomMeaning(symptom: string): string {
  switch (symptom) {
    case "transient":
      return "a network error, a timeout or a rate limit -- it retries inside the run's budget";
    case "environment":
      return "a permission, a missing thing, or a literal that no longer holds here";
    case "contract":
      return "the postcondition did not hold, so the step is repaired from here rather than rerun";
    case "plan":
      return "the remaining steps are re-planned from here; the prefix is kept";
    case "human":
      return "it parked and asked you";
    default:
      return "";
  }
}

// ===========================================================================
// APPROVALS
// ===========================================================================

export function approvalKindWord(kind: string): string {
  switch (kind) {
    case "sideEffect":
      return "Side effect";
    case "scopeElevation":
      return "More access";
    case "budget":
      return "Budget";
    case "skillMint":
      return "New skill";
    case "feedback":
      return "Question";
    case "planReview":
      return "Plan change";
    default:
      return kind === "" ? "--" : kind;
  }
}

/** What deciding it actually does, said before the person decides. */
export function approvalKindMeaning(kind: string): string {
  switch (kind) {
    case "sideEffect":
      return "A step wants to do something outside the graph. Approving lets that one thing happen.";
    case "scopeElevation":
      return "A step wants more access than it standing has. Approving widens it for this run.";
    case "budget":
      return "The run reached one of its ceilings. Approving lets it carry on spending.";
    case "skillMint":
      return "The run wants to keep what it learned as a skill it can reuse.";
    case "feedback":
      return "It cannot decide this one on its own.";
    case "planReview":
      return "A repair was proposed. Nothing is edited without you seeing it first.";
    default:
      return "";
  }
}

export function decisionWord(decision: string): string {
  switch (decision) {
    case "approved":
      return "Approved";
    case "rejected":
      return "Rejected";
    case "answered":
      return "Answered";
    default:
      return "Waiting for you";
  }
}

/** The origin of a goal, in the person's terms rather than the enum's. */
export function originWord(origin: string): string {
  switch (origin) {
    case "user":
      return "You asked for this";
    case "responsibility":
      return "A standing responsibility";
    case "system":
      return "The platform started it";
    default:
      return origin === "" ? "--" : origin;
  }
}
