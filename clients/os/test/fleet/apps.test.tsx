import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

// The connection seam, mocked at the MODULE so the real reading hooks run
// against the harness's query double. Default null; each test sets what it
// needs.
const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { AppsSection } = await import("../../src/apps/fleet/apps/AppsSection");
const { policyRowId } = await import("../../src/apps/fleet/apps/useDelegationPolicy");
const { appSessionRow, delegationPolicyRow, fakeConnection, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

// Fleet -> Apps (epic memql#5009): the delegation policy, the delegated runs
// and one run's recording.

async function click(el: Element) {
  await act(async () => {
    (el as HTMLElement).click();
  });
}

let delegation = false;
function mount(connection: Conn) {
  h.connection = connection;
  const view = render(withSession(<AppsSection />));
  if (delegation) fireEvent.click(screen.getByRole("button", { name: "Delegation" }));
  return view;
}

beforeEach(() => {
  h.connection = null;
  delegation = false;
});

describe("the delegation policy editor", () => {
  beforeEach(() => { delegation = true; });
  it("STATES that delegation is off when there is no row, and writes nothing", async () => {
    const connection = fakeConnection({ delegationPolicyForUser: [] });
    mount(connection);

    // The absent row is a SENTENCE, not a blank form: "not configured yet, so
    // some default applies" is the reading an empty form invites, and here
    // the default IS off.
    expect(await screen.findByText(/Delegation is off/)).toBeTruthy();
    const note = screen.getByText(/Delegation is off/).closest(".os-notice") as HTMLElement;
    expect(note.textContent).toContain("Tasks stay in the cluster");
    expect(note.textContent).toContain("until you save a policy");

    // Opening the section is a READ. Nothing was written.
    expect(connection.query.delegationPolicyForUser).toHaveBeenCalled();
    expect(connection.query.setDelegationPolicy).not.toHaveBeenCalled();
  });

  it("says the fallback, so nobody reads the switch as turning work off", async () => {
    mount(fakeConnection({ delegationPolicyForUser: [] }));
    await screen.findByText(/Delegation is off/);

    const master = screen.getByLabelText("Delegate eligible tasks to my local apps");
    expect(master).toBeTruthy();
    // A PREFERENCE WITH A FALLBACK. The whole risk of this form is somebody
    // believing they have switched work off.
    expect(screen.getByText(/Otherwise, tasks continue in the cluster/)).toBeTruthy();
    expect(screen.getByText(/when an allowed machine is online/)).toBeTruthy();
  });

  it("says that the app list is a PRIORITY and shows the order back", async () => {
    mount(fakeConnection({ delegationPolicyForUser: [] }));
    await screen.findByText(/Delegation is off/);

    expect(screen.getByText(/MemQL tries selected apps in the order shown below/)).toBeTruthy();
    expect(screen.getByText(/No app listed, so nothing can be selected/)).toBeTruthy();

    await click(screen.getByLabelText("Codex"));
    await click(screen.getByLabelText("Claude Code"));
    // The order is the order they were added, and it is stated rather than
    // left to be inferred from click history.
    expect(screen.getByText("Tried in this order: Codex, then Claude Code.")).toBeTruthy();
  });

  it("SAVES THE WHOLE FORM IN ONE WRITE, never one call per field", async () => {
    const connection = fakeConnection({ delegationPolicyForUser: [] });
    mount(connection);
    await screen.findByText(/Delegation is off/);

    await click(screen.getByLabelText("Delegate eligible tasks to my local apps"));
    await click(screen.getByLabelText("Claude Code"));
    await click(screen.getByLabelText("Run commands"));
    await click(screen.getByLabelText("Save results"));
    fireEvent.change(screen.getByLabelText("Workspace root on the machine"), {
      target: { value: "/Users/ana/work" },
    });

    // Five edits, and still nothing written: a per-field save would have
    // left delegation on with an empty app list halfway through.
    expect(connection.query.setDelegationPolicy).not.toHaveBeenCalled();

    await click(screen.getByRole("button", { name: "Turn delegation on" }));

    expect(connection.query.setDelegationPolicy).toHaveBeenCalledTimes(1);
    const sent = connection.query.setDelegationPolicy.mock.calls[0]?.[0];
    expect(sent.preferSubscriptionApps).toBe(true);
    expect(sent.appOrder).toEqual(["claude-code"]);
    expect(sent.eligibleKinds).toEqual(["runCommand", "persistResult"]);
    expect(sent.workspaceRoot).toBe("/Users/ana/work");
    // The id is DERIVED from the user, so a second save updates one row
    // rather than forking a second the planner picks between.
    expect(sent.policyId).toBe(policyRowId("v1:identity:user:me"));
    // ownerUserId is NOT an argument: the mutation stamps it from the actor,
    // and accepting one was a forgery hole.
    expect(sent.ownerUserId).toBeUndefined();
  });

  it("opens on the stored row and saves it back under the same id", async () => {
    const connection = fakeConnection({ delegationPolicyForUser: [delegationPolicyRow()] });
    mount(connection);

    await waitFor(() =>
      expect(
        (screen.getByLabelText("Delegate eligible tasks to my local apps") as HTMLInputElement)
          .checked,
      ).toBe(true),
    );
    expect(screen.queryByText(/Delegation is off/)).toBeNull();
    expect((screen.getByLabelText("Claude Code") as HTMLInputElement).checked).toBe(true);
    expect((screen.getByLabelText("Codex") as HTMLInputElement).checked).toBe(false);
    expect((screen.getByLabelText("Run commands") as HTMLInputElement).checked).toBe(true);

    await click(screen.getByRole("button", { name: "Save delegation policy" }));
    expect(connection.query.setDelegationPolicy).toHaveBeenCalledTimes(1);
    expect(connection.query.setDelegationPolicy.mock.calls[0]?.[0].policyId).toBe(
      "v1:worker:delegationPolicy:v1-identity-user-me",
    );
  });

  it("keeps the edit and shows the server's own sentence when the save is refused", async () => {
    const connection = fakeConnection({ delegationPolicyForUser: [] });
    connection.query.setDelegationPolicy = vi.fn(async () => {
      throw new Error("delegation_policy_refused: workspaceRoot outside allowed roots");
    });
    mount(connection);
    await screen.findByText(/Delegation is off/);

    await click(screen.getByLabelText("Delegate eligible tasks to my local apps"));
    await click(screen.getByRole("button", { name: "Turn delegation on" }));

    expect(
      await screen.findByText("delegation_policy_refused: workspaceRoot outside allowed roots"),
    ).toBeTruthy();
    expect(screen.getByText(/what is on screen is your edit, not what the cluster holds/i)).toBeTruthy();
    // The edit survives the refusal.
    expect(
      (screen.getByLabelText("Delegate eligible tasks to my local apps") as HTMLInputElement)
        .checked,
    ).toBe(true);
  });
});

describe("the delegated runs list", () => {
  it("omits unreported usage from the summary and never presents it as zero", async () => {
    mount(
      fakeConnection({
        appSessionsForUser: [appSessionRow({ id: "v1:worker:appSession:a" })],
        delegationPolicyForUser: [],
      }),
    );

    const row = await screen.findByRole("button", { name: /Claude Code/ });
    expect(row.querySelector(".os-fleet-session-tokens")).toBeNull();
    expect(row.textContent).not.toContain("0 tokens");
  });

  it("adds input and output when the app DID report", async () => {
    mount(
      fakeConnection({
        appSessionsForUser: [
          appSessionRow({
            id: "v1:worker:appSession:a",
            usage: { known: true, inputTokens: 1200, outputTokens: 340, costUSD: 0 },
          }),
        ],
        delegationPolicyForUser: [],
      }),
    );

    const row = await screen.findByRole("button", { name: /Claude Code/ });
    expect((row.querySelector(".os-fleet-session-tokens") as HTMLElement).textContent).toContain(
      "1540",
    );
  });

  it("tones each status and marks subscription spend", async () => {
    mount(
      fakeConnection({
        appSessionsForUser: [
          appSessionRow({ id: "v1:worker:appSession:ok", status: "ended" }),
          appSessionRow({ id: "v1:worker:appSession:bad", status: "failed", billing: "metered" }),
          appSessionRow({ id: "v1:worker:appSession:gone", status: "cancelled" }),
          appSessionRow({ id: "v1:worker:appSession:live", status: "running", endedAt: "" }),
        ],
        delegationPolicyForUser: [],
      }),
    );

    await screen.findByText("ended");
    const toneOf = (status: string) =>
      screen.getByText(status).getAttribute("data-tone");
    expect(toneOf("ended")).toBe("accent");
    expect(toneOf("failed")).toBe("warn");
    expect(toneOf("cancelled")).toBe("warn");
    expect(toneOf("running")).toBe("muted");
    // `unknown` and `metered` are real answers; neither is folded into the
    // other and neither reads as "subscription".
    expect(screen.getByText("metered")).toBeTruthy();
    expect(screen.getAllByText("subscription")).toHaveLength(3);
  });

  it("says an empty list is empty rather than leaving a blank", async () => {
    mount(fakeConnection({ appSessionsForUser: [], delegationPolicyForUser: [] }));
    expect(await screen.findByText(/No app sessions yet/)).toBeTruthy();
  });
});

describe("one run's recording", () => {
  function withDetail(over: Record<string, unknown> = {}) {
    return fakeConnection({
      appSessionsForUser: [appSessionRow({ id: "v1:worker:appSession:a" })],
      delegationPolicyForUser: [],
      appSessionById: [
        appSessionRow({
          id: "v1:worker:appSession:a",
          workspace: "/Users/ana/memql-workspaces/run-a",
          prompt: "Fix the failing test",
          sessionRunId: "v1:work:run:rec",
          recordedSteps: 12,
          droppedActions: 0,
          transcriptFileId: "v1:library:file:t1",
          transcriptTruncated: false,
          ...over,
        }),
      ],
    });
  }

  async function openRun(connection: Conn) {
    mount(connection);
    await click(await screen.findByRole("button", { name: /Claude Code/ }));
    return screen.findByText("Recording");
  }

  it("REPLACES the list rather than stacking a second Head under it", async () => {
    await openRun(withDetail());
    // Rule 11: one Head per view. The list's Head is gone, and the way back
    // is the quiet control on this one.
    expect(screen.getAllByRole("heading", { level: 3 })).toHaveLength(1);
    expect(screen.getByRole("heading", { level: 3 }).textContent).toContain("Claude Code");
    expect(screen.getByRole("button", { name: "Back to Activity" })).toBeTruthy();
    expect(screen.queryByRole("list", { name: "Delegated runs" })).toBeNull();
  });

  it("names the recording run, the actions counted and the transcript file", async () => {
    await openRun(withDetail());
    expect(screen.getByText("v1:work:run:rec")).toBeTruthy();
    expect(screen.getByText("v1:library:file:t1")).toBeTruthy();
    expect(screen.getByText(/Every action this app took is a step of the run above/)).toBeTruthy();
  });

  // A lost action has to be said out loud. Anything lifted from an incomplete
  // recording is incomplete, and a count nobody reads is a count that lets
  // that happen quietly.
  it("SAYS SO when the recording lost actions", async () => {
    await openRun(withDetail({ droppedActions: 3 }));
    expect(screen.getByText(/Some of this run's actions were not recorded/)).toBeTruthy();
    expect(screen.getByText(/anything built from this recording is incomplete/)).toBeTruthy();
  });

  // The opposite state must be quiet: a complete recording that warned about
  // itself would train a reader to ignore the warning.
  it("says nothing about lost actions when none were lost", async () => {
    await openRun(withDetail());
    expect(screen.queryByText(/were not recorded/)).toBeNull();
  });

  // "Nothing recorded this" and "it recorded nothing" are different facts,
  // and only one of them is about the run.
  it("distinguishes a run nothing recorded from one that recorded nothing", async () => {
    await openRun(withDetail({ sessionRunId: "", recordedSteps: undefined }));
    expect(screen.getByText(/Nothing recorded this run/)).toBeTruthy();
    expect(screen.getByText(/not the same as a run that did nothing/)).toBeTruthy();
  });

  it("SAYS a shortened transcript is shortened", async () => {
    await openRun(withDetail({ transcriptTruncated: true }));
    // A transcript that simply stopped would read as a run that stopped.
    expect(screen.getByText(/The saved transcript is shortened/)).toBeTruthy();
  });

  it("does NOT poll a finished run, and offers to re-read it instead", async () => {
    vi.useFakeTimers();
    try {
      const connection = withDetail({ status: "ended" });
      mount(connection);
      await act(async () => {});
      await click(screen.getByRole("button", { name: /Claude Code/ }));
      await act(async () => {});
      expect(connection.query.appSessionById).toHaveBeenCalledTimes(1);

      // A finished run does not change; a poll over it never settles.
      await act(async () => {
        vi.advanceTimersByTime(30_000);
      });
      expect(connection.query.appSessionById).toHaveBeenCalledTimes(1);
      expect(screen.getByText(/Last updated/)).toBeTruthy();
      expect(screen.getByRole("button", { name: "Refresh app session" })).toBeTruthy();
    } finally {
      vi.useRealTimers();
    }
  });

  it("POLLS a running one, and stops the moment it turns terminal", async () => {
    vi.useFakeTimers();
    try {
      const connection = withDetail({ status: "running", endedAt: "" });
      mount(connection);
      await act(async () => {});
      await click(screen.getByRole("button", { name: /Claude Code/ }));
      await act(async () => {});
      expect(connection.query.appSessionById).toHaveBeenCalledTimes(1);

      await act(async () => {
        vi.advanceTimersByTime(3000);
      });
      await act(async () => {});
      expect(connection.query.appSessionById).toHaveBeenCalledTimes(2);

      // The run ends: the next answer is terminal, and the timer stops.
      connection.query.appSessionById = vi.fn(async () =>
        (await import("./harness")).rowsResult([
          appSessionRow({
            id: "v1:worker:appSession:a",
            status: "ended",
            sessionRunId: "v1:work:run:rec",
            recordedSteps: 12,
          }),
        ]),
      );
      await act(async () => {
        vi.advanceTimersByTime(3000);
      });
      await act(async () => {});
      const afterEnd = connection.query.appSessionById.mock.calls.length;
      await act(async () => {
        vi.advanceTimersByTime(30_000);
      });
      expect(connection.query.appSessionById.mock.calls.length).toBe(afterEnd);
    } finally {
      vi.useRealTimers();
    }
  });
});
