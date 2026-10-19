import { vi } from "vitest";
import { useEffect, useState, type ComponentType, type ReactNode } from "react";
import { FleetApp } from "../src/apps/fleet/FleetApp";
import { useOs } from "../src/chrome/state";
import { resolveThemePack } from "../src/themes/registry";
import { MachinesProvider } from "../src/live/machines";
import { fakeConnection, machineRow, appSessionRow, shareDirectoryRow, builtinReply } from "../test/fleet/harness";
import { withSession } from "../test/deployables/harness";

type Frame = ComponentType<{ fallback: string; children: ReactNode }>;
export function fleetEquipmentScenes(Frame: Frame) {
  return Object.fromEntries(["populated", "apps", "apps-missing", "apps-offline", "activity", "activity-empty", "activity-error", "models", "models-automatic", "models-empty", "models-error", "models-save-error", "models-pending", "models-bound", "details", "details-empty", "labels", "labels-empty", "at-limit", "empty", "error", "sharing", "sharing-people", "sharing-picker", "sharing-empty", "sharing-error", "sharing-pending"].map(state => [`fleet-equipment-${state}`, {
    connect: () => {
      const shareDirectory = shareDirectoryRow({ id: "studio", people: state === "sharing-empty" ? [] : [{ id: "ana", name: "Ana Ruiz", detail: "" }, { id: "bo", name: "Bo Chen", detail: "" }], groups: state === "sharing-empty" ? [] : [{ id: "design", name: "Design", members: 4 }, { id: "ops", name: "Operations", members: 7 }] });
      if (state === "sharing-picker") {
        shareDirectory.people = [...shareDirectory.people as object[], ...Array.from({ length: 12 }, (_, i) => ({ id: `person-${i}`, name: `Person ${String(i + 1).padStart(2, "0")}`, detail: `person${i + 1}@example.com` }))];
        shareDirectory.groups = [...shareDirectory.groups as object[], ...Array.from({ length: 7 }, (_, i) => ({ id: `group-${i}`, name: `Group ${String(i + 1).padStart(2, "0")}`, members: i + 2 }))];
      }
      let currentMachine = machineRow({ id: "studio", ownerUserId: "me", displayName: "Studio Mac", sharing: (state === "sharing-people" || state === "sharing-picker") ? { mode: "people", userIds: ["ana"], groupIds: ["design"] } : { mode: "owner" }, labels: state === "empty" ? {} : { "model:qwen3.5:8b": "ctx=32768;tools=1", "model:llama3.2:3b": "ctx=8192", "model:nomic-embed-text": "embeddings=1" }, hardware: { chip: "Apple M4 Pro", memoryBytes: 51539607552, cpuCores: 12 }, apps: state === "empty" ? [] : [{ id: "claude-code", label: "Claude Code", allowed: true, signedIn: true }, { id: "codex", label: "Codex", allowed: true, signedIn: true }, { id: "gemini-cli", label: "Gemini CLI", allowed: false, signedIn: false, why: "Sign in on this machine." }] });
      currentMachine.sharingActivity = { nodeId: "agent-test", startedAt: new Date().toISOString(), observedAt: new Date().toISOString(), users: [{ id: "ana", activeCalls: 1 }], groups: [{ id: "design", activeCalls: 2 }] };
      if (state.startsWith("models")) currentMachine.labels = state === "models-empty" ? {"runtime:ollama":"0.33"} : { "model:qwen3.8:27b": "ctx=262144,params=27320697856,quant=Q4_K_M,tools=1,vision=1,structured=1", "model:qwen3.5:4b": "ctx=262144,params=4659865088,quant=Q4_K_M,tools=1,vision=1,structured=1", "model:qwen3-embedding:0.6b": "embeddings=1", "model:flux2-klein-4b": "imagegen=1" };
      if (state === "details" || state.startsWith("labels")) currentMachine = { ...currentMachine,
        name: "studio.local", platformInfo: { os: "darwin", arch: "arm64", hostname: "studio.local" }, version: "0.8.0", buildTag: "a91b2345678901234567890123456789012345678901234567890123456789012345",
        rttMs: 12, rttAt: new Date().toISOString(), clockSkewMs: 4, activeCount: 2,
        capabilityDescriptor: { displayServer: "aqua", computerUseAvailable: true },
        labels: { os: "darwin", tier: "studio", pipelines: "allowed", "runtime:ollama": "0.12.0", "model:qwen3.5:8b": "ctx=32768,tools=1,structured=1,quant=Q4_K_M" },
        operatorLabels: { tier: "production" },
        hardware: { chip: "Apple M4 Pro", memoryBytes: 51539607552, cpuCores: 12, gpu: { name: "Apple M4 Pro", backend: "metal", vramBytes: 0 }, osVersion: "macOS", diskFreeBytes: 264140488704, reportedAt: new Date().toISOString(), runtimes: [{ name: "ollama", version: "0.12.0" }] },
      };
      if (state === "labels") currentMachine.operatorLabels = { region: "us-west", room: "Studio", tier: "production", team: "Design", gpu: "metal", purpose: "Model inference", availability: "Office hours" };
      if (state === "labels-empty") currentMachine.operatorLabels = {};
      if (state === "details-empty") currentMachine = { ...currentMachine, labels: {}, hardware: {} };
      if (state.startsWith("apps")) currentMachine = { ...currentMachine,
        apps: state === "apps-missing" ? [{ id: "claude-code", label: "Claude Code", allowed: false, signedIn: false, subscription: "unknown", version: "2.1.0" }] : [{ id: "claude-code", label: "Claude Code", allowed: true, signedIn: true, subscription: "unknown", version: "2.1.0" }, { id: "codex", label: "Codex", allowed: true, signedIn: true, subscription: "present", version: "0.118.0" }],
        ...(state === "apps-offline" ? { connectedNodeId: "", lastSeenAt: "2026-01-01T00:00:00Z" } : {}),
      };
      const activityRows = state === "activity-empty" ? [] : [
        { id: "call-a", kind: "model", label: "qwen3.8:27b · chat", status: "running" },
        { id: "call-b", kind: "model", label: "qwen3-embedding:0.6b · embedding", status: "queued" },
        { id: "call-c", kind: "tool", label: "fs.read", status: "succeeded" },
        { id: "call-d", kind: "tool", label: "exec.run", status: "failed", detail: "The command exited before completing. No files were changed." },
      ].map((row, i) => ({ ...row, ownerUserId: "me", workerId: "studio", startedAt: new Date(Date.now() - i * 60_000).toISOString(), expiresAt: new Date(Date.now() + 600_000).toISOString(), ...(i > 1 ? { endedAt: new Date(Date.now() - i * 60_000 + 4_000).toISOString() } : {}) }));
      const connection = fakeConnection({
        machineActivity: state.startsWith("activity") ? activityRows : [],
        fleetRecommended: state === "details" || state.startsWith("labels") ? [{ id: "studio", machineClass: "32", usableBytes: 38654705664 }] : [],
        fleetShareDirectory: [shareDirectory],
        myWorkersWithStatus: [currentMachine],
        appSessionsForUser: state === "empty" || state.startsWith("activity") ? [] : Array.from({ length: state === "at-limit" ? 5 : 8 }, (_, i) => appSessionRow({ id: `session-${i}`, ownerUserId: "me", workerId: "studio", runId: `work-${i + 1}`, app: i % 2 ? "codex" : "claude-code", status: i === 7 ? "running" : "ended", startedAt: `2026-10-14T10:0${i}:00Z` })),
      });
      if (state === "activity-error") connection.query.machineActivity = vi.fn(async () => { throw new Error("The connection was interrupted."); });
      let setup = { id: "studio", revision: state === "models-empty" ? "" : "setup-1", choices: state === "models-empty" ? {} : { conversation: "qwen3.8:27b", structured: "qwen3.8:27b", tools: "qwen3.8:27b", vision: "qwen3.8:27b", embeddings: "qwen3-embedding:0.6b", imageGen: "flux2-klein-4b" }, usableBytes: 48 * 2**30, diskFreeBytes: 199 * 2**30, researchEnabled: false, catalog: [
        { modelId: "qwen3.8:27b", purposes: ["conversation", "structured", "tools", "vision"], memoryBytes: 22e9, sizeBytes: 18e9, recommended: true },
        { modelId: "qwen3.5:4b", purposes: ["conversation", "structured", "tools", "vision"], memoryBytes: 8e9, sizeBytes: 4e9, recommended: true },
        { modelId: "qwen3-embedding:0.6b", purposes: ["embeddings"], memoryBytes: 2e9, sizeBytes: 639e6, recommended: true },
        { modelId: "flux2-klein-4b", purposes: ["imageGen"], memoryBytes: 13e9, sizeBytes: 0, recommended: false },
        { modelId: "qwen3.5:122b", purposes: ["conversation", "structured", "tools", "vision"], memoryBytes: 90e9, sizeBytes: 81e9, blocked: "Needs more model memory", installBlocked: "Needs more model memory" },
      ].map(m => ({ runtime: "ollama", blocked: "", installBlocked: "", installed: state !== "models-empty" && m.modelId !== "qwen3.5:122b", source: "https://ollama.com/library/" + m.modelId, reviewedAt: "2026-10-15T00:00:00Z", ...m })) };
      Object.assign(setup, { suggestedChoices: setup.choices });
      // Nothing saved yet: the engine still suggests a pick for each task.
      if (state === "models-automatic") Object.assign(setup, { revision: "", choices: {} });
      if (state === "models-bound") Object.assign(setup, { boundEmbeddingModel: "qwen3-embedding:0.6b" });
      connection.query.fleetModelSetup = vi.fn(async () => {
        if (state === "models-error") throw new Error("Connection interrupted");
        return builtinReply("fleetModelSetup", [setup]);
      });
      connection.query.fleetSetModelSetup = vi.fn(async args => {
        if (delaySave) await new Promise<void>(resolve => { finishSave = resolve; });
        if (refuseSave) throw new Error("Connection interrupted. Your choices are kept; try saving again.");
        setup = { ...setup, revision: "setup-2", choices: args.choices as typeof setup.choices };
        return builtinReply("fleetSetModelSetup", [{ revision: setup.revision }]);
      });
      if (state === "error") connection.query.appSessionsForUser = vi.fn(async () => { throw new Error("Connection interrupted"); });
      let refuseSave = state === "models-save-error", delaySave = state === "models-pending";
      let finishSave: (() => void) | undefined;
      let finishRead: (() => void) | undefined;
      if (state === "sharing-error") connection.query.fleetShareDirectory = vi.fn(async () => { throw new Error("Connection interrupted"); });
      if (state === "sharing-pending") connection.query.fleetShareDirectory = vi.fn(() => new Promise(resolve => { finishRead = () => resolve(builtinReply("fleetShareDirectory", [shareDirectory])); }));
      connection.query.fleetSetSharing = vi.fn(async (choice) => {
        if (delaySave) await new Promise<void>(resolve => { finishSave = resolve; });
        if (refuseSave) throw new Error("Connection interrupted. Try saving again.");
        currentMachine = { ...currentMachine, sharing: { ...choice }, lastSeenAt: new Date().toISOString() };
        connection.subscriptions.emit("v1:worker:registration", currentMachine, "NODE_UPDATED");
        return builtinReply("fleetSetSharing", [{ id: "studio", ...choice, people: choice.userIds.length, groups: choice.groupIds.length, sentence: choice.mode === "owner" ? "Models are private." : "Model sharing saved." }]);
      });
      const beforeMachineSave = async () => {
        if (delaySave) await new Promise<void>(resolve => { finishSave = resolve; });
        if (refuseSave) throw new Error("Connection interrupted. Try saving again.");
      };
      connection.query.renameWorker = vi.fn(async ({ displayName }) => {
        await beforeMachineSave();
        currentMachine = { ...currentMachine, displayName };
        connection.subscriptions.emit("v1:worker:registration", currentMachine, "NODE_UPDATED");
        return builtinReply("renameWorker", [currentMachine]);
      });
      connection.query.setWorkerOperatorLabels = vi.fn(async ({ operatorLabels }) => {
        await beforeMachineSave();
        currentMachine = { ...currentMachine, operatorLabels };
        connection.subscriptions.emit("v1:worker:registration", currentMachine, "NODE_UPDATED");
        return builtinReply("setWorkerOperatorLabels", [currentMachine]);
      });
      connection.query.fleetRevokeMachine = vi.fn(async () => {
        await beforeMachineSave();
        currentMachine = { ...currentMachine, revokedAt: new Date().toISOString() };
        connection.subscriptions.emit("v1:worker:registration", currentMachine, "NODE_UPDATED");
        return builtinReply("fleetRevokeMachine", [{ credentialState: "revoked", sentence: "Removed." }]);
      });
      Object.assign(window, { fleetQa: {
        advance: (status: string) => {
          const row = { ...activityRows[0], status };
          connection.subscriptions.emit("v1:worker:activity", row, "NODE_UPDATED");
        },
        models: (labels: Record<string, string>) => {
          currentMachine = { ...currentMachine, labels, lastSeenAt: new Date().toISOString() };
          connection.subscriptions.emit("v1:worker:registration", currentMachine, "NODE_UPDATED");
        },
        finishRead: () => finishRead?.(),
        refuseSave: (value: boolean) => { refuseSave = value; },
        delaySave: () => { delaySave = true; },
        finishSave: () => { delaySave = false; finishSave?.(); },
        retryDirectory: () => { connection.query.fleetShareDirectory = vi.fn(async () => builtinReply("fleetShareDirectory", [shareDirectory])); },
        sharingWrites: () => connection.query.fleetSetSharing.mock.calls.length,
        lastShareChoice: () => connection.query.fleetSetSharing.mock.calls.at(-1)?.[0],
        directoryReads: () => connection.query.fleetShareDirectory.mock.calls.length,
        machineReads: () => connection.query.myWorkersWithStatus.mock.calls.length,
        labelWrites: () => connection.query.setWorkerOperatorLabels.mock.calls.length,
        operatorLabels: (labels: Record<string, string>) => {
          currentMachine = { ...currentMachine, operatorLabels: labels };
          connection.subscriptions.emit("v1:worker:registration", currentMachine, "NODE_UPDATED");
        },
        removals: () => connection.query.fleetRevokeMachine.mock.calls.length,

        activity: (calls: number) => {
          currentMachine = { ...currentMachine, lastSeenAt: new Date().toISOString(), sharingActivity: { nodeId: "agent-test", observedAt: new Date().toISOString(), users: [{ id: "ana", activeCalls: calls }], groups: [{ id: "design", activeCalls: calls }] } };
          connection.subscriptions.emit("v1:worker:registration", currentMachine, "NODE_UPDATED");
        },
        arrive: () => connection.subscriptions.emit("v1:worker:appSession", appSessionRow({ id: "new-session", ownerUserId: "me", workerId: "studio", runId: "Fresh work", status: "running", startedAt: "2026-10-14T11:00:00Z" }), "NODE_CREATED"),
        reads: () => connection.query.appSessionsForUser.mock.calls.length,
      } });
      return connection;
    },
    wrap: (el: JSX.Element, role: string) => withSession(<MachinesProvider>{el}</MachinesProvider>, { userId: "me", role }),
    render: () => <Frame fallback="Fleet"><EquipmentScene /></Frame>,
  }]));
}
function EquipmentScene() {
  const { actions } = useOs();
  useEffect(() => { actions.setThemePack(resolveThemePack(new URLSearchParams(window.location.search).get("theme") ?? "graphite").id); }, [actions]);
  const [section, setSection] = useState("machines");
  return <FleetApp sectionId={section} navigate={setSection} askContext={() => {}} store={{ load: () => ({ version: 1, defaultSection: "machines", showRevoked: false }), save: () => {} }} />;
}
