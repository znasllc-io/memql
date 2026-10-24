/** Plain descriptions from recorded fields. Never infer why an operation ran. */
export interface PresentableStep {
  key: string; callName: string; stepType: string; kind: string; status: string;
  displayTitle?: string; summary?: string; reason?: string;
  input?: Record<string, unknown> | null; result?: Record<string, unknown> | null;
  errorMessage?: string;
}

function sentence(value: unknown): string {
  return typeof value === "string" ? value.trim().replace(/\s+/g, " ").slice(0, 300) : "";
}

export function recordedText(record: Record<string, unknown> | null | undefined, keys: readonly string[]): string {
  for (const key of keys) { const value = sentence(record?.[key]); if (value) return value; }
  return "";
}

const actions: Record<string, string> = {
  runStep: "Run a pipeline step", runPipeline: "Run the repository workflow",
  executeCommand: "Run a command", runCommand: "Run a command", shell: "Run a command",
  readFile: "Read a file", writeFile: "Update a file", search: "Search for information",
  webSearch: "Search the web", compileGoal: "Plan the work", plan: "Plan the work",
  review: "Review the changes", test: "Check the changes", build: "Build the software",
  deploy: "Deploy the selected release", publish: "Publish the prepared result",
  appSession: "Work on the request", session: "Work on the request",
  workViewerContext: "Check your profile context", workReviewReply: "Prepare the final answer",
  workConversationReply: "Write the answer", workQuickReply: "Write the answer",
  ai: "Work on the request",
};

function object(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : null;
}

/** Journal results retain both their recorded value and their node envelope.
 * Read only these explicit wrappers; arbitrary nested metadata is not an outcome. */
function resultValues(step: PresentableStep): Record<string, unknown>[] {
  const result = step.result;
  if (!result) return [];
  const value = object(result.value);
  if (value) return [result, value];
  const nodes = object(result.result);
  const payloads = nodes ? Object.values(nodes).map(node => object(object(node)?.payload)).filter((value): value is Record<string, unknown> => value !== null) : [];
  return [result, ...payloads];
}

export function stepTitle(step: PresentableStep): string {
  const declared = sentence(step.displayTitle) || recordedText(step.input, ["title", "action", "summary"]);
  if (declared) return declared;
  const prompt = resultValues(step).map(result => recordedText(result, ["prompt"])).find(Boolean);
  if (prompt && actions[prompt]) return actions[prompt];
  if (step.key === "goalInput" && step.stepType === "expression") return "Read the request details";
  const call = (step.callName || step.key).split(/[.:/]/).filter(Boolean).at(-1) ?? "";
  if (actions[call]) return actions[call];
  const words = call.replace(/([a-z])([A-Z])/g, "$1 $2").replace(/[_-]+/g, " ").trim();
  if (/^[a-zA-Z][a-zA-Z ]{2,70}$/.test(words) && !/[a-f0-9]{16}/i.test(words)) return words[0]!.toUpperCase() + words.slice(1).toLowerCase();
  if (step.kind === "human") return "Review a decision";
  if (step.kind === "reasoning" || step.kind === "decision") return "Work out the next action";
  if (step.kind === "subrun") return "Run related work";
  if (step.kind === "loop") return "Work through the next items";
  return "Carry out a task";
}

export function stepOutcome(step: PresentableStep): string {
  const result = sentence(step.summary) || resultValues(step).map(value => recordedText(value, ["summary", "reply", "message", "text", "content"])).find(Boolean);
  if (result) return result;
  if (step.status === "done" && step.key === "goalInput" && step.stepType === "expression" && step.result?.valueRecorded === true) {
    const input = object(step.result.value);
    return input && Object.keys(input).length === 0 ? "No additional request details were supplied." : "The supplied request details are available to the next actions.";
  }
  if (step.status === "done" && step.callName === "workViewerContext" && resultValues(step).some(value => typeof value.checkedAt === "string" && typeof value.source === "string")) return "Your available profile and account context was checked.";
  if (step.status === "failed") return "This action did not finish successfully. Open it to read the failure details.";
  if (step.status === "waiting") return "Waiting before continuing.";
  if (step.status === "running") return "This action is in progress.";
  if (step.status === "done") return "Completed. No written summary is available.";
  if (step.status === "skipped") return "This action was skipped.";
  if (step.status === "cancelled") return "This action was stopped.";
  return "This action has not started.";
}

export function stepReason(step: PresentableStep): string {
  return sentence(step.reason) || resultValues(step).map(value => recordedText(value, ["reason", "decisionReason"])).find(Boolean) || "";
}
