import { Result, type Row } from "@znasllc-io/memql-sdk-core/client";
import type { ComponentType, ReactNode } from "react";

import { ClusterApp } from "../src/apps/cluster/ClusterApp";
import { ConnectionStatusContext } from "../src/chrome/connection";

// Cluster > Settings > Memory search (design record 2026-11-23, section 8), in
// the readings worth judging as pixels. Fixtures only: the six memorySearch*
// builtins are answered here, and an act changes nothing anywhere.
//
//   memory-search            the model in use on your machines, a backlog, the
//                            choices, and the previous model's kept index.
//                            `&click=nomic-embed-text` opens the confirmation;
//                            `&click=text-embedding-3-small` shows its cost line.
//   memory-search-switching  a switch re-indexing, with Cancel
//   memory-search-waiting    the same switch waiting for a machine
//   memory-search-decision   a switch held for Switch anyway or Cancel
//   memory-search-vendor     a vendor model nobody chose, asked once
//   memory-search-takeover   another owner's machines cannot index it
//   memory-search-unbound    no model chosen yet
//   memory-search-loading    the first read still out
//   memory-search-error      the first read refused
//   memory-search-settings   Cluster > Settings and its link row

const QWEN = "fleet:qwen3-embedding:0.6b";
const NOMIC = "fleet:nomic-embed-text";
const VENDOR = "embedding3Small";

const day = 24 * 60 * 60 * 1000;

function status(over: Row = {}): Row {
  return {
    id: "current", providerRef: QWEN, kind: "machine", label: "qwen3-embedding:0.6b",
    indexedBy: "u-me", indexedByName: "Ana Owner", chosenBy: "u-me", needsConfirmation: false,
    previousRef: "", takeOver: false, indexed: 8310, backlog: 0, backlogCountedAt: "", waitingReason: "",
    switch: null, retained: [], ...over,
  };
}

function switchRun(over: Row = {}): Row {
  return {
    state: "indexing", targetRef: NOMIC, targetLabel: "nomic-embed-text",
    counts: { total: 1300, done: 1200, skipped: 40, unsourced: 0, failed: 60 },
    startedAt: new Date(Date.now() - 20 * 60 * 1000).toISOString(), startedBy: "u-me", waitingReason: "", estimatedTotal: 8310, ...over,
  };
}

const CHOICES: Row[] = [
  { id: QWEN, providerRef: QWEN, kind: "machine", label: "qwen3-embedding:0.6b", machines: 2, paidByCluster: false, available: true, hint: "2 machines offering it are online" },
  { id: NOMIC, providerRef: NOMIC, kind: "machine", label: "nomic-embed-text", machines: 1, paidByCluster: false, available: true, hint: "1 machine offering it is online" },
  { id: "fleet:bge-m3", providerRef: "fleet:bge-m3", kind: "machine", label: "bge-m3", machines: 1, paidByCluster: false, available: false, hint: "None of the machines offering it is online" },
  { id: VENDOR, providerRef: VENDOR, kind: "vendor", label: "text-embedding-3-small", machines: 0, paidByCluster: true, available: true, hint: "Paid by the cluster's vendor account" },
];

const VENDOR_STATUS = { providerRef: VENDOR, kind: "vendor", label: "text-embedding-3-small", indexedBy: "", indexedByName: "", chosenBy: "" };

type Answer = Row | Error | "pending";

const SCENES: Record<string, { status: Answer; section?: string }> = {
  "memory-search": { status: status({ backlog: 37, waitingReason: "none of the indexer's machines offering qwen3-embedding:0.6b is online",
    previousRef: NOMIC, retained: [{ providerRef: NOMIC, retireAt: new Date(Date.now() + 29 * day).toISOString() }],
    switch: switchRun({ state: "done", targetRef: QWEN, targetLabel: "qwen3-embedding:0.6b" }) }) },
  "memory-search-switching": { status: status({ switch: switchRun() }) },
  "memory-search-waiting": { status: status({ switch: switchRun({ state: "waiting", waitingReason: "none of the indexer's machines offering nomic-embed-text is online" }) }) },
  "memory-search-decision": { status: status({ switch: switchRun({ state: "needsDecision", counts: { total: 8310, done: 8305, skipped: 0, unsourced: 2, failed: 3 } }) }) },
  "memory-search-vendor": { status: status({ ...VENDOR_STATUS, needsConfirmation: true }) },
  "memory-search-takeover": { status: status({ indexedBy: "ana", indexedByName: "Ana Owner", takeOver: true, backlog: 12,
    waitingReason: "the owner who chose this model is no longer an owner; an owner can choose again" }) },
  "memory-search-unbound": { status: status({ providerRef: "", kind: "", label: "", indexedBy: "", indexedByName: "", chosenBy: "", indexed: 0 }) },
  "memory-search-loading": { status: "pending" },
  "memory-search-error": { status: new Error("memorySearchStatus: capability_not_held: read app:cluster/memorysearch") },
  "memory-search-settings": { status: status(), section: "settings" },
};

const rows = (list: Row[]) => new Result({ data: list } as never);

function connection(answer: Answer) {
  const read = async () => {
    if (answer === "pending") return new Promise<Result>(() => {});
    if (answer instanceof Error) throw answer;
    return rows([answer]);
  };
  const act = async () => read();
  return {
    subscriptions: null,
    query: {
      memorySearchStatus: read,
      memorySearchChoices: async () => rows(CHOICES),
      memorySearchSwitch: act,
      memorySearchCancel: act,
      memorySearchSwitchAnyway: act,
      memorySearchConfirm: act,
    },
  };
}

type Frame = ComponentType<{ fallback: string; children: ReactNode }>;

export function memorySearchScenes(Frame: Frame) {
  return Object.fromEntries(
    Object.entries(SCENES).map(([name, scene]) => [
      name,
      {
        connect: () => connection(scene.status),
        framed: true,
        render: () => (
          <Frame fallback="Cluster">
            <ConnectionStatusContext.Provider value="connected">
              <ClusterApp sectionId={scene.section ?? "memorysearch"} navigate={() => {}} askContext={() => {}} />
            </ConnectionStatusContext.Provider>
          </Frame>
        ),
      },
    ]),
  );
}
