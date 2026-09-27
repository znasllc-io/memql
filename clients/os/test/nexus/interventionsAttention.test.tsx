import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

// The unseen-change marker for stepping into a run (epic memql#5414): marked
// on the rows that open the version area, never acknowledged by an ancestor --
// the Runs section or the run page -- and acknowledged where the version area
// is actually on screen, on a run a person can step into.

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { NexusApp } = await import("../../src/apps/nexus/NexusApp");
const { LocalNexusSettingsStore } = await import("../../src/apps/nexus/settings");
const { AttentionProvider } = await import("../../src/attention/Attention");
const { OS_REGISTRY } = await import("../../src/apps/registry");
const { attentionDeclarationErrors } = await import("../../src/attention/declarations");
const { fakeConnection, runRow, stepRow, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

const CHANGE = {
  id: "nexus:interventions",
  revision: "interventions-1",
  sectionId: "runs",
  target: "step-versions",
  label: "Re-run, branch and feedback",
};

function mount(connection: Conn) {
  h.connection = connection;
  render(
    withSession(
      <AttentionProvider apps={OS_REGISTRY.apps}>
        <NexusApp
          sectionId="runs"
          navigate={vi.fn()}
          askContext={() => {}}
          intent={{ id: "open", payload: { runId: "run-1" } }}
          consumeIntent={() => {}}
          store={new LocalNexusSettingsStore(null)}
        />
      </AttentionProvider>,
    ),
  );
}

const acknowledged = (conn: Conn) => conn.query.acknowledgeAttention.mock.calls.map((call) => call[0]);

async function settle() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 20));
  });
}

function steps() {
  return [
    stepRow({ id: "run-1-fetch", runId: "run-1", key: "fetch", seq: 0 }),
    stepRow({ id: "run-1-draft", runId: "run-1", key: "draft", seq: 1, kind: "reasoning" }),
  ];
}

describe("the interventions change, in the registry", () => {
  it("is declared once, on the Runs section, with the version area as its destination", () => {
    const nexus = OS_REGISTRY.apps.find((app) => app.id === "nexus")!;
    expect(nexus.attentionChanges).toContainEqual(CHANGE);
    expect(attentionDeclarationErrors([nexus])).toEqual([]);
  });
});

describe("the interventions marker", () => {
  it("marks the rows that open the version area, and is acknowledged only when that area is on screen", async () => {
    const conn = fakeConnection({ runs: [runRow({ id: "run-1" })], steps: steps() });
    mount(conn);
    const row = await screen.findByLabelText(/^Step 2, draft,/);
    await waitFor(() => expect(within(row).getByRole("img", { name: "Unseen change" })).toBeTruthy());
    // On the step a model answered, and not on every row: a dot on each of
    // forty rows is a strobe, not a pointer.
    expect(within(screen.getByLabelText(/^Step 1, fetch,/)).queryByRole("img", { name: "Unseen change" })).toBeNull();
    // THE RUN PAGE IS AN ANCESTOR: Runs and the run are both on screen, and
    // neither is the destination.
    await settle();
    expect(acknowledged(conn)).toEqual([]);

    fireEvent.click(row);
    await screen.findByRole("heading", { name: /^Version 1/ });
    await waitFor(() => expect(acknowledged(conn)).toEqual([{ changeId: CHANGE.id, revision: CHANGE.revision }]));
    await waitFor(() =>
      expect(within(screen.getByLabelText(/^Step 2, draft,/)).queryByRole("img", { name: "Unseen change" })).toBeNull(),
    );
  });

  it("marks every row of a run no model took part in, so the change is always reachable", async () => {
    const conn = fakeConnection({
      runs: [runRow({ id: "run-1" })],
      steps: [stepRow({ id: "run-1-fetch", runId: "run-1", key: "fetch", seq: 0 })],
    });
    mount(conn);
    const row = await screen.findByLabelText(/^Step 1, fetch,/);
    await waitFor(() => expect(within(row).getByRole("img", { name: "Unseen change" })).toBeTruthy());
    fireEvent.click(row);
    await waitFor(() => expect(acknowledged(conn)).toEqual([{ changeId: CHANGE.id, revision: CHANGE.revision }]));
  });

  it("is neither shown nor acknowledged on a run still going -- nothing there can be stepped into yet", async () => {
    const conn = fakeConnection({ runs: [runRow({ id: "run-1", status: "running", finishedAt: "" })], steps: steps() });
    mount(conn);
    const row = await screen.findByLabelText(/^Step 2, draft,/);
    await settle();
    expect(within(row).queryByRole("img", { name: "Unseen change" })).toBeNull();
    fireEvent.click(row);
    await screen.findByRole("heading", { name: /^Version 1/ });
    await settle();
    expect(acknowledged(conn)).toEqual([]);
    // The reachable positive: the attention service was live and asked for
    // this person's receipts, so the silence is the rule's, not a mount that
    // never listened.
    expect(conn.query.myAttentionReceipts).toHaveBeenCalled();
  });
});
