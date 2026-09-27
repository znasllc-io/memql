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

/**
 * What a waiting run is waiting on, in the person's terms.
 *
 * THE THREE THE FAILURE PATH WRITES HAD NO WORDS HERE, and the gap was the
 * whole of a bug report. A run that failed, got classified and parked itself
 * on a retry, a replan or a repair fell to the default and read "Waiting" --
 * the same word as a run waiting on a timer, with nothing saying a failure had
 * happened, that the system had decided what to do about it, or that it was
 * about to do it on its own. Somebody watched one of those for a long time
 * believing work was in flight. Each of the three now says what happened and
 * what is about to happen, and `waitsOnAPerson` still answers false for them
 * because none of them is a question for anybody.
 */
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
    case "retry":
      return "A step failed and it is going to try that step again";
    case "replan":
      return "A step failed and the rest of the plan is being worked out again";
    case "repair":
      return "A step did something other than what it promised, and it is being redone";
    default:
      return "Waiting";
  }
}

/** True when the thing this run waits on is a PERSON -- the app's one urgency. */
export function waitsOnAPerson(kind: string): boolean {
  return kind === "approval" || kind === "feedback";
}

/**
 * How a run served its model calls.
 *
 * `fork` READS "Branch" (epic memql#5414). The enum member stayed and the act
 * that makes one is "Branch from here", and an act keeps its name through the
 * whole flow -- so the run it opens is called what the act said it would be.
 */
export function runModeWord(mode: string): string {
  switch (mode) {
    case "live":
      return "Live";
    case "replay":
      return "Replay";
    case "fork":
      return "Branch";
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
      return "the steps before the branch point were reused from the run it came from; from there on it ran live";
    default:
      return "";
  }
}

// ===========================================================================
// INTERVENTION -- re-run, branch and go back (epic memql#5414)
// ===========================================================================

/** The three levels a person may ask a step for. Embeddings is never offered (D10). */
export const OVERRIDE_LEVELS = ["fast", "strong", "reasoning"] as const;

export function levelWord(level: string): string {
  switch (level) {
    case "fast":
      return "Fast";
    case "strong":
      return "Strong";
    case "reasoning":
      return "Reasoning";
    case "embeddings":
      return "Embeddings";
    default:
      return level;
  }
}

/** The five efforts an app can be asked for. */
export const OVERRIDE_EFFORTS = ["low", "medium", "high", "xhigh", "max"] as const;

export function effortWord(effort: string): string {
  switch (effort) {
    case "low":
      return "Low";
    case "medium":
      return "Medium";
    case "high":
      return "High";
    case "xhigh":
      return "Extra high";
    case "max":
      return "Max";
    default:
      return effort;
  }
}

/**
 * What a re-run in flight is doing, in the words of the act that started it.
 *
 * THE ACT KEEPS ITS NAME: "Run again" becomes "running draft again as version
 * 3" and then the version appears. A head move with nothing stale never lands
 * here -- nothing runs -- so "going back" always has steps behind it.
 */
export function rerunInFlightWords(reason: string, stepKey: string, version: number | null): string {
  const step = stepKey === "" ? "a step" : stepKey;
  switch (reason) {
    case "headMove":
      return `went back in ${step}; running the steps that depend on it again`;
    case "branch":
      return `running from ${step} on; the steps before it are reused`;
    default:
      return version === null ? `running ${step} again` : `running ${step} again as version ${version}`;
  }
}

// ===========================================================================
// FEEDBACK -- the AI Fluency framework's Discernment, in a person's words
// ===========================================================================
// The framework names three things to judge in anything an AI made: its
// PRODUCT, its PROCESS and its PERFORMANCE. Those are the wire's field names
// and the framework's own terms, and they are not what a person calls them:
// "The result", "The approach" and "The behaviour" are. The framework's names
// live in the explanation beside the question, where somebody who knows it
// can find it.

export type Verdict = "like" | "dislike" | "neutral";

export const VERDICTS: readonly Verdict[] = ["like", "dislike", "neutral"];

export function verdictWord(verdict: string): string {
  switch (verdict) {
    case "like":
      return "Like";
    case "dislike":
      return "Dislike";
    case "neutral":
      return "Neutral";
    default:
      return verdict;
  }
}

export type Axis = "product" | "process" | "performance";

export const AXES: readonly Axis[] = ["product", "process", "performance"];

/** The pill's own label. */
export function axisWord(axis: Axis): string {
  switch (axis) {
    case "product":
      return "The result";
    case "process":
      return "The approach";
    case "performance":
      return "The behaviour";
  }
}

/**
 * The axes as a phrase inside a sentence: "the approach", "the result and the
 * approach", "the result, the approach and the behaviour". Empty for none, so
 * a caller can say "a problem" instead.
 */
export function axesPhrase(axes: { product: boolean; process: boolean; performance: boolean }): string {
  const named = AXES.filter((axis) => axes[axis]).map((axis) => axisWord(axis).toLowerCase());
  if (named.length <= 1) return named[0] ?? "";
  return `${named.slice(0, -1).join(", ")} and ${named[named.length - 1]}`;
}

// ===========================================================================
// REUSE -- what the evidence, or the person, says an automation is for (D24)
// ===========================================================================

export type ReuseLabel = "reusable" | "goalSpecific" | "accountSpecific";

export function reuseWord(label: string): string {
  switch (label) {
    case "reusable":
      return "Reusable";
    case "goalSpecific":
      return "For one goal";
    case "accountSpecific":
      return "For one account";
    default:
      return "Not yet labelled";
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

/**
 * Whether a model was called. Unclassified answers NEITHER, deliberately.
 *
 * RE-EXPORTED FROM THE SCENE LIBRARY rather than defined here (epic
 * memql#4785). The rail's ink weight, the road's weight on the map and the
 * receipt's count are three surfaces reading one rule, and for a moment they
 * did not agree -- the map counted `decision` as thinking and the rail did
 * not. The leaf owns it now; this line is here so the app's own import sites
 * did not all have to move.
 */
export { kindCalledAModel } from "../../nexus/scene/world";

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
    case "inferenceUnavailable":
      return "No model";
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
      // TWO THINGS RAISE THIS KIND and the sentence has to be true of both.
      // One is the run crossing a ceiling the goal declared, where approving
      // raises it. The other is the money itself running out at the provider,
      // where approving raises nothing and somebody has to top it up first --
      // so the old sentence, "approving lets it carry on spending", promised
      // a button that does not exist in that half of the cases. What is true
      // of both is that the run stopped over money and is not retrying.
      return "The run stopped because paid model calls are no longer available to it -- a ceiling it declared, or the balance behind them. It does not retry against that on its own; the reason below says which.";
    case "skillMint":
      return "The run wants to keep what it learned as a skill it can reuse.";
    case "feedback":
      return "It cannot decide this one on its own.";
    case "planReview":
      return "A repair was proposed. Nothing is edited without you seeing it first.";
    case "inferenceUnavailable":
      // THE ONE KIND RAISED BY A CONDITION RATHER THAN A DECISION (epic
      // memql#5096). Nobody has to act for it to clear -- a laptop opening or
      // somebody signing into Claude Code changes the answer -- so the
      // sentence says so, or a person reads it as a gate only they can lift
      // and stops using their own machines.
      return "No door to a model was open, so the run stopped where it was. It starts again on its own when one opens; approving uses a paid provider now instead.";
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
