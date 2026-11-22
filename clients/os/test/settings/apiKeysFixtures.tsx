import { act, render } from "@testing-library/react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { ApiKeysSection } from "../../src/apps/settings/apikeys/ApiKeysSection";
import { SessionProvider } from "../../src/chrome/access";
import { UNKNOWN_RUNTIME_CONFIG } from "../../src/cluster/config";
import { setEffectiveCapabilities } from "../../src/system/roles";
import { installSeededAccess, seededAccessWithout } from "../seededAccess";
import { builtinReply, fakeConnection, type generatedQuery } from "../fleet/harness";

// The double behind Settings > API keys (design record
// 2026-11-21-vendor-accounts-and-api-keys, U1 to U3).
//
// EVERY VALUE HERE IS MADE UP. No fixture is a key: `TYPED_KEY` only has the
// prefix the engine's shape check asks for, and it never leaves this process.
//
// The double keeps the cluster's side of the story: a key that is added is in
// the next list, a removed one is gone from it, and a change is on the card
// that comes back. A surface that did not read again after a write would show
// the old world, and a double that returned the seed for ever would pass it.

/** What a person types into the wizard's key field. */
export const TYPED_KEY = "sk-ant-typed-in-a-test-0001";

/** The owner's card of one of their own keys, as `vendorKeysForActor` answers. */
export function keyCard(over: Row = {}): Row {
  return {
    id: "k1",
    vendor: "anthropic",
    label: "Personal",
    fingerprint: "...a1b2",
    isDefault: true,
    autoUse: false,
    checkedBy: "list",
    status: "active",
    state: "good",
    detail: "",
    addedAt: "2026-11-01T10:00:00Z",
    checkedAt: "2026-11-20T04:40:00Z",
    lastUsedAt: "2026-11-21T09:00:00Z",
    heldUntil: "",
    lentTo: 0,
    ownerUserId: "me",
    ownerName: "Me",
    relation: "mine",
    ...over,
  };
}

/** The short card of a key lent to the reader. */
export function lentCard(over: Row = {}): Row {
  return { id: "k9", vendor: "openai", label: "Team", ownerUserId: "ben", ownerName: "Ben", state: "good", paused: false, relation: "lent", ...over };
}

/** One key's activity this month, as `vendorKeyActivity` answers. */
export function activityRow(over: Row = {}): Row {
  return { keyId: "k1", month: "2026-11", since: "2026-11-01T00:00:00Z", calls: 12, estimatedCost: 1.2, priced: true, people: [], ...over };
}

/** Who a key may be lent to, as `vendorKeyShareDirectory` answers. */
export function directoryRow(over: Row = {}): Row {
  return { id: "directory", people: [], groups: [], current: [], ...over };
}

export interface KeysSeed {
  mine?: Row[];
  lent?: Row[];
  /** Every active key on the cluster, for a cluster owner who asks. */
  all?: Row[];
  /** A key's activity, by key id. A key with none answers no calls. */
  activity?: Record<string, Row>;
  directory?: Row;
}

type Args = Record<string, unknown>;

/** The key builtins this double answers, each the SDK's own generated method behind a spy. */
type KeyCall =
  | "vendorKeysForActor"
  | "vendorKeyAdd"
  | "vendorKeyUpdate"
  | "vendorKeyCheck"
  | "vendorKeyRemove"
  | "vendorKeySetSharing"
  | "vendorKeyActivity"
  | "vendorKeyShareDirectory";

interface KeysState {
  mine: Row[];
  lent: Row[];
  all: Row[];
  failures: Map<string, Error[]>;
  /** Held replies: a call of this name waits until `release` is called. */
  held: Map<string, Array<() => void>>;
  hold: Set<string>;
  /** Fields the next added key's card carries on top of the usual ones. */
  nextAdded?: Row;
  /** What the next check finds. */
  nextChecked?: Row;
  /** The last sharing a key was saved with. */
  sharing?: Row;
}

/** The key builtins' double. `fail` makes the named call refuse with the given words, once. */
export function keysConnection(seed: KeysSeed = {}) {
  const state: KeysState = {
    mine: [...(seed.mine ?? [])],
    lent: [...(seed.lent ?? [])],
    all: [...(seed.all ?? [])],
    failures: new Map<string, Error[]>(),
    held: new Map<string, Array<() => void>>(),
    hold: new Set<string>(),
  };
  const refuse = (name: string) => {
    const queued = state.failures.get(name);
    if (queued && queued.length > 0) throw queued.shift()!;
  };
  const wait = async (name: string) => {
    if (!state.hold.has(name)) return;
    await new Promise<void>((resolve) => {
      const waiting = state.held.get(name) ?? [];
      waiting.push(resolve);
      state.held.set(name, waiting);
    });
  };
  const find = (id: unknown) => [...state.mine, ...state.all].find((card) => card["id"] === id);
  const change = (id: unknown, patch: Row): Row => {
    state.mine = state.mine.map((card) => (card["id"] === id ? { ...card, ...patch } : card));
    state.all = state.all.map((card) => (card["id"] === id ? { ...card, ...patch } : card));
    return find(id) ?? keyCard({ id: String(id), ...patch });
  };

  const connection = fakeConnection({}, {
    vendorKeysForActor: async (args: Args) => {
      await wait("vendorKeysForActor");
      refuse("vendorKeysForActor");
      return builtinReply("vendorKeysForActor", [{ id: "list", mine: state.mine, lent: state.lent, all: args["all"] === true ? state.all : [] }]);
    },
    vendorKeyAdd: async (args: Args) => {
      await wait("vendorKeyAdd");
      refuse("vendorKeyAdd");
      const vendor = String(args["vendor"]);
      const card = keyCard({
        id: `k-new-${state.mine.length + 1}`,
        vendor,
        label: String(args["label"]),
        fingerprint: "...9z9z",
        isDefault: !state.mine.some((held) => held["vendor"] === vendor),
        addedAt: "2026-11-22T12:00:00Z",
        checkedAt: "2026-11-22T12:00:00Z",
        lastUsedAt: "",
        ...(state.nextAdded ?? {}),
      });
      state.nextAdded = undefined;
      state.mine = [...state.mine, card];
      return builtinReply("vendorKeyAdd", [card]);
    },
    vendorKeyUpdate: async (args: Args) => {
      refuse("vendorKeyUpdate");
      const patch: Row = {};
      for (const field of ["label", "isDefault", "autoUse"]) if (args[field] !== undefined) patch[field] = args[field];
      if (patch["isDefault"] === true) {
        const vendor = find(args["keyId"])?.["vendor"];
        state.mine = state.mine.map((card) => (card["vendor"] === vendor ? { ...card, isDefault: false } : card));
      }
      return builtinReply("vendorKeyUpdate", [change(args["keyId"], patch)]);
    },
    vendorKeyCheck: async (args: Args) => {
      await wait("vendorKeyCheck");
      refuse("vendorKeyCheck");
      return builtinReply("vendorKeyCheck", [change(args["keyId"], { ...(state.nextChecked ?? { state: "good", detail: "" }), checkedAt: "2026-11-22T12:30:00Z" })]);
    },
    vendorKeyRemove: async (args: Args) => {
      refuse("vendorKeyRemove");
      const card = { ...(find(args["keyId"]) ?? keyCard({ id: String(args["keyId"]) })), status: "removed" };
      state.mine = state.mine.filter((held) => held["id"] !== args["keyId"]);
      state.all = state.all.filter((held) => held["id"] !== args["keyId"]);
      return builtinReply("vendorKeyRemove", [card]);
    },
    vendorKeySetSharing: async (args: Args) => {
      refuse("vendorKeySetSharing");
      const users = Array.isArray(args["userIds"]) ? args["userIds"] : [];
      const groups = Array.isArray(args["groupIds"]) ? args["groupIds"] : [];
      state.sharing = { mode: args["mode"], paused: args["paused"] === true, userIds: users, groupIds: groups, pausedUserIds: args["pausedUserIds"] ?? [], pausedGroupIds: args["pausedGroupIds"] ?? [] };
      return builtinReply("vendorKeySetSharing", [change(args["keyId"], { lentTo: args["mode"] === "people" ? users.length + groups.length : 0 })]);
    },
    vendorKeyActivity: async (args: Args) => {
      refuse("vendorKeyActivity");
      const id = String(args["keyId"]);
      return builtinReply("vendorKeyActivity", [seed.activity?.[id] ?? activityRow({ keyId: id, calls: 0, estimatedCost: 0 })]);
    },
    vendorKeyShareDirectory: async () => {
      refuse("vendorKeyShareDirectory");
      return builtinReply("vendorKeyShareDirectory", [seed.directory ?? directoryRow()]);
    },
  } as never);

  const query = connection.query as unknown as ReturnType<typeof generatedQuery<KeyCall>>;
  return {
    connection,
    query,
    state,
    /** The next call of this name is refused with these words. */
    fail(name: string, error: Error | string) {
      const queued = state.failures.get(name) ?? [];
      queued.push(typeof error === "string" ? new Error(error) : error);
      state.failures.set(name, queued);
    },
    /** Calls of this name wait until released. */
    hold(name: string) {
      state.hold.add(name);
    },
    async release(name: string) {
      state.hold.delete(name);
      const waiting = state.held.get(name) ?? [];
      state.held.set(name, []);
      await act(async () => {
        waiting.forEach((resolve) => resolve());
        for (let i = 0; i < 6; i++) await Promise.resolve();
      });
    },
    /** Every call that reached the wire, as its rendered text. */
    wire(): string[] {
      return query.executeNamed.mock.calls.map((call) => String(call[1]));
    },
  };
}

/** A dropped connection, as the dispatcher reports one. */
export function droppedConnection(): Error {
  return Object.assign(new Error("stream closed"), { reason: "transport" });
}

export async function settle() {
  await act(async () => {
    for (let i = 0; i < 8; i++) await Promise.resolve();
  });
}

/**
 * Settings > API keys as SettingsApp mounts it, as a role. `sessionRole` is the
 * role the session reports; it differs from `role` only for a person holding a
 * grant.
 */
export async function renderKeys(
  role = "owner",
  props: Partial<Parameters<typeof ApiKeysSection>[0]> = {},
  /** Resources an operator's deny takes away from this person. */
  withheld: string[] = [],
) {
  if (withheld.length === 0) installSeededAccess(role);
  else setEffectiveCapabilities(seededAccessWithout(role, ...withheld));
  const ui = (active: boolean) => (
    <SessionProvider
      value={{
        access: { userId: "v1:identity:user:me", primaryEmail: "me@example.com", role, roleName: "", rank: 0 },
        config: { ...UNKNOWN_RUNTIME_CONFIG, domain: "memql.example.com" },
      }}
    >
      <ApiKeysSection active={active} {...props} />
    </SessionProvider>
  );
  const view = render(ui(props.active ?? true));
  await settle();
  return { ...view, show: async (active: boolean) => { view.rerender(ui(active)); await settle(); } };
}
