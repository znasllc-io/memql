import { describe, expect, it } from "vitest";

import {
  EMPTY_DRAFT,
  LONG_WAIT_MS,
  STEADY_BEATS,
  barFor,
  checksFor,
  checksSettled,
  draftSummary,
  installSteps,
  isSteady,
  matchRegistration,
  openStopFor,
  phaseOf,
  sameId,
  serviceSentence,
  stopsFor,
  waitedLong,
  type Draft,
  type FlowFacts,
} from "../../src/apps/fleet/addMachine/flow";
import { machineFromRow, type MachineRow } from "../../src/apps/fleet/rows";
import { machineRow } from "./harness";

// THE GUIDED INSTALL'S READING, on fixtures (design record
// 2026-09-08-cockpit-install-wizard, section 6). Every claim here is about a
// function over values; the page's tests assert what a person sees.

const NOW = new Date("2026-09-08T12:00:00Z");
const ago = (seconds: number) => new Date(NOW.getTime() - seconds * 1000).toISOString();

const MAC: Draft = { name: "studio-mac-mini", platform: "mac", computerUse: false, inference: false };
const MAC_CU: Draft = { ...MAC, computerUse: true };
const LINUX_CU: Draft = { ...MAC, platform: "linux", computerUse: true };
const MAC_INF: Draft = { ...MAC, inference: true };

function machine(over: Record<string, unknown> = {}): MachineRow {
  return machineFromRow(
    machineRow({
      id: "v1:worker:registration:m1",
      identityId: "v1:identity:identity:tok-1",
      name: "mini.local",
      platformInfo: { os: "darwin", arch: "arm64", hostname: "mini.local" },
      capabilities: ["HEADLESS"],
      buildTag: "headless",
      version: "v2026.9.1",
      lastSeenAt: ago(3),
      ...over,
    }),
  );
}

function facts(over: Partial<FlowFacts> = {}): FlowFacts {
  return {
    draft: MAC,
    connected: true,
    minting: false,
    mintError: "",
    mint: null,
    mintedAt: null,
    machine: null,
    beats: 0,
    cancelAsked: false,
    revokeError: "",
    revoking: false,
    now: NOW,
    ...over,
  };
}

const MINT = { token: "tok", identityId: "v1:identity:identity:tok-1" };

describe("phases", () => {
  it("is describe with no mint, minting while minting, waiting after a mint, connected once matched", () => {
    expect(phaseOf(facts())).toBe("describe");
    expect(phaseOf(facts({ minting: true }))).toBe("minting");
    expect(phaseOf(facts({ mint: MINT }))).toBe("waiting");
    expect(phaseOf(facts({ mint: MINT, machine: machine() }))).toBe("connected");
  });
});

describe("matching the registration by identity, never by count", () => {
  it("compares across the bare/canonical seam", () => {
    expect(sameId("v1:identity:identity:tok-1", "tok-1")).toBe(true);
    expect(sameId("tok-1", "v1:identity:identity:tok-1")).toBe(true);
    expect(sameId("tok-1", "tok-1")).toBe(true);
    expect(sameId("tok-1", "tok-2")).toBe(false);
  });

  it("never equates two empty ids", () => {
    // An unmatched row carries "" and a flow with no mint carries "";
    // equating them would match every machine in the fleet.
    expect(sameId("", "")).toBe(false);
    expect(sameId("", "tok-1")).toBe(false);
  });

  it("finds the row minted for, bare or canonical, and ignores every other machine", () => {
    const other = machine({ id: "v1:worker:registration:other", identityId: "tok-9" });
    const mine = machine({ identityId: "tok-1" });
    expect(matchRegistration([other, mine], "v1:identity:identity:tok-1")?.id).toBe(mine.id);
    expect(matchRegistration([other], "v1:identity:identity:tok-1")).toBeNull();
    // Three machines arriving is not success; the right one arriving is.
    expect(matchRegistration([other, other, other], "tok-1")).toBeNull();
  });

  it("never matches a revoked row", () => {
    // A token revoked from the cancel question and pasted anyway must not
    // read as success.
    const revoked = machine({ revokedAt: ago(1) });
    expect(matchRegistration([revoked], "tok-1")).toBeNull();
  });

  it("matches nothing for an empty identity", () => {
    expect(matchRegistration([machine()], "")).toBeNull();
  });
});

describe("steadiness", () => {
  it("is two heartbeats past the registration, and online", () => {
    const m = machine();
    expect(isSteady(m, STEADY_BEATS - 1, NOW)).toBe(false);
    expect(isSteady(m, STEADY_BEATS, NOW)).toBe(true);
    expect(isSteady(null, 5, NOW)).toBe(false);
  });

  it("is not steady once the machine has gone silent, however many beats it had", () => {
    expect(isSteady(machine({ lastSeenAt: ago(90) }), 5, NOW)).toBe(false);
  });
});

describe("the checks", () => {
  it("draws the connection current until steady, then done", () => {
    const c0 = checksFor(MAC, machine(), 0, NOW)[0]!;
    expect(c0.state).toBe("current");
    expect(c0.answer).toMatch(/first heartbeat/);
    const c1 = checksFor(MAC, machine(), 1, NOW)[0]!;
    expect(c1.state).toBe("current");
    expect(c1.answer).toMatch(/Heartbeat 1 of 2/);
    const c2 = checksFor(MAC, machine(), 2, NOW)[0]!;
    expect(c2.state).toBe("done");
    expect(c2.answer).toMatch(/steady/);
  });

  it("stops the connection check when the machine goes silent past the window, naming the log", () => {
    const c = checksFor(MAC, machine({ lastSeenAt: ago(120) }), 3, NOW)[0]!;
    expect(c.state).toBe("stopped");
    expect(c.repair).toContain("~/.memql/state/worker.log");
  });

  it("reads the build against what was asked", () => {
    expect(checksFor(MAC, machine(), 0, NOW)[1]).toMatchObject({ state: "done", answer: "Headless build (cockpit v2026.9.1)." });
    expect(checksFor(MAC_CU, machine({ buildTag: "computeruse", capabilities: ["HEADLESS", "COMPUTERUSE"] }), 0, NOW)[1]).toMatchObject({
      state: "done",
      answer: "Computer-use build (cockpit v2026.9.1).",
    });
    // Asked for computer use, got the headless build: the line was run
    // without the flag, and the check says so and how to fix it.
    const wrong = checksFor(MAC_CU, machine(), 0, NOW)[1]!;
    expect(wrong.state).toBe("stopped");
    expect(wrong.repair).toContain("--computeruse");
    // More than asked is a fact, not a failure.
    expect(checksFor(MAC, machine({ buildTag: "computeruse" }), 0, NOW)[1]!.state).toBe("done");
    // A cockpit that did not say is unknown, not wrong.
    expect(checksFor(MAC_CU, machine({ buildTag: "", version: "" }), 0, NOW)[1]).toMatchObject({
      state: "unknown",
      answer: "The cockpit did not say which build it is.",
    });
  });

  it("asks for the macOS permissions only on a computer-use Mac, and names the missing ones", () => {
    expect(checksFor(MAC, machine(), 0, NOW).map((c) => c.id)).toEqual(["connection", "build"]);
    const cu = machine({
      buildTag: "computeruse",
      permissions: { accessibility: true, screen_recording: false, x11_display: false, detail: "" },
    });
    const perms = checksFor(MAC_CU, cu, 0, NOW).find((c) => c.id === "permissions")!;
    expect(perms.state).toBe("open");
    expect(perms.answer).toBe("Screen Recording not granted yet.");
    expect(perms.repair).toContain("System Settings -> Privacy & Security -> Screen Recording");
    expect(perms.command).toBe("/usr/local/bin/memql worker setup");
  });

  it("settles the permissions once both are granted, and is unknown for a cockpit that never reported", () => {
    const granted = machine({
      buildTag: "computeruse",
      permissions: { accessibility: true, screen_recording: true, x11_display: false, detail: "" },
    });
    expect(checksFor(MAC_CU, granted, 0, NOW).find((c) => c.id === "permissions")).toMatchObject({
      state: "done",
      answer: "Accessibility and Screen Recording granted.",
    });
    const silent = machine({ buildTag: "computeruse" });
    expect(checksFor(MAC_CU, silent, 0, NOW).find((c) => c.id === "permissions")?.state).toBe("unknown");
  });

  it("skips the display check on Wayland with the installer's own sentence, never failing it", () => {
    const wayland = machine({
      platformInfo: { os: "linux", arch: "amd64", hostname: "box" },
      buildTag: "computeruse",
      capabilityDescriptor: { platform: "linux", displayServer: "wayland", computerUseAvailable: true },
    });
    const display = checksFor(LINUX_CU, wayland, 0, NOW).find((c) => c.id === "display")!;
    expect(display.state).toBe("skipped");
    expect(display.answer).toMatch(/Wayland/);
    const x11 = machine({
      platformInfo: { os: "linux", arch: "amd64", hostname: "box" },
      buildTag: "computeruse",
      capabilities: ["HEADLESS", "COMPUTERUSE"],
      capabilityDescriptor: { platform: "linux", displayServer: "x11", computerUseAvailable: true },
    });
    expect(checksFor(LINUX_CU, x11, 0, NOW).find((c) => c.id === "display")?.state).toBe("done");
  });

  it("does not mistake a missing display for Wayland or advertise an unregistered desktop", () => {
    const noDisplay = machine({
      platformInfo: { os: "linux" }, capabilities: ["HEADLESS"],
      capabilityDescriptor: { displayServer: "none", computerUseAvailable: true },
      permissions: { x11_display: false },
    });
    const missing = checksFor(LINUX_CU, noDisplay, 0, NOW).find((c) => c.id === "display")!;
    expect(missing.answer).toMatch(/No display/);
    expect(missing.answer).not.toContain("Wayland");
    const unregistered = { ...noDisplay, displayServer: "x11", permissions: { ...noDisplay.permissions, x11Display: true } };
    const check = checksFor(LINUX_CU, unregistered, 0, NOW).find((c) => c.id === "display")!;
    expect(check.state).not.toBe("done");
    expect(check.answer).toMatch(/not registered.*computer use/i);
  });

  it("keeps the display check unknown when the optional descriptor was not reported", () => {
    const unreported = machine({
      platformInfo: { os: "linux" }, buildTag: "computeruse", capabilities: ["HEADLESS", "COMPUTERUSE"],
    });
    const check = checksFor(LINUX_CU, unreported, 0, NOW).find((c) => c.id === "display")!;
    expect(check.state).toBe("unknown");
    expect(check.answer).toMatch(/not reported/i);
    expect(check.answer).not.toContain("Install");
  });

  it("asks for the runtime repair when local models were asked for and none is reported", () => {
    const checks = checksFor(MAC_INF, machine(), 0, NOW);
    const runtime = checks.find((c) => c.id === "runtime")!;
    expect(runtime.state).toBe("open");
    expect(runtime.command).toBe("/usr/local/bin/memql worker setup --inference");
    // The models line is not reachable until there is a runtime.
    expect(checks.find((c) => c.id === "models")?.state).toBe("ahead");
  });

  it("offers the recommended pull once a runtime is there, draws a live pull as moving, and reads a served model as done", () => {
    const withRuntime = machine({ hardware: { chip: "M2", memoryBytes: 1, runtimes: [{ name: "ollama", version: "0.11" }] } });
    const idle = checksFor(MAC_INF, withRuntime, 0, NOW);
    expect(idle.find((c) => c.id === "runtime")).toMatchObject({ state: "done", answer: "Found: ollama." });
    // Nothing on the runtime and nothing in flight: the person's turn, with
    // the OS's own act beside the command.
    expect(idle.find((c) => c.id === "models")).toMatchObject({ state: "open", act: "pullRecommended", command: "/usr/local/bin/memql worker setup --inference" });

    const pulling = checksFor(MAC_INF, withRuntime, 0, NOW, {
      live: { pullId: "p1", workerId: "m1", model: "llama3.1:8b", status: "running", statusLine: "pulling 8eeb52dfb3bb", layer: "", completedBytes: 1, totalBytes: 2, readvertised: false, errorMessage: "", requestedAt: "", updatedAt: "", endedAt: "" },
    });
    expect(pulling.find((c) => c.id === "models")).toMatchObject({ state: "current" });
    expect(pulling.find((c) => c.id === "models")?.answer).toContain("Pulling llama3.1:8b -- pulling 8eeb52dfb3bb");

    const serving = machine({
      hardware: { chip: "M2", memoryBytes: 1, runtimes: [{ name: "ollama", version: "0.11" }] },
      labels: { "model:llama3.1:8b": "ctx=131072,structured=1,max=2" },
    });
    expect(checksFor(MAC_INF, serving, 0, NOW).find((c) => c.id === "models")).toMatchObject({
      state: "done",
      answer: "Serving one model: llama3.1:8b.",
      act: "askIt",
    });
  });

  it("names the cluster's round trip on the connection once it is measured, and never before", () => {
    const unmeasured = checksFor(MAC, machine(), STEADY_BEATS, NOW)[0]!;
    expect(unmeasured.answer).not.toContain("round trip");
    const measured = checksFor(MAC, machine({ rttMs: 12, rttAt: ago(40) }), STEADY_BEATS, NOW)[0]!;
    expect(measured.answer).toContain("round trip 12 ms, checked 40s ago");
  });

  it("treats an advertised model as proof of a runtime an older cockpit did not describe", () => {
    const old = machine({ labels: { "model:llama3.1:8b": "ctx=8192,structured=1" } });
    expect(checksFor(MAC_INF, old, 0, NOW).find((c) => c.id === "runtime")?.state).toBe("done");
  });

  it("settles on done, skipped and unknown, and not on open, current or stopped", () => {
    expect(checksSettled(checksFor(MAC, machine(), STEADY_BEATS, NOW))).toBe(true);
    expect(checksSettled(checksFor(MAC, machine(), 0, NOW))).toBe(false);
    expect(checksSettled([])).toBe(false);
  });
});

describe("the stops", () => {
  it("opens This machine and keeps the rest ahead before a mint", () => {
    const stops = stopsFor(facts(), []);
    expect(stops.map((s) => s.state)).toEqual(["open", "ahead", "ahead", "ahead"]);
    expect(openStopFor(stops)).toBe("machine");
  });

  it("lights two marks while waiting: Install open for the person, Connect current for the cluster", () => {
    const stops = stopsFor(facts({ mint: MINT, mintedAt: NOW }), []);
    expect(stops.map((s) => s.state)).toEqual(["done", "open", "current", "ahead"]);
    expect(stops[0]!.answer).toBe("studio-mac-mini -- macOS");
    expect(stops[1]!.answer).toBe("Run the command on the machine");
    expect(stops[2]!.answer).toBe("Listening for the machine");
    expect(openStopFor(stops)).toBe("install");
  });

  it("settles Install and Connect on the match and moves the checks", () => {
    const m = machine();
    const checks = checksFor(MAC, m, 0, NOW);
    const stops = stopsFor(facts({ mint: MINT, machine: m }), checks);
    expect(stops.map((s) => s.state)).toEqual(["done", "done", "done", "current"]);
    expect(stops[1]!.answer).toBe("Installed on mini.local");
    expect(stops[2]!.answer).toBe("Connected as mini.local -- darwin/arm64 -- cockpit v2026.9.1");
    expect(openStopFor(stops)).toBe("checks");
  });

  it("holds the checks mark open when only a person's hand is missing, and done when all settle", () => {
    const cu = machine({
      buildTag: "computeruse",
      capabilities: ["HEADLESS", "COMPUTERUSE"],
      permissions: { accessibility: false, screen_recording: false, x11_display: false, detail: "" },
    });
    const waitingOnHand = stopsFor(facts({ draft: MAC_CU, mint: MINT, machine: cu, beats: 2 }), checksFor(MAC_CU, cu, 2, NOW));
    expect(waitingOnHand[3]!.state).toBe("open");
    expect(waitingOnHand[3]!.answer).toBe("2 of 3 settled -- macOS permissions next");

    const done = stopsFor(facts({ mint: MINT, machine: machine(), beats: 2 }), checksFor(MAC, machine(), 2, NOW));
    expect(done[3]!.state).toBe("done");
    expect(done[3]!.answer).toBe("All settled");
  });

  it("marks the checks stopped when the machine went silent", () => {
    const silent = machine({ lastSeenAt: ago(200) });
    const stops = stopsFor(facts({ mint: MINT, machine: silent, beats: 1 }), checksFor(MAC, silent, 1, NOW));
    expect(stops[3]!.state).toBe("stopped");
    expect(openStopFor(stops)).toBe("checks");
  });

  it("summarises the draft with what was asked for", () => {
    expect(draftSummary({ name: "box", platform: "linux", computerUse: true, inference: true })).toBe(
      "box -- Linux, computer-use build, local models",
    );
    expect(draftSummary(EMPTY_DRAFT)).toBe("macOS");
  });
});

describe("the action bar follows the state", () => {
  it("offers Mint only over a live connection with a name typed, and Cancel always", () => {
    expect(barFor(facts({ draft: { ...MAC, name: "" } }), []).acts.map((a) => a.id)).toEqual(["cancel"]);
    expect(barFor(facts({ connected: false }), []).acts.map((a) => a.id)).toEqual(["cancel"]);
    expect(barFor(facts({ connected: false }), []).detail).toMatch(/live connection/);
    expect(barFor(facts(), []).acts.map((a) => a.id)).toEqual(["cancel", "mint"]);
  });

  it("says a refused mint created nothing and offers Mint again", () => {
    const bar = barFor(facts({ mintError: "forbidden" }), []);
    expect(bar.state).toBe("The token was not minted");
    expect(bar.acts.map((a) => a.id)).toEqual(["cancel", "mint"]);
  });

  it("offers nothing mid-mint", () => {
    expect(barFor(facts({ minting: true }), []).acts).toEqual([]);
  });

  it("waits with Cancel, and names the long wait after ten minutes", () => {
    const bar = barFor(facts({ mint: MINT, mintedAt: NOW }), []);
    expect(bar.state).toBe("Waiting for studio-mac-mini");
    expect(bar.tone).toBe("busy");
    expect(bar.acts.map((a) => a.id)).toEqual(["cancel"]);
    const long = facts({ mint: MINT, mintedAt: new Date(NOW.getTime() - LONG_WAIT_MS) });
    expect(waitedLong(long)).toBe(true);
    expect(barFor(long, []).detail).toMatch(/taking a while/);
  });

  it("asks which of two things after Cancel on a minted token", () => {
    const bar = barFor(facts({ mint: MINT, cancelAsked: true }), []);
    expect(bar.state).toBe("Leave?");
    expect(bar.question).toMatch(/still works/);
    expect(bar.acts.map((a) => a.id)).toEqual(["keepWaiting", "revokeAndLeave", "leaveKeepToken"]);
    expect(bar.acts.find((a) => a.id === "revokeAndLeave")?.tone).toBe("danger");
    expect(bar.acts.find((a) => a.id === "leaveKeepToken")?.tone).toBe("primary");
  });

  it("keeps the question open with the refusal when the revoke failed", () => {
    const bar = barFor(facts({ mint: MINT, cancelAsked: true, revokeError: "identity not found" }), []);
    expect(bar.question).toContain("identity not found");
    expect(bar.acts.map((a) => a.id)).toContain("leaveKeepToken");
  });

  it("drops Cancel once connected and offers Open and Done, Done primary only when settled", () => {
    const m = machine();
    const pending = barFor(facts({ mint: MINT, machine: m }), checksFor(MAC, m, 0, NOW));
    expect(pending.state).toBe("Connected");
    expect(pending.acts.map((a) => [a.id, a.tone])).toEqual([
      ["open", "quiet"],
      ["done", "quiet"],
    ]);
    expect(pending.acts[0]!.label).toBe("Open mini.local");
    const settled = barFor(facts({ mint: MINT, machine: m, beats: 2 }), checksFor(MAC, m, 2, NOW));
    expect(settled.state).toBe("Ready");
    expect(settled.tone).toBe("live");
    expect(settled.acts.find((a) => a.id === "done")?.tone).toBe("primary");
  });

  it("names a problem in the state word when a check stopped", () => {
    const silent = machine({ lastSeenAt: ago(200) });
    const bar = barFor(facts({ mint: MINT, machine: silent, beats: 1 }), checksFor(MAC, silent, 1, NOW));
    expect(bar.state).toBe("Connected, with a problem");
    expect(bar.tone).toBe("paused");
  });
});

describe("the manual steps", () => {
  it("are in the order they happen, and grow with what was asked for", () => {
    const plain = installSteps(MAC);
    expect(plain[0]).toMatch(/terminal on the machine/);
    expect(plain[1]).toMatch(/password/);
    expect(plain[plain.length - 1]).toContain("LaunchAgent");
    expect(plain).toHaveLength(3);

    const cu = installSteps(MAC_CU);
    expect(cu).toHaveLength(4);
    expect(cu[2]).toMatch(/Accessibility and Screen Recording/);

    const inf = installSteps({ ...LINUX_CU, inference: true });
    expect(inf).toHaveLength(5);
    expect(inf[2]).toMatch(/Wayland/);
    expect(inf[3]).toContain("systemd");
    expect(inf[4]).toMatch(/several gigabytes/);
  });

  it("states the service as a fact of the command, per platform", () => {
    expect(serviceSentence("mac")).toContain("com.znasllc.memql-worker");
    expect(serviceSentence("linux")).toContain("memql-worker.service");
  });
});
