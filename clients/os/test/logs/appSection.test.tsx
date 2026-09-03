import { screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

// The connection seam, mocked at the MODULE so the real hooks run against the
// harness's executeNamed fake. Default null; each test sets what it needs.
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { Concepts } from "@znasllc-io/memql-sdk-core/client";

import { click, fakeConnection, logRow, logRows, renderAppLogs, settle, type } from "./harness";

// The per-app Logs section (epic memql#4895, spec H "AppLogsSection"): the
// app's slice, each facet narrowing the RENDERED call, a subject intent
// landing narrowed, and the two different empty answers.

beforeEach(() => {
  h.connection = null;
});

const FILES_CONCEPTS = [Concepts.LIBRARY_ARTIFACT, Concepts.LIBRARY_FILE] as const;
const FILES_SCOPE = 'apps: ["files"], subjectConcepts: ["v1:library:artifact", "v1:library:file"]';

async function openRefine(): Promise<void> {
  await click(screen.getByRole("button", { name: "Refine logs" }));
}

describe("the app's slice", () => {
  it("reads the app id plus the concepts it owns, with no cursor, and shows what came back", async () => {
    const connection = fakeConnection({ tail: logRows(3) });
    h.connection = connection;
    await renderAppLogs({ app: "files", subjectConcepts: FILES_CONCEPTS });

    expect(connection.callsNamed("logsTail")).toEqual([`builtin logsTail(${FILES_SCOPE})`]);
    expect(screen.getAllByRole("row")).toHaveLength(3);
    expect(screen.getByText("Last hour · 3 lines")).toBeTruthy();
    expect(screen.getByRole("button", { name: /^Following/ })).toBeTruthy();
  });

  it("says it is not connected rather than rendering an empty list", async () => {
    await renderAppLogs({ app: "files" });
    expect(screen.getByText("Not connected to the cluster.")).toBeTruthy();
    expect(screen.queryByRole("grid")).toBeNull();
  });
});

describe("each facet narrows the rendered call", () => {
  it("the level floor becomes the levels at and above it, and a chip", async () => {
    const connection = fakeConnection({ tail: logRows(2) });
    h.connection = connection;
    await renderAppLogs({ app: "files", subjectConcepts: FILES_CONCEPTS });
    await openRefine();
    await click(screen.getByRole("radio", { name: "Warnings" }));

    // The generated builder's own argument order: levels sit between the
    // apps and the concepts. The string is what the engine parses.
    const calls = connection.callsNamed("logsTail");
    expect(calls[calls.length - 1]).toBe(
      'builtin logsTail(apps: ["files"], levels: ["warn", "error"], subjectConcepts: ["v1:library:artifact", "v1:library:file"])',
    );
    expect(screen.getByText("Warnings and above")).toBeTruthy();
  });

  it("the search text becomes the text facet, once it has settled", async () => {
    const connection = fakeConnection({ tail: logRows(2) });
    h.connection = connection;
    await renderAppLogs({ app: "files" });
    await openRefine();
    await type(screen.getByLabelText("Search messages") as HTMLInputElement, "timeout");
    // Not yet: a word arrives as one question, not six.
    expect(connection.callsNamed("logsTail")).toHaveLength(1);
    await settle();
    const calls = connection.callsNamed("logsTail");
    expect(calls[calls.length - 1]).toBe('builtin logsTail(apps: ["files"], text: "timeout")');
  });

  it("a subject mark on a row narrows to that subject", async () => {
    const connection = fakeConnection({
      tail: [logRow({ id: "l-1", subject: "site-1", subjectConcept: "v1:platform:site" })],
    });
    h.connection = connection;
    await renderAppLogs({ app: "deployables" });
    await click(screen.getByRole("button", { name: "Narrow to site site-1" }));

    const calls = connection.callsNamed("logsTail");
    expect(calls[calls.length - 1]).toBe(
      'builtin logsTail(apps: ["deployables"], subject: "site-1", subjectConcept: "v1:platform:site")',
    );
    expect(screen.getByText("subject site-1")).toBeTruthy();
  });

  it("removing a chip widens again", async () => {
    const connection = fakeConnection({ tail: logRows(2) });
    h.connection = connection;
    await renderAppLogs({ app: "files" });
    await openRefine();
    await click(screen.getByRole("radio", { name: "Errors" }));
    await click(screen.getByRole("button", { name: "Remove Errors only" }));
    const calls = connection.callsNamed("logsTail");
    expect(calls[calls.length - 1]).toBe('builtin logsTail(apps: ["files"])');
  });
});

describe("a subject intent", () => {
  it("lands narrowed with a chip and is consumed by id", async () => {
    const connection = fakeConnection({ tail: logRows(1) });
    h.connection = connection;
    const consumeIntent = vi.fn();
    await renderAppLogs({
      app: "deployables",
      intent: { id: "intent-7", payload: { subject: "site-1", subjectConcept: "v1:platform:site" } },
      consumeIntent,
    });
    const calls = connection.callsNamed("logsTail");
    expect(calls[calls.length - 1]).toBe(
      'builtin logsTail(apps: ["deployables"], subject: "site-1", subjectConcept: "v1:platform:site")',
    );
    expect(screen.getByText("subject site-1")).toBeTruthy();
    expect(consumeIntent).toHaveBeenCalledWith("intent-7");
  });

  it("leaves another surface's intent standing", async () => {
    h.connection = fakeConnection({ tail: logRows(1) });
    const consumeIntent = vi.fn();
    await renderAppLogs({
      app: "files",
      intent: { id: "intent-8", payload: { place: "desktop", folderId: "f-1" } },
      consumeIntent,
    });
    expect(consumeIntent).not.toHaveBeenCalled();
  });
});

describe("the two empty answers", () => {
  it("says nothing was recorded for the app in the window when nothing narrows it", async () => {
    h.connection = fakeConnection({ tail: [] });
    await renderAppLogs({ app: "files" });
    expect(screen.getByText("Nothing recorded for this app in the last hour.")).toBeTruthy();
    expect(screen.getByText(/this view follows/)).toBeTruthy();
  });

  it("says no lines match once a facet narrows it", async () => {
    h.connection = fakeConnection({ tail: [] });
    await renderAppLogs({ app: "files" });
    await openRefine();
    await click(screen.getByRole("radio", { name: "Errors" }));
    expect(screen.getByText("No lines match.")).toBeTruthy();
    expect(screen.queryByText(/Nothing recorded/)).toBeNull();
  });

  it("the window facet renames the Head and the empty sentence, and ages old lines out", async () => {
    const old = new Date(Date.now() - 20 * 60_000).toISOString();
    h.connection = fakeConnection({ tail: [logRow({ id: "l-old", occurredAt: old })] });
    await renderAppLogs({ app: "files" });
    expect(screen.getByText("Last hour · 1 line")).toBeTruthy();
    await openRefine();
    await click(screen.getByRole("radio", { name: "15 min" }));
    expect(screen.getByText("Last 15 minutes · 0 lines")).toBeTruthy();
    expect(screen.getByText("Nothing recorded for this app in the last 15 minutes.")).toBeTruthy();
  });
});

describe("a row", () => {
  it("names its level in full and carries the rule for warn and error", async () => {
    h.connection = fakeConnection({
      tail: [logRow({ id: "l-w", level: "warn", message: "slow disk" }), logRow({ id: "l-i", message: "fine" })],
    });
    await renderAppLogs({ app: "fleet" });
    const warn = screen.getByText("slow disk").closest(".os-logs-line") as HTMLElement;
    expect(warn.getAttribute("data-level")).toBe("warn");
    expect(within(warn).getByText("Warning")).toBeTruthy();
    const info = screen.getByText("fine").closest(".os-logs-line") as HTMLElement;
    expect(info.getAttribute("data-level")).toBe("info");
    expect(within(info).getByText("Info")).toBeTruthy();
  });

  it("opens the detail on click, with the whole line and the subject copyable, and closes it", async () => {
    h.connection = fakeConnection({
      tail: [
        logRow({
          id: "l-1",
          message: "deploy finished",
          attributes: { ms: 1200, host: "edge-0" },
          subject: "dep-1",
          subjectConcept: "v1:platform:packageDeployment",
        }),
      ],
    });
    await renderAppLogs({ app: "deployables" });
    await click(screen.getByText("deploy finished").closest("[role=row]"));
    const panel = screen.getByRole("region", { name: "Log line" });
    expect(within(panel).getByText("deploy finished")).toBeTruthy();
    expect(within(panel).getByRole("button", { name: "Copy Subject" })).toBeTruthy();
    expect(within(panel).getByText("ms")).toBeTruthy();
    expect(within(panel).getByText("1200")).toBeTruthy();
    await click(within(panel).getByRole("button", { name: "Close the line" }));
    expect(screen.queryByRole("region", { name: "Log line" })).toBeNull();
  });
});
