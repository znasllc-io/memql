import { useState, type ComponentType, type ReactNode } from "react";
import { NexusApp } from "../src/apps/nexus/NexusApp";
import { LocalNexusSettingsStore } from "../src/apps/nexus/settings";
import { approvalRow, fakeConnection, runRow, withSession } from "../test/nexus/harness";
import { AskProvider } from "../src/ask/AskProvider";
import { AskSheet } from "../src/ask/AskSheet";
import type { AskTransport } from "../src/ask/askController";
import type { AskTurn } from "../src/ask/conversationSession";
import { OsProvider } from "../src/chrome/state";
import { OS_REGISTRY } from "../src/apps/registry";

const directory = { version: 1, kind: "directory_read", registrationId: "registration", machineId: "studio", machineLabel: "Studio Mac", tool: "workerHost", action: "fs_list", directory: "/Users/dana/projects/memql", argsHash: "bound-inputs", allowedModes: ["once", "always"] };
const command = { ...directory, kind: "exact_command", directory: undefined, command: "git log --oneline --max-count=10 -- docs/public/operate/reproduce-the-cloud-locally.md clients/os/SUPERVISED-VISUAL-COMPOSITION.md clients/os/DESIGN.md", cwd: "/Users/dana/projects/memql", timeoutSeconds: 60, action: "exec" };
const approvals = [
  approvalRow({ id: "failure", stepKey: "inspectRepository", kind: "planReview", question: "How should this work continue?", subject: { symptom: "environment", stepKey: "inspect", errorMessage: "Context checkpoint did not free space; originals were retained." }, options: [{ value: "retry", label: "Retry" }, { value: "abandon", label: "Abandon" }] }),
  approvalRow({ id: "directory", stepKey: "inspectRepository", kind: "scopeElevation", question: "Read the repository on Studio Mac", subject: { computerPermission: directory }, requestedAt: "2026-09-01T09:05:00Z" }),
  approvalRow({ id: "command", stepKey: "inspectRepositoryChanges", kind: "scopeElevation", question: "Check the repository for local changes", subject: { computerPermission: command }, requestedAt: "2026-09-01T09:06:00Z" }),
  approvalRow({ id: "message", stepKey: "sendInspectionReport", kind: "sideEffect", question: "Send the completed inspection report to the project team", subject: { summary: "Send the completed inspection report to the project team", recipients: ["team@example.com"] }, requestedAt: "2026-09-01T09:07:00Z" }),
];
const saved = [approvalRow({ id: "saved-directory", decision: "approved", decidedAt: "2026-09-01T09:04:00Z", subject: { computerPermission: directory }, answer: { permissionMode: "always" } }), approvalRow({ id: "saved-command", decision: "approved", subject: { computerPermission: command }, answer: { permissionMode: "always" } })];
type Frame = ComponentType<{ fallback: string; children: ReactNode }>;
function NexusApprovalWindow({ frame: Frame, approvalId, advice }: { frame: Frame; approvalId?: string; advice?: "running" | "done" | "recorded" }) {
  const [section, setSection] = useState("approvals");
  const [store] = useState(() => new LocalNexusSettingsStore(null));
  const [transport] = useState<AskTransport>(() => {
    const turns: AskTurn[] = [{ id: "original", prompt: "Inspect my repository and introduce yourself to the project.", answer: "I need your input before this work can continue.", state: "waiting", runId: "run-1", background: true, activity: [], startedAt: "2026-10-12T19:14:00Z" }];
    if (advice) turns.push({ id: "advice", approvalId: "failure", advisor: true, prompt: "Investigate why the work is blocked.", answer: advice === "running" ? "" : "The saved checkpoint was larger than the short exchange it replaced. Keep the original request and compact a later complete exchange, then retry the read of the repository.", state: advice === "running" ? "queued" : "done", background: true, runId: "advice-run", startedAt: "2026-10-12T19:15:00Z", ...(advice !== "running" ? { endedAt: "2026-10-12T19:16:00Z" } : {}), activity: [] });
    let canSendGuidance = true;
    let guidanceToResume: string | undefined;
    return {
      ask: () => ({ cancel() {} }),
      discussApproval: async id => ({ conversationId: "original-conversation", approval: { id, runId: "run-1", kind: "planReview", question: "How should this work continue?", summary: "Context checkpoint did not free space; originals were retained.", status: canSendGuidance ? "pending" : "answered" }, canSendGuidance, canAskAdvisor: canSendGuidance, guidanceToResume }),
      approvalGuidance: async (approvalId, feedback) => { canSendGuidance = false; guidanceToResume = feedback; if (!turns.some(turn => turn.id === "guidance")) turns.push({ id: "guidance", approvalId, prompt: `Guidance sent:\n${feedback}`, answer: "", answerOnly: true, state: "done", startedAt: "2026-10-12T19:17:00Z", endedAt: "2026-10-12T19:17:00Z", activity: [] }); return { runResumed: false, resumeError: "The planner is temporarily unavailable." }; },
      approvalMessage: async request => ({ conversationId: request.conversationId, turnId: request.requestId, runId: "advice-run", status: "queued" }),
      conversations: { list: async () => [{ id: "original-conversation", title: "Introduce the repository" }], create: async () => ({ id: "unused", title: "Unused" }), read: async () => turns },
    };
  });
  return <AskProvider transport={transport} availability={{ state: "ready", message: "", refresh() {} }}><Frame fallback="Nexus"><NexusApp sectionId={section} navigate={setSection} askContext={() => {}} store={store}
    intent={approvalId ? { id: `qa-${approvalId}`, payload: { approvalId } } : undefined} consumeIntent={() => {}} /></Frame><AskSheet /></AskProvider>;
}
export function nexusApprovalScenes(frame: Frame) {
  const view = (approvalId?: string, empty = false, advice?: "running" | "done" | "recorded") => ({
    connect: () => fakeConnection({ approvals: empty ? [] : approvals, computerPermissions: empty ? [] : saved, runs: [runRow({ id: "run-1", workTitle: "Introduce the repository", status: "waiting" })] }),
    wrap: (element: JSX.Element, role: string) => withSession(<OsProvider registry={OS_REGISTRY} actorRole={role} grid={{ cols: 8, rows: 5 }}>{element}</OsProvider>, { role }),
    render: () => <NexusApprovalWindow frame={frame} approvalId={approvalId} advice={advice} />,
  });
  return {
    "nexus-approvals": view(), "nexus-approvals-empty": view(undefined, true),
    "nexus-approval-failure": view("failure"), "nexus-approval-directory": view("directory"),
    "nexus-approval-command": view("command"), "nexus-approval-message": view("message"),
    "nexus-permissions": view(), "nexus-permissions-empty": view(undefined, true), "nexus-permission": view(),
    "nexus-approval-discussion": view("failure"), "nexus-approval-advice": view("failure", false, "done"), "nexus-approval-advice-running": view("failure", false, "running"), "nexus-approval-guidance-recorded": view("failure", false, "recorded"), "nexus-approval-guidance-history": view("failure", false, "recorded"),
  };
}

export const NEXUS_APPROVAL_CLICKS: Record<string, string> = {
  "nexus-permissions": "Saved permissions", "nexus-permissions-empty": "Saved permissions",
  "nexus-permission": "Saved permissions|/Users/dana/projects/memql",
  "nexus-approval-discussion": "Discuss in Ask|fill:Ask=Why did the checkpoint fail?",
  "nexus-approval-advice": "Discuss in Ask|fill:Ask=Keep my original request. Try the larger exchange next.",
  "nexus-approval-advice-running": "Discuss in Ask|fill:Ask=While the other agent investigates, please keep the completed steps.",
  "nexus-approval-guidance-recorded": "Discuss in Ask|fill:Ask=Use a smaller read.|Send guidance",
  "nexus-approval-guidance-history": "Discuss in Ask|fill:Ask=Use a smaller read.|Send guidance|Return to conversation",
};
