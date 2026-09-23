import type { ReactNode } from "react";
import { vi } from "vitest";
import { Result, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useAddMachineFlow } from "../../src/apps/fleet/addMachine/useAddMachineFlow";
import { MachinesSection } from "../../src/apps/fleet/machines/MachinesSection";
import { SessionProvider } from "../../src/chrome/access";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";

// The Fleet's test harness: a connection-shaped double, and the session the
// app reads its actor and cluster domain from.
//
// The double is deliberately CONNECTION-SHAPED rather than a mocked hook per
// call site. Every read in this app goes through `connection.query.<generated
// method>` and every subscription through `connection.subscriptions`, so a
// fake that answers those two is exercising the real LiveCollection, the real
// retain/seed path and the real projections -- which is where the behaviour
// under test actually lives. `subscriptions: null` is a supported shape (the
// collection guards for it and simply has no liveness), which is what lets a
// jsdom test seed a live list without a server.

export function rowsResult(rows: Row[]): Result {
  // Result reads its rows off `data` -- the envelope the engine returns for a
  // shape-projected query -- so this is the wire's own shape rather than a
  // convenience the class does not have.
  return new Result({ data: rows } as never);
}

/**
 * What a top-level `builtin X(...)` answers ON THE WIRE: ONE data value keyed
 * by node id, each value the node envelope with the handler's fields under
 * `payload`, stamped with DECREASING createdAt in slice order the way the
 * engine's PreserveOrder path does (executor_builtin.go).
 *
 * NOT `rowsResult`. A fake answering flat rows for a builtin passes every test
 * against a shape the engine never sends -- which is how the Logs app and
 * Connect GitHub shipped reading nothing. Copied from the Deployables harness,
 * which found that out first.
 */
export function builtinReply(name: string, rows: Row[]): Result {
  const wrapper: Record<string, unknown> = {};
  rows.forEach((row, index) => {
    const own = typeof row["id"] === "string" && row["id"] !== "" ? (row["id"] as string) : name;
    const id = own in wrapper ? `${own}-${index}` : own;
    wrapper[id] = {
      id,
      concept: `integration:test:${name}`,
      type: "object",
      createdAt: `2026-01-01T00:00:00.${String(999_999_999 - index).padStart(9, "0")}Z`,
      payload: row,
    };
  });
  return new Result({ data: rows.length === 0 ? [] : [wrapper] } as never);
}

/**
 * The one row `fleetShareDirectory` answers (epic memql#5344, design section
 * 4): who the caller may lend this machine to, and who is already on it.
 *
 * EMPTY BY DEFAULT, and that is a real answer rather than a placeholder: a
 * person in no group, below admin rank, is offered nobody. Pass `people`,
 * `groups` and `current` to model anything else. Ids are BARE, as the engine
 * sends them.
 */
export function shareDirectoryRow(over: Partial<Row> = {}): Row {
  return {
    id: "v1:worker:registration:live",
    machineId: "v1:worker:registration:live",
    everyone: false,
    people: [],
    groups: [],
    current: { people: [], groups: [] },
    ...over,
  };
}

/**
 * What `fleetSetSharing` answers when the write lands: the receipt row, in the
 * engine's own sentence (fleet_machine_acts.go, sharingReceiptSentence -- the
 * machine-half clause is omitted because this fake does not know what the
 * cockpit said).
 */
export function sharingReceiptRow(args: {
  registrationId: string;
  mode: string;
  userIds?: string[];
  groupIds?: string[];
}): Row {
  const people = args.mode === "people" ? (args.userIds ?? []).length : 0;
  const groups = args.mode === "people" ? (args.groupIds ?? []).length : 0;
  const count = [
    people === 1 ? "1 person" : people > 1 ? `${people} people` : "",
    groups === 1 ? "1 group" : groups > 1 ? `${groups} groups` : "",
  ]
    .filter(Boolean)
    .join(" and ");
  return {
    id: args.registrationId,
    machineId: args.registrationId,
    mode: args.mode,
    people,
    groups,
    sentence:
      args.mode === "people"
        ? `Lent to ${count}.`
        : args.mode === "cluster"
          ? "Offered to everyone on this cluster, and to the cluster's own work."
          : "Kept to you. Nobody else's work will run on this machine.",
  };
}

export interface FakeQuery {
  myWorkersWithStatus: ReturnType<typeof vi.fn>;
  myRoutingPolicies: ReturnType<typeof vi.fn>;
  myWorkspaces: ReturnType<typeof vi.fn>;
  clusterNodes: ReturnType<typeof vi.fn>;
  invocationsForWorker: ReturnType<typeof vi.fn>;
  /** The per-machine model pulls (epic memql#5103). LIVE, unlike the two
   *  Models-section reads beside it: v1:worker:modelPull is a real row with a
   *  broadcast routing rule, so this seeds a collection the fake
   *  subscriptions can then fold events into. */
  modelPullsForWorker: ReturnType<typeof vi.fn>;
  /**
   * The Models SECTION's two readings (epic memql#5096) -- the fleet catalog
   * and which doors this cluster can reach.
   *
   * NEITHER IS LIVE and neither is a graph row: both are virtual projections
   * computed per request, so they are plain on-demand reads rather than seeds
   * behind a subscription. They are here because a `FakeQuery` missing them
   * makes the whole section unmountable -- `useInference` calls
   * `connection.query.fleetModels(...)` and an absent key is a TypeError, not
   * an empty read -- which is why nothing mounted that section until the acts
   * sweep needed to (memql#5159).
   */
  fleetModels: ReturnType<typeof vi.fn>;
  inferenceStatus: ReturnType<typeof vi.fn>;
  // The Apps section's reads (epic memql#5009). Neither concept broadcasts,
  // so both are on-demand reads rather than seeds behind a subscription.
  delegationPolicyForUser: ReturnType<typeof vi.fn>;
  appSessionsForUser: ReturnType<typeof vi.fn>;
  appSessionById: ReturnType<typeof vi.fn>;
  renameWorker: ReturnType<typeof vi.fn>;
  setWorkerOperatorLabels: ReturnType<typeof vi.fn>;
  fleetRevokeMachine: ReturnType<typeof vi.fn>;
  createRoutingPolicy: ReturnType<typeof vi.fn>;
  updateRoutingPolicy: ReturnType<typeof vi.fn>;
  setDelegationPolicy: ReturnType<typeof vi.fn>;
  fleetModelPull: ReturnType<typeof vi.fn>;
  // The scanner's reads and acts (epic memql#5146). None of the three reads is
  // live: `fleetRecommended` and `fleetSharingLedger` are virtual projections
  // with no graph event behind them, and a measurement changes when somebody
  // presses Probe and at no other time -- a fact with a date rather than an
  // event with a duration.
  fleetRecommended: ReturnType<typeof vi.fn>;
  measurementsForMachine: ReturnType<typeof vi.fn>;
  fleetSharingLedger: ReturnType<typeof vi.fn>;
  fleetPullRecommended: ReturnType<typeof vi.fn>;
  fleetModelProbe: ReturnType<typeof vi.fn>;
  /** The owner's half of the consent (epic memql#5344). Answers the RECEIPT
   *  row, in the builtin wire shape, built from what was sent. */
  fleetSetSharing: ReturnType<typeof vi.fn>;
  /** Who the owner may lend a machine to. On demand, never live: a virtual
   *  row computed per request, read once each time the share dialog opens. */
  fleetShareDirectory: ReturnType<typeof vi.fn>;
  // The shared attention service's two calls (src/attention), so a suite can
  // mount the real AttentionProvider over this connection: the receipts it
  // seeds from, and the write that records a change as seen.
  myAttentionReceipts: ReturnType<typeof vi.fn>;
  acknowledgeAttention: ReturnType<typeof vi.fn>;
}

// The subscription seam, faithful to the one bit of it a collection uses:
// `subscribeGraph(handler, opts)` returning an unregister. `emit` is the
// test's hand on the wire.
//
// WITHOUT THIS EVERY TEST IS SEED-ONLY, and a seed-only test cannot see the
// fold -- which is the half of a live surface that runs for the rest of the
// session. That gap hid a real defect: the collection upserts a folded
// event's payload AS THE ROW TYPE, with no projection, so a collection typed
// with a projected row holds raw wire rows the moment anything updates.
export interface FakeSubscriptions {
  subscribeGraph: (handler: (event: FakeEvent) => void, opts: { concept?: string }) => () => void;
  /** Push an event to every handler subscribed to `concept`. */
  emit: (concept: string, payload: Row, kind?: string) => void;
}

export interface FakeEvent {
  subscriptionId: string;
  kind: string;
  timestamp: Date | null;
  payload: Row | null;
  payloadOmitted: boolean;
  seq: number;
  gapBefore: boolean;
}

export interface FakeConnection {
  query: FakeQuery;
  subscriptions: FakeSubscriptions;
  dispatcher: { sendAndWait: ReturnType<typeof vi.fn> };
}

function fakeSubscriptions(): FakeSubscriptions {
  const handlers = new Map<string, Set<(event: FakeEvent) => void>>();
  return {
    subscribeGraph(handler, opts) {
      const concept = opts.concept ?? "*";
      const set = handlers.get(concept) ?? new Set();
      set.add(handler);
      handlers.set(concept, set);
      return () => set.delete(handler);
    },
    emit(concept, payload, kind = "NODE_UPDATED") {
      for (const handler of handlers.get(concept) ?? []) {
        handler({
          subscriptionId: "sub-1",
          kind,
          timestamp: new Date(),
          payload,
          payloadOmitted: false,
          seq: 0,
          gapBefore: false,
        });
      }
    },
  };
}

export function fakeConnection(seed: Partial<Record<keyof FakeQuery, Row[]>> = {}): FakeConnection {
  const read = (key: keyof FakeQuery) => vi.fn(async () => rowsResult(seed[key] ?? []));
  // A BUILTIN'S seed, answered in the builtin wire shape (see builtinReply).
  const builtin = (key: keyof FakeQuery) => vi.fn(async () => builtinReply(key, seed[key] ?? []));
  return {
    query: {
      myWorkersWithStatus: read("myWorkersWithStatus"),
      myRoutingPolicies: read("myRoutingPolicies"),
      myWorkspaces: read("myWorkspaces"),
      clusterNodes: read("clusterNodes"),
      invocationsForWorker: read("invocationsForWorker"),
      modelPullsForWorker: read("modelPullsForWorker"),
      fleetModels: builtin("fleetModels"),
      inferenceStatus: builtin("inferenceStatus"),
      delegationPolicyForUser: read("delegationPolicyForUser"),
      appSessionsForUser: read("appSessionsForUser"),
      appSessionById: read("appSessionById"),
      renameWorker: vi.fn(async () => rowsResult([])),
      setWorkerOperatorLabels: vi.fn(async () => rowsResult([])),
      // The removal builtin answers a RECEIPT row (epic memql#5327, design
      // D2): a removal is two writes -- the registration and the credential --
      // and the surface has to be able to say which of them landed.
      fleetRevokeMachine: vi.fn(async () =>
        builtinReply("fleetRevokeMachine", [
          {
            machineId: "v1:worker:registration:live",
            registrationState: "revoked",
            credentialState: "revoked",
            alreadyRevoked: false,
            sentence:
              "Removed. The machine is out of the fleet and its credential no longer connects.",
          },
        ]),
      ),
      createRoutingPolicy: vi.fn(async () => rowsResult([])),
      updateRoutingPolicy: vi.fn(async () => rowsResult([])),
      setDelegationPolicy: vi.fn(async () => rowsResult([])),
      fleetModelPull: vi.fn(async () => builtinReply("fleetModelPull", [])),
      fleetRecommended: builtin("fleetRecommended"),
      measurementsForMachine: read("measurementsForMachine"),
      // EVERY BUILTIN HERE ANSWERS IN THE BUILTIN WIRE SHAPE (builtinReply);
      // only the shaped queries and the mutations answer flat rows.
      fleetSharingLedger: builtin("fleetSharingLedger"),
      fleetPullRecommended: vi.fn(async () => builtinReply("fleetPullRecommended", [])),
      fleetModelProbe: vi.fn(async () => builtinReply("fleetModelProbe", [])),
      fleetSetSharing: vi.fn(
        async (args: { registrationId: string; mode: string; userIds?: string[]; groupIds?: string[] }) =>
          builtinReply("fleetSetSharing", [sharingReceiptRow(args)]),
      ),
      fleetShareDirectory: vi.fn(async () =>
        builtinReply("fleetShareDirectory", seed.fleetShareDirectory ?? [shareDirectoryRow()]),
      ),
      myAttentionReceipts: read("myAttentionReceipts"),
      acknowledgeAttention: vi.fn(async () => rowsResult([])),
    },
    subscriptions: fakeSubscriptions(),
    dispatcher: { sendAndWait: vi.fn() },
  };
}

/**
 * The Machines section as FleetApp mounts it: the guided install's flow is
 * held ABOVE the section (design record 2026-09-08-cockpit-install-wizard,
 * D7), so a suite mounting the section alone has to hold it the same way.
 * Must sit inside a MachinesProvider and a session.
 */
export function MachinesWithFlow({
  showRevoked = false,
  intent,
  consumeIntent,
}: {
  showRevoked?: boolean;
  intent?: { id: string; payload: Record<string, unknown> };
  consumeIntent?: (intentId: string) => void;
}) {
  const flow = useAddMachineFlow();
  return (
    <MachinesSection showRevoked={showRevoked} flow={flow} intent={intent} consumeIntent={consumeIntent} />
  );
}

export function withSession(
  children: ReactNode,
  overrides: { userId?: string; domain?: string } = {},
) {
  const config: OsRuntimeConfig = {
    ...UNKNOWN_RUNTIME_CONFIG,
    domain: overrides.domain ?? "memql.example.com",
  };
  return (
    <SessionProvider
      value={{
        access: {
          userId: overrides.userId ?? "v1:identity:user:me",
          primaryEmail: "me@example.com",
          role: "owner",
          roleName: "",
          rank: 0,
        },
        config,
      }}
    >
      {children}
    </SessionProvider>
  );
}

/** A `v1:worker:modelPull` row with sane defaults (epic memql#5103).
 *
 *  `totalBytes` IS ZERO BY DEFAULT, which is the state most easily got wrong:
 *  a runtime that stated no size for the step did not state a size of zero,
 *  and the difference is a progress bar that is absent rather than empty. Pass
 *  one explicitly to model a step whose size the runtime DID report. */
export function modelPullRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    workerId: "laptop",
    model: "llama3.1:8b",
    status: "running",
    statusLine: "",
    layer: "",
    completedBytes: 0,
    totalBytes: 0,
    readvertised: false,
    errorMessage: "",
    targetNodeId: "agent-0",
    requestedAt: "2026-09-07T12:00:00Z",
    updatedAt: "2026-09-07T12:00:00Z",
    endedAt: "",
    ...over,
  };
}

/** A registration row with sane defaults, overridable field by field. */
export function machineRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    name: "host.local",
    displayName: "",
    platformInfo: { os: "darwin", arch: "arm64", hostname: "host.local" },
    labels: {},
    operatorLabels: {},
    concurrency: { HEADLESS: 4 },
    activeCount: 0,
    registeredAt: "2026-08-01T00:00:00Z",
    connectedNodeId: "agent-test",
    lastSeenAt: new Date().toISOString(),
    ...over,
  };
}

/**
 * A delegated app-session row (epic memql#5009).
 *
 * `usage` IS ABSENT BY DEFAULT, which is the state most easily got wrong: an
 * app that reported nothing did not report zero, and the reading has to keep
 * those apart all the way to the pixel. Pass one explicitly to model an app
 * that DID report.
 */
export function appSessionRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    workerId: "v1:worker:registration:laptop",
    app: "claude-code",
    kind: "run",
    runId: "",
    stepId: "",
    status: "ended",
    billing: "subscription",
    startedAt: "2026-09-01T09:00:00Z",
    endedAt: "2026-09-01T09:04:00Z",
    ...over,
  };
}

/** A delegation policy row. ABSENT is the common case -- pass no row at all
 *  for it, rather than one of these with the switch off: they are different
 *  facts about a person. */
export function delegationPolicyRow(over: Partial<Row> = {}): Row {
  return {
    id: "v1:worker:delegationPolicy:v1-identity-user-me",
    ownerUserId: "v1:identity:user:me",
    preferSubscriptionApps: true,
    eligibleKinds: ["runCommand"],
    appOrder: ["claude-code"],
    maxConcurrentSessions: 2,
    workspaceRoot: "/Users/ana/memql-workspaces",
    credentialLifetimeSeconds: 14400,
    updatedAt: "2026-09-01T08:00:00Z",
    ...over,
  };
}
