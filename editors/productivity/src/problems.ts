import { buildLogsRecordClient } from "@znasllc-io/memql-sdk-core/client";
import type { ConnectionLease, EditorConnectionAPI } from "../../vscode/src/connection/api.js";

/** Only messages authored by this extension may be displayed verbatim. */
export class UserInputError extends Error {}
export interface Problem { message: string; reference?: string }
export class ReportedError extends Error {
  constructor(readonly problem: Problem) {
    super(problem.message + (problem.reference ? ` Reference: ${problem.reference}. Search it in MemQL OS Logs.` : ""));
  }
}

export function userMessage(error: unknown, operation: string): string {
  if (error instanceof UserInputError) return error.message;
  const detail = error instanceof Error ? error.message : String(error);
  if (/idle ceiling|idle.timeout|stopped producing output/i.test(detail)) return "The AI stopped responding. Try again.";
  if (/every_door_shut|speech recognition|transcription is not configured/i.test(detail)) return "No suitable AI model is available for this request. Check the models in Fleet, then try again.";
  if (/microphone.*(denied|permission)|permission.*microphone/i.test(detail) || /transcrib|dictat/i.test(operation) && /NotAllowedError|permission denied/i.test(error instanceof Error ? `${error.name}: ${detail}` : detail)) return "Microphone access is blocked. Allow it in your browser settings, then try dictating again.";
  if (/connection changed|another cluster|different cluster/i.test(detail)) return "The MemQL connection changed. Reconnect to the file’s cluster and compare with its latest version before saving.";
  if (/conflict|stale|revision.*changed|changed.*revision|version.*mismatch|expected.*version/i.test(detail)) return "This document changed elsewhere. Compare with the latest revision and review your edits before trying again.";
  if (/unauthenticated|sign.?in|session.*expired|\b401\b/i.test(detail)) return "Your session has ended. Sign in to MemQL, then try again.";
  if (/permission denied|forbidden|not authorized|\b403\b/i.test(detail)) return "You don’t have permission for this action. Ask the file owner for access.";
  if (/disconnected|connection.*closed|network|failed to fetch|ECONN|unavailable|deadline|timed?\s*out/i.test(detail)) return "MemQL couldn’t complete the request. Check your connection and try again.";
  return `Couldn’t ${operation}. Try again. If this continues, use the troubleshooting reference.`;
}

/** Credential material must not reach either the cluster log or local output. */
export function diagnosticText(error: unknown): string {
  const raw = error instanceof Error ? error.stack || error.message : String(error);
  const redacted = raw.replace(/\bBearer\s+[^\s"']+/gi, "Bearer [redacted]")
    .replace(/\bmql_[a-z]{3}_[A-Za-z0-9_-]+/g, "[redacted]")
    .replace(/\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/g, "[redacted]")
    .replace(/((?:token|password|secret|api[_-]?key|authorization)["']?\s*[=:]\s*["']?)[^\s&,;"']+/gi, "$1[redacted]");
  return new TextDecoder().decode(new TextEncoder().encode(redacted).slice(0, 1800));
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
