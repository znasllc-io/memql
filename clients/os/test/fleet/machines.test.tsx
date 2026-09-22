import { act, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection is a module-level context read, and its provider dials a
// real socket. Replacing the READ is what lets the real collection, the real
// retain path and the real projections run under jsdom.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { MachinesProvider } = await import("../../src/live/machines");
const { fakeConnection, machineRow, MachinesWithFlow, rowsResult, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

function mount(connection: Conn, showRevoked = false) {
  h.connection = connection;
  return render(
    withSession(
      <MachinesProvider>
        <MachinesWithFlow showRevoked={showRevoked} />
      </MachinesProvider>,
    ),
  );
}

// Clicks and typing go through act(): a state update outside it is not
// flushed before the next assertion, which reads exactly like a control that
// did nothing.
async function click(el: Element) {
  await act(async () => {
    (el as HTMLElement).click();
  });
}

async function type(el: HTMLInputElement, value: string) {
  await act(async () => {
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
    setter.call(el, value);
    el.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

const LIVE = machineRow({
  id: "v1:worker:registration:live",
  displayName: "Studio mini",
  labels: { os: "darwin", tier: "cheap" },
  operatorLabels: { tier: "gold", room: "studio" },
  activeCount: 1,
  apps: [{ id: "claude-code", version: "1.2.3", allowed: true, signedIn: true, subscription: "present" }],
});

const REVOKED = machineRow({
  id: "v1:worker:registration:gone",
  displayName: "Old laptop",
  // A revoked machine whose last heartbeat is FRESH: the case a clock-only
  // online rule renders green.
  lastSeenAt: new Date().toISOString(),
  revokedAt: "2026-08-30T11:00:00Z",
  revokeReason: "sold it",
});

beforeEach(() => {
  h.connection = null;
});

describe("the machines directory", () => {
  it.each([
    { name: "Wayland with the computer-use build", server: "wayland", caps: ["HEADLESS", "MODEL"], build: true, os: "linux", permissions: {}, answer: /Wayland.*X11/ },
    { name: "supported X11", server: "x11", caps: ["HEADLESS", "COMPUTERUSE"], build: true, os: "linux", permissions: { x11_display: true }, answer: /^Available.*X11/ },
    { name: "displayless session", server: "none", caps: ["HEADLESS"], build: true, os: "linux", permissions: { x11_display: false }, answer: /No display.*X11/ },
    { name: "unregistered desktop capability", server: "x11", caps: ["HEADLESS", "MODEL"], build: true, os: "linux", permissions: { x11_display: true }, answer: /not registered.*computer use/i },
    { name: "headless installation", server: "none", caps: ["HEADLESS"], build: false, os: "linux", permissions: {}, answer: /computer-use support.*not reported.*Install/i },
    { name: "unusable X11 display", server: "x11", caps: ["HEADLESS", "COMPUTERUSE"], build: true, os: "linux", permissions: { x11_display: false }, answer: /X11 display.*not available/i },
    { name: "macOS Accessibility denied", server: "quartz", caps: ["HEADLESS", "COMPUTERUSE"], build: true, os: "darwin", permissions: { accessibility: false, screen_recording: true }, answer: /Accessibility.*not granted/i },
    { name: "macOS Screen Recording denied", server: "quartz", caps: ["HEADLESS", "COMPUTERUSE"], build: true, os: "darwin", permissions: { accessibility: true, screen_recording: false }, answer: /Screen Recording.*not granted/i },
    { name: "macOS permissions unknown", server: "quartz", caps: ["HEADLESS", "COMPUTERUSE"], build: true, os: "darwin", permissions: {}, answer: /permissions.*not fully verified/i },
    { name: "macOS permissions granted", server: "quartz", caps: ["HEADLESS", "COMPUTERUSE"], build: true, os: "darwin", permissions: { accessibility: true, screen_recording: true }, answer: /^Available.*Accessibility.*Screen Recording/ },
  ])("reports usable computer use for $name", async ({ server, caps, build, os, permissions, answer }) => {
    mount(fakeConnection({ myWorkersWithStatus: [machineRow({
      id: "desktop-status", displayName: "Desktop status", capabilities: caps,
      platformInfo: { os, arch: "amd64", hostname: "desktop" },
      capabilityDescriptor: { displayServer: server, computerUseAvailable: build }, permissions,
    })] }));
    await click(await screen.findByText("Desktop status"));
    expect(screen.getByText("Computer use").nextElementSibling?.textContent).toMatch(answer);
  });

  it("keeps computer-use availability unknown when the optional desktop descriptor is absent", async () => {
    mount(fakeConnection({ myWorkersWithStatus: [machineRow({
      id: "desktop-unreported", displayName: "Desktop unreported", buildTag: "computeruse",
      capabilities: ["HEADLESS", "COMPUTERUSE"], platformInfo: { os: "linux" },
    })] }));
    await click(await screen.findByText("Desktop unreported"));
    const answer = screen.getByText("Computer use").nextElementSibling?.textContent;
    expect(answer).toMatch(/not reported.*unknown/i);
    expect(answer).not.toContain("Install");
  });

  it("seeds from the cluster and lists the caller's machines", async () => {
    // THE REGRESSION GUARD for the foundation's un-retained collection: a
    // LiveCollection that is subscribed but never retained never seeds, so
    // this list renders "Loading from the cluster" forever with nothing
    // logged and nothing thrown. The assertion is that a row is on screen.
    const connection = fakeConnection({ myWorkersWithStatus: [LIVE] });
    mount(connection);
    expect(await screen.findByText("Studio mini")).toBeTruthy();
    expect(connection.query.myWorkersWithStatus).toHaveBeenCalled();
  });

  it("hides revoked machines by default and marks them when shown, never as online", async () => {
    const connection = fakeConnection({ myWorkersWithStatus: [LIVE, REVOKED] });
    const view = mount(connection);
    await screen.findByText("Studio mini");
    expect(screen.queryByText("Old laptop")).toBeNull();

    view.unmount();
    mount(fakeConnection({ myWorkersWithStatus: [LIVE, REVOKED] }), true);
    const revoked = await screen.findByText("Old laptop");
    const row = revoked.closest(".os-machine") as HTMLElement;
    expect(within(row).getByText("revoked")).toBeTruthy();
    // The dot is an img with an accessible name; a revoked machine has none.
    expect(within(row).queryByLabelText("Online")).toBeNull();
  });

  it("offers no edit affordance anywhere for the machine-reported labels", async () => {
    mount(fakeConnection({ myWorkersWithStatus: [LIVE] }));
    await click(await screen.findByText("Studio mini"));

    const reported = screen.getByRole("list", { name: "Reported labels" });
    // The reported values are SHOWN -- the router matches on them --
    // but nothing in that group removes or edits one.
    expect(within(reported).getByText(/os=darwin/)).toBeTruthy();
    expect(within(reported).queryByRole("button")).toBeNull();

    // The operator group is where the controls are.
    const operator = screen.getByRole("list", { name: "Operator labels" });
    expect(within(operator).getByLabelText("Remove tier=gold")).toBeTruthy();
  });

  it("shows a reported value an operator has overridden as overridden", async () => {
    mount(fakeConnection({ myWorkersWithStatus: [LIVE] }));
    await click(await screen.findByText("Studio mini"));
    const reported = screen.getByRole("list", { name: "Reported labels" });
    // `tier` is reported cheap and set gold: the machine says one thing and
    // the routing acts on another, which is a fact about the operator's own
    // configuration.
    expect(within(reported).getByText("overridden")).toBeTruthy();
  });

  it("renames through displayName and never through the reported name", async () => {
    const connection = fakeConnection({ myWorkersWithStatus: [LIVE] });
    mount(connection);
    await click(await screen.findByText("Studio mini"));

    await type(screen.getByLabelText("Name for Studio mini") as HTMLInputElement, "Studio");
    await click(screen.getByRole("button", { name: "Rename" }));

    expect(connection.query.renameWorker).toHaveBeenCalledWith({
      registrationId: "v1:worker:registration:live",
      displayName: "Studio",
    });
  });

  it("writes ONLY the operator map when a label is added", async () => {
    const connection = fakeConnection({ myWorkersWithStatus: [LIVE] });
    mount(connection);
    await click(await screen.findByText("Studio mini"));

    await click(screen.getByLabelText("Remove room=studio"));

    expect(connection.query.setWorkerOperatorLabels).toHaveBeenCalledWith({
      registrationId: "v1:worker:registration:live",
      // The reported labels are absent: this write replaces the operator map
      // as a set, and folding the reported ones in would make them
      // permanent -- the exact thing the two-map split prevents.
      operatorLabels: { tier: "gold" },
    });
  });

  it("rolls an optimistic chip back when the cluster refuses the write", async () => {
    const connection = fakeConnection({ myWorkersWithStatus: [LIVE] });
    connection.query.setWorkerOperatorLabels.mockRejectedValue(new Error("row authz refused"));
    mount(connection);
    await click(await screen.findByText("Studio mini"));

    await click(screen.getByLabelText("Remove room=studio"));

    // The chip comes back, because a chip left on screen would claim a label
    // the router will never match on.
    await waitFor(() => expect(screen.getByLabelText("Remove room=studio")).toBeTruthy());
    expect(screen.getByText("row authz refused")).toBeTruthy();
  });

  it("asks an in-surface confirm that NAMES the machine, then removes it in ONE act", async () => {
    const connection = fakeConnection({ myWorkersWithStatus: [LIVE] });
    mount(connection);
    await click(await screen.findByText("Studio mini"));

    await click(screen.getByRole("button", { name: "Remove this machine" }));
    // Nothing has been written yet -- the confirm is a step, not a label.
    expect(connection.query.fleetRevokeMachine).not.toHaveBeenCalled();
    expect(screen.getByRole("group", { name: "Removing Studio mini" })).toBeTruthy();
    // The machine's half of the act is right there: the uninstall line for
    // its platform (design record 2026-09-08-cockpit-install-wizard, D12).
    expect((screen.getByLabelText("the uninstall command") as HTMLInputElement).value).toContain("uninstall-mac.sh");

    await type(screen.getByLabelText("Reason (optional)") as HTMLInputElement, "returned it");
    await click(screen.getByRole("button", { name: "Remove Studio mini" }));

    // ONE CALL, AND IT IS THE BUILTIN (epic memql#5327, design D2). This used
    // to render `revokeWorker`, which revoked the registration ROW and left
    // the credential active -- so a machine an operator had "removed" kept its
    // stream and could re-register. The shell composing two calls instead
    // would own the window between them.
    expect(connection.query.fleetRevokeMachine).toHaveBeenCalledWith({
      registrationId: "v1:worker:registration:live",
      reason: "returned it",
    });
    expect(connection.query.fleetRevokeMachine).toHaveBeenCalledTimes(1);
  });

  it("never sends revokedBy or revokedAt -- the engine stamps both", async () => {
    // THE RECORD OF WHO REMOVED A MACHINE MUST NOT BE WRITABLE BY WHOEVER IS
    // HOLDING THE KEYBOARD (epic memql#5327, design D2), which is the rule
    // sharing already follows. The old call sent both fields from the browser.
    const connection = fakeConnection({ myWorkersWithStatus: [LIVE] });
    mount(connection);
    await click(await screen.findByText("Studio mini"));
    await click(screen.getByRole("button", { name: "Remove this machine" }));
    await click(screen.getByRole("button", { name: "Remove Studio mini" }));

    const args = connection.query.fleetRevokeMachine.mock.calls[0]?.[0] as Record<string, unknown>;
    expect(Object.keys(args).sort()).toEqual(["registrationId"]);
  });

  it("says so when the registration went but the credential did not", async () => {
    // A REMOVAL IS TWO WRITES AND EITHER CAN LAND ALONE (design D2). The
    // engine revokes the row first, because that is the write the mesh
    // broadcasts and the one that ends the live stream; if the credential
    // write then fails, the machine is out of the fleet with a token that
    // still exists. Telling somebody removing a stolen laptop that the whole
    // job was done is the failure this epic closes -- so the partial outcome
    // is on screen, with its repair.
    const connection = fakeConnection({ myWorkersWithStatus: [LIVE] });
    connection.query.fleetRevokeMachine.mockResolvedValue(
      rowsResult([
        {
          machineId: "v1:worker:registration:live",
          registrationState: "revoked",
          credentialState: "revoke_failed",
          alreadyRevoked: false,
          sentence:
            "Removed from the fleet, but its credential could not be revoked. The machine cannot be routed to, and it will be disconnected -- but revoke the credential from Settings before trusting that it cannot reconnect.",
        },
      ]),
    );
    mount(connection);
    await click(await screen.findByText("Studio mini"));
    await click(screen.getByRole("button", { name: "Remove this machine" }));
    await click(screen.getByRole("button", { name: "Remove Studio mini" }));

    expect(await screen.findByText(/could not be revoked/)).toBeTruthy();
    expect(screen.getByText(/Settings > Credentials/)).toBeTruthy();
  });

  it.each([false, true])("offers the correct uninstall location when revoked=%s", async (revoked) => {
    const row = revoked ? REVOKED : LIVE;
    const connection = fakeConnection({ myWorkersWithStatus: [row] });
    mount(connection, revoked);
    await click(await screen.findByText(revoked ? "Old laptop" : "Studio mini"));
    if (!revoked) await click(screen.getByRole("button", { name: "Remove this machine" }));

    const command = () => (screen.getByLabelText("the uninstall command") as HTMLInputElement).value;
    expect(command()).not.toContain("--user-local");
    await click(screen.getByRole("radio", { name: /My account only/ }));
    expect(command()).toContain("--user-local");
    await click(screen.getByRole("radio", { name: /System installation/ }));
    expect(command()).not.toContain("--user-local");
    expect(connection.query.fleetRevokeMachine).not.toHaveBeenCalled();
  });

  it("renders the reported local apps, marking only the runnable ones", async () => {
    const connection = fakeConnection({
      myWorkersWithStatus: [
        machineRow({
          id: "v1:worker:registration:apps",
          displayName: "Apps box",
          apps: [
            { id: "claude-code", allowed: true, signedIn: true, subscription: "present" },
            { id: "codex", allowed: false, signedIn: true, subscription: "none" },
          ],
        }),
      ],
    });
    mount(connection);
    await click(await screen.findByText("Apps box"));

    expect(screen.getByText("Ready")).toBeTruthy();
    expect(screen.getByText("Not allowed by Cockpit")).toBeTruthy();
  });

  it("folds a live update into the list without losing the projection", async () => {
    // THE FOLD IS NOT THE SEED. A collection's seed maps wire rows through
    // machineFromRow; its fold upserts the event payload AS THE ROW TYPE with
    // no projection hook in between (liveCollection.ts: `upsert(id, payload as
    // unknown as T)`). A collection typed with a PROJECTED row therefore holds
    // a raw wire row from the first update onward -- and every derived field
    // the surface reads (mergedLabels, platform, the display name) is simply
    // absent on it.
    //
    // Nothing caught this while the harness had no subscriptions: a seed-only
    // test exercises the half of a live surface that runs once.
    const connection = fakeConnection({ myWorkersWithStatus: [LIVE] });
    mount(connection);
    await screen.findByText("Studio mini");

    await act(async () => {
      connection.subscriptions.emit(
        "v1:worker:registration",
        machineRow({
          id: "v1:worker:registration:live",
          displayName: "Studio mini renamed",
          labels: { os: "darwin" },
          operatorLabels: { tier: "gold" },
        }),
      );
    });

    // The renamed row is on screen...
    expect(await screen.findByText("Studio mini renamed")).toBeTruthy();
    // ...and it is still a PROJECTED row: the merged label chip is a derived
    // field, absent on the raw payload, so this is what fails when the fold
    // bypasses the projection.
    expect(screen.getByText("tier=gold")).toBeTruthy();
  });

  it("cues a real change and stays SILENT on a heartbeat", async () => {
    // The two halves of a good arrival cue, and they pull against each other:
    // it has to fire when something happened, and it has to not fire the rest
    // of the time. A machine heartbeats every 15 seconds forever, so a cue
    // keyed on liveness is a permanent strobe -- the standing-badge failure
    // this cue exists to avoid.
    const connection = fakeConnection({ myWorkersWithStatus: [LIVE] });
    mount(connection);
    await screen.findByText("Studio mini");

    const rowOf = (name: string) =>
      (screen.getByText(name).closest(".os-livelist-row") as HTMLElement) ?? null;

    // A heartbeat: nothing but lastSeenAt moves.
    await act(async () => {
      connection.subscriptions.emit(
        "v1:worker:registration",
        machineRow({
          id: "v1:worker:registration:live",
          displayName: "Studio mini",
          labels: { os: "darwin", tier: "cheap" },
          operatorLabels: { tier: "gold", room: "studio" },
          activeCount: 1,
          lastSeenAt: new Date(Date.now() + 15_000).toISOString(),
        }),
      );
    });
    expect(rowOf("Studio mini").getAttribute("data-arrival")).toBeNull();

    // A rename: news.
    await act(async () => {
      connection.subscriptions.emit(
        "v1:worker:registration",
        machineRow({
          id: "v1:worker:registration:live",
          displayName: "Studio",
          labels: { os: "darwin", tier: "cheap" },
          operatorLabels: { tier: "gold", room: "studio" },
          activeCount: 1,
          lastSeenAt: new Date(Date.now() + 30_000).toISOString(),
        }),
      );
    });
    expect(rowOf("Studio").getAttribute("data-arrival")).toBe("updated");
  });

  it("cues a machine that ARRIVES, and lets the cue decay", async () => {
    const connection = fakeConnection({ myWorkersWithStatus: [LIVE] });
    mount(connection);
    await screen.findByText("Studio mini");

    await act(async () => {
      connection.subscriptions.emit(
        "v1:worker:registration",
        machineRow({ id: "v1:worker:registration:new", displayName: "Just paired" }),
      );
    });

    const row = screen.getByText("Just paired").closest(".os-livelist-row") as HTMLElement;
    expect(row.getAttribute("data-arrival")).toBe("added");
    // ...and it says so in words too, which is the cue a reduced-motion
    // reader gets when the animation is suppressed.
    expect(within(row).getByText("new")).toBeTruthy();
  });
});
