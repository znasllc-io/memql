import { act, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection is a module-level context read; replacing the READ lets the
// real collection, the real seed and the real projections run under jsdom.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { MachinesProvider } = await import("../../src/live/machines");
const { machineFromRow, pipelineStepsStatus } = await import("../../src/apps/fleet/rows");
const { fakeConnection, machineRow, MachinesWithFlow, withSession } = await import("./harness");

// Whether a machine takes pipeline steps (epic memql#5478, ruling R35). The
// answer is the MACHINE's: its Cockpit advertises `pipelines=allowed` when its
// own policy.yaml allows steps, and refuses any step that policy does not
// allow. A label set in Fleet is a routing preference and cannot make a
// machine accept one -- so it is never read here.

beforeEach(() => {
  h.connection = null;
});

describe("pipelineStepsStatus", () => {
  it("is allowed when the machine's own policy reports it", () => {
    expect(pipelineStepsStatus(machineFromRow(machineRow({ id: "m", labels: { pipelines: "allowed" } })))).toEqual({
      state: "allowed",
      answer: "Allowed by this machine's policy",
    });
  });

  it.each([
    ["the machine reports nothing about pipelines", {}, {}],
    ["the machine reports another value", { pipelines: "denied" }, {}],
    ["the value differs only in case -- labels match exactly", { pipelines: "Allowed" }, {}],
    ["only an operator label claims it", {}, { pipelines: "allowed" }],
  ])("is not allowed when %s", (_why, labels, operatorLabels) => {
    expect(pipelineStepsStatus(machineFromRow(machineRow({ id: "m", labels, operatorLabels })))).toEqual({
      state: "not_allowed",
      answer: "Not allowed",
    });
  });
});

async function openMachine(row: ReturnType<typeof machineRow>, name: string) {
  h.connection = fakeConnection({ myWorkersWithStatus: [row] });
  render(
    withSession(
      <MachinesProvider>
        <MachinesWithFlow />
      </MachinesProvider>,
    ),
  );
  const line = await screen.findByText(name);
  await act(async () => {
    (line as HTMLElement).click();
  });
}

// The fact's term, not the info dialog's heading of the same words.
function factTerm(label: string): HTMLElement {
  const term = screen.getAllByText(label).find((el) => el.tagName === "DT");
  if (!term) throw new Error(`no fact is labelled ${label}`);
  return term;
}

describe("the machine's detail", () => {
  it("says beside Computer use whether the machine takes pipeline steps", async () => {
    await openMachine(machineRow({ id: "v1:worker:registration:ci", displayName: "CI mini", labels: { pipelines: "allowed" } }), "CI mini");
    const term = factTerm("Pipeline steps");
    // Beside Computer use: the fact right after it, the same kind of answer
    // about what this machine will run.
    expect(factTerm("Computer use").nextElementSibling?.nextElementSibling).toBe(term);
    expect(term.nextElementSibling?.textContent).toContain("Allowed by this machine's policy");
  });

  it("says Not allowed for a machine whose policy says nothing, whatever its operator labels", async () => {
    await openMachine(
      machineRow({ id: "v1:worker:registration:laptop", displayName: "Laptop", operatorLabels: { pipelines: "allowed" } }),
      "Laptop",
    );
    expect(factTerm("Pipeline steps").nextElementSibling?.textContent).toContain("Not allowed");
  });

  it("says where the answer is set, behind a control the keyboard reaches", async () => {
    await openMachine(machineRow({ id: "v1:worker:registration:ci", displayName: "CI mini" }), "CI mini");
    const value = factTerm("Pipeline steps").nextElementSibling as HTMLElement;
    const about = within(value).getByRole("button", { name: "About Pipeline steps" });
    // A native button, in the tab order: keyboard focus reaches it.
    expect(about.tagName).toBe("BUTTON");
    expect(about.tabIndex).toBe(0);
    about.focus();
    expect(document.activeElement).toBe(about);
    // What it opens names the place a person changes the answer.
    const detail = value.querySelector("dialog")?.textContent ?? "";
    for (const words of ["policy.yaml", "pipelines", "allow: true", "repos"]) {
      expect(detail).toContain(words);
    }
  });
});
