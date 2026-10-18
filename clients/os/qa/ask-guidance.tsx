import { createRoot } from "react-dom/client";
import { AskSurface } from "../src/ask/AskSurface";
import { ConversationSession, type AskTurn, type ApprovalDiscussion } from "../src/ask/conversationSession";
import type { WorkGuidanceRequest, WorkGuidanceReceipt, ApprovalGuidanceReceipt } from "../src/ask/askController";
import { applyPackStyles } from "../src/themes/registry";
import "../src/styles/index.css";

// Synthetic source-only fixture. No live connection or model call.
const params = new URLSearchParams(location.search);
const progress = params.has("progress");
document.documentElement.dataset.theme = params.get("mode") === "light" ? "light" : "dark";
document.documentElement.dataset.osTheme = "graphite";
applyPackStyles([]);
let turns: AskTurn[] = Array.from({ length: params.get("many") ? 4 : 1 }, (_, index) => ({
  id: `turn-${index}`, runId: `run-${index}`, guidanceEpoch: `epoch-${index}`, canGuideWork: !progress,
  prompt: "Review the service report and prepare the requested changes.", workTitle: ["Review service report", "Check a longer report title with useful context", "Prepare the review", "Verify the result"][index],
  answer: "I’ll work through the report.", acknowledgement: "I’ll work through the report.", background: true, state: progress ? "waiting" : "queued", workStatus: progress ? "awaiting_approval" : "running", startedAt: new Date().toISOString(), activity: [],
}));
const scene = params.get("scene");
if (scene === "empty") turns = [];
if (scene === "pending" || scene === "quick") turns = [{ ...turns[0]!, prompt: "Hola", answer: scene === "quick" ? "¡Hola! ¿Cómo estás?" : "", acknowledgement: undefined, background: false, workload: "quick", state: scene === "quick" ? "done" : "queued", workStatus: scene === "quick" ? "succeeded" : "compiling" }];
if (scene === "error") turns = [{ ...turns[0]!, state: "error", error: "The model is unavailable. Try again when it is connected.", endedAt: new Date().toISOString() }];
if (scene === "format-error" || scene === "model-error") turns = [{ ...turns[0]!, prompt: "Create an image of the first bird in my saved list.", answer: "I’ll check the saved list and create its first bird.", acknowledgement: "I’ll check the saved list and create its first bird.", workTitle: "Create the bird image", state: "error", canGuideWork: false, workStatus: "failed", error: scene === "format-error" ? 'automation "workSpineDraft" execution failed: Spine refused: work compile: triage fileFormat: compose: unknown format "png" (offered: markdown, html, txt, csv, json, docx, pdf)' : "every_door_shut: no eligible fleet machine for this model", endedAt: new Date().toISOString() }];
let discussion: ApprovalDiscussion = { conversationId: "fixture", approval: { id: "progress", runId: "run-0", kind: "feedback", question: "This work reached its limit.", status: "pending" }, canSendGuidance: true, canAskAdvisor: true };
const transport = {
  ask() { throw new Error("Ordinary Ask is not connected in this fixture."); },
  conversations: { list: async () => [{ id: "fixture", title: "Service report" }], create: async () => ({ id: "new", title: "New" }), read: async () => turns },
  discussApproval: async () => discussion,
  approvalGuidance: async (_id: string, _feedback: string): Promise<ApprovalGuidanceReceipt> => {
    if (params.get("state") === "pending") return new Promise(() => {});
    if (params.get("state") === "error") throw new Error("The connection closed before guidance was confirmed.");
    discussion = { ...discussion, canSendGuidance: false };
    return { runResumed: true };
  },
  guideWork: async (request: WorkGuidanceRequest): Promise<WorkGuidanceReceipt> => {
    if (params.get("state") === "pending") return new Promise(() => {});
    if (params.get("state") === "error") throw new Error("The connection closed before guidance was confirmed.");
    const status = params.get("state") === "applied" ? "applied" : "queued";
    const turn: AskTurn = { id: request.requestId, guidanceTargetRunId: request.runId, guidanceStatus: status, prompt: request.prompt, answer: "", answerOnly: true, state: "done", startedAt: new Date().toISOString(), activity: [] };
    turns = [...turns, turn]; return { ...request, status, turn };
  },
};
const session = new ConversationSession(transport);
await session.select("fixture");
if (progress) {
  await session.discussApproval("progress");
  if (params.get("state") !== "saved") session.setDraft("Use the verified record and check the result before continuing.");
  if (["pending", "error"].includes(params.get("state") ?? "")) void session.sendGuidance();
} else if (params.get("state")) { session.beginWorkGuidance("turn-0"); session.setDraft("Please keep the review focused on the current report."); if (params.get("state") !== "target") session.send(session.getSnapshot().draft, null); }
createRoot(document.getElementById("root")!).render(<main style={{ margin: "20px auto", width: params.has("narrow") ? "min(100% - 24px, 360px)" : "min(100% - 24px, 720px)", height: "calc(100dvh - 40px)", border: "1px solid var(--os-line)", borderRadius: "var(--os-radius)", overflow: "hidden", background: "var(--os-glass-solid)" }}><AskSurface conversation={session} transport={transport} variant="sheet" availability={{ state: "ready", message: "", refresh() {} }} onOpenWork={() => {}} onOpenFleet={() => { document.title = "Fleet action selected"; }} /></main>);
