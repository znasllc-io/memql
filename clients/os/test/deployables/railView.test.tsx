import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { Rail } from "../../src/apps/deployables/page/Rail";
import type { RailInput, StandingInput } from "../../src/apps/deployables/page/rail";
import type { DeploymentRow } from "../../src/apps/deployables/packages/rows";

// What the rail SAYS is pinned in rail.test.ts against the pure reading. This
// pins what a person SEES of it: the label and note of every stop, the state
// on the element the CSS keys on, the compact form's accessible names, and
// the reversed rail keeping its DOM order.

afterEach(cleanup);

const REPO = { sourceKind: "repo", repoUrl: "https://github.com/acme/shop", repoRef: "main" };

function deployment(over: Partial<DeploymentRow> = {}): DeploymentRow {
  return {
    id: "dep-1",
    packageId: "pkg-1",
    sourceVersion: "abc1234",
    status: "succeeded",
    report: { deployables: [{ name: "shop", kind: "spa", path: "apps/shop", buildPlan: "prebuilt", output: "dist", prebuilt: true }], dslDomains: [], problems: [], ok: true },
    dslVersion: "",
    deployables: [],
    snapshotArtifactId: "",
    buildLogTail: "",
    error: null,
    requestedBy: "u-me",
    startedAt: "2026-09-01T12:00:00Z",
    finishedAt: "2026-09-01T12:00:30Z",
    createdAt: "2026-09-01T12:00:00Z",
    ...over,
  };
}

const STANDING: StandingInput = {
  mode: "standing",
  pkg: REPO,
  app: "shop",
  run: deployment(),
  site: { hostname: "shop.memql.example.com", kind: "spa", status: "draft", bundleRef: "blob://sites/site-1/v1/" },
};

function statesIn(list: HTMLElement): string[] {
  return [...list.querySelectorAll("li")].map((li) => li.getAttribute("data-state") ?? "");
}

describe("the rail, drawn", () => {
  it("draws every stop with its label, its note and its state", () => {
    render(<Rail input={STANDING} />);
    const list = screen.getByRole("list", { name: "Deployable stops" });
    expect(statesIn(list)).toEqual(["done", "done", "done", "skipped", "open"]);
    // The labels, the answers, and the skipped reason -- what did not happen,
    // and why -- all read in surface.
    expect(screen.getByText("Source")).toBeTruthy();
    expect(screen.getByText("acme/shop at main")).toBeTruthy();
    expect(screen.getByText("shop.memql.example.com")).toBeTruthy();
    expect(screen.getByText("its built output is in the source")).toBeTruthy();
    expect(screen.getByText("Published to shop.memql.example.com. Not serving yet.")).toBeTruthy();
    // The state is also a sentence for a screen reader, per stop.
    expect(screen.getByText("waiting on you")).toBeTruthy();
    expect(screen.getAllByText("finished")).toHaveLength(3);
  });

  it("draws the blurb as the note of a stop that has nothing to say yet", () => {
    const compose: RailInput = { mode: "compose", answered: ["source"], open: "whatItIs", probeReason: "", report: null, problem: null };
    render(<Rail input={compose} />);
    expect(screen.getByText("What deploying this source would do, read from the tree")).toBeTruthy();
    expect(screen.getByText("complete")).toBeTruthy();
    expect(screen.getAllByText("not reachable yet")).toHaveLength(3);
  });

  it("compact: the five marks in a row, each named by its stop and state", () => {
    render(<Rail input={STANDING} compact />);
    const list = screen.getByRole("list", { name: "Deployable stops" });
    expect(list.getAttribute("data-compact")).toBe("true");
    expect(statesIn(list)).toEqual(["done", "done", "done", "skipped", "open"]);
    // No labels drawn...
    expect(screen.queryByText("Source")).toBeNull();
    expect(screen.queryByText("its built output is in the source")).toBeNull();
    // ...and every mark still says what it is.
    expect(screen.getByLabelText("Source, finished")).toBeTruthy();
    expect(screen.getByLabelText("Build, skipped")).toBeTruthy();
    expect(screen.getByLabelText("Live, waiting on you")).toBeTruthy();
  });

  it("the deploy mode keeps its name and its six stages, and reversed keeps the DOM order", () => {
    render(<Rail input={{ mode: "deploy", deployment: deployment() }} reversed />);
    const list = screen.getByRole("list", { name: "Deploy stages" });
    expect(list.getAttribute("data-reversed")).toBe("true");
    // Reversed is a READING direction: the array comes back reversed so the
    // picture runs bottom-up, and that is the one thing that changes.
    expect([...list.querySelectorAll(".os-rail-label")].map((el) => el.textContent)).toEqual([
      "Publish",
      "Roll",
      "Stage DSL",
      "Build",
      "Confirm",
      "Analyze",
    ]);
    expect(screen.getByText("this package ships no MemQL, so there is nothing to stage")).toBeTruthy();
  });

  it("mounts a stop's body beneath its note when the page hands one in", () => {
    render(<Rail input={STANDING} stopBody={(stage) => (stage.id === "live" ? <button type="button">Make it live</button> : null)} />);
    expect(screen.getByRole("button", { name: "Make it live" })).toBeTruthy();
  });
});
