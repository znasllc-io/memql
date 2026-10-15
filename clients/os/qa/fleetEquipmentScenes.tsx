import { vi } from "vitest";
import { useEffect, useState, type ComponentType, type ReactNode } from "react";
import { FleetApp } from "../src/apps/fleet/FleetApp";
import { useOs } from "../src/chrome/state";
import { resolveThemePack } from "../src/themes/registry";
import { MachinesProvider } from "../src/live/machines";
import { fakeConnection, machineRow, appSessionRow } from "../test/fleet/harness";
import { withSession } from "../test/deployables/harness";

type Frame = ComponentType<{ fallback: string; children: ReactNode }>;
export function fleetEquipmentScenes(Frame: Frame) {
  return Object.fromEntries(["populated", "empty", "error"].map(state => [`fleet-equipment-${state}`, {
    connect: () => {
      const connection = fakeConnection({
        myWorkersWithStatus: [machineRow({ id: "studio", ownerUserId: "me", displayName: "Studio Mac", labels: state === "empty" ? {} : { "model:qwen3.5:8b": "ctx=32768;tools=1", "model:llama3.2:3b": "ctx=8192", "model:nomic-embed-text": "embeddings=1" }, hardware: { chip: "Apple M4 Pro", memoryBytes: 51539607552, cpuCores: 12 }, apps: state === "empty" ? [] : [{ id: "claude-code", label: "Claude Code", allowed: true, signedIn: true }, { id: "codex", label: "Codex", allowed: true, signedIn: true }, { id: "gemini-cli", label: "Gemini CLI", allowed: false, signedIn: false, why: "Sign in on this machine." }] })],
        appSessionsForUser: state === "empty" ? [] : Array.from({ length: 8 }, (_, i) => appSessionRow({ id: `session-${i}`, ownerUserId: "me", workerId: "studio", runId: `work-${i + 1}`, app: i % 2 ? "codex" : "claude-code", status: i === 7 ? "running" : "ended", startedAt: `2026-10-14T10:0${i}:00Z` })),
      });
      if (state === "error") connection.query.appSessionsForUser = vi.fn(async () => { throw new Error("Connection interrupted"); });
      Object.assign(window, { fleetQa: {
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
