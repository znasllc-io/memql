import { act, fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { chooseOption } from "../selectControl";

// A new goal names ONE account (account row ownership, section G): the single
// picker, sent to the engine as a one-element `accountIds` list, or an empty
// list for none, because the goal still declares the list.

vi.mock("../../src/apps/accounts/tie", () => ({
  useAccountOptions: () => [
    { id: "acme", name: "Acme Consulting", status: "active" },
    { id: "globex", name: "Globex", status: "active" },
  ],
}));

const { NewGoal } = await import("../../src/apps/nexus/NewGoal");

function mount() {
  const create = vi.fn(async (_facts: { statement: string; accountIds: string[] }) => "goal-1");
  render(
    <NewGoal
      create={{ busy: false, error: "", reset: () => {}, createdGoalId: "", createdRunId: "", create: create as never }}
      onCreated={() => {}}
      onCancel={() => {}}
    />,
  );
  fireEvent.change(screen.getByLabelText("The goal, in your own words"), { target: { value: "Close the books" } });
  return create;
}

describe("the new goal's account", () => {
  it("is a single choice, sent as a one-element list", async () => {
    const create = mount();
    expect(screen.queryByRole("group", { name: /Who this work is for/ })).toBeNull();
    chooseOption(screen.getByLabelText("Who this work is for"), "Globex");
    await act(async () => fireEvent.click(screen.getByRole("button", { name: "Start work" })));
    expect(create.mock.calls[0]![0].accountIds).toEqual(["globex"]);
  });

  it("sends an empty list when no account is chosen", async () => {
    const create = mount();
    await act(async () => fireEvent.click(screen.getByRole("button", { name: "Start work" })));
    expect(create.mock.calls[0]![0].accountIds).toEqual([]);
  });
});
