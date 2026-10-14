import { createRoot } from "react-dom/client";
import { Result } from "@znasllc-io/memql-sdk-core/client";
import { EvaluationsSection } from "../src/apps/training/EvaluationsSection";
import { SkillsSection } from "../src/apps/training/SkillsSection";
import { evaluationFixture } from "../test/training/evaluationFixture";
import { installQaConnection } from "./connectionShim";
import "../src/styles/index.css";

const params = new URLSearchParams(location.search);
const mode = params.get("mode") === "dark" ? "dark" : "light";
document.documentElement.setAttribute("data-theme", mode);
document.documentElement.setAttribute("data-os-theme", "graphite");
document.documentElement.style.colorScheme = mode;
const state = params.get("state") ?? "populated";
const fixture = await evaluationFixture();
const skill = { id: "software-development", name: "Software development", description: "Investigate, implement, verify and deliver changes while preserving unrelated work.", category: "engineering", reviewedBaseline: true, active: true, status: "active", version: 1, domainIds: ["software-development"], instructions: { body: "Reproduce the problem. Demonstrate a useful failing regression. Preserve unrelated edits, verify the combined tree and retain evidence before cleanup." } };
const reading = (value: Record<string, unknown>[]) => state === "error" ? Promise.reject(new Error("Synthetic connection failure")) : state === "loading" ? new Promise<Result>(() => {}) : Promise.resolve(new Result({ data: state === "empty" ? [] : value }));
installQaConnection({ query: {
  workGoalEvaluationsForOwner: () => reading([{ data: { goalEvaluation: fixture } }]),
  workSkillCatalog: () => Promise.resolve(new Result({ data: [{ skills: state === "empty" ? [] : [skill] }] })),
  workSkillSelectionsForOwner: () => reading([{ runId: "fixture-run", stepKey: "implement", createdAt: "2026-10-13T00:00:00Z", data: { skillSelection: { schemaVersion: 1, selected: [{ id: skill.id, text: skill.instructions.body, version: 1, digest: "a".repeat(64), reason: "matched-declared-terms", terms: ["software", "regression"] }], rejected: [] } } }]),
} });
const url = new URL(location.href); url.searchParams.delete("width");
createRoot(document.getElementById("root")!).render(params.get("width") === "mobile"
  ? <iframe title="Narrow Training preview" src={url.toString()} style={{ width: 360, height: "100dvh", border: 0 }} />
  : <main className="os-app-body" style={{ padding: 24, maxWidth: 1120, margin: "0 auto" }}><p>Preview · Synthetic records</p>{params.get("view") === "skills" ? <SkillsSection /> : <EvaluationsSection />}</main>);
