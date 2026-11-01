import { createRoot } from "react-dom/client";
import { AskSurface } from "../src/ask/AskSurface";
import { ConversationSession, type AskTurn } from "../src/ask/conversationSession";
import { routeOptionsFrom } from "../src/ask/askRoute";
import type { UploadHandle, UploadProvider, UploadResult } from "../src/items/upload";
import { applyPackStyles } from "../src/themes/registry";
import "../src/styles/index.css";

// Synthetic source-only fixture for Ask's message field (routes design, "Ask's
// message field"). No live connection, upload or model call.
//
//   ?scene=empty | multiline | attachments | picker | not-ready | apps | workbench
//   &mode=light|dark  &narrow=1  &widget=1
const params = new URLSearchParams(location.search);
const scene = params.get("scene") ?? "empty";
document.documentElement.dataset.theme = params.get("mode") === "light" ? "light" : "dark";
document.documentElement.dataset.osTheme = params.get("theme") ?? "graphite";
applyPackStyles([]);

const appsReady = scene === "apps" || scene === "workbench";
const routes = routeOptionsFrom({ routes: [
  { id: "auto", name: "Auto", kind: "auto", scope: "cluster", ready: true },
  { id: "local", name: "Local", kind: "local", scope: "cluster", ready: true },
  { id: "apps", name: "Apps", kind: "apps", scope: "cluster", ready: appsReady, reason: appsReady ? "" : "No app is ready on your machines" },
  { id: "r1", name: "Emergency", kind: "custom", scope: "personal", ready: true },
  { id: "r2", name: "Out of credits", kind: "custom", scope: "personal", ready: scene !== "not-ready", reason: "No local model reachable" },
] });

const turns: AskTurn[] = scene === "empty" ? [] : [{
  id: "t1", prompt: "What changed in the onboarding deck since last week?", answer: "Three slides changed: pricing, the roadmap and the team page.", state: "done", startedAt: new Date(Date.now() - 120000).toISOString(), activity: [],
  attachments: scene === "attachments" ? [{ artifactId: "a0", fileId: "f0", name: "onboarding-deck.pdf" }] : undefined,
}];

const transport = {
  ask() { throw new Error("Sending is not connected in this fixture."); },
  conversations: { list: async () => [{ id: "fixture", title: "Onboarding" }], create: async () => ({ id: "new", title: "New" }), read: async () => turns },
  routes: async () => routes,
};

/** Uploads that stay where the scene puts them: one landed, one halfway, one refused. */
const uploads: UploadProvider = {
  upload(file: File): UploadHandle {
    let progress: ((p: { sentBytes: number; totalBytes: number }) => void) | undefined;
    const done = new Promise<UploadResult>((resolve, reject) => {
      setTimeout(() => {
        if (file.name.startsWith("ready")) resolve({ artifactId: `a-${file.name}`, fileId: `f-${file.name}`, title: file.name, fileKind: "file", source: "uploaded" });
        else if (file.name.startsWith("failed")) reject(new Error("upload refused: 170 MB would exceed this account's quota of 150 MB (MEMQL_LIBRARY_USER_QUOTA_BYTES)"));
        else progress?.({ sentBytes: 45, totalBytes: 100 });
      }, 30);
    });
    return { done, abort() {}, onProgress: listener => { progress = listener; return () => {}; } };
  },
};

const session = new ConversationSession(transport);
await session.select("fixture");
if (scene === "multiline") session.setDraft("Compare the onboarding deck with last quarter's version.\nList every slide that changed, what changed on it, and who changed it.\nThen draft a short note for the team.\nKeep it under 200 words.\nUse the same tone as the deck.");
if (scene === "attachments") {
  session.attach([new File(["x"], "ready-q3-plan.md"), new File(["x"], "uploading-board-notes.pdf"), new File(["x"], "failed-recording.mov")], uploads);
  session.setDraft("Summarize these and flag anything that disagrees with the deck.");
}
if (scene === "apps") { session.setRoute("apps"); session.setDraft("Refactor the billing module and run its tests."); }
if (scene === "workbench") { session.setDestination("workbench"); session.setDraft("Compare these two spreadsheets."); }

createRoot(document.getElementById("root")!).render(
  <main style={{ margin: "20px auto", width: params.has("narrow") ? "min(100% - 24px, 360px)" : "min(100% - 24px, 720px)", height: params.has("widget") ? "420px" : "calc(100dvh - 40px)", border: "1px solid var(--os-line)", borderRadius: "var(--os-radius)", overflow: "hidden", background: "var(--os-glass-solid)" }}>
    <AskSurface conversation={session} transport={transport} uploads={uploads} variant={params.has("widget") ? "widget" : "sheet"} availability={{ state: "ready", message: "", refresh() {} }} onOpenWork={() => {}} onOpenFile={() => {}} onManageRoutes={() => { document.title = "Manage routes"; }} />
  </main>,
);

// A one-shot capture cannot click, so the menu scenes open their menu here.
if (scene === "picker" || scene === "not-ready" || params.has("menu")) {
  setTimeout(() => {
    const route = Array.from(document.querySelectorAll<HTMLButtonElement>('[role="combobox"]')).find(button => document.querySelector(`label[for="${CSS.escape(button.id)}"]`)?.textContent === (params.get("menu") === "where" ? "Where" : "Route"));
    route?.click();
  }, 300);
}
