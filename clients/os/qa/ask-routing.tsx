import { createRoot } from "react-dom/client";
import { AskSurface } from "../src/ask/AskSurface";
import { ConversationSession, type ApprovalDiscussion, type AskRouted, type AskRouteTarget, type AskTurn } from "../src/ask/conversationSession";
import type { ApprovalGuidanceReceipt } from "../src/ask/askController";
import { applyPackStyles } from "../src/themes/registry";
import "../src/styles/index.css";

// Synthetic source-only fixture. No live connection or model call.
//
// Ask routing a message to goals already running: scene=labels (where each
// message went), split (the reply before a split request's own answer;
// state=quick answers it inline), stop (the stop question on a goal's card;
// state=pending holds an answer in flight, state=error refuses it) and many
// (a card that asks is never folded). budget=1 is the budget approval
// discussion (state=saved|pending|error). scene=empty|pending|quick|error|
// format-error|model-error are the general Ask states. mode=light|dark;
// narrow=1 for a narrow column.
const params = new URLSearchParams(location.search);
const budget = params.has("budget");
const scene = params.get("scene");
const state = params.get("state");
document.documentElement.dataset.theme = params.get("mode") === "light" ? "light" : "dark";
document.documentElement.dataset.osTheme = "graphite";
applyPackStyles([]);

const now = Date.now();
const ago = (minutes: number) => new Date(now - minutes * 60_000).toISOString();
function goal(id: string, title: string, prompt: string, acknowledgement: string, minutes: number, fields: Partial<AskTurn> = {}): AskTurn {
  return { id, runId: `run-${id}`, goalId: `goal-${id}`, prompt, workTitle: title, answer: acknowledgement, acknowledgement, background: true, state: "queued", workStatus: "running", startedAt: ago(minutes), acknowledgedAt: ago(minutes - 0.05), activity: [], ...fields };
}
function target(id: string, title: string, status: AskRouteTarget["status"], text: string, fields: Partial<AskRouteTarget> = {}): AskRouteTarget {
  return { goalId: `goal-${id}`, runId: `run-${id}`, title, status, text, ...fields };
}
// A message that went only to goals already running: its reply is its answer.
function routed(id: string, prompt: string, minutes: number, route: AskRouted): AskTurn {
  return { id, prompt, answer: route.reply ?? "", state: "done", startedAt: ago(minutes), endedAt: ago(minutes - 0.05), activity: [], routed: route };
}
const waitingOnYou: Partial<AskTurn> = { state: "waiting", workStatus: "waiting" };

const weather = goal("weather", "Weather in Phoenix", "What’s the weather in Tucson this weekend?", "I’ll check the weekend forecast.", 16);
const tucson = { ...weather, workTitle: "Weather in Tucson" };
const hike = goal("hike", "Saturday hike near Phoenix", "Plan a short hike for Saturday morning.", "I’ll put together a few trail options.", 14);
const deploy = goal("deploy", "Deploy storefront update", "Deploy the storefront update to production.", "I’ll prepare the deploy and ask before publishing.", 12, waitingOnYou);
const report = goal("report", "Quarterly report", "Draft the quarterly report from the shared numbers.", "I’ll draft the report from the shared numbers.", 11, waitingOnYou);
const dinner = goal("dinner", "Dinner downtown Phoenix", "Find a dinner spot downtown for Saturday.", "I’ll find a few dinner options downtown.", 9);
const neverMind = routed("never", "Never mind the Tucson one.", 1, { reply: "Should I stop the Tucson weather check? Confirm on its card.", stops: [{ goalId: "goal-weather", runId: "run-weather", title: "Weather in Tucson", decision: "" }] });

const routing: Record<string, AskTurn[]> = {
  labels: [
    weather, { ...hike, ...waitingOnYou }, deploy, report,
    routed("phoenix", "Actually, I’m in Phoenix, not Tucson.", 7, { reply: "Switching the forecast and the hike to Phoenix.", targets: [
      target("weather", "Weather in Phoenix", "received", "Actually, I’m in Phoenix, not Tucson."),
      target("hike", "Saturday hike near Phoenix", "answered", "Actually, I’m in Phoenix, not Tucson.", { approvalId: "hike-question" }),
    ] }),
    routed("staging", "Use the staging bucket, and leave the appendix out of the report.", 4, { reply: "Sent to the deploy as a correction; publishing still needs your approval. The report keeps your note until its limit is raised.", targets: [
      target("deploy", "Deploy storefront update", "notApproved", "Use the staging bucket, and leave the appendix out of the report.", { approvalId: "deploy-permission" }),
      target("report", "Quarterly report", "saved", "Use the staging bucket, and leave the appendix out of the report.", { approvalId: "report-budget" }),
    ] }),
    routed("sunset", "Add a sunset viewpoint to the plans.", 2, { reply: "Added to the hike. The trip recap had already finished, and the event plan is waiting on your review, so neither got it.", targets: [
      target("hike", "Saturday hike near Phoenix", "queued", "Add a sunset viewpoint to the plans."),
      target("recap", "Trip recap", "notReceived", "Add a sunset viewpoint to the plans.", { reason: "ended" }),
      target("plan", "Event plan", "notDelivered", "Add a sunset viewpoint to the plans.", { approvalId: "plan-review", reason: "review" }),
    ] }),
  ],
  split: [
    weather,
    state === "quick"
      ? { ...goal("sunset", "Sunset in Phoenix", "Actually, I’m in Phoenix. When is sunset there on Saturday?", "", 2), answer: "Sunset in Phoenix on Saturday is at 6:41 PM.", acknowledgement: undefined, background: false, state: "done", workStatus: "succeeded", endedAt: ago(1.9), routed: { reply: "Switching the forecast to Phoenix.", statement: "When is sunset in Phoenix on Saturday?", targets: [target("weather", "Weather in Phoenix", "queued", "Actually, I’m in Phoenix.")] } }
      : { ...goal("dinner", "Dinner downtown Phoenix", "Actually, I’m in Phoenix. Also find a dinner spot downtown for Saturday.", "I’ll find a few dinner options downtown.", 2), routed: { reply: "Switching the forecast to Phoenix.", statement: "Find a dinner spot downtown for Saturday.", targets: [target("weather", "Weather in Phoenix", "queued", "Actually, I’m in Phoenix.")] } },
  ],
  stop: [tucson, hike, neverMind],
  many: [tucson, hike, dinner, deploy, report, neverMind],
};

// The budget discussion and the general states share one piece of work.
let turns: AskTurn[] = Array.from({ length: params.get("many") ? 4 : 1 }, (_, index) => ({
  id: `turn-${index}`, runId: `run-${index}`,
  prompt: "Review the service report and prepare the requested changes.", workTitle: ["Review service report", "Check a longer report title with useful context", "Prepare the review", "Verify the result"][index],
  answer: "I’ll work through the report.", acknowledgement: "I’ll work through the report.", background: true, state: budget ? "waiting" : "queued", workStatus: budget ? "awaiting_approval" : "running", startedAt: new Date().toISOString(), activity: [],
}));
if (scene && routing[scene]) turns = routing[scene];
if (scene === "empty") turns = [];
if (scene === "pending" || scene === "quick") turns = [{ ...turns[0]!, prompt: "Hola", answer: scene === "quick" ? "¡Hola! ¿Cómo estás?" : "", acknowledgement: undefined, background: false, workload: "quick", state: scene === "quick" ? "done" : "queued", workStatus: scene === "quick" ? "succeeded" : "compiling" }];
if (scene === "error") turns = [{ ...turns[0]!, state: "error", error: "The model is unavailable. Try again when it is connected.", endedAt: new Date().toISOString() }];
if (scene === "format-error" || scene === "model-error") turns = [{ ...turns[0]!, prompt: "Create an image of the first bird in my saved list.", answer: "I’ll check the saved list and create its first bird.", acknowledgement: "I’ll check the saved list and create its first bird.", workTitle: "Create the bird image", state: "error", workStatus: "failed", error: scene === "format-error" ? 'automation "workSpineDraft" execution failed: Spine refused: work compile: triage fileFormat: compose: unknown format "png" (offered: markdown, html, txt, csv, json, docx, pdf)' : "every_door_shut: no eligible fleet machine for this model", endedAt: new Date().toISOString() }];
let discussion: ApprovalDiscussion = { conversationId: "fixture", approval: { id: "budget", runId: "run-0", kind: "budget", question: "This work reached its limit.", status: "pending" }, canSendGuidance: true, canAskAdvisor: true, guidanceMode: "deferred", savedGuidance: state === "saved" ? "Use the verified record and check the result before continuing." : "" };
const transport = {
  ask() { throw new Error("Ordinary Ask is not connected in this fixture."); },
  conversations: { list: async () => [{ id: "fixture", title: "Weekend in Phoenix" }], create: async () => ({ id: "new", title: "New" }), read: async () => turns },
  discussApproval: async () => discussion,
  approvalGuidance: async (_id: string, feedback: string): Promise<ApprovalGuidanceReceipt> => {
    if (state === "pending") return new Promise(() => {});
    if (state === "error") throw new Error("The connection closed before guidance was confirmed.");
    discussion = { ...discussion, savedGuidance: feedback };
    return { runResumed: false, guidanceSaved: true, savedGuidance: feedback };
  },
  // Records the answer the way the cluster does; a stopped goal's card stays
  // until its run notices the request at its next step.
  decideStop: async (request: { turnId: string; runId: string; decision: "stop" | "keep" }): Promise<void> => {
    if (state === "pending") return new Promise(() => {});
    if (state === "error") throw new Error("this work could not be stopped; try again");
    turns = turns.map(turn => turn.id !== request.turnId || !turn.routed?.stops ? turn : { ...turn, routed: { ...turn.routed, stops: turn.routed.stops.map(stop => stop.runId === request.runId ? { ...stop, decision: request.decision === "stop" ? "stopped" : "kept" } : stop) } });
  },
};
const session = new ConversationSession(transport);
await session.select("fixture");
if (budget) {
  await session.discussApproval("budget");
  if (state !== "saved") session.setDraft("Use the verified record and check the result before continuing.");
  if (["pending", "error"].includes(state ?? "")) void session.sendGuidance();
} else if ((scene === "stop" || scene === "many") && (state === "pending" || state === "error")) {
  void session.decideStop("never", "run-weather", "stop");
}
createRoot(document.getElementById("root")!).render(<main style={{ margin: "20px auto", width: params.has("narrow") ? "min(100% - 24px, 360px)" : "min(100% - 24px, 880px)", height: "min(720px, calc(100dvh - 40px))", border: "1px solid var(--os-line)", borderRadius: "var(--os-radius)", overflow: "hidden", background: "var(--os-glass-solid)" }}><AskSurface conversation={session} transport={transport} variant="sheet" availability={{ state: "ready", message: "", refresh() {} }} onOpenWork={() => {}} onOpenFleet={() => { document.title = "Fleet action selected"; }} /></main>);
