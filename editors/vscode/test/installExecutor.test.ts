// The capability-script runner and the graph executor.
//
// Four assertions carry this file, and each one is a bug that has shipped in
// some installer somewhere:
//
//   1. INDEPENDENT STEPS OVERLAP. The graph says three tool installs are
//      independent; an executor that runs them one after another turns a
//      two-minute install into six. The test below deadlocks if they are
//      serialised, which is the only way to prove concurrency rather than
//      assert it hopefully.
//   2. A FAILURE SKIPS ONLY ITS DEPENDENT SUBTREE. Everything downstream of a
//      broken step is unsafe to run; everything beside it is not, and
//      abandoning it wastes the operator's time and hides a second failure.
//   3. VERIFY FAILURE IS FAILURE, exit 0 or not. A capability script that
//      exits 0 having done nothing is the failure mode the whole verify
//      vocabulary exists for.
//   4. NO SHELL. Each param is its own argv element, so a value containing
//      `;` or `$(...)` is an inert string, not something a shell re-parses.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import { graphDocumentPath, loadGraph, loadGraphFile, type Graph } from "../src/install/graph.js";
import { readReceipt, type Receipt } from "../src/install/receipt.js";
import { installPlan, parseCliArgs, uninstallPlan } from "../src/install/cli.js";
import {
  CAPABILITY_SCRIPTS,
  capabilityScriptPath,
  parseEnvelope,
  runCapabilityScript,
  toArgv,
  type ScriptOutcome,
  type ScriptRun,
} from "../src/install/runner.js";
import { executeGraph, type StepPlan } from "../src/install/executor.js";

const REPO_ROOT = path.resolve(__dirname, "..", "..", "..", "..");

async function tempDir(): Promise<string> {
  return fs.mkdtemp(path.join(os.tmpdir(), "memql-exec-"));
}

/** Writes an executable script and returns its path. */
async function script(dir: string, name: string, body: string): Promise<string> {
  const file = path.join(dir, name);
  await fs.writeFile(file, `#!/usr/bin/env bash\n${body}\n`, { encoding: "utf8", mode: 0o755 });
  return file;
}

function envelope(over: Record<string, unknown> = {}): string {
  return JSON.stringify({ ok: true, capability: "test", changed: true, result: {}, error: null, ...over });
}

// -----------------------------------------------------------------------------
// the runner
// -----------------------------------------------------------------------------

test("toArgv -- one argv element per param, in the script's own spelling", () => {
  assert.deepEqual(toArgv({ tool: "k3d", "pre-existing": "false" }), ["--tool=k3d", "--pre-existing=false"]);
});

test("runCapabilityScript -- no shell: a metacharacter value arrives literally", async () => {
  const dir = await tempDir();
  const s = await script(
    dir,
    "echo.sh",
    `printf '{"ok":true,"capability":"t","changed":false,"result":{"seen":"%s"},"error":null}\\n' "\${1#--value=}"`,
  );
  const nasty = "a; rm -rf /tmp/x $(whoami) \`id\`";
  const out = await runCapabilityScript({ scriptPath: s, params: { value: nasty } });
  assert.equal(out.exitCode, 0);
  assert.equal(out.envelope?.result.seen, nasty);
});

test("runCapabilityScript -- stdout is the envelope, stderr is the log", async () => {
  const dir = await tempDir();
  const s = await script(dir, "logs.sh", `echo "INFO: doing the thing" >&2\necho '${envelope({ result: { x: 1 } })}'`);
  const lines: string[] = [];
  const out = await runCapabilityScript({ scriptPath: s, params: {}, onLog: (l) => lines.push(l) });
  assert.equal(out.envelope?.result.x, 1);
  assert.match(out.stderr, /doing the thing/);
  assert.deepEqual(lines, ["INFO: doing the thing"]);
});

test("runCapabilityScript -- an honest non-zero exit keeps its envelope", async () => {
  const dir = await tempDir();
  const s = await script(
    dir,
    "refuse.sh",
    `echo '{"ok":false,"capability":"install.removeArtifact","changed":false,"result":{},"error":{"code":3,"message":"refusing"}}'\nexit 3`,
  );
  const out = await runCapabilityScript({ scriptPath: s, params: {} });
  assert.equal(out.exitCode, 3);
  assert.equal(out.envelope?.ok, false);
  assert.equal(out.envelope?.error?.code, 3);
});

test("runCapabilityScript -- a script that prints no envelope is a parse error, not a crash", async () => {
  const dir = await tempDir();
  const s = await script(dir, "quiet.sh", `echo "surprise"`);
  const out = await runCapabilityScript({ scriptPath: s, params: {} });
  assert.equal(out.envelope, null);
  assert.ok(out.parseError);
});

test("runCapabilityScript -- a missing script fails instead of throwing", async () => {
  const dir = await tempDir();
  const out = await runCapabilityScript({ scriptPath: path.join(dir, "nope.sh"), params: {} });
  assert.equal(out.envelope, null);
  assert.notEqual(out.exitCode, 0);
  assert.ok(out.parseError);
});

test("parseEnvelope -- takes the envelope even when the script chatted on stdout", () => {
  const parsed = parseEnvelope(`some noise\n${envelope({ result: { a: 1 } })}\n`);
  assert.ok(parsed.ok);
  assert.equal(parsed.envelope.capability, "test");
});

test("parseEnvelope -- rejects a JSON object that is not a result envelope", () => {
  assert.equal(parseEnvelope('{"capability":"t","params":[]}').ok, false);
  assert.equal(parseEnvelope("").ok, false);
});

test("capability ids resolve to the same paths the engine allowlists", async () => {
  // The Go allowlist is the security boundary for the in-engine path; this map
  // is what the CLI and the extension resolve through. Two copies of one table
  // drift, so the drift is asserted rather than hoped away.
  const go = await fs.readFile(
    path.join(REPO_ROOT, "component", "automations", "steps", "capability_script.go"),
    "utf8",
  );
  const start = go.indexOf("capabilityScriptAllowlist = map[string]string{");
  assert.ok(start > 0, "allowlist map not found in capability_script.go");
  const block = go.slice(start, go.indexOf("\n}", start));
  const expected: Record<string, string> = {};
  for (const m of block.matchAll(/"([A-Za-z0-9_.]+)":\s*"(scripts\/[^"]+)"/g)) {
    expected[m[1]!] = m[2]!;
  }
  assert.ok(Object.keys(expected).length > 10);
  assert.deepEqual(CAPABILITY_SCRIPTS, expected);
});

test("capabilityScriptPath resolves under a repo root and refuses an unknown id", () => {
  assert.equal(
    capabilityScriptPath("install.detect", "/repo"),
    path.join("/repo", "scripts", "install", "detect.sh"),
  );
  assert.throws(() => capabilityScriptPath("install.nope", "/repo"), /not an allowlisted capability/i);
});

// -----------------------------------------------------------------------------
// the executor
// -----------------------------------------------------------------------------

/** A graph of read-only steps, so the fixtures stay small. */
function graphOf(steps: Array<Record<string, unknown>>, kind: "install" | "uninstall" = "install"): Graph {
  return loadGraph(
    JSON.stringify({
      name: kind,
      kind,
      description: "d",
      steps: steps.map((s) => ({
        script: "install.detect",
        description: "d",
        readOnly: true,
        elevation: "none",
        verify: { kind: "scriptOk" },
        ...s,
      })),
    }),
    "fixture.json",
  );
}

/** A runner that returns a scripted outcome per step, recording every call. */
function fakeRunner(
  handler: (argv: string[], run: ScriptRun) => Promise<Partial<ScriptOutcome>> | Partial<ScriptOutcome>,
): { run: (run: ScriptRun) => Promise<ScriptOutcome>; calls: ScriptRun[] } {
  const calls: ScriptRun[] = [];
  return {
    calls,
    run: async (run: ScriptRun): Promise<ScriptOutcome> => {
      calls.push(run);
      const partial = await handler(toArgv(run.params), run);
      return {
        argv: toArgv(run.params),
        exitCode: 0,
        signal: null,
        stdout: "",
        stderr: "",
        envelope: { ok: true, capability: "t", changed: true, result: {}, error: null },
        ...partial,
      };
    },
  };
}

const okPlan = (): StepPlan => ({ action: "run", params: {} });

test("independent steps in a wave overlap", async () => {
  // Deliberately a DEADLOCK test: each of the two independent steps waits for
  // the other to have started. A serialising executor never finishes, so a
  // passing run is proof of concurrency rather than a hopeful timing check.
  const g = graphOf([{ id: "a" }, { id: "b" }, { id: "c", dependsOn: ["a", "b"] }]);
  const started = new Map<string, () => void>();
  const arrivals: Record<string, Promise<void>> = {};
  for (const id of ["a", "b"]) {
    arrivals[id] = new Promise<void>((resolve) => started.set(id, resolve));
  }

  const runner = fakeRunner(async (_argv, run) => {
    const id = run.params.id;
    if (id === "a" || id === "b") {
      started.get(id)!();
      await Promise.all([arrivals.a, arrivals.b]);
    }
    return {};
  });

  // The race turns "serialised" from a hang into a failure.
  const report = await Promise.race([
    executeGraph({
      graph: g,
      scriptPath: () => "/bin/true",
      run: runner.run,
      plan: (step) => ({ action: "run", params: { id: step.id } }),
    }),
    new Promise<never>((_, reject) =>
      setTimeout(() => reject(new Error("the wave was serialised: its steps never overlapped")), 3000).unref(),
    ),
  ]);
  assert.equal(report.ok, true);
  assert.deepEqual(report.waves, [["a", "b"], ["c"]]);
});

test("a failed step skips only its dependent subtree", async () => {
  //     a (fails)        d
  //     |                |
  //     b                e
  //     |
  //     c
  // b and c must be skipped; d and e must still finish.
  const g = graphOf([
    { id: "a" },
    { id: "b", dependsOn: ["a"] },
    { id: "c", dependsOn: ["b"] },
    { id: "d" },
    { id: "e", dependsOn: ["d"] },
  ]);
  const runner = fakeRunner((_argv, run) =>
    run.params.id === "a"
      ? { exitCode: 5, envelope: { ok: false, capability: "t", changed: false, result: {}, error: { code: 5, message: "boom" } } }
      : {},
  );

  const report = await executeGraph({
    graph: g,
    scriptPath: () => "/bin/true",
    run: runner.run,
    plan: (step) => ({ action: "run", params: { id: step.id } }),
  });

  const status = Object.fromEntries(report.outcomes.map((o) => [o.id, o.status]));
  assert.deepEqual(status, { a: "failed", b: "skipped", c: "skipped", d: "ok", e: "ok" });
  assert.equal(report.ok, false);
  // The skipped steps were never invoked.
  assert.deepEqual(runner.calls.map((c) => c.params.id).sort(), ["a", "d", "e"]);
  assert.match(report.outcomes.find((o) => o.id === "b")!.reason ?? "", /a/);
});

test("a step whose verify fails is a failure even though the script exited 0", async () => {
  const g = graphOf([
    {
      id: "a",
      readOnly: false,
      verify: { kind: "resultTrue", field: "result.installed" },
    },
    { id: "b", dependsOn: ["a"] },
  ]);
  const runner = fakeRunner(() => ({
    exitCode: 0,
    envelope: { ok: true, capability: "install.binary", changed: false, result: { installed: false }, error: null },
  }));

  const report = await executeGraph({ graph: g, scriptPath: () => "/bin/true", run: runner.run, plan: okPlan });
  const a = report.outcomes.find((o) => o.id === "a")!;
  assert.equal(a.status, "failed");
  assert.equal(a.exitCode, 0);
  assert.equal(a.verified, false);
  assert.match(a.reason ?? "", /verify/i);
  assert.equal(report.outcomes.find((o) => o.id === "b")!.status, "skipped");
});

test("a step that printed no envelope fails, whatever its exit code says", async () => {
  const g = graphOf([{ id: "a" }]);
  const runner = fakeRunner(() => ({ exitCode: 0, envelope: null, parseError: "no envelope on stdout" }));
  const report = await executeGraph({ graph: g, scriptPath: () => "/bin/true", run: runner.run, plan: okPlan });
  assert.equal(report.outcomes[0]?.status, "failed");
  assert.match(report.outcomes[0]?.reason ?? "", /envelope/i);
});

// -----------------------------------------------------------------------------
// params
// -----------------------------------------------------------------------------

test("graph-pinned params and run-time params both reach the argv", async () => {
  const g = graphOf([{ id: "a", params: { tool: "k3d" } }]);
  const runner = fakeRunner(() => ({}));
  await executeGraph({
    graph: g,
    scriptPath: () => "/bin/true",
    run: runner.run,
    plan: () => ({ action: "run", params: { dest: "/opt/bin" } }),
  });
  assert.deepEqual(runner.calls[0]?.params, { dest: "/opt/bin", tool: "k3d" });
});

test("a graph-pinned param wins over a run-time one -- policy is not run input", async () => {
  const g = graphOf([{ id: "a", params: { confirm: "add-memql-hosts" } }]);
  const runner = fakeRunner(() => ({}));
  await executeGraph({
    graph: g,
    scriptPath: () => "/bin/true",
    run: runner.run,
    plan: () => ({ action: "run", params: { confirm: "whatever-i-typed" } }),
  });
  assert.equal(runner.calls[0]?.params.confirm, "add-memql-hosts");
});

test("a plan can skip a step, and its dependents skip with it", async () => {
  const g = graphOf([{ id: "a" }, { id: "b", dependsOn: ["a"] }]);
  const runner = fakeRunner(() => ({}));
  const report = await executeGraph({
    graph: g,
    scriptPath: () => "/bin/true",
    run: runner.run,
    plan: (step) => (step.id === "a" ? { action: "skip", reason: "no receipt entry" } : { action: "run", params: {} }),
  });
  assert.equal(report.outcomes.find((o) => o.id === "a")?.status, "skipped");
  assert.equal(report.outcomes.find((o) => o.id === "b")?.status, "skipped");
  // A skip on plan is not a failure: nothing was left half-done.
  assert.equal(report.ok, true);
  assert.equal(runner.calls.length, 0);
});

// -----------------------------------------------------------------------------
// the receipt
// -----------------------------------------------------------------------------

test("preExisting survives into the receipt", async () => {
  const g = graphOf([
    {
      id: "stackCheckout",
      script: "install.cloneStack",
      readOnly: false,
      receipt: "checkout",
      preExistingPath: "!result.cloned",
      verify: { kind: "resultNonEmpty", field: "result.commit" },
    },
    {
      id: "toolK3d",
      script: "install.binary",
      readOnly: false,
      params: { tool: "k3d" },
      receipt: "binary",
      preExistingPath: "result.preExisting",
      verify: { kind: "resultTrue", field: "result.installed" },
    },
  ]);
  const dir = await tempDir();
  const file = path.join(dir, "install-receipt.json");

  const runner = fakeRunner((_argv, run) =>
    run.capability === "install.cloneStack"
      ? {
          envelope: {
            ok: true,
            capability: "install.cloneStack",
            changed: false,
            // cloned:false -- the checkout was ALREADY there.
            result: { commit: "abc123", dest: "/home/dev/.memql/src", cloned: false },
            error: null,
          },
        }
      : {
          envelope: {
            ok: true,
            capability: "install.binary",
            changed: true,
            result: { installed: true, path: "/home/dev/.memql/bin/k3d", preExisting: false },
            error: null,
          },
        },
  );

  const report = await executeGraph({
    graph: g,
    scriptPath: () => "/bin/true",
    run: runner.run,
    plan: okPlan,
    receiptFile: file,
  });
  assert.equal(report.ok, true);

  const receipt = await readReceipt(file);
  assert.equal(receipt?.entries.length, 2);
  const checkout = receipt!.entries.find((e) => e.stepId === "stackCheckout")!;
  assert.equal(checkout.receipt, "checkout");
  assert.equal(checkout.preExisting, true, "a checkout we did not clone is one uninstall must not delete");
  assert.equal(checkout.result.dest, "/home/dev/.memql/src");
  const binary = receipt!.entries.find((e) => e.stepId === "toolK3d")!;
  assert.equal(binary.preExisting, false);
  assert.deepEqual(binary.params, { tool: "k3d" });
});

test("the receipt is written per step, so a run killed mid-way is still reversible", async () => {
  const g = graphOf([
    {
      id: "a",
      readOnly: false,
      receipt: "binary",
      preExistingPath: "result.preExisting",
      verify: { kind: "resultTrue", field: "result.installed" },
    },
    {
      id: "b",
      dependsOn: ["a"],
      readOnly: false,
      receipt: "binary",
      preExistingPath: "result.preExisting",
      verify: { kind: "resultTrue", field: "result.installed" },
    },
  ]);
  const dir = await tempDir();
  const file = path.join(dir, "r.json");

  let seenAfterA: number | null = null;
  const runner = fakeRunner(() => ({
    envelope: {
      ok: true,
      capability: "install.detect",
      changed: true,
      result: { installed: true, path: "/x", preExisting: false },
      error: null,
    },
  }));

  await executeGraph({
    graph: g,
    scriptPath: () => "/bin/true",
    run: runner.run,
    receiptFile: file,
    plan: okPlan,
    onEvent: async (ev) => {
      if (ev.type === "stepFinished" && ev.outcome.id === "a") {
        seenAfterA = (await readReceipt(file))?.entries.length ?? 0;
      }
    },
  });

  assert.equal(seenAfterA, 1, "a's artifact was not on the receipt before b started");
  assert.equal((await readReceipt(file))?.entries.length, 2);
});

// -----------------------------------------------------------------------------
// uninstall
// -----------------------------------------------------------------------------

test("a refusal on a pre-existing artifact is PRESERVED, not a failure", async () => {
  // remove-artifact.sh exits 3 on --pre-existing=true. That refusal is the
  // system working: the operator's own k3d survives. Treating it as a failure
  // would abandon every removal downstream of it.
  const g = graphOf(
    [
      {
        id: "removeLocalCA",
        script: "install.removeArtifact",
        readOnly: false,
        reverses: "localCA",
        verify: { kind: "resultEquals", field: "result.kind", value: "mkcertCA" },
      },
      {
        id: "removeToolMkcert",
        script: "install.removeArtifact",
        readOnly: false,
        dependsOn: ["removeLocalCA"],
        reverses: "toolMkcert",
        verify: { kind: "resultEquals", field: "result.kind", value: "binary" },
      },
    ],
    "uninstall",
  );

  const runner = fakeRunner((_argv, run) =>
    run.params["pre-existing"] === "true"
      ? {
          exitCode: 3,
          envelope: {
            ok: false,
            capability: "install.removeArtifact",
            changed: false,
            result: {},
            error: { code: 3, message: "refusing to remove mkcertCA" },
          },
        }
      : {
          envelope: {
            ok: true,
            capability: "install.removeArtifact",
            changed: true,
            result: { kind: "binary", removed: true },
            error: null,
          },
        },
  );

  const report = await executeGraph({
    graph: g,
    scriptPath: () => "/bin/true",
    run: runner.run,
    plan: (step) =>
      step.id === "removeLocalCA"
        ? { action: "run", params: { kind: "mkcertCA", "pre-existing": "true" }, preservedOnRefusal: true }
        : { action: "run", params: { kind: "binary", "pre-existing": "false" } },
  });

  assert.equal(report.outcomes.find((o) => o.id === "removeLocalCA")?.status, "preserved");
  // The dependent still ran: preservation is not breakage.
  assert.equal(report.outcomes.find((o) => o.id === "removeToolMkcert")?.status, "ok");
  assert.equal(report.ok, true);
});

test("a refusal NOT explained by pre-existence is still a failure", async () => {
  const g = graphOf(
    [
      {
        id: "removeCheckout",
        script: "install.removeArtifact",
        readOnly: false,
        reverses: "stackCheckout",
        verify: { kind: "resultEquals", field: "result.kind", value: "checkout" },
      },
    ],
    "uninstall",
  );
  const runner = fakeRunner(() => ({
    exitCode: 3,
    envelope: {
      ok: false,
      capability: "install.removeArtifact",
      changed: false,
      result: {},
      error: { code: 3, message: "refusing: that is a home directory" },
    },
  }));
  const report = await executeGraph({
    graph: g,
    scriptPath: () => "/bin/true",
    run: runner.run,
    plan: () => ({ action: "run", params: { kind: "checkout", "pre-existing": "false" } }),
  });
  assert.equal(report.outcomes[0]?.status, "failed");
  assert.equal(report.ok, false);
});

// -----------------------------------------------------------------------------
// end to end against real scripts
// -----------------------------------------------------------------------------

test("the executor drives real capability scripts through the real runner", async () => {
  const dir = await tempDir();
  const good = await script(
    dir,
    "good.sh",
    `echo "INFO: working" >&2\necho '{"ok":true,"capability":"install.detect","changed":false,"result":{"supported":true},"error":null}'`,
  );
  const bad = await script(
    dir,
    "bad.sh",
    `echo '{"ok":true,"capability":"install.detect","changed":false,"result":{"supported":false},"error":null}'`,
  );
  const g = graphOf([
    { id: "a", verify: { kind: "resultTrue", field: "result.supported" } },
    { id: "b", verify: { kind: "resultTrue", field: "result.supported" } },
  ]);
  const report = await executeGraph({
    graph: g,
    scriptPath: (step) => (step.id === "b" ? bad : good),
    plan: okPlan,
  });
  assert.equal(report.outcomes.find((o) => o.id === "a")?.status, "ok");
  assert.equal(report.outcomes.find((o) => o.id === "b")?.status, "failed");
});

// -----------------------------------------------------------------------------
// a skip that satisfies its dependents (install(17), #3374)
// -----------------------------------------------------------------------------

test("a satisfied skip does not block its dependents", async () => {
  // An uninstall's removeCluster has nothing to remove when no cluster was
  // ever created. The state it would have established -- no cluster -- already
  // holds, so removeCheckout, which waits for it, is safe to run. Without this
  // an install that stopped before the cluster leaves an uninstall that does
  // nothing at all.
  const g = graphOf([{ id: "a" }, { id: "b", dependsOn: ["a"] }]);
  const runner = fakeRunner(() => ({}));
  const report = await executeGraph({
    graph: g,
    scriptPath: () => "/bin/true",
    run: runner.run,
    plan: (step) =>
      step.id === "a" ? { action: "skip", reason: "nothing to remove", satisfied: true } : { action: "run", params: {} },
  });
  assert.equal(report.outcomes.find((o) => o.id === "a")?.status, "skipped");
  assert.equal(report.outcomes.find((o) => o.id === "b")?.status, "ok");
  assert.equal(report.ok, true);
});

// -----------------------------------------------------------------------------
// the CLI harness
// -----------------------------------------------------------------------------

test("parseCliArgs -- a command is required and unknown flags are refused", () => {
  assert.throws(() => parseCliArgs([]), /install\|uninstall/);
  assert.throws(() => parseCliArgs(["reinstall"]), /install\|uninstall/);
  assert.throws(() => parseCliArgs(["install", "--wat=1"]), /unknown flag/i);
  assert.throws(() => parseCliArgs(["install", "positional"]), /positional/i);
  assert.equal(parseCliArgs(["uninstall"]).command, "uninstall");
});

test("parseCliArgs -- --param is scoped to a step and its flag", () => {
  const opts = parseCliArgs(["install", "--param=stackCheckout.repo=/src", "--param=hostsBlock.hosts-file=/tmp/h"]);
  assert.deepEqual(opts.stepParams, {
    stackCheckout: { repo: "/src" },
    hostsBlock: { "hosts-file": "/tmp/h" },
  });
  assert.throws(() => parseCliArgs(["install", "--param=nodot"]), /step/i);
});

test("the install plan supplies exactly what the graph does not pin", async () => {
  const g = await loadGraphFile(graphDocumentPath("install", REPO_ROOT));
  const opts = parseCliArgs([
    "install",
    "--tag=v1.4.0",
    "--provider-key-file=/run/secrets/key",
    "--domain=local.znas.io",
    "--owner-email=dev@example.com",
    "--owner-first-name=Dev",
    "--owner-last-name=Eloper",
    "--registration-mode=invite_only",
  ]);
  const plan = installPlan(opts);
  const paramsFor = (id: string): Record<string, string> => {
    const p = plan(g.steps.find((s) => s.id === id)!);
    assert.equal(p.action, "run");
    return p.action === "run" ? p.params : {};
  };

  assert.equal(paramsFor("stackCheckout").tag, "v1.4.0");
  assert.equal(paramsFor("providerKey")["key-file"], "/run/secrets/key");
  assert.deepEqual(paramsFor("seedBootstrap"), {
    domain: "local.znas.io",
    "owner-email": "dev@example.com",
    "owner-first-name": "Dev",
    "owner-last-name": "Eloper",
    "registration-mode": "invite_only",
    provider: "anthropic",
    "provider-key-file": "/run/secrets/key",
  });
  // Nothing else is invented for a step the graph already pins.
  assert.deepEqual(paramsFor("detect"), {});
});

test("the AI provider key is a FILE PATH and never a value on argv", async () => {
  // argv is world-readable in `ps`, so the scripts declare --key-file and
  // --provider-key-file and there is deliberately no flag that takes the key
  // itself. A CLI that accepted one would put the operator's Anthropic key in
  // every process listing on the machine for the length of the install.
  assert.throws(() => parseCliArgs(["install", "--provider-key=sk-secret"]), /unknown flag/i);

  const g = await loadGraphFile(graphDocumentPath("install", REPO_ROOT));
  const plan = installPlan(parseCliArgs(["install", "--tag=v1", "--provider-key-file=/run/secrets/key"]));
  for (const step of g.steps) {
    const p = plan(step);
    if (p.action !== "run") continue;
    for (const [flag, value] of Object.entries(p.params)) {
      assert.doesNotMatch(value, /^sk-/, `${step.id} --${flag} carries a key value`);
    }
  }
});

test("--skip removes a step, and the graph carries the removal downstream", async () => {
  const g = await loadGraphFile(graphDocumentPath("install", REPO_ROOT));
  const opts = parseCliArgs(["install", "--tag=v1", "--skip=providerKey"]);
  const plan = installPlan(opts);
  assert.equal(plan(g.steps.find((s) => s.id === "providerKey")!).action, "skip");
  assert.equal(plan(g.steps.find((s) => s.id === "detect")!).action, "run");
});

test("the uninstall plan reads its target and its verdict off the receipt", async () => {
  const g = await loadGraphFile(graphDocumentPath("uninstall", REPO_ROOT));
  const receipt: Receipt = {
    version: 1,
    graph: "install",
    startedAt: "t",
    updatedAt: "t",
    entries: [
      {
        stepId: "toolK3d",
        script: "install.binary",
        receipt: "binary",
        preExisting: false,
        params: { tool: "k3d" },
        result: { path: "/home/dev/.memql/bin/k3d" },
        changed: true,
        recordedAt: "t",
      },
      {
        stepId: "localCA",
        script: "install.mkcert",
        receipt: "mkcertCA",
        preExisting: true,
        params: {},
        result: { caroot: "/home/dev/.local/share/mkcert" },
        changed: false,
        recordedAt: "t",
      },
    ],
  };
  const plan = uninstallPlan(receipt);

  const k3d = plan(g.steps.find((s) => s.id === "removeToolK3d")!);
  assert.equal(k3d.action, "run");
  assert.equal(k3d.action === "run" ? k3d.params.path : "", "/home/dev/.memql/bin/k3d");
  assert.equal(k3d.action === "run" ? k3d.params["pre-existing"] : "", "false");

  // The CA was already on the machine: the flag says so, and the refusal that
  // follows is the expected outcome rather than a failure.
  const ca = plan(g.steps.find((s) => s.id === "removeLocalCA")!);
  assert.equal(ca.action === "run" ? ca.params["pre-existing"] : "", "true");
  assert.equal(ca.action === "run" ? ca.preservedOnRefusal : false, true);

  // Nothing was ever installed for these, so there is nothing to take back --
  // and the removals that depend on them must still run.
  const cluster = plan(g.steps.find((s) => s.id === "removeCluster")!);
  assert.equal(cluster.action, "skip");
  assert.equal(cluster.action === "skip" ? cluster.satisfied : false, true);
});
