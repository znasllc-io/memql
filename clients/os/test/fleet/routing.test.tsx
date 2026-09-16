import { fireEvent, act, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { RoutingSection } = await import("../../src/apps/fleet/routing/RoutingSection");
const { RoutingRecordView } = await import("../../src/apps/fleet/routing/RoutingRecordView");
const { invocationFromRow } = await import("../../src/apps/fleet/rows");
const { fakeConnection, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

// The picker is the shell's own selection control (a button carrying
// role="radio" + aria-checked), not the platform's radio input. Asserting on
// aria-checked and the accessible name is what a person -- or a screen reader
// -- actually perceives, and it survives the control being restyled again.
function isChosen(name: RegExp): boolean {
  return name.test(screen.getByRole("combobox", { name: "Routing strategy" }).textContent ?? "");
}

async function chooseStrategy(value: string) {
  fireEvent.click(screen.getByRole("combobox", { name: "Routing strategy" }));
  await click(screen.getByRole("option", { name: ({ firstFit: "First eligible", roundRobin: "Take turns", leastLoaded: "Least busy", labelMatch: "Best label match" } as Record<string,string>)[value] }));
}

async function click(el: Element) {
  await act(async () => {
    (el as HTMLElement).click();
  });
}

function mount(connection: Conn) {
  h.connection = connection;
  const view = render(withSession(<RoutingSection />));
  view.container.querySelectorAll("details").forEach(el => { el.open = true; });
  return view;
}

beforeEach(() => {
  h.connection = null;
});

describe("the routing policy editor", () => {
  it("states the absent case and FABRICATES NO ROW until a save", async () => {
    const connection = fakeConnection({ myRoutingPolicies: [] });
    mount(connection);

    expect(await screen.findByText(/Using the default/)).toBeTruthy();
    // The defaults are NAMED, because they are what the router applies to
    // this person today -- "not configured" and "configured to the defaults"
    // are different facts and only one of them is true here.
    const note = screen.getByText(/Using the default/);
    expect(note.textContent).toContain("first eligible machine");
    expect(note.textContent).toContain("next match");

    // Opening the section is a READ. Nothing was written.
    expect(connection.query.createRoutingPolicy).not.toHaveBeenCalled();
    expect(connection.query.updateRoutingPolicy).not.toHaveBeenCalled();
  });

  it("offers exactly the enum values, each with its meaning", async () => {
    mount(fakeConnection({ myRoutingPolicies: [] }));
    await screen.findByText(/Using the default/);

    await click(screen.getByRole("combobox", { name: "Routing strategy" }));
    expect(screen.getAllByRole("option").map(el => el.textContent)).toEqual(["First eligible", "Take turns", "Least busy", "Best label match"]);
    await click(screen.getByRole("option", { name: "Least busy" }));
    expect(screen.getByText(/Fewest calls in flight/)).toBeTruthy();
    await click(screen.getByRole("combobox", { name: "Routing fallback" }));
    expect(screen.getAllByRole("option").map(el => el.textContent)).toEqual(["Return the refusal", "Try the next match"]);
    await click(screen.getByRole("option", { name: "Try the next match" }));
    expect(screen.getByText(/never a re-run/)).toBeTruthy();
  });

  it("CREATES on the first save and mints an id for it", async () => {
    const connection = fakeConnection({ myRoutingPolicies: [] });
    mount(connection);
    await screen.findByText(/Using the default/);

    await chooseStrategy("leastLoaded");
    await click(screen.getByRole("button", { name: "Create policy" }));

    expect(connection.query.updateRoutingPolicy).not.toHaveBeenCalled();
    const args = connection.query.createRoutingPolicy.mock.calls[0]?.[0] as {
      policyId: string;
      strategy: string;
      fallback: string;
      requireLabels: Record<string, string>;
    };
    expect(args.strategy).toBe("leastLoaded");
    // The fallback the caption promised is what gets written, not a blank.
    expect(args.fallback).toBe("nextMatching");
    expect(args.policyId.length).toBeGreaterThan(0);
    expect(args.requireLabels).toEqual({});
  });

  it("UPDATES an existing row in place, keeping its id", async () => {
    const connection = fakeConnection({
      myRoutingPolicies: [
        {
          id: "v1:worker:routingPolicy:p1",
          strategy: "roundRobin",
          fallback: "none",
          requireLabels: { gpu: "true" },
          preferLabels: {},
          active: true,
        },
      ],
    });
    mount(connection);
    await waitFor(() =>
      expect(isChosen(/Take turns/)).toBe(
        true,
      ),
    );

    await chooseStrategy("labelMatch");
    await click(screen.getByRole("button", { name: "Save policy" }));

    expect(connection.query.createRoutingPolicy).not.toHaveBeenCalled();
    expect(connection.query.updateRoutingPolicy).toHaveBeenCalledWith({
      policyId: "v1:worker:routingPolicy:p1",
      strategy: "labelMatch",
      fallback: "none",
      // The label maps are re-sent, and that is load-bearing: the mutation
      // writes `requireLabels: args.requireLabels ?? {}`, so an omitted
      // argument BLANKS the map. A strategy-only save that dropped them
      // would silently wipe the operator's requirements.
      requireLabels: { gpu: "true" },
      preferLabels: {},
      // Re-sent for the same reason the label maps are: `updateRoutingPolicy`
      // writes `modelPreference: args.modelPreference ?? []`, so an omitted
      // argument BLANKS the order (epic memql#5096). A strategy-only save
      // that dropped it would silently discard the operator's model ranking.
      modelPreference: [],
    });
  });

  it("resolves an untouched draft toward the row when the policy changes elsewhere", async () => {
    const connection = fakeConnection({
      myRoutingPolicies: [
        { id: "p1", strategy: "firstFit", fallback: "nextMatching", active: true },
      ],
    });
    mount(connection);
    await waitFor(() =>
      expect(isChosen(/First eligible/)).toBe(true),
    );

    // Delivered as a FOLDED EVENT, which is how a policy edited in another
    // tab or in the portal actually reaches this editor --
    // v1:worker:routingPolicy is broadcast. The editor offers no refresh
    // button while its feed is live, precisely because it does not need one.
    await act(async () => {
      connection.subscriptions.emit("v1:worker:routingPolicy", {
        id: "p1",
        strategy: "leastLoaded",
        fallback: "none",
        active: true,
      });
    });

    await waitFor(() =>
      expect(isChosen(/Least busy/)).toBe(
        true,
      ),
    );
  });

  it("does NOT cry stale for a purely local edit", async () => {
    const connection = fakeConnection({
      myRoutingPolicies: [
        { id: "p1", strategy: "firstFit", fallback: "nextMatching", active: true },
      ],
    });
    mount(connection);
    await waitFor(() =>
      expect(isChosen(/First eligible/)).toBe(true),
    );

    await chooseStrategy("labelMatch");

    // A draft differs from its row the instant anybody types. Saying "this
    // changed somewhere else" then trains an operator to ignore the one
    // message that means their save is about to overwrite somebody.
    expect(screen.queryByText(/changed somewhere else/)).toBeNull();
  });

  it("keeps an operator's edits when the row changes underneath, and says so", async () => {
    const connection = fakeConnection({
      myRoutingPolicies: [
        { id: "p1", strategy: "firstFit", fallback: "nextMatching", active: true },
      ],
    });
    mount(connection);
    await waitFor(() =>
      expect(isChosen(/First eligible/)).toBe(true),
    );

    await chooseStrategy("labelMatch");

    await act(async () => {
      connection.subscriptions.emit("v1:worker:routingPolicy", {
        id: "p1",
        strategy: "roundRobin",
        fallback: "none",
        active: true,
      });
    });

    // Silently discarding somebody's typing is worse than either resolution,
    // so the edit stands and the disagreement is shown.
    await waitFor(() => expect(screen.getByText(/changed somewhere else/)).toBeTruthy());
    expect(isChosen(/Best label match/)).toBe(true);
  });

  it("renders a refusal in surface, keeping the edits", async () => {
    const connection = fakeConnection({ myRoutingPolicies: [] });
    connection.query.createRoutingPolicy.mockRejectedValue(new Error("policy refused"));
    mount(connection);
    await screen.findByText(/Using the default/);

    await chooseStrategy("labelMatch");
    await click(screen.getByRole("button", { name: "Create policy" }));

    await waitFor(() => expect(screen.getByText("policy refused")).toBeTruthy());
    expect(isChosen(/Best label match/)).toBe(true);
  });
});

describe("the routing record", () => {
  function renderRecord(routing: unknown) {
    return render(
      <RoutingRecordView routing={invocationFromRow({ id: "i", routing } as never).routing} />,
    );
  }

  it("renders a multi-attempt nextMatching run with the candidates in TRY ORDER", () => {
    renderRecord({
      policyId: "p1",
      strategy: "labelMatch",
      candidatesConsidered: ["reg-b", "reg-a", "reg-c"],
      attempts: 2,
      selectedBy: "policy",
    });
    const candidates = screen.getAllByRole("listitem").map((el) => el.textContent ?? "");
    expect(candidates[0]).toContain("reg-b");
    expect(candidates[1]).toContain("reg-a");
    expect(candidates[2]).toContain("reg-c");
    // The first two were tried; the third was never reached.
    expect(candidates[0]).toContain("tried");
    expect(candidates[1]).toContain("tried");
    expect(candidates[2]).not.toContain("tried");
  });

  it("names what a call was rerouted FROM", () => {
    renderRecord({
      policyId: "",
      strategy: "firstFit",
      candidatesConsidered: ["reg-a"],
      attempts: 1,
      selectedBy: "reroute",
      reroutedFrom: "workbench",
    });
    expect(screen.getByText(/Rerouted from/).textContent).toContain("workbench");
    // An empty policyId is not a missing value: it means the defaults applied.
    expect(screen.getByText("default policy")).toBeTruthy();
  });

  it("says an EMPTY routing object recorded no decision, rather than drawing an empty table", () => {
    renderRecord({});
    expect(screen.getByText(/No routing decision recorded/)).toBeTruthy();
    expect(screen.queryByText("Strategy")).toBeNull();
  });

  it("says the same for a denial that happened before a machine was picked", () => {
    render(
      <RoutingRecordView
        routing={
          invocationFromRow({ id: "i", outcome: "no_worker_available" } as never).routing
        }
      />,
    );
    expect(screen.getByText(/refused before a machine was chosen/)).toBeTruthy();
  });

  it("renders the merged require/prefer labels the candidates were filtered by", () => {
    renderRecord({
      strategy: "labelMatch",
      candidatesConsidered: [],
      attempts: 1,
      requireLabels: { gpu: "true" },
      preferLabels: { room: "studio" },
    });
    expect(screen.getByText("gpu=true")).toBeTruthy();
    expect(screen.getByText("room=studio")).toBeTruthy();
    // No candidates is stated rather than left blank.
    expect(screen.getByText(/No candidates were recorded/)).toBeTruthy();
  });
});
