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
  return Object.fromEntries(["populated", "at-limit", "empty", "error", "sharing", "sharing-people", "sharing-empty", "sharing-error", "sharing-pending"].map(state => [`fleet-equipment-${state}`, {
    connect: () => {
      const shareDirectory = shareDirectoryRow({ id: "studio", people: state === "sharing-empty" ? [] : [{ id: "ana", name: "Ana Ruiz", detail: "" }, { id: "bo", name: "Bo Chen", detail: "" }], groups: state === "sharing-empty" ? [] : [{ id: "design", name: "Design", members: 4 }, { id: "ops", name: "Operations", members: 7 }] });
      let currentMachine = machineRow({ id: "studio", ownerUserId: "me", displayName: "Studio Mac", sharing: state === "sharing-people" ? { mode: "people", userIds: ["ana"], groupIds: ["design"] } : { mode: "owner" }, labels: state === "empty" ? {} : { "model:qwen3.5:8b": "ctx=32768;tools=1", "model:llama3.2:3b": "ctx=8192", "model:nomic-embed-text": "embeddings=1" }, hardware: { chip: "Apple M4 Pro", memoryBytes: 51539607552, cpuCores: 12 }, apps: state === "empty" ? [] : [{ id: "claude-code", label: "Claude Code", allowed: true, signedIn: true }, { id: "codex", label: "Codex", allowed: true, signedIn: true }, { id: "gemini-cli", label: "Gemini CLI", allowed: false, signedIn: false, why: "Sign in on this machine." }] });
      currentMachine.sharingActivity = { nodeId: "agent-test", startedAt: new Date().toISOString(), observedAt: new Date().toISOString(), users: [{ id: "ana", activeCalls: 1 }], groups: [{ id: "design", activeCalls: 2 }] };
      const connection = fakeConnection({
        fleetShareDirectory: [shareDirectory],
        myWorkersWithStatus: [currentMachine],
        appSessionsForUser: state === "empty" ? [] : Array.from({ length: state === "at-limit" ? 5 : 8 }, (_, i) => appSessionRow({ id: `session-${i}`, ownerUserId: "me", workerId: "studio", runId: `work-${i + 1}`, app: i % 2 ? "codex" : "claude-code", status: i === 7 ? "running" : "ended", startedAt: `2026-10-14T10:0${i}:00Z` })),
      });
      if (state === "error") connection.query.appSessionsForUser = vi.fn(async () => { throw new Error("Connection interrupted"); });
      let refuseSave = false, delaySave = false;
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
      Object.assign(window, { fleetQa: {
        finishRead: () => finishRead?.(),
        refuseSave: (value: boolean) => { refuseSave = value; },
        delaySave: () => { delaySave = true; },
        finishSave: () => { delaySave = false; finishSave?.(); },
        retryDirectory: () => { connection.query.fleetShareDirectory = vi.fn(async () => builtinReply("fleetShareDirectory", [shareDirectory])); },
        directoryReads: () => connection.query.fleetShareDirectory.mock.calls.length,

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
