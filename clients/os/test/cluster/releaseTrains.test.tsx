import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Result } from "@znasllc-io/memql-sdk-core/client";
import { ReleaseTrainPanel, type TrainQueries } from "../../src/apps/cluster/releases/ReleaseTrainPanel";
import { readTheBar } from "../readTheBar";

beforeEach(() => { vi.useFakeTimers({ toFake: ["Date"] }); });
afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); });

function row(fields: Record<string, unknown>) {
  return { id: String(fields.id ?? "t1"), concept: "v1:pipelines:train", payload: { repository: "acme/shop", branch: "train/2026-11-02-1", state: "landed", version: "0.31.5", departedAt: "2026-11-02T04:00:00Z", pullRequestUrl: "https://github.com/acme/shop/pull/7", ...fields } };
}

function deployment(fields: Record<string, unknown>) {
  return { id: String(fields.id ?? "d1"), concept: "v1:pipelines:pipelineDeployment", payload: { repository: "acme/shop", version: "0.31.5", state: "deployed", ...fields } };
}

const fromMain = { repository: "acme/shop", fromMain: true, canRelease: false, canDeploy: false, release: false, deploy: false, paused: false };
const engine = { ...fromMain, canRelease: true, canDeploy: true, installRepository: "acme/install", windowStart: "00:00", windowEnd: "07:00", timeZone: "UTC" };

const empty = () => new Result({ data: [] } as never);

function fixture({ trains = [], deployments = [], repositories = [fromMain], express }: { trains?: object[]; deployments?: object[]; repositories?: object[]; express?: TrainQueries["pipelinesTrainExpress"] } = {}) {
  const calls: { name: string; args: unknown }[] = [];
  const note = (name: string, args: unknown) => { calls.push({ name, args }); return empty(); };
  const query = {
    pipelineTrainsForOwner: async () => new Result({ data: trains } as never),
    pipelineDeploymentsForOwner: async () => new Result({ data: deployments } as never),
    pipelinesTrainStatus: async () => new Result({ data: [{ id: "s", concept: "", payload: { repositories } }] } as never),
    pipelinesTrainExpress: async (args: never, opts: never) => { calls.push({ name: "express", args }); return express ? express(args, opts) : empty(); },
    pipelinesTrainStanding: async (args: never) => note("standing", args),
    pipelinesDeployRelease: async (args: never) => note("deploy", args),
    pipelinesDeploymentResume: async (args: never) => note("resume", args),
  } as unknown as TrainQueries;
  return { query, calls };
}

async function press(name: string) {
  fireEvent.click(await screen.findByRole("button", { name }));
  await act(async () => { await Promise.resolve(); });
}

// A question answered in place holds its buttons a moment, so a press meant for what
// was there before cannot answer it.
async function answer(name: string) {
  await screen.findByRole("button", { name });
  await readTheBar();
  await press(name);
}

describe("the latest release", () => {
  it("shows nothing for repositories with no release settings", async () => {
    const { query } = fixture({ repositories: [] });
    const { container } = render(<ReleaseTrainPanel query={query} connected />);
    await act(async () => { await Promise.resolve(); });
    expect(container.textContent).toBe("");
  });

  it("shows a repository before its first release, and lets one start", async () => {
    const { query, calls } = fixture();
    render(<ReleaseTrainPanel query={query} connected />);
    expect(await screen.findByText("No release yet")).toBeTruthy();
    await press("Start release now");
    expect(calls).toEqual([{ name: "express", args: { repository: "acme/shop", pullRequests: [] } }]);
  });

  it("shows the newest release of each repository, its state and why it stopped", async () => {
    const { query } = fixture({
      repositories: [fromMain, { ...fromMain, repository: "acme/other" }],
      trains: [
        row({ id: "t3", state: "held", heldReason: "tests.go-unit#1 failed again on its re-run" }),
        row({ id: "t2", state: "landed", branch: "train/2026-11-01-1" }),
        row({ id: "t1", repository: "acme/other", state: "landed", version: "1.2.3" }),
      ],
    });
    render(<ReleaseTrainPanel query={query} connected />);
    expect(await screen.findByText("Held")).toBeTruthy();
    expect(screen.getByText("tests.go-unit#1 failed again on its re-run")).toBeTruthy();
    expect(screen.getByRole("link", { name: "acme/shop" }).getAttribute("href")).toBe("https://github.com/acme/shop/pull/7");
    expect(screen.queryByText(/train\/2026-11-01-1/)).toBeNull();
    expect(screen.getByText("acme/other")).toBeTruthy();
  });

  it("says why a start that found nothing was cancelled", async () => {
    const { query } = fixture({ trains: [row({ state: "cancelled", heldReason: "Nothing new on main since the last release." })] });
    render(<ReleaseTrainPanel query={query} connected />);
    expect(await screen.findByText("Nothing new on main since the last release.")).toBeTruthy();
  });

  it("offers a train of pull requests no start of its own", async () => {
    const { query } = fixture({ repositories: [{ ...fromMain, fromMain: false }], trains: [row({ state: "landed" })] });
    render(<ReleaseTrainPanel query={query} connected />);
    expect(await screen.findByText("Landed")).toBeTruthy();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("asks before replacing a held release", async () => {
    const { query, calls } = fixture({ trains: [row({ state: "held" })] });
    render(<ReleaseTrainPanel query={query} connected />);
    await press("Replace with a new release");
    expect(calls).toEqual([]);
    expect(screen.getByText(/deletes its branch/)).toBeTruthy();
    await answer("Keep it");
    expect(calls).toEqual([]);
    await press("Replace with a new release");
    await answer("Replace it");
    expect(calls).toEqual([{ name: "express", args: { repository: "acme/shop", pullRequests: [] } }]);
  });

  it("offers nothing while one is building, testing, landing or publishing", async () => {
    for (const { state, word } of [{ state: "running", word: "Testing" }, { state: "releasing", word: "Releasing" }]) {
      const { query } = fixture({ trains: [row({ state })] });
      const { unmount } = render(<ReleaseTrainPanel query={query} connected />);
      expect(await screen.findByText(word)).toBeTruthy();
      expect(screen.queryByRole("button")).toBeNull();
      unmount();
    }
  });

  it("shows the engine's refusal in its own words", async () => {
    const { query } = fixture({ trains: [row({ state: "landed" })], express: async () => { throw new Error("pipelines: train/2026-11-02-2 is running; start the express train when it has landed or is held"); } });
    render(<ReleaseTrainPanel query={query} connected />);
    await press("Start release now");
    expect(await screen.findByText(/is running; start the express train/)).toBeTruthy();
  });
});

describe("going to production", () => {
  it("asks before deploying, and says what a failure does", async () => {
    const { query, calls } = fixture({ repositories: [engine], trains: [row({ state: "published" })] });
    render(<ReleaseTrainPanel query={query} connected />);
    expect(await screen.findByText("Released")).toBeTruthy();
    await press("Deploy newest release");
    expect(calls).toEqual([]);
    expect(screen.getByText(/goes back to what ran before, unless the release changes tables or data/)).toBeTruthy();
    await answer("Deploy it");
    expect(calls).toEqual([{ name: "deploy", args: { repository: "acme/shop" } }]);
  });

  it("shows what a deploy is doing, and offers no second one meanwhile", async () => {
    const { query } = fixture({ repositories: [engine], trains: [row({ state: "published" })], deployments: [deployment({ state: "syncing" })] });
    render(<ReleaseTrainPanel query={query} connected />);
    expect(await screen.findByText("Deploying")).toBeTruthy();
    expect(screen.getByText(/v0\.31\.5/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Deploy newest release" })).toBeNull();
  });

  it("says why a release waits for the button", async () => {
    const { query } = fixture({ repositories: [engine], deployments: [deployment({ state: "waiting", guards: [{ code: "tables_or_data", detail: "It changes tables or data: 2026_things.go." }] })] });
    render(<ReleaseTrainPanel query={query} connected />);
    expect(await screen.findByText("It changes tables or data: 2026_things.go.")).toBeTruthy();
  });

  it("offers Resume after one went back, and says why", async () => {
    const { query, calls } = fixture({ repositories: [{ ...engine, paused: true }], deployments: [deployment({ state: "rolled_back", reason: "The rollout failed its verification." })] });
    render(<ReleaseTrainPanel query={query} connected />);
    expect(await screen.findByText("Rolled back")).toBeTruthy();
    expect(screen.getByText("The rollout failed its verification.")).toBeTruthy();
    await answer("Resume automatic deploys");
    expect(calls).toEqual([{ name: "resume", args: { repository: "acme/shop" } }]);
  });
});

describe("the owner's word", () => {
  it("asks before letting code go out unread, and stops without asking", async () => {
    const { query, calls } = fixture({ repositories: [{ ...engine, release: true }] });
    render(<ReleaseTrainPanel query={query} connected />);
    const [publish, deploy] = await screen.findAllByRole("switch");
    expect((publish as HTMLInputElement).checked).toBe(true);
    expect((deploy as HTMLInputElement).checked).toBe(false);
    fireEvent.click(deploy!);
    await act(async () => { await Promise.resolve(); });
    expect(calls).toEqual([]);
    expect(screen.getByText(/reaches production overnight with nobody reading it/)).toBeTruthy();
    await answer("Turn it on");
    expect(calls).toEqual([{ name: "standing", args: { repository: "acme/shop", release: true, deploy: true } }]);
    calls.length = 0;
    fireEvent.click((await screen.findAllByRole("switch"))[0]!);
    await act(async () => { await Promise.resolve(); });
    expect(calls).toEqual([{ name: "standing", args: { repository: "acme/shop", release: false, deploy: false } }]);
  });

  it("will not take a deploy with no release, or no window", async () => {
    const { query } = fixture({ repositories: [{ ...engine, release: false }] });
    render(<ReleaseTrainPanel query={query} connected />);
    const switches = await screen.findAllByRole("switch");
    expect((switches[1] as HTMLInputElement).disabled).toBe(true);
  });

  it("shows no switch a repository cannot use", async () => {
    const { query } = fixture({ repositories: [fromMain] });
    render(<ReleaseTrainPanel query={query} connected />);
    await screen.findByText("No release yet");
    expect(screen.queryByRole("switch")).toBeNull();
  });
});
