import { Result, type Row } from "@znasllc-io/memql-sdk-core/client";
import type { ComponentType, JSX, ReactNode } from "react";

import { ProfileSection } from "../src/apps/identity/ProfileSection";
import { OS_REGISTRY } from "../src/apps/registry";
import { SessionProvider } from "../src/chrome/access";
import { OsProvider } from "../src/chrome/state";
import { UNKNOWN_RUNTIME_CONFIG } from "../src/cluster/config";
import { installSeededAccess } from "../test/seededAccess";

// Your profile (memql#6220), in the readings worth judging as pixels:
// `profile` is the control -- a person with a budget and a personal storage
// allowance and no photo yet; `profile-photo` has the photo; `profile-quiet`
// is a cluster that serves no budget, an account with no personal limit,
// passkey-only sign-in and computer use switched off.

const PHOTO = "data:image/png;base64,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";

const rows = (list: Row[]) => new Result({ data: list } as never);

interface Scene {
  me: Row;
  storage: Row | Error;
  budget: Row | Error;
  passkeys: Row[];
}

const ME: Row = {
  id: "v1:identity:user:ada",
  displayName: "Ada Lovelace",
  firstName: "Ada",
  lastName: "Lovelace",
  primaryEmail: "ada@example.com",
  role: "owner",
  phone: "",
  primaryRole: "",
  avatar: "",
  signInPolicy: "any",
  preferences: { timezone: "Europe/London" },
};

const SCENES: Record<string, Scene> = {
  profile: {
    me: ME,
    storage: { used: 23_400_000_000, reserved: 0, limit: 100_000_000_000, available: 76_600_000_000, clusterUsed: 0, clusterReserved: 0, clusterLimit: 5e12, exempt: false },
    budget: { monthlyLimitUSD: 50, spentThisMonthUSD: 12.4, resetsAt: "2026-11-01T00:00:00Z" },
    passkeys: [{ id: "p1", active: true }, { id: "p2", active: true }],
  },
  "profile-photo": {
    me: { ...ME, avatar: PHOTO, phone: "+44 20 7946 0958", primaryRole: "Engineering lead" },
    storage: { used: 91_000_000_000, reserved: 2_500_000_000, limit: 100_000_000_000, available: 6_500_000_000, clusterUsed: 0, clusterReserved: 0, clusterLimit: 5e12, exempt: false },
    budget: { monthlyLimitUSD: null, spentThisMonthUSD: 0, resetsAt: "2026-11-01T00:00:00Z" },
    passkeys: [{ id: "p1", active: true }],
  },
  "profile-quiet": {
    me: { ...ME, signInPolicy: "passkey_only", preferences: { computerUseEnabled: false } },
    storage: { used: 410_000_000, reserved: 0, limit: 0, available: 4_100_000_000_000, clusterUsed: 0, clusterReserved: 0, clusterLimit: 5e12, exempt: true },
    budget: new Error('function "myBudget" not found'),
    passkeys: [{ id: "p1", active: true }, { id: "p2", active: true }, { id: "p3", active: true }],
  },
};

function connection(scene: Scene) {
  let me = scene.me;
  let budget = scene.budget;
  const answer = async <T,>(value: T | Error) => {
    if (value instanceof Error) throw value;
    return value;
  };
  return {
    subscriptions: null,
    query: {
      currentUser: async () => rows([me]),
      libraryBackupUsage: async () => rows([await answer(scene.storage)]),
      passkeysForSelf: async () => rows(scene.passkeys),
      updateMyName: async (args: Row) => { me = { ...me, ...args }; return rows([]); },
      setMyAvatar: async (args: Row) => { me = { ...me, avatar: args["avatar"] }; return rows([]); },
      clearMyAvatar: async () => { me = { ...me, avatar: "" }; return rows([]); },
      toggleComputerUseEnabled: async () => rows([]),
      updateMyPreferences: async () => rows([]),
      executeNamed: async (name: string, call: string) => {
        if (name === "myBudget") return rows([await answer(budget)]);
        const match = /monthlyLimitUSD: ([0-9.]+)/.exec(call);
        budget = { ...(budget as Row), monthlyLimitUSD: match ? Number(match[1]) : null };
        return rows([budget]);
      },
    },
  };
}

function session(children: ReactNode, role: string): JSX.Element {
  installSeededAccess(role);
  return (
    <SessionProvider value={{ access: { userId: "v1:identity:user:ada", primaryEmail: "ada@example.com", role, roleName: role === "owner" ? "Owner" : role, rank: 400 }, config: { ...UNKNOWN_RUNTIME_CONFIG, domain: "memql.example.com" } }}>
      <OsProvider registry={OS_REGISTRY} actorRole={role} grid={{ cols: 8, rows: 5 }}>
        {children}
      </OsProvider>
    </SessionProvider>
  );
}

type Frame = ComponentType<{ fallback: string; children: ReactNode }>;

export function profileScenes(Frame: Frame) {
  return Object.fromEntries(
    Object.entries(SCENES).map(([name, scene]) => [
      name,
      {
        connect: () => connection(scene),
        wrap: (el: JSX.Element, role: string) => session(el, role),
        render: () => <Frame fallback="Identity"><ProfileSection /></Frame>,
      },
    ]),
  );
}
