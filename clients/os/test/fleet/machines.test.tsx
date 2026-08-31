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
const { MachinesSection } = await import("../../src/apps/fleet/machines/MachinesSection");
const { fakeConnection, machineRow, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

function mount(connection: Conn, showRevoked = false) {
  h.connection = connection;
  return render(
    withSession(
      <MachinesProvider>
        <MachinesSection showRevoked={showRevoked} />
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

  it("asks an in-surface confirm that NAMES the machine before revoking", async () => {
    const connection = fakeConnection({ myWorkersWithStatus: [LIVE] });
    mount(connection);
    await click(await screen.findByText("Studio mini"));

    await click(screen.getByRole("button", { name: "Revoke this machine" }));
    // Nothing has been written yet -- the confirm is a step, not a label.
    expect(connection.query.revokeWorker).not.toHaveBeenCalled();
    expect(screen.getByRole("group", { name: "Revoke Studio mini" })).toBeTruthy();

    await type(screen.getByLabelText("Reason (optional)") as HTMLInputElement, "returned it");
    await click(screen.getByRole("button", { name: "Revoke Studio mini" }));

    expect(connection.query.revokeWorker).toHaveBeenCalledWith(
      expect.objectContaining({
        registrationId: "v1:worker:registration:live",
        revokedBy: "v1:identity:user:me",
        revokeReason: "returned it",
      }),
    );
    // revokedAt is a timestamp this client stamps; assert it is one rather
    // than pinning a clock.
    const args = connection.query.revokeWorker.mock.calls[0]?.[0] as { revokedAt: string };
    expect(Number.isNaN(Date.parse(args.revokedAt))).toBe(false);
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

    expect(screen.getByText("runnable")).toBeTruthy();
    expect(screen.getByText("not runnable -- not in the machine's apps.allow")).toBeTruthy();
  });
});
