import type { ComponentType, ReactNode } from "react";

import { AddMachinePage } from "../src/apps/fleet/addMachine/AddMachinePage";
import {
  barFor,
  checksFor,
  phaseOf,
  stopsFor,
  type Draft,
  type FlowFacts,
  type PullFacts,
} from "../src/apps/fleet/addMachine/flow";
import type { AddMachineFlow } from "../src/apps/fleet/addMachine/useAddMachineFlow";
import type { ModelResponse } from "../src/apps/fleet/addMachine/useModelResponse";
import type { ModelPull } from "../src/apps/fleet/machines/models";
import { machineFromRow } from "../src/apps/fleet/rows";
import { SetupFactsScope } from "../src/apps/setup/SetupFactsScope";
import { CoreGate } from "../src/chrome/CoreGate";
import { OwnershipWizard } from "../src/auth/OwnershipWizard";
import { EntryLayout } from "../src/kit/EntryLayout";
import { fakeConnection as clusterConnection, passkeyRow, withSession as clusterSession } from "../test/cluster/harness";
import { withSession } from "../test/deployables/harness";
import { machineRow } from "../test/fleet/harness";
import { installSeededAccess } from "../test/seededAccess";
import { coreAt, withOs } from "../test/setup/harness";

// SETUP, AS PIXELS (memql#6214): the add-a-machine wizard in each phase a
// person passes through, and the first-run gate's inference choice.
//
// The wizard is rendered from a FIXTURE FLOW rather than by driving the hook
// through a mint and a registration: every reading here is a pure function of
// facts (flow.ts), so the scene states the facts and the page draws whatever
// the current functions say about them. A capture of a phase is then a capture
// of exactly that phase, with no clock or feed deciding which one landed.

type Frame = ComponentType<{ fallback: string; children: ReactNode }>;

const NOW = new Date();
const ago = (seconds: number) => new Date(NOW.getTime() - seconds * 1000).toISOString();

const LOCAL: Draft = { name: "studio-mac-mini", platform: "mac", computerUse: false, inference: true, workbench: false, userLocal: false };
const TOOLS: Draft = { ...LOCAL, inference: false };
// Display-only placeholder: deliberately not shaped like an issued worker credential.
const MINT = { token: "mql_wkr_EXAMPLE_ONLY", identityId: "v1:identity:identity:tok-studio" };

const APPS_REPORTED = [
  { id: "claude-code", allowed: true, signedIn: true, version: "2.1.302" },
  { id: "codex", allowed: true, signedIn: true, version: "0.162.1" },
];

function machine(labels: Record<string, string>, runtimes: { name: string; version: string }[] = [], apps: Record<string, unknown>[] = APPS_REPORTED, extra: Record<string, unknown> = {}) {
  return machineFromRow(machineRow({
    id: "v1:worker:registration:studio",
    identityId: MINT.identityId,
    name: "studio-mac-mini.local",
    displayName: "studio-mac-mini",
    platformInfo: { os: "darwin", arch: "arm64", hostname: "studio-mac-mini.local" },
    capabilities: ["HEADLESS"],
    buildTag: "headless",
    version: "0.20.0",
    registeredAt: ago(95),
    lastSeenAt: ago(4),
    rttMs: 11,
    rttAt: ago(4),
    labels,
    apps,
    hardware: { chip: "Apple M4 Max", memoryBytes: 38654705664, cpuCores: 14, diskFreeBytes: 729e9, runtimes, reportedAt: ago(4) },
    ...extra,
  }));
}

// THE INSTALLER'S OWN REPORT (memql#6229): the install command keeps working
// after the machine connects, and these are the states it reports.
const LOCAL_WB: Draft = { ...LOCAL, workbench: true };
const GIB = 1024 ** 3;
function installer(state: string, stages: Record<string, unknown>[]) {
  return { setup: { state, stages, updatedAt: ago(2), reportedAt: ago(2) } };
}

const PULL: ModelPull = {
  pullId: "pull-1",
  workerId: "v1:worker:registration:studio",
  model: "qwen3.5:9b",
  status: "running",
  statusLine: "pulling 4a1c7b9e",
  layer: "sha256:4a1c7b9e",
  completedBytes: 2_480_000_000,
  totalBytes: 6_600_000_000,
  readvertised: false,
  errorMessage: "",
  requestedAt: ago(70),
  updatedAt: ago(1),
  endedAt: "",
};

const FAILED: ModelPull = {
  ...PULL,
  pullId: "pull-0",
  status: "failed",
  statusLine: "",
  completedBytes: 0,
  totalBytes: 0,
  errorMessage: "Ollama is not running on this machine (connection refused on 127.0.0.1:11434).",
  endedAt: ago(30),
};

interface Scene {
  draft: Draft;
  mint?: boolean;
  machine?: ReturnType<typeof machine> | null;
  beats?: number;
  pulls?: PullFacts;
  response?: ModelResponse;
  pulling?: boolean;
  pullError?: string;
}

function fixtureFlow(scene: Scene): AddMachineFlow {
  const facts: FlowFacts = {
    draft: scene.draft,
    connected: true,
    minting: false,
    mintError: "",
    mint: scene.mint ? MINT : null,
    mintedAt: scene.mint ? new Date(NOW.getTime() - 42_000) : null,
    machine: scene.machine ?? null,
    beats: scene.beats ?? 0,
    cancelAsked: false,
    leaveAsked: false,
    revokeError: "",
    revoking: false,
    now: NOW,
  };
  const checks = facts.machine === null ? [] : checksFor(scene.draft, facts.machine, facts.beats, NOW, scene.pulls, scene.response);
  const noop = () => {};
  return {
    active: true,
    draft: scene.draft,
    phase: phaseOf(facts),
    facts,
    checks,
    stops: stopsFor(facts, checks),
    bar: barFor(facts, checks, scene.pulling === true),
    renameError: "",
    pullRecommended: async () => {},
    pulling: scene.pulling === true,
    pullError: scene.pullError ?? "",
    retryResponse: noop,
    start: noop,
    setDraft: noop,
    mint: async () => {},
    cancel: noop,
    keepWaiting: noop,
    askLeave: noop,
    leaveKeepToken: noop,
    revokeAndLeave: async () => {},
    finish: () => "",
  };
}

const RUNTIME = [{ name: "ollama", version: "0.12.6" }];
const SERVED = { "runtime:ollama": "0.12.6", "model:qwen3.5:9b": "ctx=262144,params=9653104368,quant=Q4_K_M,tools=1,structured=1", "model:qwen3-embedding:0.6b": "embeddings=1" };

const SCENES: Record<string, Scene> = {
  "add-machine-describe": { draft: LOCAL },
  "add-machine-waiting": { draft: LOCAL, mint: true },
  "add-machine-runtime": { draft: LOCAL, mint: true, machine: machine({}), beats: 2, pulls: { live: null } },
  "add-machine-pulling": { draft: LOCAL, mint: true, machine: machine({ "runtime:ollama": "0.12.6" }, RUNTIME), beats: 2, pulls: { live: PULL } },
  "add-machine-failed": { draft: LOCAL, mint: true, machine: machine({ "runtime:ollama": "0.12.6" }, RUNTIME), beats: 2, pulls: { live: null, failed: FAILED } },
  "add-machine-refused": { draft: LOCAL, mint: true, machine: machine({ "runtime:ollama": "0.12.6" }, RUNTIME), beats: 2, pulls: { live: null, startError: "The recommended downloads exceed free storage: 31.2 GB needed, 24.0 GB free." } },
  "add-machine-ready": { draft: LOCAL, mint: true, machine: machine(SERVED, RUNTIME), beats: 3, pulls: { live: null }, response: { state: "passed" } },
  "add-machine-tools-ready": { draft: TOOLS, mint: true, machine: machine({}), beats: 3, pulls: { live: null } },
  "add-machine-installer-runtime": { draft: LOCAL_WB, mint: true, beats: 2, pulls: { live: null }, machine: machine({}, [], APPS_REPORTED, installer("running", [
    { name: "runtime", state: "running" }, { name: "models", state: "waiting" }, { name: "workbench", state: "waiting" },
  ])) },
  "add-machine-installer-downloading": { draft: LOCAL_WB, mint: true, beats: 2, pulls: { live: null }, machine: machine({ "runtime:ollama": "0.12.6" }, RUNTIME, APPS_REPORTED, installer("running", [
    { name: "runtime", state: "done" },
    { name: "models", state: "running", model: "qwen3.8:27b", completedBytes: 6.1 * GIB, totalBytes: 18.2 * GIB, modelsDone: 0, modelsTotal: 2 },
    { name: "workbench", state: "waiting" },
  ])) },
  "add-machine-installer-workbench": { draft: LOCAL_WB, mint: true, beats: 2, pulls: { live: null }, machine: machine(SERVED, RUNTIME, APPS_REPORTED, installer("running", [
    { name: "runtime", state: "done" }, { name: "models", state: "done" }, { name: "workbench", state: "running" },
  ])) },
  "add-machine-installer-workbench-failed": { draft: LOCAL_WB, mint: true, beats: 3, pulls: { live: null }, response: { state: "passed" }, machine: machine(SERVED, RUNTIME, APPS_REPORTED, installer("finished", [
    { name: "runtime", state: "done" }, { name: "models", state: "done" },
    { name: "workbench", state: "failed", detail: "Start Docker with Linux containers, then retry: Cannot connect to the Docker daemon. Is the docker daemon running?" },
  ])) },
};

export function setupWizardScenes(Frame: Frame) {
  const views = Object.fromEntries(Object.entries(SCENES).map(([name, scene]) => [name, {
    wrap: (el: JSX.Element) => withSession(el, { role: "owner" }),
    render: () => (
      <Frame fallback="Machines">
        <AddMachinePage flow={fixtureFlow(scene)} onLeave={() => {}} />
      </Frame>
    ),
  }]));
  return {
    ...views,
    // TAKING OWNERSHIP, the first thing a fresh local cluster asks: the same
    // wizard frame, every step with its subtitle.
    "setup-ownership": {
      wrap: (el: JSX.Element) => el,
      render: () => (
        <EntryLayout>
          <OwnershipWizard
            data={{ Local: true, PrefillDomain: "memql.znas.localhost", PrefillOrgName: "ZNAS", PrefillOwnerFirstName: "Ana", PrefillOwnerLastName: "Ruiz", PrefillOwnerEmail: "ana@example.com" }}
            busy={false}
            error=""
            submit={() => {}}
          />
        </EntryLayout>
      ),
    },
    // THE GATE, mounted the way Shell.tsx mounts it: the facts scope, the shell
    // provider, an owner, and a cluster that cannot reach a model yet.
    "setup-gate": {
      connect: () => clusterConnection({ passkeysForSelf: [passkeyRow({ id: "pk-1" })] }),
      wrap: (el: JSX.Element) => el,
      render: () => {
        installSeededAccess("owner");
        return clusterSession(
          <SetupFactsScope>
            {withOs(<CoreGate onSignOut={() => {}}><p>desk</p></CoreGate>, "owner")}
          </SetupFactsScope>,
          { role: "owner", readiness: coreAt("unconfigured", "configured", "configured") },
        );
      },
    },
  };
}
