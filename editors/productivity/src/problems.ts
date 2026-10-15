import { buildLogsRecordClient, refusalSentence } from "@znasllc-io/memql-sdk-core/client";
import type { ConnectionLease, EditorConnectionAPI } from "../../vscode/src/connection/api.js";
import { UserInputError } from "./userError.js";

export { UserInputError } from "./userError.js";
export interface Problem { message: string; reference?: string }
/**
 * The last sentence of a message the person cannot fix themselves: the problem toast's one button, Show details, reveals the
 * output that holds the reference. A notification VS Code draws itself (a failed save, an editor that would not open) has no
 * such button, so a ReportedError leaves the sentence out and carries the reference in the message instead.
 */
const SHOW_DETAILS_HINT = / If [^.,]+, choose Show details\.$/;
export class ReportedError extends Error {
  constructor(readonly problem: Problem) {
    super(problem.message.replace(SHOW_DETAILS_HINT, "") + (problem.reference ? ` Reference: ${problem.reference}. Search it in MemQL OS Logs.` : ""));
  }
}

export function userMessage(error: unknown, operation: string): string {
  if (error instanceof UserInputError) return error.message;
  const detail = error instanceof Error ? error.message : String(error);
  if (/stop (?:the current|active document) review/i.test(detail)) return "This feedback is still in use by a review. Stop that review first, or wait for approved changes to finish, then try deleting it again.";
  if (/feedback in this proposal was deleted/i.test(detail)) return "A request in this review was deleted. Propose changes again with the remaining requests.";
  if (/the review changed/i.test(detail)) return "The review changed in another window. Reopen it before deleting this request.";
  if (/modelCalls ceiling|model-call cap/i.test(detail)) return "This attempt reached its AI work limit. Your feedback is saved. Try a smaller set of changes.";
  if (/context checkpoint|context.*(?:window|budget).*exceed/i.test(detail)) return "MemQL couldn’t make enough room to continue this request. Your work is saved. If retrying doesn’t help, choose Show details.";
  if (/idle ceiling|idle.timeout|stopped producing output/i.test(detail)) return "The AI stopped responding. Try again.";
  if (/(?:model|local runtime).*?(?:timeout|timed? out|deadline)|model_call_timeout/i.test(detail)) return "The AI couldn’t finish within the time allowed. Try a smaller request, or choose another model in Fleet.";
  if (/every_door_shut|speech recognition|transcription is not configured/i.test(detail)) return "No suitable AI model is available for this request. Check the models in Fleet, then try again.";
  if (/microphone.*(denied|permission)|permission.*microphone/i.test(detail) || /transcrib|dictat/i.test(operation) && /NotAllowedError|permission denied/i.test(error instanceof Error ? `${error.name}: ${detail}` : detail)) return "Microphone access is blocked. Allow it in your browser settings, then try dictating again.";
  if (/connection changed|another cluster|different cluster/i.test(detail)) return "The MemQL connection changed. Reconnect to the file’s cluster and compare with its latest version before saving.";
  // A call the cluster refused says why in its own words: those are the person's, never "Couldn’t <operation>".
  const refused = clusterSentence(error);
  if (refused !== undefined) return refused;
  if (/conflict|stale|revision.*changed|changed.*revision|version.*mismatch|expected.*version/i.test(detail)) return "This document changed elsewhere. Compare with the latest revision and review your edits before trying again.";
  if (/unauthenticated|sign.?in|session.*expired|\b401\b/i.test(detail)) return "Your session has ended. Sign in to MemQL, then try again.";
  if (/permission denied|forbidden|not authorized|\b403\b/i.test(detail)) return "You don’t have permission for this action. Ask the file owner for access.";
  if (/disconnected|connection.*closed|network|failed to fetch|ECONN|unavailable|deadline|timed?\s*out/i.test(detail)) return "MemQL couldn’t complete the request. Check your connection and try again.";
  return `Couldn’t ${operation}. Try again. If this continues, choose Show details.`;
}

/**
 * How the server frames an engine refusal ("MemQL engine failed to execute query. Details: ", buildEngineErrorMessage in
 * component/grpc/server.go), with the call's name in front as executeNamed puts it or without, and something inside it.
 * Builtins and mutations word their refusals for a person ("Someone else is saving this template. Try again in a moment."),
 * and almost every one arrives in this frame.
 */
const ENGINE_REFUSAL = /^(?:[A-Za-z_][A-Za-z0-9_]*: )?MemQL engine failed to execute query\. Details: \S/;

/**
 * The cluster's sentence for a refused call, found in the error or in what caused it (the SDK puts a refusal in words and keeps
 * the framed original as the cause), or undefined when neither is one. It is the SDK's refusalSentence: the call's name, the
 * engine's frame and a Go package name taken off. Credential material is redacted, and a sentence a Go error opens in lower
 * case starts with a capital.
 */
function clusterSentence(error: unknown): string | undefined {
  let current: unknown = error;
  for (let depth = 0; depth < 5 && current instanceof Error; depth++) {
    if (ENGINE_REFUSAL.test(current.message.trim())) {
      const sentence = redact(refusalSentence(current)).trim();
      return sentence === "" ? undefined : sentence.replace(/^[a-z]/, letter => letter.toUpperCase());
    }
    current = current.cause;
  }
  return undefined;
}

// The error with what caused it. The SDK hands a person a refusal as one sentence
// and keeps the refusal as it arrived -- the call's name, the engine's frame -- as
// the error's cause, and a diagnostic is where those belong. The causes go straight
// after the first line, ahead of the stack frames, so the length cap below cannot
// cut them off.
function withCauses(error: unknown): string {
  if (!(error instanceof Error)) return String(error);
  // WebKit stacks can contain only frames (or just "@"). Always retain the
  // error's message before those frames, as Chromium/Node normally do.
  const heading = `${error.name || "Error"}: ${error.message}`;
  const stack = error.stack?.startsWith(heading) ? error.stack : `${heading}${error.stack ? `\n${error.stack}` : ""}`;
  const causes: string[] = [];
  let cause: unknown = error.cause;
  for (let depth = 0; cause !== undefined && cause !== null && depth < 4; depth++) {
    causes.push(`Caused by: ${cause instanceof Error ? cause.message : String(cause)}`);
    cause = cause instanceof Error ? cause.cause : undefined;
  }
  if (causes.length === 0) return stack;
  const lineEnd = stack.indexOf("\n");
  return lineEnd < 0 ? [stack, ...causes].join("\n") : [stack.slice(0, lineEnd), ...causes, stack.slice(lineEnd + 1)].join("\n");
}

/** `text` with credential material taken out: a bearer token, a MemQL token, a JWT, a secret written as key=value. */
function redact(text: string): string {
  return text.replace(/\bBearer\s+[^\s"']+/gi, "Bearer [redacted]")
    .replace(/\bmql_[a-z]{3}_[A-Za-z0-9_-]+/g, "[redacted]")
    .replace(/\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/g, "[redacted]")
    .replace(/((?:token|password|secret|api[_-]?key|authorization)["']?\s*[=:]\s*["']?)[^\s&,;"']+/gi, "$1[redacted]");
}

/** Credential material must not reach either the cluster log or local output. */
export function diagnosticText(error: unknown): string {
  return new TextDecoder().decode(new TextEncoder().encode(redact(withCauses(error))).slice(0, 1800));
}

/** A bounded, lease-scoped deduplication cache: polling never floods Logs. */
export class ProblemReporter {
  private readonly session = `tools-${globalThis.crypto.randomUUID()}`;
  private readonly recent = new Map<string, { at: number; problem: Problem }>();
  private pending = 0;
  private disposed = false;
  dispose(): void { this.disposed = true; this.recent.clear(); }
  private write(line: string): void { if (!this.disposed) { try { this.output(line); } catch { /* Logging cannot cause another failure. */ } } }
  constructor(private readonly api: EditorConnectionAPI, private readonly output: (line: string) => void) {}
  report(error: unknown, operation: string, lease = this.api.current(), reference?: string): Problem {
    if (error instanceof ReportedError) return error.problem;
    if (error instanceof UserInputError) return { message: error.message };
    const technical = diagnosticText(error);
    const key = JSON.stringify([lease, operation, reference, technical]);
    const previous = this.recent.get(key);
    if (previous && (reference || Date.now() - previous.at < 60_000)) return previous.problem;
    const problem = { message: userMessage(error, operation), reference: reference || `tools-${globalThis.crypto.randomUUID()}` };
    this.recent.delete(key);
    this.recent.set(key, { at: Date.now(), problem });
    if (this.recent.size > 100) this.recent.delete(this.recent.keys().next().value!);
    const message = `[${problem.reference}] ${operation}: ${technical}`;
    this.write(`${new Date().toISOString()} ${message}`);
    // Logging is best effort and must never block, retry the failed operation,
    // recursively report a logging failure, or accumulate unbounded sends.
    if (!this.disposed && lease && this.pending < 8) {
      this.pending++;
      void Promise.resolve().then(() => this.api.execute(lease, "logsRecordClient", buildLogsRecordClient({
        session: this.session,
        lines: [{ at: new Date().toISOString(), level: "error", component: "editor.productivity", message,
          attributes: { reference: problem.reference, operation, source: "vscode-tools" } }],
      }))).then(rows => {
        if (rows[0]?.accepted !== 1) this.write(`[${problem.reference}] Cluster logging unavailable; technical details are retained in this output.`);
      }, () => this.write(`[${problem.reference}] Cluster logging unavailable; technical details are retained in this output.`))
        .finally(() => { this.pending--; });
    }
    return problem;
  }
}

let reporter: ProblemReporter | undefined;
export function configureProblems(value: ProblemReporter): void { reporter = value; }
export function reportProblem(error: unknown, operation: string, lease?: ConnectionLease, reference?: string): Problem {
  if (error instanceof ReportedError) return error.problem;
  return reporter?.report(error, operation, lease, reference) ?? { message: userMessage(error, operation) };
}
export async function userOperation<T>(operation: string, act: () => Promise<T>): Promise<T> {
  try { return await act(); }
  catch (error) { throw new ReportedError(reportProblem(error, operation)); }
}
