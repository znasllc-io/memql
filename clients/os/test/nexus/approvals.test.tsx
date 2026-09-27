import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { NexusApp } = await import("../../src/apps/nexus/NexusApp");
const { LocalNexusSettingsStore } = await import("../../src/apps/nexus/settings");
const { answerPayload } = await import("../../src/apps/nexus/ApprovalsSection");
const { approvalFromRow } = await import("../../src/apps/nexus/rows");
const { approvalRow, fakeConnection, procedureRow, runRow, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

const APPROVAL = "v1:work:approval";

function mount(connection: Conn, sectionId = "approvals") {
  h.connection = connection;
  const bag = new Map<string, string>();
  const navigate = vi.fn();
  const view = render(
    withSession(
      <NexusApp
        sectionId={sectionId}
        navigate={navigate}
        askContext={() => {}}
        store={
          new LocalNexusSettingsStore({
            getItem: (k: string) => bag.get(k) ?? null,
            setItem: (k: string, v: string) => void bag.set(k, v),
          })
        }
      />,
    ),
  );
  return { view, navigate };
}

describe("the inbox", () => {
  it("answers from the front: the longest wait is first", async () => {
    // A queue padded with this morning's arrivals at the top is how a run
    // waits a week. There is deliberately no sort control here.
    const conn = fakeConnection({
      approvals: [
        approvalRow({ id: "a2", stepKey: "newest", requestedAt: "2026-09-01T12:00:00Z" }),
        approvalRow({ id: "a1", stepKey: "oldest", requestedAt: "2026-09-01T08:00:00Z" }),
      ],
    });
    mount(conn);
    const list = await screen.findByLabelText("Approvals waiting for you");
    const names = within(list)
      .getAllByRole("button")
      .map((b) => b.textContent ?? "");
    expect(names[0]).toContain("oldest");
    expect(names[1]).toContain("newest");
  });

  it("says nothing is waiting rather than showing an empty box", async () => {
    mount(fakeConnection());
    expect(
      await screen.findByText(
        /A run that needs a decision puts it here and stops until you make it\./,
      ),
    ).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Approvals" }).parentElement?.querySelector(".os-head-meta")?.textContent).toBe("0");
  });

  it("leads a row with WHAT is being asked, not with the kind", async () => {
    const conn = fakeConnection({
      approvals: [
        approvalRow({ id: "a1", subject: { summary: "Email the invoice to Acme" } }),
      ],
    });
    mount(conn);
    expect(await screen.findByText("Email the invoice to Acme")).toBeTruthy();
    expect(screen.getAllByText("Side effect").length).toBeGreaterThan(0);
  });
});

describe("deciding one", () => {
  it("does NOT offer approve from the list -- the decision is made beside the evidence", async () => {
    // An approval is a decision about a SPECIFIC artifact and the builtin
    // refuses one whose artifact moved. A one-click approve in a list is the
    // obvious design and the wrong one.
    const conn = fakeConnection({ approvals: [approvalRow({ id: "a1" })] });
    mount(conn);
    await screen.findByText("Step sendInvoice");
    expect(screen.queryByText("Approve")).toBeNull();
  });

  it("offers Reject and Approve once one is open, and sends the id and the decision", async () => {
    const conn = fakeConnection({ approvals: [approvalRow({ id: "a1" })] });
    mount(conn);
    fireEvent.click(await screen.findByText("Step sendInvoice"));
    fireEvent.click(await screen.findByText("Approve"));
    await waitFor(() => expect(conn.query.decideApproval).toHaveBeenCalled());
    expect(conn.query.decideApproval.mock.calls[0]?.[0]).toEqual({
      approvalId: "a1",
      decision: "approved",
    });
  });

  it("shows the classifier's evidence verbatim, including the rule id", async () => {
    // The rule id is where somebody goes to change the policy; a friendlier
    // paraphrase would drop the only fact that helps.
    const conn = fakeConnection({
      approvals: [
        approvalRow({
          id: "a1",
          evidence: { tier: "high", reason: "writes outside the graph", ruleId: "sfx-004", source: "safety" },
        }),
      ],
    });
    mount(conn);
    fireEvent.click(await screen.findByText("Step sendInvoice"));
    expect(await screen.findByText("writes outside the graph")).toBeTruthy();
    expect(screen.getByText("sfx-004")).toBeTruthy();
    expect(screen.getByText("high")).toBeTruthy();
  });

  it("says an absent evidence block is a fact about the ROW, not about the decision", async () => {
    const conn = fakeConnection({ approvals: [approvalRow({ id: "a1" })] });
    mount(conn);
    fireEvent.click(await screen.findByText("Step sendInvoice"));
    expect(
      await screen.findByText(/it does not mean the gate fired for no reason/),
    ).toBeTruthy();
  });

  it("shows the artifact hash and says what it promises", async () => {
    const conn = fakeConnection({
      approvals: [approvalRow({ id: "a1", artifactHash: "sha256:deadbeef" })],
    });
    mount(conn);
    fireEvent.click(await screen.findByText("Step sendInvoice"));
    expect(await screen.findByText("sha256:deadbeef")).toBeTruthy();
    expect(
      screen.getByText(/If it changes before the run resumes, the decision is refused/),
    ).toBeTruthy();
  });

  it("renders a refusal in place, verbatim -- never a toast", async () => {
    const conn = fakeConnection({
      approvals: [approvalRow({ id: "a1" })],
      writeError: new Error("the artifact changed since this was raised"),
    });
    mount(conn);
    fireEvent.click(await screen.findByText("Step sendInvoice"));
    fireEvent.click(await screen.findByText("Approve"));
    expect(
      await screen.findByText("the artifact changed since this was raised"),
    ).toBeTruthy();
  });
});

describe("a question", () => {
  it("shows the question and its options, and holds Send back until one is picked", async () => {
    const conn = fakeConnection({
      approvals: [
        approvalRow({
          id: "a1",
          kind: "feedback",
          question: "Which account should this be filed against?",
          options: [
            { label: "Acme Consulting", value: "acme" },
            { label: "Borden Ltd", value: "borden" },
          ],
        }),
      ],
    });
    mount(conn);
    fireEvent.click(await screen.findByText("Which account should this be filed against?"));
    // Approve/Reject are not the vocabulary of a question.
    expect(screen.queryByText("Approve")).toBeNull();
    expect(screen.queryByText("Send answer")).toBeNull();

    fireEvent.click(await screen.findByText("Acme Consulting"));
    fireEvent.click(await screen.findByText("Send answer"));
    await waitFor(() => expect(conn.query.decideApproval).toHaveBeenCalled());
    expect(conn.query.decideApproval.mock.calls[0]?.[0]).toEqual({
      approvalId: "a1",
      decision: "answered",
      answer: { value: "acme", label: "Acme Consulting" },
    });
  });

  it("takes free text when the question came with no options", async () => {
    const conn = fakeConnection({
      approvals: [
        approvalRow({ id: "a1", kind: "feedback", question: "What should I call the report?" }),
      ],
    });
    mount(conn);
    fireEvent.click(await screen.findByText("What should I call the report?"));
    fireEvent.change(await screen.findByLabelText("Your answer to this question"), {
      target: { value: "September reconciliation" },
    });
    fireEvent.click(await screen.findByText("Send answer"));
    await waitFor(() => expect(conn.query.decideApproval).toHaveBeenCalled());
    expect(conn.query.decideApproval.mock.calls[0]?.[0]).toEqual({
      approvalId: "a1",
      decision: "answered",
      answer: { text: "September reconciliation" },
    });
  });

  it("clears the draft answer when a DIFFERENT question is opened", async () => {
    // An option picked for one question sent as the answer to another is a
    // mistake the artifact hash cannot catch: both artifacts are intact.
    const conn = fakeConnection({
      approvals: [
        approvalRow({
          id: "a1",
          kind: "feedback",
          question: "First question?",
          requestedAt: "2026-09-01T08:00:00Z",
          options: [{ label: "Yes", value: "yes" }],
        }),
        approvalRow({
          id: "a2",
          kind: "feedback",
          question: "Second question?",
          requestedAt: "2026-09-01T09:00:00Z",
          options: [{ label: "Maybe", value: "maybe" }],
        }),
      ],
    });
    mount(conn);
    fireEvent.click(await screen.findByText("First question?"));
    fireEvent.click(await screen.findByText("Yes"));
    expect(await screen.findByText("Send answer")).toBeTruthy();

    fireEvent.click(screen.getByText("Second question?"));
    await waitFor(() => expect(screen.queryByText("Send answer")).toBeNull());
  });
});

describe("the answer payload", () => {
  it("hands the engine back its own option rather than a shape invented here", () => {
    const approval = approvalFromRow({
      id: "a1",
      options: [{ label: "Acme", value: "acme" }],
    });
    expect(answerPayload(approval, "acme", "")).toEqual({ value: "acme", label: "Acme" });
  });

  it("names free text `text`, which is the only honest name for what was typed", () => {
    const approval = approvalFromRow({ id: "a1" });
    expect(answerPayload(approval, "", "  September  ")).toEqual({ text: "September" });
  });

  it("still sends a choice whose option left the row under an open panel", () => {
    // The engine only checks the answer map is non-empty, so falling through
    // to the free-text branch would record `{text: ""}` as a blank answer to
    // a question somebody did answer.
    const approval = approvalFromRow({ id: "a1", options: [{ label: "Yes", value: "yes" }] });
    expect(answerPayload(approval, "gone", "")).toEqual({ value: "gone" });
  });
});

describe("the queue is live", () => {
  it("takes a decided approval off the list, which is the confirmation", async () => {
    // Nothing is patched locally: the decision broadcasts and the row leaves
    // the pending feed on its own update.
    const conn = fakeConnection({ approvals: [approvalRow({ id: "a1" })] });
    mount(conn);
    await screen.findByText("Step sendInvoice");

    await act(async () => {
      conn.subscriptions.emit(APPROVAL, approvalRow({ id: "a1" }), "NODE_DELETED");
    });
    await waitFor(() => expect(screen.queryByText("Step sendInvoice")).toBeNull());
  });

  it("announces one that arrived while somebody was looking", async () => {
    const conn = fakeConnection({ approvals: [approvalRow({ id: "a1" })] });
    mount(conn);
    await screen.findByText("Step sendInvoice");

    await act(async () => {
      conn.subscriptions.emit(
        APPROVAL,
        approvalRow({ id: "a2", stepKey: "chargeCard", requestedAt: "2026-09-01T10:00:00Z" }),
        "NODE_CREATED",
      );
    });
    const row = (await screen.findByText("Step chargeCard")).closest("li");
    expect(row?.getAttribute("data-arrival")).toBe("added");
  });
});

// ---------------------------------------------------------------------------
// The ladder's one approval (epic memql#5408, #5412)
// ---------------------------------------------------------------------------

function promotionRow(over: Record<string, unknown> = {}) {
  return approvalRow({
    id: "a-promo",
    kind: "procedurePromotion",
    runId: "run-shadow",
    stepKey: "",
    artifactHash: "sha256:proc-v1",
    // THE ENGINE'S WORDING: the goal the procedure serves, quoted -- never
    // the construct's machine name.
    question:
      'Promote "Reconcile last month\'s ledger against the bank export" to canary? It matched the app 5 times beside it, and would now run for real with the app standing by.',
    options: [
      { label: "Promote to canary", value: "approved" },
      { label: "Keep it in shadow", value: "rejected" },
    ],
    evidence: {
      tier: "evidence",
      reason: "matched the app 5 consecutive times in shadow, across at least 2 distinct bindings of every parameter",
      ruleId: "ladder.promotion",
      source: "rules",
    },
    subject: {
      constructId: "p1",
      constructName: "procedureReconcileLedger",
      procedureHash: "sha256:proc-v1",
      title: "Reconcile last month's ledger against the bank export",
      shadowMatches: 5,
      distinctBindings: { "s0.command.2": 2 },
      recordedFrom: { app: "claude-code", model: "claude-sonnet-4-5", effort: "high", sessionIds: [], runIds: [] },
    },
    ...over,
  });
}

/** The question, found by its opening: the goal it serves, quoted. */
const PROMOTE_QUESTION = /^Promote "Reconcile last month's ledger against the bank export" to canary\?/;

describe("a promotion", () => {
  it("is named for what it decides, and says it once", async () => {
    mount(fakeConnection({ approvals: [promotionRow()] }));
    expect(await screen.findAllByText("Promotion")).not.toHaveLength(0);
    const line = await screen.findByText(PROMOTE_QUESTION);
    // The row does not name the goal a second time beside its own question.
    const row = line.closest("button");
    expect((row?.textContent ?? "").split("Reconcile last month's ledger").length - 1).toBe(1);
    fireEvent.click(line);
    await screen.findByRole("region", { name: "What you are promoting" });
    // The question already says what promoting does; the kind's meaning is
    // not stood under it, nor repeated on the bar.
    expect(
      screen.queryByText(
        "A learned procedure matched the app often enough to run for real, with the app standing by.",
      ),
    ).toBeNull();
  });

  it("carries its evidence in one panel, not also in the generic classifier panel", async () => {
    mount(fakeConnection({ approvals: [promotionRow()] }));
    fireEvent.click(await screen.findByText(PROMOTE_QUESTION));
    await screen.findByRole("region", { name: "What you are promoting" });
    expect(screen.queryByRole("region", { name: "Why you were asked" })).toBeNull();
    expect(screen.queryByText("The classifier's evidence")).toBeNull();
    expect(screen.queryByText("ladder.promotion")).toBeNull();
  });

  it("keeps the classifier's evidence on every other kind", async () => {
    mount(fakeConnection({ approvals: [approvalRow({ id: "a1" })] }));
    fireEvent.click(await screen.findByText("Step sendInvoice"));
    const why = await screen.findByRole("region", { name: "Why you were asked" });
    expect(within(why).getByText("The classifier's evidence")).toBeTruthy();
  });

  it("carries the construct version AND the artifact hash on the card", async () => {
    mount(fakeConnection({ approvals: [promotionRow()] }));
    fireEvent.click(await screen.findByText(PROMOTE_QUESTION));
    const card = await screen.findByRole("region", { name: "What you are promoting" });
    expect(within(card).getByText("Construct version")).toBeTruthy();
    expect(within(card).getByText("Artifact hash")).toBeTruthy();
    expect(within(card).getAllByText("sha256:proc-v1")).toHaveLength(2);
    expect(within(card).getByRole("button", { name: "Copy construct version" })).toBeTruthy();
    expect(within(card).getByRole("button", { name: "Copy artifact hash" })).toBeTruthy();
    // Said once: the generic hash block does not repeat it.
    expect(screen.queryByText("The exact thing you are deciding")).toBeNull();
  });

  it("shows the evidence, naming each parameter by its goal input once the procedure is in hand", async () => {
    mount(
      fakeConnection({
        approvals: [promotionRow()],
        learnedProcedures: [procedureRow({ id: "p1" })],
      }),
    );
    fireEvent.click(await screen.findByText(PROMOTE_QUESTION));
    const card = await screen.findByRole("region", { name: "What you are promoting" });
    expect(within(card).getByText("5 in a row")).toBeTruthy();
    // The count leads, so "month 2" is never read as a value of month.
    const month = await within(card).findByText("month");
    expect(month.parentElement?.textContent).toMatch(/^\d+ for month$/);
    const from = within(card).getByText("Recorded from").nextElementSibling;
    expect(from?.textContent).toBe("Claude Code, claude-sonnet-4-5, high effort");
  });

  it("links to the procedure's page", async () => {
    const { navigate } = mount(
      fakeConnection({ approvals: [promotionRow()], learnedProcedures: [procedureRow({ id: "p1" })] }),
    );
    fireEvent.click(await screen.findByText(PROMOTE_QUESTION));
    const card = await screen.findByRole("region", { name: "What you are promoting" });
    fireEvent.click(
      within(card).getByRole("button", { name: "Reconcile last month's ledger against the bank export" }),
    );
    expect(navigate).toHaveBeenCalledWith("automations");
  });

  it("names its acts with the outcomes the ladder offered, and decides through the one verb", async () => {
    const conn = fakeConnection({ approvals: [promotionRow()] });
    mount(conn);
    fireEvent.click(await screen.findByText(PROMOTE_QUESTION));
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    // Declining never claims a step fails: the shadow run already finished.
    const keep = within(bar).getByRole("button", { name: /^Keep it in shadow: it goes on replaying beside the app/ });
    expect(keep.getAttribute("aria-label") ?? "").not.toMatch(/step fails/);
    fireEvent.click(within(bar).getByText("Promote to canary"));
    await waitFor(() => expect(conn.query.decideApproval).toHaveBeenCalled());
    expect(conn.query.decideApproval.mock.calls[0]?.[0]).toEqual({ approvalId: "a-promo", decision: "approved" });
  });

  it("shows the refusal of a changed procedure verbatim, in place", async () => {
    const conn = fakeConnection({
      approvals: [promotionRow()],
      writeError: new Error("work: the approval's artifact changed since it was raised"),
    });
    mount(conn);
    fireEvent.click(await screen.findByText(PROMOTE_QUESTION));
    fireEvent.click(await screen.findByText("Promote to canary"));
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(
      await within(bar).findByText("work: the approval's artifact changed since it was raised"),
    ).toBeTruthy();
    // The act stays offered: the refusal is a fact about this attempt.
    expect(within(bar).getByText("Promote to canary")).toBeTruthy();
  });

  it("warns before deciding when the catalog holds a newer version than the one proposed", async () => {
    mount(
      fakeConnection({
        approvals: [promotionRow()],
        learnedProcedures: [procedureRow({ id: "p1", procedureHash: "sha256:proc-v2" })],
      }),
    );
    fireEvent.click(await screen.findByText(PROMOTE_QUESTION));
    expect(await screen.findByText("This procedure has changed since it was proposed.")).toBeTruthy();
  });

  it("does not print its structured subject as a key/value dump", async () => {
    mount(fakeConnection({ approvals: [promotionRow()] }));
    fireEvent.click(await screen.findByText(PROMOTE_QUESTION));
    await screen.findByRole("region", { name: "What you are promoting" });
    expect(screen.queryByText("What it says")).toBeNull();
    expect(document.body.textContent ?? "").not.toContain('{"s0.command.2"');
  });
});

describe("the routing review", () => {
  it("has a word of its own rather than the raw enum member", async () => {
    mount(
      fakeConnection({
        approvals: [
          approvalRow({
            id: "a-route",
            kind: "routingReview",
            runId: "",
            stepKey: "",
            question: "gpt-x failed 17 of 41 structured calls at strong this week",
          }),
        ],
      }),
    );
    expect(await screen.findAllByText("Routing rule")).not.toHaveLength(0);
    expect(screen.queryByText("routingReview")).toBeNull();
    fireEvent.click(await screen.findByText("gpt-x failed 17 of 41 structured calls at strong this week"));
    // There is no run and no step to show, so neither is drawn as an em dash.
    const attached = await screen.findByRole("region", { name: "What it is attached to" });
    expect(within(attached).queryByText("Run")).toBeNull();
    expect(within(attached).queryByText("Step")).toBeNull();
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar).getByRole("button", { name: /^Reject this: routing is left as it is/ })).toBeTruthy();
  });
});

describe("crossing to the run", () => {
  it("names the run an approval is parked on and can open it", async () => {
    const conn = fakeConnection({
      approvals: [approvalRow({ id: "a1", runId: "run-1" })],
      runs: [runRow({ id: "run-1", status: "waiting", waitingOn: { kind: "approval" } })],
      steps: [],
    });
    const { navigate } = mount(conn);
    fireEvent.click(await screen.findByText("Step sendInvoice"));
    const detail = await screen.findByRole("region", { name: "What it is attached to" });
    fireEvent.click(within(detail).getByText("nightlyReconcile"));
    expect(navigate).toHaveBeenCalledWith("runs");
  });
});
