import { act, render, screen, waitFor, within } from "@testing-library/react";
import { useEffect } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { packageFromRow } from "../../../src/apps/deployables/packages/rows";
import { ALL_PARTS, partsWithout, type PartsHeld } from "../../../src/apps/deployables/parts";
import { ConnectPage } from "../../../src/apps/deployables/pipelines/connect/ConnectPage";
import { useConnectFlow, type ConnectFlow } from "../../../src/apps/deployables/pipelines/connect/useConnectFlow";
import { pipelineFromRow, type PipelineRow } from "../../../src/apps/deployables/pipelines/rows";
import type { Breadcrumb } from "../../../src/kit/Breadcrumbs";
import { MachinesProvider, WORKER_REGISTRATION_CONCEPT } from "../../../src/live/machines";
import { chooseOption, selectedLabel } from "../../selectControl";
import { click, emit, fakeConnection, rowsResult, withSession, type FakeConnection, type FakeSeed } from "../harness";
import { PACKAGE_ID, SHA_A, VIEWER, packageRow, pipelineRow } from "./fixtures";

// CONNECT PIPELINE, as a person meets it (issue memql#5502): the page over a
// source, its flow held ABOVE it the way DeployablesApp holds it, the shell's
// machines feed beside it. The real hook, the real readings, the real
// generated builders: what is asserted is what a person sees and the call
// string that reaches the wire.

// ---------------------------------------------------------------------------
// The wire
// ---------------------------------------------------------------------------

function stepRow(name: string, over: Record<string, unknown> = {}): Row {
  return { name, packages: "", only: "", shards: 0, bucket: "", needs: [], secrets: [], services: [], ...over } as Row;
}

/** A manifest no step of which needs a machine. */
const PLAIN = {
  stages: [
    { name: "checks", on: [], channel: "", steps: [stepRow("build-vet")] },
    { name: "tests", on: [], channel: "", steps: [stepRow("go-tests", { shards: 4 }), stepRow("db-tests")] },
    { name: "deploy", on: ["push"], channel: "", steps: [stepRow("verify-rollout", { secrets: ["VERIFY_TOKEN"] })] },
    { name: "notify", on: ["merge_group", "push"], channel: "znas-instance", steps: [] },
  ],
  needs: [] as string[],
};

/** The same manifest with a step that needs Docker. */
const NEEDS = {
  stages: [
    { name: "checks", on: [], channel: "", steps: [stepRow("build-vet")] },
    { name: "tests", on: [], channel: "", steps: [stepRow("go-tests", { shards: 4 }), stepRow("os-checks", { needs: ["docker"] })] },
    { name: "deploy", on: ["push"], channel: "", steps: [stepRow("verify-rollout", { secrets: ["VERIFY_TOKEN"] })] },
  ],
  needs: ["docker"],
};

/** What pipelinesPreview answers: one row, a refusal as data rather than an error. */
function previewRow(over: Record<string, unknown> = {}): Row {
  return {
    repository: "acme/shop",
    defaultBranch: "main",
    sha: SHA_A,
    name: "shop",
    checkName: "MemQL / shop",
    ...PLAIN,
    secrets: ["VERIFY_TOKEN"],
    suggestedDelivery: "webhook",
    existing: null,
    refusal: null,
    ...over,
  } as Row;
}

/** One of the viewer's registrations, as myWorkersWithStatus answers it. */
function machineRow(over: Record<string, unknown> = {}): Row {
  return {
    id: "v1:worker:registration:m-studio",
    ownerUserId: VIEWER,
    name: "studio-mac",
    displayName: "",
    platformInfo: { os: "darwin", arch: "arm64", hostname: "studio-mac" },
    labels: {},
    operatorLabels: {},
    concurrency: { HEADLESS: 2 },
    activeCount: 0,
    registeredAt: "2026-09-01T00:00:00Z",
    connectedNodeId: "agent-0",
    lastSeenAt: new Date().toISOString(),
    ...over,
  } as Row;
}

const ALLOWED = machineRow({ labels: { pipelines: "allowed" } });

/**
 * The Deployables fake, answering the machines read too.
 *
 * The shell's machines feed seeds with `query myWorkersWithStatus()`, which
 * the Deployables harness has no answer for; this answers it here rather than
 * teaching that harness about another app's concept. `holdPreview` keeps the
 * manifest read in flight until the test lets it land.
 */
function connectionFor(seed: FakeSeed, opts: { machines?: Row[]; holdPreview?: Promise<void> } = {}): FakeConnection {
  const connection = fakeConnection(seed);
  const query = connection.query as unknown as { executeNamed: (name: string, call: string, callOpts?: unknown) => Promise<unknown> };
  const inner = query.executeNamed;
  query.executeNamed = vi.fn(async (name: string, call: string, callOpts?: unknown) => {
    if (call === "query myWorkersWithStatus()") return rowsResult(opts.machines ?? []);
    if (opts.holdPreview !== undefined && call.startsWith("builtin pipelinesPreview(")) await opts.holdPreview;
    return inner(name, call, callOpts);
  });
  return connection;
}

// ---------------------------------------------------------------------------
// The page, under a flow held above it
// ---------------------------------------------------------------------------

interface HarnessProps {
  shown?: boolean;
  pipeline?: PipelineRow | null;
  can?: PartsHeld;
  onBack?: () => void;
  onDone?: () => void;
  flowRef?: { current: ConnectFlow | null };
  trail?: readonly Breadcrumb[];
}

/**
 * What DeployablesApp and its section do: the flow lives in a component that
 * outlives the page, which is started over a source and then shown. `shown`
 * false is the page gone -- another tab, the source page -- with the flow
 * still held.
 */
function Harness({ shown = true, pipeline = null, can = ALL_PARTS, onBack = () => {}, onDone = () => {}, flowRef, trail }: HarnessProps) {
  const flow = useConnectFlow();
  const pkg = packageFromRow(packageRow());
  const { start } = flow;
  useEffect(() => {
    start(pkg.id, pipeline);
  }, [start, pkg.id, pipeline]);
  if (flowRef) flowRef.current = flow;
  if (!shown || flow.packageId === "") return null;
  return <ConnectPage pkg={pkg} pipeline={pipeline} flow={flow} can={can} backLabel="shop" onBack={onBack} onDone={onDone} trail={trail} />;
}

function tree(props: HarnessProps) {
  return withSession(
    <MachinesProvider>
      <Harness {...props} />
    </MachinesProvider>,
    { role: "owner", userId: VIEWER },
  );
}

function mount(connection: FakeConnection, props: HarnessProps = {}) {
  h.connection = connection;
  return render(tree(props));
}

// ---------------------------------------------------------------------------
// Reading the page
// ---------------------------------------------------------------------------

function floor(): HTMLElement {
  const bar = document.querySelector<HTMLElement>(".os-actbar");
  if (bar === null) throw new Error("no action bar on the page");
  return bar;
}

function floorWord(): string {
  return floor().querySelector(".os-actbar-word")?.textContent ?? "";
}

function act_(label: string): HTMLElement {
  return within(floor()).getByRole("button", { name: label });
}

function noAct(label: string): boolean {
  return within(floor()).queryByRole("button", { name: label }) === null;
}

function railLine(name: string): HTMLElement {
  const line = [...document.querySelectorAll<HTMLElement>(".os-rail-line")].find(
    (el) => el.querySelector(".os-rail-label")?.textContent === name,
  );
  if (line === undefined) throw new Error(`no opening line for ${name} on the rail`);
  return line;
}

function railAnswer(name: string): string {
  return railLine(name).querySelector(".os-rail-answer")?.textContent ?? "";
}

function fact(label: string): string {
  const term = [...document.querySelectorAll(".os-facts dt")].find((el) => el.textContent === label);
  return term?.nextElementSibling?.textContent ?? "";
}

function choices(): HTMLElement[] {
  return screen.queryAllByRole("radio");
}

function choiceNames(): string[] {
  return choices().map((el) => el.querySelector(".os-choice-card-name")?.textContent?.trim() ?? "");
}

function choice(name: string): HTMLElement {
  const found = choices().find((el) => el.querySelector(".os-choice-card-name")?.textContent?.trim() === name);
  if (found === undefined) throw new Error(`no choice ${name}`);
  return found;
}

function chosen(): string {
  return choices().find((el) => el.getAttribute("aria-checked") === "true")?.querySelector(".os-choice-card-name")?.textContent?.trim() ?? "";
}

describe("Connect pipeline", () => {
  beforeEach(() => {
    h.connection = null;
  });

  it("reads memql-package.yaml on the way in, and shows its stages in Repository before anything is confirmed", async () => {
    let release = () => {};
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    const connection = connectionFor({ pipelinePreview: previewRow() }, { holdPreview: held });
    mount(connection);

    // A RUNNING READ: its words and its measure on the floor, the shape of the
    // stages in the step, and nothing to press but the ways out.
    await waitFor(() => expect(floorWord()).toBe("Reading memql-package.yaml"));
    expect(floor().dataset.tone).toBe("busy");
    expect(floor().querySelector(".os-actbar-meta")?.textContent).toMatch(/^\d+:\d\d$/);
    expect(document.querySelector('.os-content-skeleton[aria-busy="true"]')).not.toBeNull();
    // Loading copy is for a screen reader, never painted.
    expect(screen.queryAllByText(/^Loading/).every((el) => el.classList.contains("os-sr-only"))).toBe(true);
    expect(noAct("Continue")).toBe(true);
    expect(noAct("Connect pipeline")).toBe(true);
    expect(act_("Leave")).toBeTruthy();

    await act(async () => {
      release();
    });

    expect(await screen.findByText("go-tests, 4 shards")).toBeTruthy();
    expect(connection.calls).toContain(`builtin pipelinesPreview(packageId: "${PACKAGE_ID}")`);
    expect(connection.callsNamed("pipelinesConnect")).toEqual([]);
    for (const text of ["build-vet", "db-tests", "verify-rollout", "Runs on pushes only", "Notifies znas-instance", "Runs in the merge queue and on pushes"]) {
      expect(screen.getByText(text)).toBeTruthy();
    }
    expect(screen.getByText("Read at 3f9c2ab, the head of main.")).toBeTruthy();
    expect(railAnswer("Repository")).toBe("acme/shop at main, 4 stages");
    // THE PAGE STAYS ON THE STAGES: the marks have moved on, the person has not.
    expect(railLine("Repository").getAttribute("aria-expanded")).toBe("true");
    expect(floorWord()).toBe("Review the stages");
    expect(noAct("Connect pipeline")).toBe(true);
  });

  it("stops Repository at a refusal with its copy, and offers Read again in its place", async () => {
    const seed: FakeSeed = {
      pipelinePreview: previewRow({
        stages: [],
        needs: [],
        secrets: [],
        refusal: {
          code: "pipeline_not_declared",
          message: "memql-package.yaml at the head of main declares no pipeline block, so there is nothing to connect.",
          scope: "",
        },
      }),
    };
    const connection = connectionFor(seed);
    mount(connection);

    expect(await screen.findByText("This repository declares no pipeline")).toBeTruthy();
    expect(screen.getByText("memql-package.yaml at the head of main declares no pipeline block, so there is nothing to connect.")).toBeTruthy();
    expect(screen.getByText("Add a pipeline: block to its memql-package.yaml.")).toBeTruthy();
    expect(railAnswer("Repository")).toBe("Cannot be connected");
    expect(floorWord()).toBe("Cannot be connected");
    expect(noAct("Continue")).toBe(true);
    expect(noAct("Connect pipeline")).toBe(true);

    // The manifest is fixed and pushed; reading again finds its stages.
    seed.pipelinePreview = previewRow();
    await click(act_("Read again"));
    expect(await screen.findByText("go-tests, 4 shards")).toBeTruthy();
    expect(connection.callsNamed("pipelinesPreview")).toHaveLength(2);
    expect(screen.queryByText("This repository declares no pipeline")).toBeNull();
  });

  it("says a read that failed for no stated reason could not be read, and reads again", async () => {
    const seed: FakeSeed = { pipelinePreview: previewRow(), pipelinePreviewError: "the network went away" };
    mount(connectionFor(seed));
    expect(await screen.findByText("memql-package.yaml could not be read.")).toBeTruthy();
    expect(screen.getByText("the network went away")).toBeTruthy();
    expect(floorWord()).toBe("Not read");
    delete seed.pipelinePreviewError;
    await click(act_("Read again"));
    expect(await screen.findByText("go-tests, 4 shards")).toBeTruthy();
  });

  it("offers your fleet only when a machine of yours allows pipelines", async () => {
    mount(connectionFor({ pipelinePreview: previewRow() }, { machines: [ALLOWED] }));
    await screen.findByText("go-tests, 4 shards");
    await click(act_("Continue"));

    await waitFor(() => expect(choiceNames()).toEqual(["Cluster", "Cluster and your fleet"]));
    // Nothing needs a machine, so the cluster is SUGGESTED -- in words, with
    // no card drawn as chosen until somebody chooses one -- and the fleet's
    // own sentence says what it also takes.
    expect(chosen()).toBe("");
    expect(screen.getByText("No step needs a machine yet, so the cluster is suggested.")).toBeTruthy();
    expect(screen.getByText(/Computer use must be on for your machines\./)).toBeTruthy();
    expect(floorWord()).toBe("Choose where steps run");
    expect(noAct("Connect pipeline")).toBe(true);

    // CHOOSING ANSWERS IT: no Continue, and Confirm opens.
    await click(choice("Cluster and your fleet"));
    expect(railAnswer("Compute")).toBe("Cluster and your fleet");
    expect(fact("Where steps run")).toBe("Cluster and your fleet");
    expect(act_("Connect pipeline")).toBeTruthy();
  });

  it("does not offer the fleet for a machine that has not allowed pipelines, is revoked, or is somebody else's", async () => {
    mount(
      connectionFor(
        { pipelinePreview: previewRow() },
        {
          machines: [
            machineRow({ id: "v1:worker:registration:m-team", labels: { team: "core" } }),
            machineRow({ id: "v1:worker:registration:m-gone", labels: { pipelines: "allowed" }, revokedAt: "2026-10-01T00:00:00Z" }),
            machineRow({ id: "v1:worker:registration:m-theirs", ownerUserId: "v1:identity:user:someone-else", labels: { pipelines: "allowed" } }),
          ],
        },
      ),
    );
    await screen.findByText("go-tests, 4 shards");
    await click(act_("Continue"));

    // NOTHING TO CHOOSE: the cluster is the one answer, so Compute answered
    // itself and the page went on to Confirm.
    await waitFor(() => expect(fact("Where steps run")).toBe("Cluster only"));
    await click(railLine("Compute"));
    expect(choiceNames()).toEqual(["Cluster"]);
    expect(screen.getByText("Your fleet is offered once one of your machines allows pipelines in its policy.yaml.")).toBeTruthy();
  });

  it("stops Compute with the remedy when a step needs a machine and none of yours allows pipelines", async () => {
    const connection = connectionFor({ pipelinePreview: previewRow({ ...NEEDS }) });
    mount(connection);

    expect(await screen.findByText("os-checks needs docker.")).toBeTruthy();
    expect(
      screen.getByText("None of your machines allows pipelines. Allow pipelines in a machine's policy.yaml (pipelines.allow), or remove the need from the step."),
    ).toBeTruthy();
    expect(railAnswer("Compute")).toBe("None of your machines allows pipelines");
    expect(floorWord()).toBe("A step needs one of your machines");
    expect(noAct("Continue")).toBe(true);
    expect(noAct("Connect pipeline")).toBe(true);
    expect(act_("Read again")).toBeTruthy();

    // ALLOWING PIPELINES ON A MACHINE REACHES THE PAGE BY ITSELF, through the
    // machines feed: Compute turns into a question where the person is.
    await emit(connection, WORKER_REGISTRATION_CONCEPT, ALLOWED, "NODE_CREATED");
    await waitFor(() => expect(choiceNames()).toEqual(["Cluster and your fleet"]));
    // Cluster is not offered: the engine refuses a step's need on a
    // cluster-only pipeline, so the sentence says how to keep steps here. The
    // one option left is still not chosen for the person: running steps on
    // their machines is consent, so it waits, unchecked, for their click.
    expect(chosen()).toBe("");
    expect(screen.getByText(/Only your machines offer what this step needs: os-checks needs docker\./)).toBeTruthy();
    expect(floorWord()).toBe("Choose where steps run");
  });

  it("defaults how changes arrive to the cluster's suggestion, and connects with the manifest's secret names", async () => {
    const onDone = vi.fn();
    const flowRef: { current: ConnectFlow | null } = { current: null };
    const connection = connectionFor({ pipelinePreview: previewRow({ suggestedDelivery: "poll" }) });
    mount(connection, { onDone, flowRef });
    await screen.findByText("go-tests, 4 shards");
    await click(act_("Continue"));

    await waitFor(() => expect(selectedLabel(screen.getByLabelText("How changes arrive"))).toBe("Polling"));
    expect(screen.getByText("This cluster asks GitHub every minute. Merge queue and release runs need a webhook.")).toBeTruthy();
    expect(fact("Check on GitHub")).toBe("MemQL / shop");
    expect(fact("Stages")).toBe("checks, tests, deploy, notify");
    expect(fact("Where steps run")).toBe("Cluster only");
    expect(fact("Secrets the steps read")).toBe("VERIFY_TOKEN");
    expect(screen.getByText(/Connecting lets their values reach the steps that name them\./)).toBeTruthy();
    expect(floorWord()).toBe("Ready to connect");
    // Nothing is written until the act.
    expect(connection.callsNamed("pipelinesConnect")).toEqual([]);

    await click(act_("Connect pipeline"));
    await waitFor(() =>
      expect(connection.callsNamed("pipelinesConnect")).toEqual([
        `builtin pipelinesConnect(packageId: "${PACKAGE_ID}", delivery: "poll", compute: "cluster", secretNames: ["VERIFY_TOKEN"])`,
      ]),
    );
    await waitFor(() => expect(floorWord()).toBe("Connected"));
    expect(railAnswer("Confirm")).toBe("Connected");
    expect(screen.queryByLabelText("How changes arrive")).toBeNull();
    expect(fact("How changes arrive")).toBe("Polling");

    await click(act_("Done"));
    expect(onDone).toHaveBeenCalledTimes(1);
    expect(flowRef.current?.packageId).toBe("");
  });

  it("connects with the delivery the person chose over the suggestion", async () => {
    const connection = connectionFor({ pipelinePreview: previewRow({ suggestedDelivery: "webhook" }) });
    mount(connection);
    await screen.findByText("go-tests, 4 shards");
    await click(act_("Continue"));
    await waitFor(() => expect(selectedLabel(screen.getByLabelText("How changes arrive"))).toBe("Webhook"));
    chooseOption(screen.getByLabelText("How changes arrive"), "Polling");
    await click(act_("Connect pipeline"));
    await waitFor(() => expect(connection.callsNamed("pipelinesConnect")[0]).toContain(`delivery: "poll"`));
  });

  it("lands a refusal of the fleet at Compute, with its copy and its scope", async () => {
    const connection = connectionFor({
      pipelinePreview: previewRow(),
      pipelinesConnectError:
        "builtin pipelinesConnect: pipeline_fleet_not_consented (tests/os-checks): The step needs docker, which sends it to a fleet machine, and this pipeline's compute is cluster: the owner has not consented to the fleet (compute: cluster_and_fleet).",
    });
    mount(connection);
    await screen.findByText("go-tests, 4 shards");
    await click(act_("Continue"));
    await click(act_("Connect pipeline"));

    expect(await screen.findByText("A step needs a fleet machine, and this pipeline runs on the cluster only")).toBeTruthy();
    expect(screen.getByText(/the owner has not consented to the fleet/)).toBeTruthy();
    expect(screen.getByText("tests/os-checks")).toBeTruthy();
    expect(railLine("Compute").getAttribute("aria-expanded")).toBe("true");
    expect(floorWord()).toBe("Not connected");
    expect(noAct("Connect pipeline")).toBe(true);
  });

  it("lands a refusal about the grant at Repository, where reading again is the way forward", async () => {
    mount(connectionFor({ pipelinePreview: previewRow(), pipelinesConnectError: "credential_revoked: the GitHub connection this source uses was revoked" }));
    await screen.findByText("go-tests, 4 shards");
    await click(act_("Continue"));
    await click(act_("Connect pipeline"));

    expect(await screen.findByText("This source's credential was revoked")).toBeTruthy();
    expect(railLine("Repository").getAttribute("aria-expanded")).toBe("true");
    expect(act_("Read again")).toBeTruthy();
    expect(noAct("Connect pipeline")).toBe(true);
  });

  it("keeps an unexplained failure at Confirm, and offers the act again", async () => {
    const seed: FakeSeed = { pipelinePreview: previewRow(), pipelinesConnectError: "the network went away" };
    const connection = connectionFor(seed);
    mount(connection);
    await screen.findByText("go-tests, 4 shards");
    await click(act_("Continue"));
    await click(act_("Connect pipeline"));

    expect(await screen.findByText("The pipeline was not connected.")).toBeTruthy();
    expect(screen.getByText("the network went away")).toBeTruthy();
    delete seed.pipelinesConnectError;
    await click(act_("Connect pipeline"));
    await waitFor(() => expect(floorWord()).toBe("Connected"));
    expect(connection.callsNamed("pipelinesConnect")).toHaveLength(2);
  });

  it("keeps every answer when the person leaves, and resumes where they were", async () => {
    const onBack = vi.fn();
    const flowRef: { current: ConnectFlow | null } = { current: null };
    const connection = connectionFor({ pipelinePreview: previewRow() }, { machines: [ALLOWED] });
    const view = mount(connection, { onBack, flowRef });
    await screen.findByText("go-tests, 4 shards");
    await click(act_("Continue"));
    await waitFor(() => expect(choiceNames()).toHaveLength(2));
    await click(choice("Cluster and your fleet"));
    chooseOption(screen.getByLabelText("How changes arrive"), "Polling");

    // GOING BACK IS LEAVING: it goes, and drops nothing.
    await click(screen.getByRole("button", { name: "Back to shop" }));
    expect(onBack).toHaveBeenCalledTimes(1);
    expect(flowRef.current?.packageId).toBe(PACKAGE_ID);

    // The page goes -- another tab, the source page -- and comes back the
    // way the source page brings it back: Connect pipeline starts the flow
    // over the same source again, which resumes it.
    view.rerender(tree({ onBack, flowRef, shown: false }));
    expect(screen.queryByText("go-tests, 4 shards")).toBeNull();
    expect(document.querySelector(".os-actbar")).toBeNull();
    await act(async () => {
      flowRef.current?.start(PACKAGE_ID, null);
    });
    view.rerender(tree({ onBack, flowRef, shown: true }));

    expect(fact("Where steps run")).toBe("Cluster and your fleet");
    expect(selectedLabel(screen.getByLabelText("How changes arrive"))).toBe("Polling");
    // Coming back is not asking again.
    expect(connection.callsNamed("pipelinesPreview")).toHaveLength(1);

    await click(act_("Connect pipeline"));
    await waitFor(() =>
      expect(connection.callsNamed("pipelinesConnect")).toEqual([
        `builtin pipelinesConnect(packageId: "${PACKAGE_ID}", delivery: "poll", compute: "cluster_and_fleet", secretNames: ["VERIFY_TOKEN"])`,
      ]),
    );
  });

  it("cancels by going, and drops the answers: nothing was written", async () => {
    const onBack = vi.fn();
    const flowRef: { current: ConnectFlow | null } = { current: null };
    const connection = connectionFor({ pipelinePreview: previewRow() });
    mount(connection, { onBack, flowRef });
    await screen.findByText("go-tests, 4 shards");
    await click(act_("Cancel"));
    expect(onBack).toHaveBeenCalledTimes(1);
    expect(flowRef.current?.packageId).toBe("");
    expect(connection.callsNamed("pipelinesConnect")).toEqual([]);
  });

  it("changes an active pipeline, prefilled from what it restates", async () => {
    const pipeline = pipelineFromRow(pipelineRow({ compute: "cluster_and_fleet", delivery: "poll" }));
    const connection = connectionFor({ pipelinePreview: previewRow({ suggestedDelivery: "webhook" }) }, { machines: [ALLOWED] });
    mount(connection, { pipeline });

    expect(await screen.findByRole("heading", { name: "Change pipeline" })).toBeTruthy();
    await screen.findByText("go-tests, 4 shards");
    await click(act_("Continue"));

    // Prefilled: Compute is answered, and Confirm opens on the pipeline's own delivery.
    await waitFor(() => expect(fact("Where steps run")).toBe("Cluster and your fleet"));
    expect(selectedLabel(screen.getByLabelText("How changes arrive"))).toBe("Polling");
    expect(floorWord()).toBe("Ready to save");
    await click(act_("Save changes"));
    await waitFor(() =>
      expect(connection.callsNamed("pipelinesConnect")).toEqual([
        `builtin pipelinesConnect(packageId: "${PACKAGE_ID}", delivery: "poll", compute: "cluster_and_fleet", secretNames: ["VERIFY_TOKEN"])`,
      ]),
    );
    await waitFor(() => expect(floorWord()).toBe("Saved"));
  });

  it("names itself once, at the end of the trail it is handed", async () => {
    const onList = vi.fn();
    const onBack = vi.fn();
    mount(connectionFor({ pipelinePreview: previewRow() }), { onBack, trail: [{ label: "Sources", onSelect: onList }, { label: "shop", onSelect: onBack }] });
    await screen.findByText("go-tests, 4 shards");
    const crumbs = within(screen.getByRole("navigation", { name: "Breadcrumbs" })).getAllByRole("listitem").map((li) => li.textContent);
    expect(crumbs).toEqual(["Sources", "shop", "Connect pipeline"]);
    await click(screen.getByRole("button", { name: "Sources" }));
    expect(onList).toHaveBeenCalledTimes(1);
  });

  it("tells a viewer without the connect part so, and offers nothing that would be refused", async () => {
    mount(connectionFor({ pipelinePreview: previewRow() }), { can: partsWithout("connect") });
    expect(await screen.findByText("This needs a part of Deployables you have not been granted")).toBeTruthy();
    expect(noAct("Read again")).toBe(true);
    expect(noAct("Continue")).toBe(true);
    expect(noAct("Connect pipeline")).toBe(true);
    expect(act_("Cancel")).toBeTruthy();
  });
});
