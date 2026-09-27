import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { NexusApp } = await import("../../src/apps/nexus/NexusApp");
const { LocalNexusSettingsStore } = await import("../../src/apps/nexus/settings");
const { approvalRow, fakeConnection, ladderPolicyRow, procedureRow, runRow, withSession } = await import(
  "./harness"
);
const { CONSTRUCT_CONCEPT } = await import("../../src/apps/nexus/concepts");

type Conn = ReturnType<typeof fakeConnection>;

// A LEARNED PROCEDURE'S PAGE (epic memql#5408, #5412).
//
// The page is where a person reads whether the system has earned the right to
// do this without a model -- so the tests are about the three ways that reading
// goes wrong quietly: a rung drawn by colour alone, a threshold invented where
// the cluster published none, and an act offered that the state does not allow.

function memoryStore() {
  const bag = new Map<string, string>();
  return new LocalNexusSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
}

function mountPage(connection: Conn, procedureId = "p1") {
  h.connection = connection;
  const navigate = vi.fn();
  const view = render(
    withSession(
      <NexusApp
        sectionId="automations"
        navigate={navigate}
        askContext={vi.fn()}
        intent={{ id: "i1", payload: { procedureId } }}
        consumeIntent={vi.fn()}
        store={memoryStore()}
      />,
    ),
  );
  return { view, navigate };
}

async function openPage(connection: Conn, procedureId = "p1") {
  const mounted = mountPage(connection, procedureId);
  await screen.findByRole("list", { name: /The certification ladder/ });
  return mounted;
}

function rung(name: string): HTMLElement {
  const ladder = screen.getByRole("list", { name: /The certification ladder/ });
  const item = within(ladder)
    .getAllByRole("listitem")
    .find((li) => li.getAttribute("data-rung") === name);
  if (item === undefined) throw new Error(`no rung ${name}`);
  return item;
}

const SHADOW: Row = procedureRow({ id: "p1" });

describe("the page opens from the list and goes back to it", () => {
  it("replaces the list with the procedure, named by the goal it serves", async () => {
    h.connection = fakeConnection({ learnedProcedures: [SHADOW], ladderPolicy: [ladderPolicyRow()] });
    render(
      withSession(
        <NexusApp sectionId="automations" navigate={vi.fn()} askContext={vi.fn()} store={memoryStore()} />,
      ),
    );
    fireEvent.click(await screen.findByText("Reconcile last month's ledger against the bank export"));
    expect(await screen.findByRole("list", { name: /The certification ladder/ })).toBeTruthy();
    // ONE HEAD: the page replaced the list rather than being appended to it.
    expect(document.querySelectorAll(".os-head")).toHaveLength(1);
    expect(screen.queryByLabelText("Automations this instance can replay")).toBeNull();

    // Outside a window the Head keeps its inline way back.
    fireEvent.click(screen.getByRole("button", { name: "Back to Automations" }));
    expect(await screen.findByLabelText("Automations this instance can replay")).toBeTruthy();
  });

  it("names a procedure with no statement by how many runs it was learned from", async () => {
    const untitled = procedureRow({
      id: "p1",
      procedure: { ...(SHADOW["procedure"] as Record<string, unknown>), title: "" },
    });
    await openPage(fakeConnection({ learnedProcedures: [untitled], ladderPolicy: [ladderPolicyRow()] }));
    expect(screen.getByRole("heading", { name: "Procedure learned from 2 runs" })).toBeTruthy();
  });
});

describe("the ladder", () => {
  it("names the current rung in words, checks the ones passed, and leaves the rest unreached", async () => {
    await openPage(fakeConnection({ learnedProcedures: [SHADOW], ladderPolicy: [ladderPolicyRow()] }));
    expect(rung("shadow").getAttribute("aria-current")).toBe("step");
    expect(within(rung("shadow")).getByText("Running beside the app")).toBeTruthy();
    expect(within(rung("candidate")).getByText("Passed")).toBeTruthy();
    expect(within(rung("canary")).getByText("Not reached")).toBeTruthy();
    expect(within(rung("trusted")).getByText("Not reached")).toBeTruthy();
    // What each rung means is one tap away, not stood under every rung.
    expect(within(rung("trusted")).queryByText("Runs without a model")).toBeNull();
    expect(screen.getByRole("button", { name: "About The certification ladder" })).toBeTruthy();
    // Retired is not a fifth rung.
    expect(within(screen.getByRole("list", { name: /The certification ladder/ })).getAllByRole("listitem")).toHaveLength(4);
  });

  it("counts the evidence against the policy's numbers, as marks and never as a percentage", async () => {
    await openPage(fakeConnection({ learnedProcedures: [SHADOW], ladderPolicy: [ladderPolicyRow()] }));
    expect(screen.getByRole("img", { name: "Matches beside the app: 3 of the 5 needed" })).toBeTruthy();
    // The parameter is named by the goal input that binds it.
    expect(screen.getByRole("img", { name: "month: 1 of the 2 needed" })).toBeTruthy();
    expect(document.body.textContent ?? "").not.toMatch(/\d+%/);
  });

  it("says what the next rung needs, computed from the policy row", async () => {
    await openPage(fakeConnection({ learnedProcedures: [SHADOW], ladderPolicy: [ladderPolicyRow()] }));
    expect(
      screen.getByText(
        "It needs 2 more matches in a row and a second binding for month; then promotion to canary is put to you.",
      ),
    ).toBeTruthy();
  });

  it("uses the cluster's numbers, not the defaults", async () => {
    await openPage(
      fakeConnection({
        learnedProcedures: [SHADOW],
        ladderPolicy: [ladderPolicyRow({ shadowMatches: 8, distinctBindings: 3 })],
      }),
    );
    expect(screen.getByRole("img", { name: "Matches beside the app: 3 of the 8 needed" })).toBeTruthy();
    expect(screen.getByText(/It needs 5 more matches in a row and 2 more bindings for month/)).toBeTruthy();
  });

  it("invents no threshold when the cluster published no policy", async () => {
    await openPage(fakeConnection({ learnedProcedures: [SHADOW], ladderPolicy: [] }));
    expect(
      await screen.findByText("How far it has to go is not shown: this cluster has not published its ladder values."),
    ).toBeTruthy();
    expect(screen.getByRole("img", { name: "Matches beside the app: 3" })).toBeTruthy();
    expect(screen.queryByText(/of 5/)).toBeNull();
  });

  it("says a policy refusal verbatim and still draws the procedure's own counts", async () => {
    await openPage(
      fakeConnection({
        learnedProcedures: [SHADOW],
        ladderPolicy: new Error("PERMISSION_DENIED: below the reader rung"),
      }),
    );
    expect(await screen.findByText("PERMISSION_DENIED: below the reader rung")).toBeTruthy();
    expect(screen.getByRole("img", { name: "Matches beside the app: 3" })).toBeTruthy();
    // A refused read is not an absent policy: the page does not say the
    // cluster published none.
    expect(screen.queryByText(/has not published its ladder values/)).toBeNull();
  });

  it("says nothing about the next rung until the policy has answered", async () => {
    const conn = fakeConnection({ learnedProcedures: [SHADOW], ladderPolicy: [ladderPolicyRow()] });
    // The policy read never settles in this test.
    conn.query.ladderPolicyCurrent = vi.fn(() => new Promise<never>(() => {}));
    await openPage(conn);
    expect(screen.getByRole("img", { name: "Matches beside the app: 3" })).toBeTruthy();
    expect(screen.queryByText(/has not published its ladder values/)).toBeNull();
    expect(screen.queryByText(/^It needs/)).toBeNull();
  });

  it("counts clean replays on a canary toward trust", async () => {
    await openPage(
      fakeConnection({
        learnedProcedures: [procedureRow({ id: "p1", ladder: "canary", canaryMatches: 2 })],
        ladderPolicy: [ladderPolicyRow()],
      }),
    );
    expect(rung("canary").getAttribute("aria-current")).toBe("step");
    expect(within(rung("shadow")).getByText("Passed")).toBeTruthy();
    expect(screen.getByRole("img", { name: "Clean replays in a row: 2 of the 5 needed" })).toBeTruthy();
    expect(
      screen.getByText("3 more clean replays in a row and it is trusted to run without a model. Nobody is asked again."),
    ).toBeTruthy();
  });

  it("draws a demotion counter only once it has moved, in its own ink", async () => {
    const trusted = procedureRow({ id: "p1", ladder: "trusted", failures: 1 });
    await openPage(fakeConnection({ learnedProcedures: [trusted], ladderPolicy: [ladderPolicyRow()] }));
    const failures = screen.getByRole("img", { name: "Failed replays in a row: 1 of the 2 that demote it" });
    expect(failures.getAttribute("data-tone")).toBe("fall");
    // The top rung's column is the last, so its evidence hangs to its left.
    expect(screen.getByRole("group", { name: "What moves it" }).getAttribute("data-side")).toBe("left");
    expect(screen.getByText(/2 failed replays in a row, or 1 that fails after passing its checks, return it to shadow/)).toBeTruthy();
  });

  it("hangs the evidence under its own rung when the ladder stands upright", async () => {
    // Upright the rungs run top-down from trusted, and the evidence takes the
    // row just under the rung it stands on -- at the ladder's foot it would
    // read as the bottom rung's. The rows are the page's arithmetic; the
    // container query only places them.
    await openPage(fakeConnection({ learnedProcedures: [SHADOW], ladderPolicy: [ladderPolicyRow()] }));
    const row = (name: string) => rung(name).style.getPropertyValue("--os-ladder-row");
    expect(["trusted", "canary", "shadow", "candidate"].map(row)).toEqual(["1", "2", "3", "5"]);
    const evidence = screen.getByRole("group", { name: "What moves it" });
    expect(evidence.style.getPropertyValue("--os-ladder-evidence-row")).toBe("4");
    expect(evidence.hasAttribute("data-rungs-below")).toBe(true);
  });

  it("shows no demotion marks on a healthy trusted procedure", async () => {
    await openPage(
      fakeConnection({
        learnedProcedures: [procedureRow({ id: "p1", ladder: "trusted" })],
        ladderPolicy: [ladderPolicyRow()],
      }),
    );
    expect(screen.queryByRole("img", { name: /Failed replays/ })).toBeNull();
    expect(within(rung("trusted")).getByText("Running on its own")).toBeTruthy();
  });
});

describe("retired is a notice, not a rung", () => {
  it("says why and when, draws no ladder, and offers nothing", async () => {
    const retired = procedureRow({
      id: "p1",
      ladder: "retired",
      ladderReason: "unused for 31 days, longer than the 30-day window, so it retires",
    });
    mountPage(fakeConnection({ learnedProcedures: [retired], ladderPolicy: [ladderPolicyRow()] }));
    // When, as a fact; why, in the ladder's own words -- and nothing more.
    expect(await screen.findByText(/^Retired .+ ago\.$/)).toBeTruthy();
    expect(screen.getByText("Unused for 31 days, longer than the 30-day window, so it retires.")).toBeTruthy();
    expect(screen.queryByRole("list", { name: /The certification ladder/ })).toBeNull();
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar).queryAllByRole("button")).toHaveLength(0);
  });
});

describe("the one act", () => {
  const WAITING = procedureRow({ id: "p1", shadowMatches: 5, promotionApprovalId: "a-promo" });

  it("offers Review promotion only while a promotion is open, and opens it", async () => {
    const conn = fakeConnection({
      learnedProcedures: [WAITING],
      ladderPolicy: [ladderPolicyRow()],
      approvals: [approvalRow({ id: "a-promo", kind: "procedurePromotion" })],
    });
    const { navigate } = await openPage(conn);
    expect(within(rung("shadow")).getByText("Proposed for canary")).toBeTruthy();
    expect(
      screen.getByText(
        "Promotion to canary is waiting for your decision. Promoted, it runs for real with the app standing by to take over.",
      ),
    ).toBeTruthy();
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    fireEvent.click(within(bar).getByRole("button", { name: /Review promotion/ }));
    expect(navigate).toHaveBeenCalledWith("approvals");
  });

  it("offers nothing while no promotion is open -- absent, never disabled", async () => {
    await openPage(fakeConnection({ learnedProcedures: [SHADOW], ladderPolicy: [ladderPolicyRow()] }));
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar).queryByRole("button")).toBeNull();
    // Nothing that would arm, retire or promote a procedure is anywhere.
    expect(screen.queryByText("Arm it")).toBeNull();
    expect(screen.queryByText("Retire it")).toBeNull();
  });

  it("withdraws the act once the approvals feed shows the promotion was decided", async () => {
    // The procedure still names the approval; the approvals feed no longer
    // holds it -- decided, and the ladder has not written the answer yet.
    await openPage(
      fakeConnection({ learnedProcedures: [WAITING], ladderPolicy: [ladderPolicyRow()], approvals: [] }),
    );
    await waitFor(() => expect(screen.queryByRole("button", { name: /Review promotion/ })).toBeNull());
    expect(
      screen.getByText("The promotion it was waiting on has been decided; the ladder has not recorded the answer yet."),
    ).toBeTruthy();
  });

  it("follows the ladder when it records the answer, without a re-read", async () => {
    const conn = fakeConnection({ learnedProcedures: [WAITING], ladderPolicy: [ladderPolicyRow()], approvals: [] });
    await openPage(conn);
    await screen.findByText(/has not recorded the answer yet/);
    act(() => {
      conn.subscriptions.emit(
        CONSTRUCT_CONCEPT,
        procedureRow({ id: "p1", ladder: "canary", shadowMatches: 5, promotionApprovalId: "" }),
      );
    });
    await waitFor(() => expect(rung("canary").getAttribute("aria-current")).toBe("step"));
    expect(within(rung("shadow")).getByText("Passed")).toBeTruthy();
    expect(screen.queryByText(/has not recorded the answer yet/)).toBeNull();
    expect(conn.query.learnedProceduresForOwner).toHaveBeenCalledTimes(1);
  });
});

describe("the record", () => {
  it("shows the evidence, the provenance, the preconditions and every step", async () => {
    await openPage(
      fakeConnection({
        learnedProcedures: [SHADOW],
        ladderPolicy: [ladderPolicyRow()],
        runs: [runRow({ id: "run-rec-1", automationName: "recordedSession" })],
      }),
    );
    for (const panel of ["Where it stands", "Evidence", "Recorded from", "Preconditions", "Steps"]) {
      expect(screen.getByRole("region", { name: panel })).toBeTruthy();
    }
    const evidence = screen.getByRole("region", { name: "Evidence" });
    // SAID ONCE: the shadow counts are the ladder's marks, not repeated here;
    // the counts that belong to other rungs are.
    expect(within(evidence).queryByText("Shadow matches")).toBeNull();
    expect(within(evidence).getByText("Canary replays")).toBeTruthy();
    expect(screen.getByRole("img", { name: "Matches beside the app: 3 of the 5 needed" })).toBeTruthy();
    // Reliability is a word, never a number.
    expect(within(evidence).getByText("Mixed")).toBeTruthy();
    expect(within(evidence).getByText(/Matched the app 3 of the 5 consecutive times promotion needs\./)).toBeTruthy();

    const from = screen.getByRole("region", { name: "Recorded from" });
    expect(within(from).getByText("Claude Code")).toBeTruthy();
    expect(within(from).getByText("claude-sonnet-4-5")).toBeTruthy();
    expect(within(from).getByText("sess-1")).toBeTruthy();
    expect(within(from).getByText("sha256:proc-v1")).toBeTruthy();
    // A recording this window holds is a link; one it does not is text.
    expect(within(from).getByRole("button", { name: "recordedSession" })).toBeTruthy();
    expect(within(from).getByText("run-rec-2")).toBeTruthy();

    const pre = screen.getByRole("region", { name: "Preconditions" });
    expect(within(pre).getByText("Checked before every replay")).toBeTruthy();
    expect(within(pre).getByText("node 22.1.0")).toBeTruthy();
    expect(within(pre).getByText("Starts empty")).toBeTruthy();
  });

  it("writes each step back as a command line with the varying values as named chips", async () => {
    await openPage(fakeConnection({ learnedProcedures: [SHADOW], ladderPolicy: [ladderPolicyRow()] }));
    const steps = screen.getByRole("list", { name: "Steps, in the order they run" });
    const [first, second] = within(steps).getAllByRole("listitem");
    expect(first?.textContent).toContain("Run");
    expect(first?.textContent).toContain("node scripts/reconcile.js");
    // The free parameter is named by its goal input.
    expect(within(first!).getByText("month")).toBeTruthy();
    // A constant is its value; a data-flow value names the step it comes from.
    expect(second?.textContent).toContain("reports/ledger.csv");
    expect(within(second!).getByText("from step 1")).toBeTruthy();
  });

  it("leads each binding count with its number, so it is never read as a value", async () => {
    await openPage(
      fakeConnection({
        learnedProcedures: [procedureRow({ id: "p1", ladder: "canary", canaryMatches: 2 })],
        ladderPolicy: [ladderPolicyRow()],
      }),
    );
    const evidence = screen.getByRole("region", { name: "Evidence" });
    const month = within(evidence).getByText("month");
    expect(month.parentElement?.textContent).toBe("1 for month");
  });

  it("leaves out a passed rung's zero rather than say it earned none", async () => {
    // Trusted means canary WAS passed; a zero there is a counter reset on the
    // way up, and "none yet" would say the opposite of what happened.
    await openPage(
      fakeConnection({
        learnedProcedures: [procedureRow({ id: "p1", ladder: "trusted", canaryMatches: 0 })],
        ladderPolicy: [ladderPolicyRow()],
      }),
    );
    const evidence = screen.getByRole("region", { name: "Evidence" });
    expect(within(evidence).queryByText("Canary replays")).toBeNull();
    expect(within(evidence).queryByText("none yet")).toBeNull();
  });

  it("says a retired procedure's zero is none, not none yet", async () => {
    mountPage(
      fakeConnection({
        learnedProcedures: [procedureRow({ id: "p1", ladder: "retired", canaryMatches: 0 })],
        ladderPolicy: [ladderPolicyRow()],
      }),
    );
    const evidence = await screen.findByRole("region", { name: "Evidence" });
    expect(within(evidence).getByText("Canary replays").nextElementSibling?.textContent).toBe("none");
    expect(within(evidence).queryByText("none yet")).toBeNull();
  });

  it("says Nothing learned when the recordings agreed on nothing to check", async () => {
    await openPage(
      fakeConnection({
        learnedProcedures: [procedureRow({ id: "p1", preconditions: {} })],
        ladderPolicy: [ladderPolicyRow()],
      }),
    );
    expect(screen.getByText(/Nothing learned\. The recordings agreed on nothing/)).toBeTruthy();
  });

  it("says a variable is recorded and never checked", async () => {
    await openPage(
      fakeConnection({
        learnedProcedures: [
          procedureRow({ id: "p1", preconditions: { variables: { CI: "unset", TOKEN: "sha256:x" } } }),
        ],
        ladderPolicy: [ladderPolicyRow()],
      }),
    );
    const pre = screen.getByRole("region", { name: "Preconditions" });
    // Which group a line is in IS the fact: a gate, or not a gate.
    expect(within(pre).getByText("Recorded, not checked")).toBeTruthy();
    expect(within(pre).queryByText("Checked before every replay")).toBeNull();
    expect(pre.textContent ?? "").toContain("CI not set");
    // The digest is never printed.
    expect(pre.textContent ?? "").not.toContain("sha256:x");
  });
});

describe("a link to a procedure the catalog does not hold", () => {
  it("says so once the feed has answered, and then lets the link go", async () => {
    const conn = fakeConnection({ learnedProcedures: [SHADOW], ladderPolicy: [ladderPolicyRow()] });
    mountPage(conn, "nope");
    expect(await screen.findByText("That procedure is not in your catalog.")).toBeTruthy();
    expect(screen.getByLabelText("Automations this instance can replay")).toBeTruthy();
    await act(async () => {});
    expect(screen.queryByRole("list", { name: /The certification ladder/ })).toBeNull();
  });
});
