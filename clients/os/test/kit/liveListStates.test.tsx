import { cleanup, render, screen } from "@testing-library/react";
import type { LiveSnapshot } from "@znasllc-io/memql-sdk-core/client";
import { afterEach, describe, expect, it } from "vitest";

import { EmptyState, LiveList, Notice, type LiveListSource } from "../../src/kit";

afterEach(cleanup);

// What a live list says when it has nothing to show, and who says it when the
// read failed. A list used to draw the engine's failure as a bare red line AND
// require an `emptyText` even for callers that draw their own empty state, so a
// caller that drew a Notice for the failure showed it twice (audit C11) and a
// caller with an EmptyState passed a sentence it never used.

interface Row {
  id: string;
  name: string;
}

/** A list source that never changes: the state under test, held. */
function held(snapshot: LiveSnapshot<Row>): LiveListSource<Row> {
  return { snapshot, subscribe: () => () => {} };
}

const snapshot = (rows: Row[], state: LiveSnapshot<Row>["state"], error = ""): LiveSnapshot<Row> => ({
  rows,
  state,
  error,
  version: 1,
});

const ALPHA: Row = { id: "a", name: "alpha" };

describe("LiveList states", () => {
  it("draws the caller's error instead of the raw line", () => {
    const { container } = render(
      <LiveList<Row>
        source={held(snapshot([], "disconnected", "unavailable: the cluster did not answer"))}
        rowId={(row) => row.id}
        fingerprint={(row) => row.name}
        label="Campaigns"
        renderRow={(row) => <span>{row.name}</span>}
        errorContent={<Notice tone="error" sentence="This cluster did not return your campaigns." />}
      />,
    );
    expect(screen.getByText("This cluster did not return your campaigns.")).toBeTruthy();
    expect(container.querySelector(".os-ask-error")).toBeNull();
    // The engine's own text is the caller's to place: the raw line is gone,
    // not moved.
    expect(screen.queryByText("unavailable: the cluster did not answer")).toBeNull();
  });

  it("draws the caller's error beside the rows it still holds", () => {
    // An error with rows on screen is a different situation from an empty list
    // (the snapshot keeps them apart on purpose): the rows stay, the error
    // is the caller's.
    const { container } = render(
      <LiveList<Row>
        source={held(snapshot([ALPHA], "degraded", "unavailable"))}
        rowId={(row) => row.id}
        fingerprint={(row) => row.name}
        label="Campaigns"
        renderRow={(row) => <span>{row.name}</span>}
        errorContent={<Notice tone="error" sentence="Nothing below is current." />}
      />,
    );
    expect(screen.getByText("alpha")).toBeTruthy();
    expect(screen.getByText("Nothing below is current.")).toBeTruthy();
    expect(container.querySelector(".os-ask-error")).toBeNull();
  });

  it("keeps the raw line when the caller gives no error content", () => {
    const { container } = render(
      <LiveList<Row>
        source={held(snapshot([ALPHA], "degraded", "unavailable: the cluster did not answer"))}
        rowId={(row) => row.id}
        fingerprint={(row) => row.name}
        label="Campaigns"
        emptyText="Nothing yet."
        renderRow={(row) => <span>{row.name}</span>}
      />,
    );
    expect(container.querySelector(".os-ask-error")?.textContent).toBe("unavailable: the cluster did not answer");
  });

  it("never swallows a failure: empty error content falls back to the raw line", () => {
    // A caller whose own error node came out empty (a flag not set yet) must
    // not make the list silent about a read that failed.
    const { container } = render(
      <LiveList<Row>
        source={held(snapshot([ALPHA], "degraded", "unavailable: the cluster did not answer"))}
        rowId={(row) => row.id}
        fingerprint={(row) => row.name}
        label="Campaigns"
        renderRow={(row) => <span>{row.name}</span>}
        errorContent={null}
      />,
    );
    expect(container.querySelector(".os-ask-error")?.textContent).toBe("unavailable: the cluster did not answer");
  });

  it("draws no error content while the last read succeeded", () => {
    const { container } = render(
      <LiveList<Row>
        source={held(snapshot([ALPHA], "live"))}
        rowId={(row) => row.id}
        fingerprint={(row) => row.name}
        label="Campaigns"
        renderRow={(row) => <span>{row.name}</span>}
        errorContent={<Notice tone="error" sentence="This cluster did not return your campaigns." />}
      />,
    );
    expect(screen.getByText("alpha")).toBeTruthy();
    expect(screen.queryByText("This cluster did not return your campaigns.")).toBeNull();
    expect(container.querySelector(".os-notice")).toBeNull();
  });

  it("an empty live list needs only its empty content", () => {
    render(
      <LiveList<Row>
        source={held(snapshot([], "live"))}
        rowId={(row) => row.id}
        fingerprint={(row) => row.name}
        label="Campaigns"
        renderRow={(row) => <span>{row.name}</span>}
        emptyContent={<EmptyState title="No campaigns yet">Add a campaign to send a template to an audience.</EmptyState>}
      />,
    );
    expect(screen.getByRole("heading", { name: "No campaigns yet" })).toBeTruthy();
    expect(screen.getByText("Add a campaign to send a template to an audience.")).toBeTruthy();
  });

  it("an empty live list says its empty text when it has no empty content", () => {
    render(
      <LiveList<Row>
        source={held(snapshot([], "live"))}
        rowId={(row) => row.id}
        fingerprint={(row) => row.name}
        label="Rows"
        emptyText="Nothing yet."
        renderRow={(row) => <span>{row.name}</span>}
      />,
    );
    expect(screen.getByText("Nothing yet.")).toBeTruthy();
  });

  it("an empty live list uses its empty text only when there is no empty content", () => {
    render(
      <LiveList<Row>
        source={held(snapshot([], "live"))}
        rowId={(row) => row.id}
        fingerprint={(row) => row.name}
        label="Rows"
        emptyText="Nothing yet."
        emptyContent={<EmptyState title="No rows">Add one.</EmptyState>}
        renderRow={(row) => <span>{row.name}</span>}
      />,
    );
    expect(screen.getByRole("heading", { name: "No rows" })).toBeTruthy();
    expect(screen.queryByText("Nothing yet.")).toBeNull();
  });

  it("an empty live list with neither draws nothing, not an empty caption", () => {
    const { container } = render(
      <LiveList<Row>
        source={held(snapshot([], "live"))}
        rowId={(row) => row.id}
        fingerprint={(row) => row.name}
        label="Rows"
        renderRow={(row) => <span>{row.name}</span>}
      />,
    );
    expect(container.querySelector(".os-caption")).toBeNull();
    expect(container.querySelectorAll("li")).toHaveLength(0);
  });

  it("a live list still reading is never called empty", () => {
    const { container } = render(
      <LiveList<Row>
        source={held(snapshot([], "seeding"))}
        rowId={(row) => row.id}
        fingerprint={(row) => row.name}
        label="Rows"
        emptyContent={<EmptyState title="No rows">Add one.</EmptyState>}
        renderRow={(row) => <span>{row.name}</span>}
      />,
    );
    expect(screen.queryByRole("heading", { name: "No rows" })).toBeNull();
    expect(container.querySelector(".os-record-skeleton")).not.toBeNull();
  });
});
