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
};

export function stepTitle(step: PresentableStep): string {
  const declared = sentence(step.displayTitle) || recordedText(step.input, ["title", "action", "summary"]);
  if (declared) return declared;
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
  const result = sentence(step.summary) || recordedText(step.result, ["summary", "message", "text", "content"]);
  if (result) return result;
  if (step.status === "failed") return "This action did not finish successfully. Open it to read the failure details.";
  if (step.status === "waiting") return "Waiting before continuing.";
  if (step.status === "running") return "This action is in progress.";
  if (step.status === "done") return "Completed. No written result was recorded.";
  if (step.status === "skipped") return "This action was skipped.";
  if (step.status === "cancelled") return "This action was stopped.";
  return "This action has not started.";
}

export function stepReason(step: PresentableStep): string {
  return sentence(step.reason) || recordedText(step.result, ["reason", "decisionReason"]);
}
