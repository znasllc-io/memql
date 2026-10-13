import { createRoot } from "react-dom/client";
import { AskSurface } from "../src/ask/AskSurface";
import { ConversationSession, type AskTurn } from "../src/ask/conversationSession";
import type { WorkGuidanceRequest, WorkGuidanceReceipt } from "../src/ask/askController";
import { applyPackStyles } from "../src/themes/registry";
import "../src/styles/index.css";

// Synthetic source-only fixture. No live connection or model call.
const params = new URLSearchParams(location.search);
document.documentElement.dataset.theme = params.get("mode") === "light" ? "light" : "dark";
document.documentElement.dataset.osTheme = "graphite";
applyPackStyles([]);
let turns: AskTurn[] = Array.from({ length: params.get("many") ? 4 : 1 }, (_, index) => ({
  id: `turn-${index}`, runId: `run-${index}`, guidanceEpoch: `epoch-${index}`, canGuideWork: true,
  prompt: "Review the service report and prepare the requested changes.", workTitle: ["Review service report", "Check a longer report title with useful context", "Prepare the review", "Verify the result"][index],
  answer: "I’ll work through the report.", acknowledgement: "I’ll work through the report.", background: true, state: "queued", workStatus: "running", startedAt: new Date().toISOString(), activity: [],
}));
const transport = {
  ask() { throw new Error("Ordinary Ask is not connected in this fixture."); },
  conversations: { list: async () => [{ id: "fixture", title: "Service report" }], create: async () => ({ id: "new", title: "New" }), read: async () => turns },
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
if (params.get("state")) { session.beginWorkGuidance("turn-0"); session.setDraft("Please keep the review focused on the current report."); if (params.get("state") !== "target") session.send(session.getSnapshot().draft, null); }
createRoot(document.getElementById("root")!).render(<main style={{ margin: "20px auto", width: "min(100% - 24px, 720px)", height: "calc(100dvh - 40px)", border: "1px solid var(--os-line)", borderRadius: "var(--os-radius)", overflow: "hidden", background: "var(--os-glass-solid)" }}><AskSurface conversation={session} transport={transport} variant="sheet" availability={{ state: "ready", message: "", refresh() {} }} onOpenWork={() => {}} /></main>);
