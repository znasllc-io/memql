import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
// and one run's transcript.

async function click(el: Element) {
  await act(async () => {
    (el as HTMLElement).click();
  });
}

function mount(connection: Conn) {
  h.connection = connection;
  return render(withSession(<AppsSection />));
}

beforeEach(() => {
  h.connection = null;
});

describe("the delegation policy editor", () => {
  it("STATES that delegation is off when there is no row, and writes nothing", async () => {
    const connection = fakeConnection({ delegationPolicyForUser: [] });
    mount(connection);

    // The absent row is a SENTENCE, not a blank form: "not configured yet, so
    // some default applies" is the reading an empty form invites, and here
    // the default IS off.
    expect(await screen.findByText(/Delegation is off/)).toBeTruthy();
    const note = screen.getByText(/Delegation is off/).closest(".os-notice") as HTMLElement;
    expect(note.textContent).toContain("every task runs in the cluster");
    expect(note.textContent).toContain("Nothing is written until you save");

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
    expect(screen.getByText(/otherwise it runs in the cluster exactly as before/)).toBeTruthy();
    expect(screen.getByText(/A plan never waits for a laptop to wake up/)).toBeTruthy();
  });

  it("says that the app list is a PRIORITY and shows the order back", async () => {
    mount(fakeConnection({ delegationPolicyForUser: [] }));
    await screen.findByText(/Delegation is off/);

    expect(screen.getByText(/the FIRST app on this list that a machine actually has wins/)).toBeTruthy();
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
    await click(screen.getByLabelText("runCommand"));
    await click(screen.getByLabelText("persistResult"));
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
    expect((screen.getByLabelText("runCommand") as HTMLInputElement).checked).toBe(true);

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
  it("renders an UNREPORTED usage as an em dash and never as 0", async () => {
    mount(
      fakeConnection({
        appSessionsForUser: [appSessionRow({ id: "v1:worker:appSession:a" })],
        delegationPolicyForUser: [],
      }),
    );

    const row = await screen.findByRole("button", { name: /Claude Code/ });
    // An app that reported nothing did not report zero. The dash is the
    // shell's spelling for "no answer" and carries the reason on hover.
    const tokens = row.querySelector(".os-fleet-session-tokens") as HTMLElement;
    expect(tokens.textContent).toBe("—");
    expect(tokens.textContent).not.toContain("0");
    expect(within(tokens).getByTitle(/Nothing has reported this yet/)).toBeTruthy();
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
    expect(toneOf("ended")).toBe("ok");
    expect(toneOf("failed")).toBe("danger");
    expect(toneOf("cancelled")).toBe("warn");
    expect(toneOf("running")).toBe("neutral");
    // `unknown` and `metered` are real answers; neither is folded into the
    // other and neither reads as "subscription".
    expect(screen.getByText("metered")).toBeTruthy();
    expect(screen.getAllByText("subscription")).toHaveLength(3);
  });

  it("says an empty list is empty rather than leaving a blank", async () => {
    mount(fakeConnection({ appSessionsForUser: [], delegationPolicyForUser: [] }));
    expect(await screen.findByText(/No app session has run yet/)).toBeTruthy();
  });
});

describe("one run's transcript", () => {
  const LINES = "step 1\n  indented   spaces\nstep 2\n";

  function withDetail(over: Record<string, unknown> = {}) {
    return fakeConnection({
      appSessionsForUser: [appSessionRow({ id: "v1:worker:appSession:a" })],
      delegationPolicyForUser: [],
      appSessionById: [
        appSessionRow({
          id: "v1:worker:appSession:a",
          workspace: "/Users/ana/memql-workspaces/run-a",
          prompt: "Fix the failing test",
          transcript: LINES,
          transcriptBytes: LINES.length,
          transcriptTruncated: false,
          ...over,
        }),
      ],
    });
  }

  async function openRun(connection: Conn) {
    mount(connection);
    await click(await screen.findByRole("button", { name: /Claude Code/ }));
    return screen.findByLabelText("Run transcript");
  }

  it("REPLACES the list rather than stacking a second Head under it", async () => {
    await openRun(withDetail());
    // Rule 11: one Head per view. The list's Head is gone, and the way back
    // is the quiet control on this one.
    expect(screen.getAllByRole("heading", { level: 3 })).toHaveLength(1);
    expect(screen.getByRole("heading", { level: 3 }).textContent).toContain("Claude Code");
    expect(screen.getByRole("button", { name: /Apps/ })).toBeTruthy();
    expect(screen.queryByLabelText("Delegated runs")).toBeNull();
  });

  it("renders the transcript VERBATIM in a pre and states the byte count", async () => {
    const pre = await openRun(withDetail());
    // Byte for byte, whitespace and all -- never parsed, prettified or
    // re-wrapped.
    expect(pre.textContent).toBe(LINES);
    expect(pre.tagName).toBe("PRE");
    expect(screen.getByText(/kept$/)).toBeTruthy();
  });

  it("SAYS a truncated transcript is truncated and points at the artifacts", async () => {
    await openRun(
      withDetail({
        transcriptTruncated: true,
        transcriptBytes: 1048576,
        producedArtifactIds: ["v1:library:artifact:full-transcript"],
      }),
    );
    // A transcript that simply stopped would read as a run that stopped.
    expect(
      screen.getByText(/reached the size the row keeps, so what is below stops short of the end/),
    ).toBeTruthy();
    expect(screen.getByText(/pushed to your Library at the end of the run/)).toBeTruthy();
    expect(screen.getByText("v1:library:artifact:full-transcript")).toBeTruthy();
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
      expect(screen.getByText(/A finished run does not change, so this is not polled/)).toBeTruthy();
      expect(screen.getByRole("button", { name: "Re-read" })).toBeTruthy();
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
            transcript: LINES,
            transcriptBytes: LINES.length,
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
