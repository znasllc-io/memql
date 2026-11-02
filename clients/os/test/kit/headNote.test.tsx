import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, it } from "vitest";

import { Head, HeadNote } from "../../src/kit";
import { Overview } from "../../src/kit/Overview";
import { PageFrame } from "../pageHarness";

afterEach(cleanup);

// WHAT STOPPED ARRIVING STANDS ON THE TITLE'S LINE. It comes on its own, whatever the
// person is doing, and as a block above the content it moved the whole page under the
// pointer. jsdom lays nothing out, so this holds the two halves the browser combines:
// the note is a child of the heading's line, and the stylesheet gives it only the room
// left over, so the line never wraps or grows for it (measured in a browser at 375,
// 480, 620, 760 and 1000 wide: the header's height does not change).

const CSS = readFileSync(join(__dirname, "..", "..", "src", "styles", "index.css"), "utf8");
const note = <HeadNote sentence="Updates are interrupted." next="Showing the last known state." />;

it.each([
  ["alone", (head: React.ReactNode) => head],
  ["inside a window, where the frame draws the trail", (head: React.ReactNode) => <PageFrame>{head}</PageFrame>],
])("stands on the title's line, %s", (_where, frame) => {
  render(<>{frame(<Head title="Machines" meta={3} note={note}><button type="button">Add</button></Head>)}</>);
  const line = screen.getByRole("heading", { name: "Machines" }).parentElement!;
  expect(screen.getByRole("status").parentElement).toBe(line);
  // Between what it qualifies and the acts, which keep their edge.
  expect([...line.children].map(child => child.className || child.tagName)).toEqual(["os-settings-title", "os-head-meta", "os-head-note", "os-head-actions"]);
});

it("draws no line of its own while nothing is wrong", () => {
  render(<Head title="Machines" note={null} />);
  expect(screen.queryByRole("status")).toBeNull();
});

it("says what is true now, and keeps the server's reason for hover", () => {
  render(<HeadNote sentence="Updates are interrupted." next="Showing the last known state." detail="stream closed" />);
  expect(screen.getByText("Updates are interrupted.")).toBeTruthy();
  expect(screen.getByText("Showing the last known state.")).toBeTruthy();
  expect(screen.queryByText("stream closed")).toBeNull();
  expect(screen.getByRole("status").title).toBe("stream closed");
});

it("spells itself out on hover when there is no reason to give, because the line can cut it off", () => {
  render(note);
  expect(screen.getByRole("status").title).toBe("Updates are interrupted. Showing the last known state.");
});

it("takes the overview's heading line too", () => {
  render(<Overview metrics={[]} scope="This cluster" note={note}>{null}</Overview>);
  expect(screen.getByRole("status").parentElement).toBe(screen.getByRole("heading", { name: "Overview" }).parentElement);
});

it("takes only the room left over, in the kit's tone mark with readable text", () => {
  expect(CSS).toContain(".os-head-note { flex: 1 1 0; min-width: 0; overflow: hidden; white-space: nowrap; text-overflow: ellipsis;");
  expect(CSS).toContain("border-left: 2px solid var(--os-warn);");
  // A row that pushes its spans to the far edge must not push this one.
  expect(CSS).toContain(".fleet-bank-heading > .os-head-note { margin-left: 0; }");
});
