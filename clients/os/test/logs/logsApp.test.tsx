import { screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { OS_REGISTRY } from "../../src/apps/registry";
import {
  DEFAULT_LOGS_SETTINGS,
  LocalLogsSettingsStore,
  LOGS_SECTIONS,
  sanitizeLogsSettings,
} from "../../src/apps/logs/settings";
import { appById } from "../../src/system/registry";
import { click, fakeConnection, logRow, logRows, memLogsStore, renderLogsApp, settle, type } from "./harness";

// The Logs app (epic memql#4897): Stream over the whole store with the
// cluster's own sources as facets, Search paged older by keyset, and
// Settings with what this cluster keeps and the archived days.

beforeEach(() => {
  h.connection = null;
});

describe("the manifest and its settings", () => {
  it("is a real app on the roster, admin-floored, Stream first", () => {
    const logs = appById(OS_REGISTRY, "logs");
    expect(logs?.component.name).toBe("LogsApp");
    expect(logs?.sections).toBe(LOGS_SECTIONS);
    expect(LOGS_SECTIONS[0]?.id).toBe("stream");
    expect(DEFAULT_LOGS_SETTINGS.defaultSection).toBe("stream");
  });

  it("repairs each field independently and falls back wholesale on a wrong version", () => {
    expect(
      sanitizeLogsSettings({ version: 1, defaultSection: "nope", density: "compact", levelFloor: "warn", streamWindow: "6h" }),
    ).toEqual({ version: 1, defaultSection: "stream", density: "compact", levelFloor: "warn", streamWindow: "6h" });
    // Settings is a section, never a place a window opens ON.
    expect(sanitizeLogsSettings({ version: 1, defaultSection: "settings" }).defaultSection).toBe("stream");
    expect(sanitizeLogsSettings({ version: 2, density: "compact" })).toEqual(DEFAULT_LOGS_SETTINGS);
    for (const raw of [null, undefined, 7, "x", []]) {
      expect(sanitizeLogsSettings(raw)).toEqual(DEFAULT_LOGS_SETTINGS);
    }
  });

  it("round-trips through storage and tolerates a broken document", () => {
    const data = new Map<string, string>();
    const store = new LocalLogsSettingsStore({
      getItem: (k) => data.get(k) ?? null,
      setItem: (k, v) => void data.set(k, v),
    });
    store.save({ ...DEFAULT_LOGS_SETTINGS, density: "compact" });
    expect(store.load().density).toBe("compact");
    data.set("memql-os-logs-v1", "{not json");
    expect(store.load()).toEqual(DEFAULT_LOGS_SETTINGS);
  });

  it("applies the default section once, only when the window opened on the shell default", async () => {
    const opened = await renderLogsApp({ section: "stream", settings: { defaultSection: "search" } });
    expect(opened.navigate).toHaveBeenCalledWith("search");
    const named = await renderLogsApp({ section: "search", settings: { defaultSection: "stream" } });
    expect(named.navigate).not.toHaveBeenCalled();
  });

  it("hands a subject intent to Search whichever section the window is on", async () => {
    const { navigate } = await renderLogsApp({
      section: "stream",
      intent: { id: "i-1", payload: { subject: "site-1", subjectConcept: "v1:platform:site" } },
    });
    expect(navigate).toHaveBeenCalledWith("search");
  });
});

describe("the Stream", () => {
  const SOURCES: Row[] = [
    { kind: "component", value: "packages.pipeline", count: 12 },
    { kind: "component", value: "edge.serve", count: 3 },
    { kind: "node", value: "bff-0", nodeType: "bff", count: 40 },
    { kind: "app", value: "files", count: 2 },
    { kind: "mystery", value: "ignored", count: 1 },
  ];

  it("reads the whole store and the sources of its window", async () => {
    const connection = fakeConnection({ tail: logRows(3), sources: SOURCES });
    h.connection = connection;
    await renderLogsApp({ section: "stream" });
    expect(connection.callsNamed("logsTail")).toEqual(["builtin logsTail()"]);
    expect(connection.callsNamed("logsSources")[0]).toMatch(
      /^builtin logsSources\(windowStart: "[^"]+", windowEnd: "[^"]+"\)$/,
    );
    expect(screen.getByText("Last 15 minutes · 3 lines")).toBeTruthy();
  });

  it("offers the cluster's sources with counts, and picking one narrows the call", async () => {
    const connection = fakeConnection({ tail: logRows(1), sources: SOURCES });
    h.connection = connection;
    await renderLogsApp({ section: "stream" });
    await click(screen.getByRole("button", { name: "Refine stream" }));
    await click(screen.getByRole("combobox", { name: "Component" }));
    const list = screen.getByRole("listbox", { name: "Component" });
    expect(within(list).getAllByRole("option").map((o) => o.textContent)).toEqual([
      "Any component",
      "packages.pipeline (12)",
      "edge.serve (3)",
    ]);
    await click(within(list).getByRole("option", { name: "packages.pipeline (12)" }));
    const calls = connection.callsNamed("logsTail");
    expect(calls[calls.length - 1]).toBe('builtin logsTail(components: ["packages.pipeline"])');
    // The chip, scoped to the Refine group: the row's own component cell
    // says the same word.
    const refine = screen.getByRole("group", { name: "Refine stream" });
    expect(within(refine).getByText("packages.pipeline")).toBeTruthy();

    await click(screen.getByRole("combobox", { name: "Node" }));
    expect(screen.getByRole("option", { name: "bff-0 · bff (40)" })).toBeTruthy();
  });

  it("starts on the settings' level floor and stream window", async () => {
    const connection = fakeConnection({ tail: logRows(1) });
    h.connection = connection;
    await renderLogsApp({ section: "stream", settings: { levelFloor: "warn", streamWindow: "6h" } });
    expect(connection.callsNamed("logsTail")[0]).toBe('builtin logsTail(levels: ["warn", "error"])');
    expect(screen.getByText(/^Last 6 hours/)).toBeTruthy();
  });
});

describe("Search", () => {
  it("reads the window newest first with the page size on the wire, and pages older by keyset", async () => {
    const page = logRows(200);
    const oldest = page[page.length - 1]!;
    const connection = fakeConnection({
      search: (_call, nth) => (nth === 1 ? page : [logRow({ id: "l-older", message: "from before" })]),
    });
    h.connection = connection;
    await renderLogsApp({ section: "search" });

    expect(connection.callsNamed("logsSearch")[0]).toMatch(
      /^builtin logsSearch\(windowStart: "[^"]+", windowEnd: "[^"]+", limit: 200\)$/,
    );
    expect(screen.getByText("Last 24 hours · 200 lines")).toBeTruthy();
    expect(screen.getByText(/Newest first/)).toBeTruthy();

    await click(screen.getByRole("button", { name: "Older lines" }));
    const calls = connection.callsNamed("logsSearch");
    expect(calls[1]).toMatch(
      new RegExp(`limit: 200, beforeAt: "${oldest.occurredAt as string}", beforeId: "${oldest.id as string}"\\)$`),
    );
    expect(screen.getByText("Last 24 hours · 201 lines")).toBeTruthy();
    // A short page is the end of the window.
    expect(screen.getByText("That is every line in the window.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Older lines" })).toBeNull();
  });

  it("a subject intent lands narrowed with a copyable chip, and is consumed", async () => {
    const connection = fakeConnection({ search: logRows(1) });
    h.connection = connection;
    const consumeIntent = vi.fn();
    await renderLogsApp({
      section: "search",
      intent: { id: "i-2", payload: { subject: "site-1", subjectConcept: "v1:platform:site" } },
      consumeIntent,
    });
    const calls = connection.callsNamed("logsSearch");
    expect(calls[calls.length - 1]).toContain('subject: "site-1", subjectConcept: "v1:platform:site"');
    const line = screen.getByRole("group", { name: "Subject" });
    expect(within(line).getByText("site")).toBeTruthy();
    expect(within(line).getByRole("button", { name: "Copy Subject" })).toBeTruthy();
    expect(consumeIntent).toHaveBeenCalledWith("i-2");
  });

  it("a custom window asks for both ends, then searches between them", async () => {
    const connection = fakeConnection({ search: logRows(1) });
    h.connection = connection;
    await renderLogsApp({ section: "search" });
    await click(screen.getByRole("button", { name: "Refine search" }));
    await click(screen.getByRole("radio", { name: "Custom" }));
    expect(screen.getByText(/Pick a From and a To/)).toBeTruthy();

    await type(screen.getByLabelText("From") as HTMLInputElement, "2026-09-01T10:00");
    await type(screen.getByLabelText("To") as HTMLInputElement, "2026-09-02T10:00");
    const calls = connection.callsNamed("logsSearch");
    expect(calls[calls.length - 1]).toBe(
      `builtin logsSearch(windowStart: "${new Date("2026-09-01T10:00").toISOString()}", windowEnd: "${new Date("2026-09-02T10:00").toISOString()}", limit: 200)`,
    );
    expect(screen.getByText(/^Custom window/)).toBeTruthy();
  });

  it("the search text settles into the call, and the level floor joins it", async () => {
    const connection = fakeConnection({ search: logRows(1) });
    h.connection = connection;
    await renderLogsApp({ section: "search" });
    await click(screen.getByRole("button", { name: "Refine search" }));
    await click(screen.getByRole("radio", { name: "Errors" }));
    await type(screen.getByLabelText("Search messages") as HTMLInputElement, "refused");
    await settle();
    const calls = connection.callsNamed("logsSearch");
    expect(calls[calls.length - 1]).toMatch(/levels: \["error"\], text: "refused", limit: 200\)$/);
  });

  it("renders a refusal in surface", async () => {
    h.connection = fakeConnection({ refuse: { logsSearch: "logsSearch: admin and above" } });
    await renderLogsApp({ section: "search" });
    expect(screen.getByRole("alert").textContent).toContain("logsSearch: admin and above");
  });
});

describe("Settings", () => {
  const STATUS: Row = {
    retentionDays: 30,
    level: "info",
    maxLinesPerSecond: 2000,
    archiveConfigured: true,
    archiveContainer: "memql-logs",
    written: 1200,
    dropped: 3,
    droppedByReason: { rate: 3, queue: 0 },
    oldestAt: "2026-08-04T00:00:00Z",
    newestAt: "2026-09-03T11:59:00Z",
    rowEstimate: 100000,
  };
  const ARCHIVE: Row[] = [
    { day: "2026-08-01", nodeType: "bff", object: "logs/2026-08-01/bff.ndjson.gz", bytes: 2048 },
    { day: "2026-08-01", nodeType: "edge", object: "logs/2026-08-01/edge.ndjson.gz", bytes: 1024 },
    { day: "2026-07-31", nodeType: "bff", object: "logs/2026-07-31/bff.ndjson.gz", bytes: 512 },
  ];

  it("offers Stream and Search as the places a window opens on, never Settings", async () => {
    h.connection = fakeConnection({ status: STATUS });
    await renderLogsApp({ section: "settings" });
    const group = screen.getByRole("radiogroup", { name: "Default section" });
    expect(within(group).getAllByRole("radio").map((r) => r.textContent)).toEqual(["Stream", "Search"]);
  });

  it("renders what this cluster keeps from logsStatus, including the drops by reason", async () => {
    h.connection = fakeConnection({ status: STATUS });
    await renderLogsApp({ section: "settings" });
    const panel = screen.getByRole("region", { name: "This cluster" });
    expect(within(panel).getByText("30 days")).toBeTruthy();
    expect(within(panel).getByText("memql-logs")).toBeTruthy();
    expect(within(panel).getByText(/lines a second, per node/)).toBeTruthy();
    expect(within(panel).getByText(/^3 \(rate 3\)$/)).toBeTruthy();
    expect(within(panel).getByText(/^about /)).toBeTruthy();
  });

  it("says when no archive is configured, in a sentence rather than a blank", async () => {
    h.connection = fakeConnection({ status: { ...STATUS, archiveConfigured: false, archiveContainer: "" } });
    await renderLogsApp({ section: "settings" });
    expect(screen.getByText("No archive configured -- lines are kept until one is.")).toBeTruthy();
  });

  it("lists the archived days folded per day and offers Bring back to an owner, whose reply renders beside the day", async () => {
    const connection = fakeConnection({
      status: STATUS,
      archive: ARCHIVE,
      restore: { restored: 120, skipped: 0, objects: 2 },
    });
    h.connection = connection;
    await renderLogsApp({ section: "settings", role: "owner" });
    const days = screen.getByRole("list", { name: "Archived days" });
    const entries = within(days).getAllByRole("listitem");
    expect(entries.map((li) => li.querySelector(".os-logs-day-name")?.textContent)).toEqual(["2026-08-01", "2026-07-31"]);
    expect(entries[0]?.textContent).toContain("bff, edge");

    await click(within(days).getByRole("button", { name: "Bring back 2026-08-01" }));
    expect(connection.callsNamed("logsArchiveRestore")).toEqual(['builtin logsArchiveRestore(day: "2026-08-01")']);
    expect(within(entries[0] as HTMLElement).getByText(/Brought back 120 lines from 2 objects; 0 already present/)).toBeTruthy();
  });

  it("renders a restore's refusal in surface, verbatim", async () => {
    h.connection = fakeConnection({
      status: STATUS,
      archive: ARCHIVE,
      refuse: { logsArchiveRestore: "logsArchiveRestore: owner only" },
    });
    await renderLogsApp({ section: "settings", role: "owner" });
    await click(screen.getByRole("button", { name: "Bring back 2026-07-31" }));
    expect(screen.getByRole("alert").textContent).toContain("logsArchiveRestore: owner only");
  });

  it("gives an admin the sentence instead of the action", async () => {
    h.connection = fakeConnection({ status: STATUS, archive: ARCHIVE });
    await renderLogsApp({ section: "settings", role: "admin" });
    expect(screen.queryByRole("button", { name: /^Bring back/ })).toBeNull();
    expect(screen.getByText(/Bringing a day back is an owner's action/)).toBeTruthy();
  });

  it("saves a density choice through the store", async () => {
    h.connection = fakeConnection({ status: STATUS });
    const store = memLogsStore();
    await renderLogsApp({ section: "settings", store });
    await click(screen.getByRole("radio", { name: "compact" }));
    expect(store.saved.at(-1)?.density).toBe("compact");
  });
});
