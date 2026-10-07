import type { Result } from "@znasllc-io/memql-sdk-core/client";
import { flatten } from "../../../kit/rows";
import { body, readTargets, type Target } from "./model";

export interface AssemblyPlan {
  name: string;
  components: { name: string; repository: string; runs: string[]; artifacts: { name: string; run: string; stepKey: string; path: string; kind: string; platform: string }[] }[];
  targets: Target[];
  compatibility: { component: string; requires: string; minVersion: string; maxExclusive: string }[];
}
export interface BuildRun { id: string; workRunId: string; repository: string; commit: string; title: string; finishedAt: string }
function object(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("The release assembly configuration is invalid.");
  return value as Record<string, unknown>;
}
function list(value: unknown, limit: number): unknown[] {
  if (!Array.isArray(value) || value.length > limit) throw new Error("The release assembly collection is invalid.");
  return value;
}
function text(value: unknown): string {
  if (typeof value !== "string" || !value) throw new Error("The release assembly record is incomplete.");
  return value;
}
export function readAssemblies(result: Result): AssemblyPlan[] {
  const config = body(result), targets = readTargets(result);
  const sources = list(config.sources ?? [], 64).map((entry) => { const s = object(entry); return { name: text(s.component), repository: text(s.repository) }; });
  return list(config.assemblies ?? [], 64).map((entry) => {
    const p = object(entry);
    const components = list(p.components, 64).map((entry) => {
      const c = object(entry), name = text(c.name), source = sources.find((s) => s.name === name);
      if (!source) throw new Error("A release component has no configured source.");
      return { name, repository: source.repository, runs: list(c.runs, 64).map(text), artifacts: list(c.artifacts, 128).map((entry) => {
        const a = object(entry);
        return { name: text(a.name), run: text(a.run), stepKey: text(a.stepKey), path: text(a.path), kind: text(a.kind), platform: text(a.platform) };
      }) };
    });
    const aliases = components.flatMap((c) => c.runs);
    if (!components.length || !aliases.length || aliases.length > 256 || new Set(aliases.map((a) => a.toLowerCase())).size !== aliases.length) throw new Error("The release plan has conflicting run inputs.");
    return { name: text(p.name), components, targets: list(p.targets, 1024).map((id) => {
      const target = targets.find((t) => t.targetId === text(id));
      if (!target) throw new Error("A release destination is no longer configured.");
      return target;
    }), compatibility: list(p.compatibility ?? [], 256).map((entry) => {
      const r = object(entry);
      return { component: text(r.component), requires: text(r.requires), minVersion: text(r.minVersion), maxExclusive: text(r.maxExclusive) };
    }) };
  });
}
export function readBuildRuns(result: Result): { runs: BuildRun[]; nextCursor: string } {
  // These rows help the person select inputs; the native assembler independently
  // verifies the work journal, source, receipts and bytes before creating a candidate.
  const runs = result.rows().map(flatten).filter((r) => r.status === "completed" && r.conclusion === "success" && r.mode === "full" && typeof r.workRunId === "string" && r.workRunId !== "").map((r) => {
    const commit = text(r.sha);
    if (!/^[a-f0-9]{40}$/.test(commit)) throw new Error("A completed run has no exact source commit.");
    return { id: text(r.id), workRunId: text(r.workRunId), repository: text(r.repository), commit, title: typeof r.title === "string" ? r.title : "", finishedAt: typeof r.finishedAt === "string" ? r.finishedAt : "" };
  });
  return { runs, nextCursor: result.meta()?.cursor ?? "" };
}
export function runFits(plan: AssemblyPlan, alias: string, run: BuildRun, selections: Record<string, BuildRun>): boolean {
  const component = plan.components.find((c) => c.runs.includes(alias));
  return !!component && run.repository === component.repository && component.runs.every((other) => other === alias || !selections[other] || (selections[other]!.commit === run.commit && selections[other]!.workRunId !== run.workRunId));
}
