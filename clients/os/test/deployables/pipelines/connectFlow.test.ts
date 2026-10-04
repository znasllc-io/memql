import { describe, expect, it } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { previewFromRow, type PipelinePreview } from "../../../src/apps/deployables/pipelines/calls";
import {
  PART_NOT_HELD,
  barFor,
  computeOf,
  deliveryCaption,
  fleetAvailable,
  fleetReason,
  movementOf,
  needersOf,
  needsRemedy,
  onWords,
  openStepFor,
  readFromWords,
  readingOf,
  repositoryAnswer,
  stepPhrase,
  stepsFor,
  stopForProblem,
  waitWords,
  type ConnectFacts,
  type FlowBar,
} from "../../../src/apps/deployables/pipelines/connect/flow";
import { machineFromRow } from "../../../src/apps/fleet/rows";
import { SHA_A } from "./fixtures";

// THE CONNECT RAIL'S READING, on values (issue memql#5502). Every claim here is
// about a function in connect/flow.ts; connectPage.test.tsx asserts what a
// person sees and what reaches the wire.

const NOW = Date.parse("2026-10-04T12:00:00Z");

function stepRow(name: string, over: Record<string, unknown> = {}): Row {
  return { name, packages: "", only: "", shards: 0, bucket: "", needs: [], secrets: [], services: [], ...over } as Row;
}

/** A preview as pipelinesPreview answers it, read through the app's own reader. */
function preview(over: Record<string, unknown> = {}): PipelinePreview {
  return previewFromRow({
    repository: "acme/shop",
    defaultBranch: "main",
    sha: SHA_A,
    name: "shop",
    checkName: "MemQL / shop",
    stages: [
      { name: "checks", on: [], channel: "", steps: [stepRow("build-vet")] },
      { name: "tests", on: [], channel: "", steps: [stepRow("go-tests", { shards: 4 }), stepRow("os-checks", { needs: ["docker"] })] },
      { name: "deploy", on: ["push"], channel: "", steps: [stepRow("verify-rollout", { secrets: ["VERIFY_TOKEN"] })] },
      { name: "notify", on: ["merge_group", "push"], channel: "znas-instance", steps: [] },
    ],
    needs: ["docker"],
    secrets: ["VERIFY_TOKEN"],
    suggestedDelivery: "webhook",
    existing: null,
    refusal: null,
    ...over,
  } as Row);
}

/** The same manifest with no step that needs a machine. */
function plain(over: Record<string, unknown> = {}): PipelinePreview {
  return preview({
    stages: [
      { name: "checks", on: [], channel: "", steps: [stepRow("build-vet")] },
      { name: "tests", on: [], channel: "", steps: [stepRow("go-tests", { shards: 4 }), stepRow("db-tests")] },
    ],
    needs: [],
    ...over,
  });
}

function facts(over: Partial<ConnectFacts> = {}): ConnectFacts {
  return {
    mode: "connect",
    can: true,
    held: true,
    online: true,
    repository: "acme/shop",
    read: { state: "read", preview: plain() },
    existing: null,
    fleet: { known: true, available: false },
    answers: { compute: null, delivery: null },
    outcome: { state: "idle" },
    reviewed: false,
    now: NOW,
    ...over,
  };
}

function states(f: ConnectFacts): Record<string, string> {
  return Object.fromEntries(stepsFor(f).map((s) => [s.id, s.state]));
}

function answers(f: ConnectFacts): Record<string, string> {
  return Object.fromEntries(stepsFor(f).map((s) => [s.id, s.answer]));
}

function actLabels(bar: FlowBar): string[] {
  return bar.acts.map((a) => (a.text ? `${a.label} (text)` : a.label));
}

const open = (f: ConnectFacts) => openStepFor(stepsFor(f), f);

describe("the stops", () => {
  it("reads first: Repository is the cluster's while it reads, and nothing after it is reachable", () => {
    const f = facts({ read: { state: "reading", startedAt: NOW - 7_000 } });
    expect(readingOf(f).phase).toBe("reading");
    expect(states(f)).toEqual({ repository: "current", compute: "ahead", confirm: "ahead" });
    expect(open(f)).toBe("repository");
    // An unanswered read with a connection is a read about to start.
    expect(readingOf(facts({ read: { state: "idle" } })).phase).toBe("reading");
  });

  it("answers Repository with the repository, its branch and its stage count", () => {
    const f = facts();
    expect(states(f).repository).toBe("done");
    expect(answers(f).repository).toBe("acme/shop at main, 2 stages");
    expect(repositoryAnswer(preview({ stages: [{ name: "only", on: [], channel: "", steps: [] }] }))).toBe("acme/shop at main, 1 stage");
  });

  it("keeps the stages on the page until the person moves past them", () => {
    // The read lands, the marks move at once, and the page stays on what was read.
    const before = facts();
    expect(states(before).confirm).toBe("open");
    expect(open(before)).toBe("repository");
    const after = facts({ reviewed: true });
    expect(open(after)).toBe("confirm");
  });

  it("stops Repository at a refusal, and dims every stop after it", () => {
    const refusal = { code: "pipeline_not_declared", message: "no pipeline block", scope: "" };
    const f = facts({ read: { state: "read", preview: plain({ refusal }) } });
    const r = readingOf(f);
    expect(r.phase).toBe("refused");
    expect(r.problem).toEqual({ at: "repository", problem: refusal });
    expect(states(f)).toEqual({ repository: "stopped", compute: "ahead", confirm: "ahead" });
    expect(answers(f).repository).toBe("Cannot be connected");
    expect(open(facts({ ...f, reviewed: true }))).toBe("repository");
  });

  it("reads a failed read with a code as a refusal, and one without as a fault", () => {
    const coded = facts({ read: { state: "failed", problem: { code: "capability_not_held", message: "not granted", scope: "" } } });
    expect(readingOf(coded).phase).toBe("refused");
    const fault = facts({ read: { state: "failed", problem: { code: "", message: "the network went away", scope: "" } } });
    expect(readingOf(fault).phase).toBe("failed");
    expect(states(fault).repository).toBe("stopped");
    expect(answers(fault).repository).toBe("Could not be read");
  });

  it("says a viewer without the connect part cannot connect, whatever was read", () => {
    const f = facts({ can: false });
    const r = readingOf(f);
    expect(r.phase).toBe("refused");
    expect(r.problem?.problem).toEqual(PART_NOT_HELD);
    // Not even a read is offered over a flow held elsewhere.
    expect(readingOf(facts({ can: false, held: false })).phase).toBe("refused");
    expect(actLabels(barFor(facts({ can: false, held: false })))).toEqual(["Cancel (text)"]);
  });

  it("stops Compute when steps need a machine and none of yours allows pipelines", () => {
    const f = facts({ read: { state: "read", preview: preview() } });
    const r = readingOf(f);
    expect(r.phase).toBe("needsFleet");
    expect(r.compute?.options).toEqual([]);
    expect(states(f)).toEqual({ repository: "done", compute: "stopped", confirm: "ahead" });
    expect(answers(f).compute).toBe("None of your machines allows pipelines");
    // The stop that stopped is the question, before the stages are reviewed.
    expect(open(f)).toBe("compute");
  });

  it("waits on the person at Compute when the fleet is offered", () => {
    const f = facts({ read: { state: "read", preview: preview() }, fleet: { known: true, available: true }, reviewed: true });
    expect(readingOf(f).phase).toBe("compute");
    expect(states(f)).toEqual({ repository: "done", compute: "open", confirm: "ahead" });
    expect(open(f)).toBe("compute");
  });

  it("waits on the person at Confirm once every answer is given", () => {
    const f = facts({ answers: { compute: "cluster", delivery: null }, reviewed: true });
    expect(readingOf(f).phase).toBe("confirm");
    expect(states(f)).toEqual({ repository: "done", compute: "done", confirm: "open" });
    expect(answers(f)).toMatchObject({ compute: "Cluster only", confirm: "Webhook" });
  });

  it("locks Compute while connecting, and closes every stop once connected", () => {
    const connecting = facts({ reviewed: true, outcome: { state: "connecting", startedAt: NOW - 2_000 } });
    expect(states(connecting)).toEqual({ repository: "done", compute: "done", confirm: "current" });
    expect(stepsFor(connecting).find((s) => s.id === "compute")?.openable).toBe(false);
    expect(open(connecting)).toBe("confirm");
    const connected = facts({ reviewed: true, outcome: { state: "connected", pipelineId: "pl-shop", reconnected: false } });
    expect(states(connected)).toEqual({ repository: "done", compute: "done", confirm: "done" });
    expect(answers(connected).confirm).toBe("Connected");
    expect(open(connected)).toBe("confirm");
    expect(answers(facts({ mode: "change", outcome: { state: "connected", pipelineId: "pl-shop", reconnected: true } })).confirm).toBe("Saved");
  });

  it("lands a connect refusal at the stop it is about", () => {
    const at = (code: string) => {
      const f = facts({ reviewed: true, outcome: { state: "refused", problem: { code, message: "no", scope: "" } } });
      return { at: readingOf(f).problem?.at, states: states(f), open: open(f) };
    };
    expect(at("pipeline_fleet_not_consented")).toEqual({ at: "compute", states: { repository: "done", compute: "stopped", confirm: "ahead" }, open: "compute" });
    expect(at("pipeline_secret_not_allowed")).toEqual({ at: "confirm", states: { repository: "done", compute: "done", confirm: "stopped" }, open: "confirm" });
    expect(at("credential_revoked")).toEqual({ at: "repository", states: { repository: "stopped", compute: "ahead", confirm: "ahead" }, open: "repository" });
    // A failure that names no code stays where the act was taken.
    expect(at("").at).toBe("confirm");
  });

  it("draws a page over another source's flow as one that has read nothing", () => {
    const f = facts({ held: false });
    expect(readingOf(f).phase).toBe("unheld");
    expect(states(f)).toEqual({ repository: "open", compute: "ahead", confirm: "ahead" });
  });

  it("says nothing is known before a read it cannot make", () => {
    const f = facts({ online: false, read: { state: "idle" } });
    expect(readingOf(f).phase).toBe("offline");
    expect(states(f).repository).toBe("unknown");
  });
});

describe("Compute's answers", () => {
  it("offers only what can work, and preselects without answering", () => {
    // Needs and a fleet: the fleet alone, checked, waiting for the person.
    expect(computeOf(facts({ fleet: { known: true, available: true } }), preview())).toMatchObject({
      options: ["cluster_and_fleet"], value: "cluster_and_fleet", answered: false, needers: ["os-checks needs docker"],
    });
    // No needs and a fleet: both, the cluster checked, still the person's.
    expect(computeOf(facts({ fleet: { known: true, available: true } }), plain())).toMatchObject({
      options: ["cluster", "cluster_and_fleet"], value: "cluster", answered: false,
    });
    // No needs and no fleet: the one possibility answers itself.
    expect(computeOf(facts(), plain())).toMatchObject({ options: ["cluster"], value: "cluster", answered: true });
    // Needs and no fleet: nothing can work.
    expect(computeOf(facts(), preview())).toMatchObject({ options: [], answered: false });
  });

  it("does not know what to offer until the machines feed has answered", () => {
    expect(computeOf(facts({ fleet: { known: false, available: false } }), preview()).options).toBeNull();
    // A cluster answer with nothing that needs a machine stands without it.
    expect(computeOf(facts({ fleet: { known: false, available: false }, answers: { compute: "cluster", delivery: null } }), plain()).answered).toBe(true);
  });

  it("prefills a reconnect, the preview's reading of the pipeline before the feed's, and drops what is no longer offered", () => {
    const existingFleet = { status: "active" as const, compute: "cluster_and_fleet" as const, delivery: "poll" as const };
    const withFleet = facts({ existing: existingFleet, fleet: { known: true, available: true } });
    expect(computeOf(withFleet, plain())).toMatchObject({ value: "cluster_and_fleet", answered: true });
    // The server's reading wins over the row the feed had.
    const fromServer = plain({ existing: { pipelineId: "pl-shop", status: "active", delivery: "webhook", compute: "cluster", secretNames: [] } });
    expect(computeOf(withFleet, fromServer)).toMatchObject({ value: "cluster", answered: true });
    // A fleet answer with no machine allowing pipelines is not kept: chosen again.
    expect(computeOf(facts({ existing: existingFleet, fleet: { known: true, available: false } }), preview())).toMatchObject({ options: [], answered: false });
    // ...and not quietly replaced by the one answer left, either.
    expect(computeOf(facts({ existing: existingFleet, fleet: { known: true, available: false } }), plain())).toMatchObject({
      options: ["cluster"], value: "cluster", answered: false,
    });
    expect(readingOf(facts({ existing: existingFleet, reviewed: true })).phase).toBe("compute");
  });
});

describe("the floor", () => {
  it("has no forward act while reading, and measures the wait", () => {
    const bar = barFor(facts({ read: { state: "reading", startedAt: NOW - 7_000 } }));
    expect(bar).toMatchObject({ word: "Reading memql-package.yaml", tone: "busy", meta: "0:07" });
    expect(bar.detail).toContain("acme/shop");
    expect(actLabels(bar)).toEqual(["Cancel (text)", "Leave"]);
  });

  it("offers Read again and no forward act at a refusal or a fault", () => {
    const refused = barFor(facts({ read: { state: "read", preview: plain({ refusal: { code: "pipeline_not_declared", message: "no", scope: "" } }) } }));
    expect(refused.word).toBe("Cannot be connected");
    expect(actLabels(refused)).toEqual(["Cancel (text)", "Read again (text)"]);
    const fault = barFor(facts({ read: { state: "failed", problem: { code: "", message: "gone", scope: "" } } }));
    expect(actLabels(fault)).toEqual(["Cancel (text)", "Read again (text)"]);
    // Reading again cannot help a viewer the part's gate refuses.
    expect(actLabels(barFor(facts({ can: false })))).toEqual(["Cancel (text)"]);
  });

  it("offers Read again when steps need a machine none of yours allows", () => {
    const bar = barFor(facts({ read: { state: "read", preview: preview() } }));
    expect(bar.word).toBe("A step needs one of your machines");
    expect(actLabels(bar)).toEqual(["Cancel (text)", "Read again (text)"]);
  });

  it("asks the person to review the stages, then to choose, then offers Connect only on Confirm with every answer", () => {
    const fleet = { known: true, available: true };
    const review = barFor(facts({ fleet }));
    expect(review.word).toBe("Review the stages");
    expect(actLabels(review)).toEqual(["Cancel (text)", "Continue"]);

    const choose = barFor(facts({ fleet, reviewed: true }));
    expect(choose.word).toBe("Choose where steps run");
    expect(actLabels(choose)).toEqual(["Cancel (text)"]);

    const ready = barFor(facts({ fleet, reviewed: true, answers: { compute: "cluster_and_fleet", delivery: null } }));
    expect(ready).toMatchObject({ word: "Ready to connect", detail: "checks report on GitHub as MemQL / shop" });
    expect(actLabels(ready)).toEqual(["Cancel (text)", "Connect pipeline"]);
    expect(ready.acts.at(-1)?.tone).toBe("primary");
  });

  it("names the forward act by what it does to a pipeline that is already there", () => {
    const bar = barFor(facts({ mode: "change", reviewed: true }));
    expect(bar.word).toBe("Ready to save");
    expect(actLabels(bar)).toEqual(["Cancel (text)", "Save changes"]);
  });

  it("holds the act busy while connecting, with no way to cancel a write already sent", () => {
    const bar = barFor(facts({ reviewed: true, outcome: { state: "connecting", startedAt: NOW - 3_000 } }));
    expect(bar).toMatchObject({ word: "Connecting the pipeline", tone: "busy", meta: "0:03" });
    expect(bar.acts).toEqual([{ id: "connect", label: "Connect pipeline", tone: "primary", busy: true }]);
  });

  it("finishes with Done", () => {
    const bar = barFor(facts({ reviewed: true, outcome: { state: "connected", pipelineId: "pl-shop", reconnected: false } }));
    expect(bar).toMatchObject({ word: "Connected", tone: "live" });
    expect(actLabels(bar)).toEqual(["Done"]);
  });

  it("says where a refused connect stopped, and offers that stop's way forward", () => {
    const refused = (code: string) => barFor(facts({ reviewed: true, outcome: { state: "refused", problem: { code, message: "no", scope: "" } } }));
    expect(refused("credential_revoked")).toMatchObject({ word: "Not connected", detail: "the Repository step says why" });
    expect(actLabels(refused("credential_revoked"))).toEqual(["Cancel (text)", "Read again (text)"]);
    expect(actLabels(refused("pipeline_fleet_not_consented"))).toEqual(["Cancel (text)"]);
    expect(actLabels(refused(""))).toEqual(["Cancel (text)", "Connect pipeline"]);
  });

  it("offers nothing that needs the cluster while there is no connection to it", () => {
    expect(actLabels(barFor(facts({ online: false, read: { state: "idle" } })))).toEqual(["Cancel (text)"]);
    const ready = barFor(facts({ online: false, reviewed: true }));
    expect(ready.word).toBe("Offline");
    expect(actLabels(ready)).toEqual(["Cancel (text)"]);
  });

  it("starts this source's flow only when the person asks, on a page whose source is not the flow's", () => {
    expect(actLabels(barFor(facts({ held: false })))).toEqual(["Cancel (text)", "Read memql-package.yaml"]);
  });
});

describe("a person's own choice of stop", () => {
  it("holds until the flow moves", () => {
    const before = facts();
    const reviewed = facts({ reviewed: true });
    const answered = facts({ reviewed: true, answers: { compute: "cluster", delivery: null } });
    expect(movementOf(readingOf(before), before)).not.toBe(movementOf(readingOf(reviewed), reviewed));
    const choosing = facts({ fleet: { known: true, available: true }, reviewed: true });
    const chosen = facts({ fleet: { known: true, available: true }, reviewed: true, answers: { compute: "cluster", delivery: null } });
    expect(movementOf(readingOf(choosing), choosing)).not.toBe(movementOf(readingOf(chosen), chosen));
    // A delivery changed on Confirm is not the flow moving.
    const delivery = facts({ reviewed: true, answers: { compute: "cluster", delivery: "poll" } });
    expect(movementOf(readingOf(answered), answered)).toBe(movementOf(readingOf(delivery), delivery));
  });
});

describe("the fleet", () => {
  function machine(over: Record<string, unknown> = {}) {
    return machineFromRow({
      id: "v1:worker:registration:m1",
      ownerUserId: "v1:identity:user:u-me",
      name: "studio-mac",
      labels: { pipelines: "allowed" },
      operatorLabels: {},
      lastSeenAt: "2026-10-04T11:59:50Z",
      ...over,
    } as Row);
  }

  it("is offered by a machine of the owner's that reports pipelines=allowed", () => {
    expect(fleetAvailable([machine()], "u-me")).toBe(true);
    // Either spelling of the owner is the owner.
    expect(fleetAvailable([machine({ ownerUserId: "u-me" })], "v1:identity:user:u-me")).toBe(true);
  });

  it("is not offered by a revoked machine, another label, an operator's label or somebody else's machine", () => {
    expect(fleetAvailable([machine({ revokedAt: "2026-10-01T00:00:00Z" })], "u-me")).toBe(false);
    expect(fleetAvailable([machine({ labels: { pipelines: "denied" } })], "u-me")).toBe(false);
    expect(fleetAvailable([machine({ labels: { team: "core" } })], "u-me")).toBe(false);
    expect(fleetAvailable([machine({ labels: {}, operatorLabels: { pipelines: "allowed" } })], "u-me")).toBe(false);
    expect(fleetAvailable([machine({ ownerUserId: "v1:identity:user:someone-else" })], "u-me")).toBe(false);
    expect(fleetAvailable([], "u-me")).toBe(false);
    // An unresolved owner owns nothing.
    expect(fleetAvailable([machine()], "")).toBe(false);
  });
});

describe("words", () => {
  it("says when a stage runs, each event with its own preposition", () => {
    expect(onWords([])).toBe("");
    expect(onWords(["push"])).toBe("Runs on pushes only");
    expect(onWords(["merge_group", "push"])).toBe("Runs in the merge queue and on pushes");
    expect(onWords(["pull_request", "push", "release"])).toBe("Runs on pull requests, pushes and releases");
    expect(onWords(["affected"])).toBe("Runs in affected runs only");
    expect(onWords(["full", "merge_group"])).toBe("Runs in full runs and the merge queue");
    // A word this build does not know reads as itself, never as a code.
    expect(onWords(["schedule_nightly"])).toBe("Runs on schedule nightly only");
  });

  it("says a step's shards and needs beside its name", () => {
    expect(stepPhrase({ name: "build-vet", shards: 0, needs: [] })).toBe("build-vet");
    expect(stepPhrase({ name: "go-tests", shards: 4, needs: [] })).toBe("go-tests, 4 shards");
    expect(stepPhrase({ name: "go-tests", shards: 1, needs: [] })).toBe("go-tests");
    expect(stepPhrase({ name: "os-checks", shards: 0, needs: ["docker"] })).toBe("os-checks, needs docker");
    expect(stepPhrase({ name: "ui", shards: 2, needs: ["display", "gpu"] })).toBe("ui, 2 shards, needs display and gpu");
  });

  it("names every step that needs a machine, and the way out", () => {
    expect(needersOf(preview())).toEqual(["os-checks needs docker"]);
    expect(needsRemedy(["os-checks needs docker"])).toBe(
      "None of your machines allows pipelines. Allow pipelines in a machine's policy.yaml (pipelines.allow), or remove the need from the step.",
    );
    expect(fleetReason(["os-checks needs docker"])).toContain("remove the need from the step");
  });

  it("says what each delivery does, and what polling cannot see", () => {
    expect(deliveryCaption("webhook", "webhook")).toBe("GitHub sends each push and pull request.");
    expect(deliveryCaption("webhook", "poll")).toContain("polling is suggested");
    expect(deliveryCaption("poll", "poll")).toBe("This cluster asks GitHub every minute. Merge queue and release runs need a webhook.");
  });

  it("names the commit that was read", () => {
    expect(readFromWords(preview())).toBe("Read at 3f9c2ab, the head of main.");
    expect(readFromWords(preview({ sha: "" }))).toBe("");
  });

  it("measures a wait in minutes and seconds", () => {
    expect(waitWords(0)).toBe("0:00");
    expect(waitWords(-500)).toBe("0:00");
    expect(waitWords(65_400)).toBe("1:05");
  });

  it("files a refusal under its stop", () => {
    expect(stopForProblem({ code: "pipeline_fleet_disabled", message: "", scope: "" })).toBe("compute");
    expect(stopForProblem({ code: "pipeline_secret_invalid", message: "", scope: "secretNames" })).toBe("confirm");
    expect(stopForProblem({ code: "pipeline_already_connected", message: "", scope: "" })).toBe("repository");
    expect(stopForProblem({ code: "package_manifest_invalid", message: "", scope: "" })).toBe("repository");
  });
});
