import type { ReactNode } from "react";
import { act, fireEvent, render } from "@testing-library/react";
import { vi } from "vitest";
import { QueryClient, Result, type Row } from "@znasllc-io/memql-sdk-core/client";

import { SessionProvider } from "../../src/chrome/access";
import { OsProvider } from "../../src/chrome/state";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";
import { OS_REGISTRY } from "../../src/apps/registry";
import { LogsApp } from "../../src/apps/logs/LogsApp";
import {
  DEFAULT_LOGS_SETTINGS,
  type LogsSettings,
  type LogsSettingsStore,
} from "../../src/apps/logs/settings";
import { AppLogsSection, type AppLogsSectionProps } from "../../src/logs/AppLogsSection";

// The Logs surfaces' test harness (epic memql#4895).
//
// THE FAKE SITS UNDER `executeNamed`, NOT OVER THE GENERATED METHODS -- the
// Files and Deployables harnesses' rule, kept for the reason their headers
// state: a double that stubs `query.logsTail` records ARGUMENTS and never
// renders the call, so the generated builder (the thing that turns them into
// MemQL text the engine has to parse) would run in production and nowhere
// else. The stub answers at the funnel, so every test exercises the real
// builders and asserts on the STRING that reached the wire -- which is also
// how "each facet narrows the call" is a property rather than a hope.

export function rowsResult(rows: Row[]): Result {
  return new Result({ data: rows } as never);
}

/** An answer as rows, or as a function of the rendered call and how many
 *  times this construct has been asked -- so a test can answer a baseline
 *  and a cursor poll differently. */
export type Answer = Row[] | ((call: string, nth: number) => Row[]);

export interface FakeLogsSeed {
  tail?: Answer;
  search?: Answer;
  sources?: Row[];
  status?: Row;
  archive?: Row[];
  restore?: Row;
  record?: Row;
  /** Refusal sentences, keyed by construct name (e.g. logsArchiveRestore). */
  refuse?: Record<string, string>;
}

export interface FakeConnection {
  query: QueryClient;
  /** Every call string that reached the wire, in order. */
  calls: string[];
  callsNamed: (construct: string) => string[];
  subscriptions: { subscribeGraph: () => () => void };
}

export function fakeConnection(seed: FakeLogsSeed = {}): FakeConnection {
  const calls: string[] = [];
  const asked = new Map<string, number>();
  const answer = (from: Answer | undefined, name: string, call: string): Row[] => {
    const nth = (asked.get(name) ?? 0) + 1;
    asked.set(name, nth);
    if (from === undefined) return [];
    return typeof from === "function" ? from(call, nth) : from;
  };
  const stub = {
    executeNamed: vi.fn(async (name: string, call: string) => {
      calls.push(call);
      const refusal = seed.refuse?.[name];
      if (refusal !== undefined) throw new Error(refusal);
      if (call.startsWith("builtin logsTail(")) return rowsResult(answer(seed.tail, "logsTail", call));
      if (call.startsWith("builtin logsSearch(")) return rowsResult(answer(seed.search, "logsSearch", call));
      if (call.startsWith("builtin logsSources(")) return rowsResult(seed.sources ?? []);
      if (call === "builtin logsStatus()") return rowsResult(seed.status ? [seed.status] : []);
      if (call === "builtin logsArchiveList()") return rowsResult(seed.archive ?? []);
      if (call.startsWith("builtin logsArchiveRestore(")) return rowsResult(seed.restore ? [seed.restore] : []);
      if (call.startsWith("builtin logsRecordClient(")) {
        return rowsResult([seed.record ?? ({ accepted: 0, dropped: 0, reason: "" } as Row)]);
      }
      return rowsResult([]);
    }),
  };
  return {
    query: Object.setPrototypeOf(stub, QueryClient.prototype) as QueryClient,
    calls,
    callsNamed: (construct: string) => calls.filter((c) => c.includes(`${construct}(`)),
    subscriptions: { subscribeGraph: () => () => {} },
  };
}

export function withSession(
  children: ReactNode,
  overrides: { userId?: string; role?: string } = {},
) {
  const config: OsRuntimeConfig = { ...UNKNOWN_RUNTIME_CONFIG, domain: "memql.example.com" };
  const role = overrides.role ?? "owner";
  return (
    <SessionProvider
      value={{
        access: {
          userId: overrides.userId ?? "u-me",
          primaryEmail: "owner@example.com",
          clusterRole: role,
        },
        config,
      }}
    >
      <OsProvider registry={OS_REGISTRY} actorRole={role} grid={{ cols: 8, rows: 5 }}>
        {children}
      </OsProvider>
    </SessionProvider>
  );
}

export function memLogsStore(over: Partial<LogsSettings> = {}): LogsSettingsStore & { saved: LogsSettings[] } {
  let value: LogsSettings = { ...DEFAULT_LOGS_SETTINGS, ...over };
  const saved: LogsSettings[] = [];
  return {
    saved,
    load: () => value,
    save: (next: LogsSettings) => {
      value = next;
      saved.push(next);
    },
  };
}

/** Render the Logs app inside the providers it really mounts under. */
export async function renderLogsApp(opts: {
  section?: string;
  role?: string;
  settings?: Partial<LogsSettings>;
  store?: LogsSettingsStore;
  intent?: { id: string; payload: Record<string, unknown> };
  consumeIntent?: (intentId: string) => void;
  navigate?: (sectionId: string) => void;
} = {}) {
  const store = opts.store ?? memLogsStore(opts.settings ?? {});
  const navigate = opts.navigate ?? vi.fn();
  const view = render(
    withSession(
      <LogsApp
        sectionId={opts.section ?? "stream"}
        navigate={navigate}
        askContext={() => {}}
        intent={opts.intent}
        consumeIntent={opts.consumeIntent ?? (() => {})}
        store={store}
      />,
      { role: opts.role ?? "owner" },
    ),
  );
  // Let the reads run.
  await act(async () => {});
  return { view, store, navigate };
}

/** Render one app's Logs section, as that app would mount it. */
export async function renderAppLogs(props: AppLogsSectionProps, opts: { role?: string } = {}) {
  const view = render(withSession(<AppLogsSection {...props} />, { role: opts.role ?? "owner" }));
  await act(async () => {});
  return view;
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

/** One log line, a second ago unless told otherwise, so it sits inside every
 *  window a surface offers. */
export function logRow(over: Partial<Row> & { id: string }): Row {
  return {
    occurredAt: new Date(Date.now() - 1_000).toISOString(),
    nodeType: "bff",
    node: "bff-0",
    level: "info",
    component: "packages.pipeline",
    app: "",
    message: `line ${over.id}`,
    attributes: {},
    subject: "",
    subjectConcept: "",
    session: "",
    userId: "",
    ...over,
  };
}

/** A run of lines, oldest first, `n` of them, each a second apart. */
export function logRows(n: number, over: Partial<Row> = {}): Row[] {
  const base = Date.now() - n * 1_000;
  return Array.from({ length: n }, (_, i) =>
    logRow({ id: `l-${String(i).padStart(5, "0")}`, occurredAt: new Date(base + i * 1_000).toISOString(), ...over }),
  );
}

export async function click(el: Element | null | undefined): Promise<void> {
  if (!el) throw new Error("click() was handed nothing to click");
  await act(async () => {
    fireEvent.click(el);
  });
}

export async function type(el: HTMLInputElement, value: string): Promise<void> {
  await act(async () => {
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
    setter.call(el, value);
    el.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

/** Wait for the debounce and the read it triggers. */
export async function settle(ms = 300): Promise<void> {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, ms));
  });
}
