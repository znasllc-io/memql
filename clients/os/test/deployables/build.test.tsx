import { existsSync, readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { copyFor, knownCodes } from "../../src/apps/deployables/packages/refusals";
import {
  BUILD_SURFACES,
  buildSurfaceLabel,
  deploymentFingerprint,
  deploymentFromRow,
  packageFingerprint,
  packageFromRow,
} from "../../src/apps/deployables/packages/rows";
import { railFor } from "../../src/apps/deployables/page/rail";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { click, fakeConnection, siteRow, withSession, type FakeConnection } from "./harness";

// build.test.tsx is the OS half of the Build epic (memql#4900, task
// memql#4905): what the surface SAYS once builds are real, what it calls, and
// the two fingerprint rules a heartbeat and a switch make load-bearing.

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

const ACME: Row = {
  id: "pkg-acme",
  ownerUserId: "u-me",
  name: "acme",
  sourceKind: "repo",
  repoUrl: "https://github.com/acme/storefront",
  repoRef: "main",
  repoTokenRef: "",
  artifactId: "",
  deployedVersion: "aaaaaaaaaaaaaaaaaaaa",
  latestKnownVersion: "aaaaaaaaaaaaaaaaaaaa",
  updateAvailable: false,
  autoDeploy: false,
  status: "active",
  createdAt: "2026-09-01T10:00:00Z",
} as unknown as Row;

function deploymentRow(over: Record<string, unknown> = {}): Row {
  return {
    id: "dep-1",
    packageId: "pkg-acme",
    sourceVersion: "bbbbbbbbbbbbbbbbbbbb",
    status: "succeeded",
    report: { deployables: [], dslDomains: [], problems: [], ok: true },
    dslVersion: "",
    deployables: [],
    snapshotArtifactId: "blob://packages/snapshots/x.tar.gz",
    buildLogTail: "",
    builtOn: { surface: "workbench", nodeId: "workbench-2" },
    error: null,
    requestedBy: "u-me",
    automatic: false,
    nodeId: "bff-1",
    stoppedAt: "",
    startedAt: "2026-09-01T12:00:00Z",
    finishedAt: "2026-09-01T12:00:30Z",
    heartbeatAt: "2026-09-01T12:00:25Z",
    createdAt: "2026-09-01T12:00:00Z",
    ...over,
  } as unknown as Row;
}

// The deployable this source produces. Since epic memql#4885 the page is the
// DEPLOYABLE rather than the package, so everything this epic added to the
// surface is read through the site that carries the source.
const STOREFRONT = siteRow({
  id: "site-acme",
  hostname: "acme.memql.example.com",
  kind: "spa",
  status: "live",
  bundleRef: "blob://sites/site-acme/v2/",
  packageId: "pkg-acme",
  packageDeployableName: "storefront",
  createdAt: "2026-09-01T12:01:00Z",
});

function mount(connection: FakeConnection | null) {
  h.connection = connection;
  return render(
    withSession(
      <DeployablesApp sectionId="deployables" navigate={() => {}} askContext={() => {}} store={memStore()} />,
      { role: "owner", userId: "u-me" },
    ),
  );
}

/** Opens the deployable whose source is ACME, and returns its page. */
async function openPackage(connection: FakeConnection) {
  mount(connection);
  await waitFor(() =>
    expect(document.querySelector("[data-os-livelist]")?.getAttribute("data-state")).toBe("live"),
  );
  await click((await screen.findByText("acme.memql.example.com")).closest("button"));
  return screen.findByRole("region", { name: "Deployable acme.memql.example.com" });
}

beforeEach(() => {
  h.connection = null;
});

// ---------------------------------------------------------------------------
// The fingerprints, both directions
// ---------------------------------------------------------------------------

describe("what a person calls a change, now that a deploy has a heartbeat", () => {
  const base = deploymentFromRow(deploymentRow());

  it("stays silent on the heartbeat", () => {
    // A running deploy writes heartbeatAt every fifteen seconds and every one
    // of those broadcasts the row. Naming it in the fingerprint would strobe
    // the timeline hardest for the run somebody is already watching.
    expect(deploymentFingerprint({ ...base, heartbeatAt: "2026-09-01T12:30:00Z" })).toBe(deploymentFingerprint(base));
  });

  it("still fires on the stage the run reached", () => {
    // The reachable positive: without it, the assertion above would pass
    // against a fingerprint that had stopped saying anything at all.
    expect(deploymentFingerprint({ ...base, status: "building" })).not.toBe(deploymentFingerprint(base));
    expect(deploymentFingerprint({ ...base, status: "abandoned" })).not.toBe(deploymentFingerprint(base));
  });

  it("fires when somebody arms auto-deploy", () => {
    // The one field on a source that another person can flip, and the
    // consequence is that pushes start deploying themselves.
    const pkg = packageFromRow(ACME);
    expect(packageFingerprint({ ...pkg, autoDeploy: true })).not.toBe(packageFingerprint(pkg));
  });
});

// ---------------------------------------------------------------------------
// The readings
// ---------------------------------------------------------------------------

describe("what the Build reading says", () => {
  it("names each surface in the words the design fixes", () => {
    expect(buildSurfaceLabel(deploymentFromRow(deploymentRow({ builtOn: { surface: "prebuilt" } })))).toContain(
      "in the source",
    );
    expect(buildSurfaceLabel(deploymentFromRow(deploymentRow({ builtOn: { surface: "workbench" } })))).toContain(
      "sandbox",
    );
    expect(buildSurfaceLabel(deploymentFromRow(deploymentRow({ builtOn: { surface: "fleet" } })))).toContain(
      "your own machine",
    );
  });

  it("says nothing for a run that never reached the build stage", () => {
    // An empty row of labels would be four words claiming a fact this cluster
    // does not have.
    expect(buildSurfaceLabel(deploymentFromRow(deploymentRow({ builtOn: null })))).toBe("");
  });

  it("covers every surface the engine declares", () => {
    // A surface with no label would render as nothing, which reads as "this
    // run did not build" -- the opposite of what it means.
    for (const surface of BUILD_SURFACES) {
      expect(buildSurfaceLabel(deploymentFromRow(deploymentRow({ builtOn: { surface } })))).not.toBe("");
    }
  });
});

describe("an abandoned run", () => {
  it("stops the rail where it stopped rather than reading as finished", () => {
    const row = deploymentFromRow(deploymentRow({ status: "abandoned", buildLogTail: "npm ..." }));
    const rail = railFor({ mode: "deploy", deployment: row });
    const build = rail.stages.find((s) => s.id === "building");
    const publish = rail.stages.find((s) => s.id === "publishing");
    expect(build?.state).toBe("stopped");
    // NOTHING WAS PUBLISHED, and the rail has to show that: it is the whole
    // reason an abandoned run is not an emergency.
    expect(publish?.state).toBe("ahead");
  });

  it("draws the stage it actually reached, not the one its leftovers suggest", () => {
    // A run that died mid-build has no report, no staged version and no log
    // tail, so the evidence rule alone would draw it as having stopped at
    // Analyze. The sweep keeps the stage before it overwrites the status, and
    // this is what that field is for.
    const row = deploymentFromRow(deploymentRow({ status: "abandoned", stoppedAt: "building", report: null }));
    const build = railFor({ mode: "deploy", deployment: row }).stages.find((s) => s.id === "building");
    expect(build?.state).toBe("stopped");
    const analyze = railFor({ mode: "deploy", deployment: row }).stages.find((s) => s.id === "analyzing");
    expect(analyze?.state).toBe("done");
  });

  it("falls back to the evidence when the stage was not kept", () => {
    // The reachable positive: every run closed before this field existed has
    // an empty one, and the rail must still draw those rather than throwing
    // or drawing nothing.
    const row = deploymentFromRow(deploymentRow({ status: "abandoned", stoppedAt: "" }));
    expect(railFor({ mode: "deploy", deployment: row }).stages.some((s) => s.state === "stopped")).toBe(true);
  });

  it("has copy that says nothing failed", () => {
    const copy = copyFor("deployment_abandoned");
    expect(copy?.title).toContain("lost the node");
    expect(copy?.next).toContain("nothing failed");
    expect(copy?.next).toContain("Retry");
  });
});

// ---------------------------------------------------------------------------
// The calls
// ---------------------------------------------------------------------------

describe("the seams the surface calls", () => {
  it("Retry sends the lost run's id, so the retry deploys what it was deploying", async () => {
    const connection = fakeConnection({
      sites: [STOREFRONT],
      packages: [ACME],
      deployments: { "pkg-acme": [deploymentRow({ id: "dep-lost", status: "abandoned" })] },
    });
    const page = await openPackage(connection);

    await click(within(page).getByRole("button", { name: /Retry/ }));

    const deploys = connection.callsNamed("packageDeploy");
    expect(deploys.length, `no packageDeploy reached the wire: ${connection.calls.join(" | ")}`).toBeGreaterThan(0);
    const retry = deploys[deploys.length - 1]!;
    expect(retry).toContain("fromDeploymentId");
    expect(retry).toContain("dep-lost");
    // NOT confirmed: a retry parks with its report exactly as a first deploy
    // does, because the plan may have changed under it.
    expect(retry).toContain("confirm: false");
  });

  it("the switch sends the package and the value, and nothing else", async () => {
    const connection = fakeConnection({ sites: [STOREFRONT], packages: [ACME], deployments: { "pkg-acme": [] } });
    const page = await openPackage(connection);

    await click(within(page).getByRole("checkbox", { name: /Deploy the update by itself/ }));

    const calls = connection.callsNamed("packageSetAutoDeploy");
    expect(calls.length, `no switch call reached the wire: ${connection.calls.join(" | ")}`).toBe(1);
    expect(calls[0]).toContain("pkg-acme");
    expect(calls[0]).toContain("autoDeploy: true");
  });

  it("says what the switch does before somebody uses it", async () => {
    const connection = fakeConnection({ sites: [STOREFRONT], packages: [ACME], deployments: { "pkg-acme": [] } });
    const page = await openPackage(connection);
    // The promise is the whole value of the control: without it somebody
    // either will not arm it or will arm it believing something untrue.
    expect(within(page).getByText(/deploys itself/)).toBeTruthy();
  });

  it("marks a run nobody clicked", async () => {
    const connection = fakeConnection({
      sites: [STOREFRONT],
      packages: [ACME],
      deployments: { "pkg-acme": [deploymentRow({ id: "dep-auto", automatic: true })] },
    });
    const page = await openPackage(connection);
    expect(within(page).getByText("automatic")).toBeTruthy();
  });

  it("does not mark a run somebody did click", async () => {
    const connection = fakeConnection({
      sites: [STOREFRONT],
      packages: [ACME],
      deployments: { "pkg-acme": [deploymentRow({ id: "dep-manual" })] },
    });
    const page = await openPackage(connection);
    expect(within(page).queryByText("automatic")).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// Refusal copy coverage
// ---------------------------------------------------------------------------

const packagesDir = join(dirname(fileURLToPath(import.meta.url)), "../../../../component/packages");

/** Every code literal the engine can emit: the catalogue's constants, plus the
 *  ones spelled inline at a raise site. */
function engineCodes(): string[] {
  const out = new Set<string>();
  for (const name of readdirSync(packagesDir)) {
    if (!name.endsWith(".go") || name.endsWith("_test.go")) continue;
    const source = readFileSync(join(packagesDir, name), "utf8");
    for (const m of source.matchAll(/^\s*Code\w+\s*=\s*"([a-z_]+)"/gm)) out.add(m[1]!);
    for (const m of source.matchAll(/\b(?:refuse|refuseScoped)\(\s*"([a-z_]+)"/g)) out.add(m[1]!);
    for (const m of source.matchAll(/\bCode:\s*"([a-z_]+)"/g)) out.add(m[1]!);
  }
  return [...out].sort();
}

describe("refusal copy coverage", () => {
  it("reads the engine from the tree, and fails rather than skips when it is gone", () => {
    // A skip here would make the gate silently vacuous the moment the
    // catalogue moved -- which is exactly when the two sides are most likely
    // to have diverged.
    expect(existsSync(join(packagesDir, "refusal.go"))).toBe(true);
    expect(engineCodes().length).toBeGreaterThan(10);
  });

  it("has copy for every code the engine emits", () => {
    const missing = engineCodes().filter((code) => !knownCodes().includes(code));
    expect(
      missing.join(", "),
      "each of these needs an entry in refusals.ts -- otherwise it renders under a neutral heading with the server's sentence alone",
    ).toBe("");
  });

  it("does not claim copy for a code nobody emits", () => {
    // Without this the assertion above would be satisfied by a copyFor that
    // answered something for everything.
    expect(copyFor("made_up_code_nobody_emits")).toBeNull();
  });

  it("carries this epic's new codes by name", () => {
    // Pinned individually because these are the ones a person meets when a
    // build goes wrong, and each sends them somewhere different.
    expect(copyFor("deployable_build_timeout")?.next).toContain("MEMQL_PACKAGES_BUILD_TIMEOUT_SECONDS");
    expect(copyFor("no_workbench_peer")?.next).toContain("cluster problem");
    expect(copyFor("deployment_abandoned")?.next).toContain("nothing failed");
    expect(copyFor("snapshot_unavailable")?.title).toContain("nothing stored");
  });
});
