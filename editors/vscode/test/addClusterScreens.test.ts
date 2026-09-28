// The Add a cluster screens (src/webview/addClusterScreens.ts), each held to
// the page anatomy and the voice of the design record
// (docs/internal/design/2026-09-28-vscode-extension-ux.md):
//
//   - one heading per page; at most three acts on the bar, at most one button;
//   - an act that is not legal is ABSENT, never disabled;
//   - on/off choices are switches, never bare checkboxes;
//   - no inline `style` (the CSP drops it), no "--" as punctuation, no word a
//     reader cannot act on (receipt, graph, capability, envelope, portal ...).
//
// The screens are pure, so every one of them is rendered here from its view
// model -- the same view models the gallery photographs.

import test from "node:test";
import assert from "node:assert/strict";

import { removalRows, sharedToolRows, type RowStep } from "../src/install/removalPreview.js";
import { landingView, type Inputs } from "../src/state/addCluster.js";
import {
  addedScreen,
  collectScreen,
  connectScreen,
  doneScreen,
  landingScreen,
  runScreen,
  uninstallPreviewScreen,
  uninstalledScreen,
  type RunInput,
  type RunPhase,
} from "../src/webview/addClusterScreens.js";
import type { RegionParts } from "../src/webview/ui/liveView.js";

const ADA: Inputs = { domain: "memql.localhost", ownerFirstName: "Ada", ownerLastName: "Lovelace", ownerEmail: "ada@example.com", version: "v0.21.3" };

function whole(parts: RegionParts): string {
  return parts.head + parts.body + parts.actions;
}

function barActs(parts: RegionParts): string[] {
  return [...parts.actions.matchAll(/data-act="([^"]+)"/g)].map((m) => m[1]!);
}

function run(phase: RunPhase, over: Partial<RunInput> = {}): RegionParts {
  return runScreen({
    mode: "install",
    phase,
    percent: 40,
    status: "Creating the cluster",
    stepText: "Step 10 of 16",
    startedAt: 1,
    failures: [],
    retryable: true,
    logsOpen: false,
    now: 1000,
    ...over,
  });
}

const PLAN = {
  removals: [
    rowStep({ id: "removeCluster", params: { kind: "stack", cluster: "memql" } }),
    rowStep({ id: "removeHostsBlock", elevation: "sudo", params: { kind: "hostsEntries", path: "/etc/hosts" } }),
    rowStep({ id: "removeLocalCA", shared: true, dependsOn: ["removeCluster"], params: { kind: "mkcertCA" } }),
    rowStep({ id: "removeToolMkcert", shared: true, dependsOn: ["removeLocalCA"], params: { kind: "binary", path: "/x/mkcert" } }),
  ],
  preserved: [] as RowStep[],
};

/** What completeLocalUninstall says when the list entry could not be dropped. */
const LIST_PROBLEM =
  "the cluster is off this machine, but \"memql\" could not be removed from the cluster list: EACCES: permission denied, open '/Users/ada/.memql/clusters.yaml'";

function rowStep(over: Partial<RowStep> & { id: string }): RowStep {
  return { description: "", action: "run", reason: "", preserved: false, target: "", elevation: "none", shared: false, sharedReason: "", ...over };
}

/** Every screen, in the states that matter. */
function everyScreen(): { name: string; parts: RegionParts }[] {
  const out: { name: string; parts: RegionParts }[] = [];
  out.push({ name: "landing loading", parts: landingScreen({}) });
  for (const verdict of ["absent", "installed-healthy", "installed-unreachable", "present-unreceipted"] as const) {
    out.push({
      name: `landing ${verdict}`,
      parts: landingScreen({
        view: landingView({ verdict, registered: verdict === "installed-healthy", hasReceipt: true, platform: "supported", signedIn: false }),
      }),
    });
  }
  out.push({
    name: "collect install",
    parts: collectScreen({ action: "install", values: ADA, errors: [], versionChoices: ["v0.21.3", "main"], moreOpen: true, checks: [] }),
  });
  out.push({ name: "collect repair", parts: collectScreen({ action: "repair", values: ADA, errors: [], versionChoices: [], moreOpen: false }) });
  out.push({
    name: "connect",
    parts: connectScreen({
      values: { name: "s", domain: "example.com", endpoint: "", token: "" },
      errors: [],
      failure: "",
      probe: { state: "failed", endpoint: "api.example.com:443", reason: "no answer" },
      derivation: "Connects to api.example.com:443",
      composedEndpoint: "api.example.com:443",
      moreOpen: false,
    }),
  });
  out.push({ name: "added", parts: addedScreen({ name: "s", address: "api.example.com:443", osUrl: "https://os.example.com/", hasToken: false, reachable: false }) });
  for (const phase of ["running", "stopping", "finishing", "failed", "stopped", "settling"] as const) {
    out.push({
      name: `run ${phase}`,
      parts: run(phase, phase === "failed" || phase === "finishing" ? { failures: [{ id: "clusterUp", line: "Port 443 is in use.", next: "Retry.", remedy: "k3d cluster stop memql" }] } : {}),
    });
  }
  out.push({
    name: "done",
    parts: doneScreen({
      kind: "installed",
      name: "memql",
      address: "api.memql.localhost:443",
      osUrl: "https://os.memql.localhost/",
      signedIn: false,
      canEnrol: true,
      claim: true,
      recoveryKey: { state: "claimed", value: "k", revealed: false, copied: false },
      startedAt: 1,
      endedAt: 2,
      now: 3,
    }),
  });
  out.push({
    name: "done not listed",
    parts: doneScreen({ kind: "installed", name: "", address: "a", osUrl: "", signedIn: false, canEnrol: false, claim: false, notListed: true }),
  });
  out.push({
    name: "uninstall preview",
    parts: uninstallPreviewScreen({
      loading: false,
      rows: removalRows(PLAN),
      sharedTools: sharedToolRows(PLAN),
      chosen: new Set(),
      deleteData: { on: true, phrase: "delete memql" },
      clusterName: "memql",
    }),
  });
  out.push({ name: "uninstall loading", parts: uninstallPreviewScreen({ loading: true, rows: [], sharedTools: [], chosen: new Set(), clusterName: "memql" }) });
  out.push({
    name: "uninstall nothing",
    parts: uninstallPreviewScreen({ loading: false, nothingHere: { removeFromList: true }, rows: [], sharedTools: [], chosen: new Set(), clusterName: "memql" }),
  });
  out.push({
    name: "uninstall unreadable",
    parts: uninstallPreviewScreen({ loading: false, unreadable: true, rows: [], sharedTools: [], chosen: new Set(), clusterName: "memql" }),
  });
  out.push({ name: "uninstalled", parts: uninstalledScreen({ removed: 3, kept: 1, followUpProblem: "x", logsOpen: false }) });
  out.push({
    name: "uninstalled, still listed",
    parts: uninstalledScreen({ removed: 3, kept: 0, followUpProblem: LIST_PROBLEM, stillListed: true, logsOpen: false }),
  });
  out.push({
    name: "done, key lost",
    parts: doneScreen({
      kind: "installed", name: "m", address: "a", osUrl: "o", signedIn: false, canEnrol: false, claim: false,
      recoveryKey: { state: "revealLost", value: "", revealed: false, copied: false },
    }),
  });
  return out;
}

test("every screen follows the page anatomy", () => {
  for (const { name, parts } of everyScreen()) {
    const html = whole(parts);
    assert.equal(html.split("<h1").length - 1, 1, `${name}: exactly one heading`);
    assert.ok(barActs(parts).length <= 3, `${name}: more than three acts on the bar`);
    assert.ok(parts.actions.split('data-tone="primary"').length + parts.actions.split('data-tone="danger" data-act').length - 2 <= 1, `${name}: two buttons`);
    assert.doesNotMatch(html, /\sstyle="/, `${name}: an inline style the CSP would drop`);
    // A disabled SWITCH is allowed -- a choice unavailable until another is
    // made, with a line saying which. A disabled ACT is not.
    for (const tag of html.match(/<(?:button|input)\b[^>]*>/g) ?? []) {
      if (/\sdisabled(?=[\s>])/.test(tag)) assert.match(tag, /role="switch"/, `${name}: a disabled act -- an illegal act is absent`);
    }
    assert.doesNotMatch(html, /type="checkbox"(?![^>]*role="switch")/, `${name}: an on/off choice that is not a switch`);
  }
});

test("no screen says what a reader cannot act on", () => {
  const banned =
    /memql#|\breceipt\b|\bgraph\b|capability|envelope|stderr|exit code|SecretStorage|clusters\.yaml|refresh_token|client_id|\bportal\b|\bconsole\b|\bwave\b|installer did not|guided/i;
  for (const { name, parts } of everyScreen()) {
    // The text a person reads: tags and attribute values out.
    const text = whole(parts).replace(/<[^>]+>/g, " ");
    assert.doesNotMatch(text, banned, `${name}: ${text.match(banned)?.[0]}`);
    assert.doesNotMatch(text, /\s--\s/, `${name}: "--" used as punctuation`);
  }
});

test("the run's bar offers exactly what its phase allows", () => {
  assert.deepEqual(barActs(run("running")), ["cancel"]);
  assert.deepEqual(barActs(run("stopping")), [], "nothing to press while it stops");
  assert.deepEqual(barActs(run("finishing")), [], "no Retry while other steps finish");
  assert.deepEqual(barActs(run("settling")), []);
  assert.deepEqual(barActs(run("failed")), ["leave", "retry"]);
  assert.deepEqual(barActs(run("failed", { retryable: false })), ["leave"], "Retry is absent when it cannot help");
  assert.deepEqual(barActs(run("stopped")), ["leave", "resume"]);
  assert.match(run("stopping").actions, /Stopping after the current step/);
  assert.match(run("finishing").actions, /Finishing other steps/);
});

test("the run screen is the one progress screen, titled in the act's own words", () => {
  assert.match(run("running").body, /data-part="title">Installing MemQL</);
  assert.match(run("running", { mode: "repair" }).body, /Repairing MemQL/);
  assert.match(run("running", { mode: "uninstall" }).body, /Uninstalling MemQL/);
  // No step checklist under the bar: the bar, the status and the log say it.
  assert.doesNotMatch(run("running").body, /install-steps|<h2[^>]*>Steps/);
  assert.match(run("running").body, /Step 10 of 16/);
});

test("a failure's notice carries the fix as Run in terminal, keyed by step id, never by command", () => {
  const failed = run("failed", { failures: [{ id: "hostsBlock", line: "Needs your password.", next: "Run the command in a terminal, then retry.", remedy: "sudo x" }] });
  assert.match(failed.body, /data-act="remedy" data-value="hostsBlock"[^>]*>Run in terminal</);
  assert.doesNotMatch(failed.body, /data-value="sudo x"/);
});

test("the done screen has one next act, and its facts claim no reachability", () => {
  const done = doneScreen({
    kind: "added",
    name: "memql",
    address: "api.memql.localhost:443",
    osUrl: "https://os.memql.localhost/",
    signedIn: false,
    canEnrol: false,
    claim: false,
  });
  assert.deepEqual(barActs(done), ["back", "signIn"]);
  assert.doesNotMatch(whole(done), /answers at|responding|reachable|running/);
  const signedIn = doneScreen({ kind: "installed", name: "m", address: "a", osUrl: "o", signedIn: true, canEnrol: true, claim: false });
  assert.deepEqual(barActs(signedIn), ["back", "openOs"], "signed in: MemQL OS, and no passkey prompt");
  // Never four acts: an owner to enrol and a claim link do not both apply,
  // and if a caller says they do, the passkey wins and the bar stays legal.
  const both = doneScreen({ kind: "installed", name: "m", address: "a", osUrl: "o", signedIn: false, canEnrol: true, claim: true });
  assert.deepEqual(barActs(both), ["back", "enrolPasskey", "signIn"]);
});

test("the recovery key is masked until Show, and copyable without being shown", () => {
  const key = "mql_rec_SECRET";
  const hidden = doneScreen({
    kind: "installed", name: "m", address: "a", osUrl: "o", signedIn: false, canEnrol: false, claim: false,
    recoveryKey: { state: "claimed", value: key, revealed: false, copied: false },
  });
  assert.ok(!whole(hidden).includes(key));
  assert.match(hidden.body, /data-act="revealRecoveryKey"/);
  assert.match(hidden.body, /data-act="copyRecoveryKey"[^>]*>Copy</);
  const shown = doneScreen({
    kind: "installed", name: "m", address: "a", osUrl: "o", signedIn: false, canEnrol: false, claim: false,
    recoveryKey: { state: "claimed", value: key, revealed: true, copied: true },
  });
  assert.ok(shown.body.includes(key));
  assert.match(shown.body, />Copied</);
  assert.doesNotMatch(shown.body, /revealRecoveryKey/);
});

test("the uninstall bar: Uninstall when it would remove something, the danger act only on the phrase", () => {
  const screen = (chosen: string[], deleteData?: { on: boolean; phrase: string }, plan = PLAN) =>
    barActs(
      uninstallPreviewScreen({
        loading: false,
        rows: removalRows(plan),
        sharedTools: sharedToolRows(plan),
        chosen: new Set(chosen),
        ...(deleteData === undefined ? {} : { deleteData }),
        clusterName: "memql",
      }),
    );
  assert.deepEqual(screen([]), ["uninstallBack", "uninstallStart"]);
  assert.deepEqual(screen([], { on: true, phrase: "" }), ["uninstallBack"], "the switch is on and not yet confirmed");
  assert.deepEqual(screen([], { on: true, phrase: "Delete memql data" }), ["uninstallBack"], "exact, not case-folded");
  assert.deepEqual(screen([], { on: true, phrase: "delete memql data" }), ["uninstallBack", "uninstallStart"]);
  const nothing = { removals: [] as RowStep[], preserved: [rowStep({ id: "removeCluster", preserved: true, params: { kind: "stack", cluster: "memql" } })] };
  assert.deepEqual(screen([], { on: false, phrase: "" }, nothing), ["uninstallBack"], "absent, not disabled, when it would do nothing");
});

test("the phrase field is there only while the switch is on", () => {
  const html = (on: boolean) =>
    uninstallPreviewScreen({ loading: false, rows: [], sharedTools: [], chosen: new Set(), deleteData: { on, phrase: "" }, clusterName: "memql" }).body;
  assert.doesNotMatch(html(false), /data-field="deletePhrase"/);
  assert.match(html(true), /data-field="deletePhrase"/);
  assert.match(html(true), /data-tone="danger"/);
});

test("a lost recovery key names no screen that is not there", () => {
  // MemQL OS has no recovery-key page, and the portal is retired: the old
  // "Rotate it in MemQL OS, under Users" sent a person looking for nothing.
  const lost = doneScreen({
    kind: "installed", name: "m", address: "a", osUrl: "o", signedIn: false, canEnrol: false, claim: false,
    recoveryKey: { state: "revealLost", value: "", revealed: false, copied: false },
  }).body;
  assert.match(lost, /Nobody holds this cluster&#39;s recovery key|Nobody holds this cluster's recovery key/);
  const notice = lost.slice(lost.indexOf("mq-notice"));
  assert.match(notice, /An owner can replace it later\./);
  assert.doesNotMatch(notice, /MemQL OS|under Users|portal/);
});

test("an uninstall that could not be worked out is a failure with a way forward, never an empty computer", () => {
  const parts = uninstallPreviewScreen({ loading: false, unreadable: true, rows: [], sharedTools: [], chosen: new Set(), clusterName: "memql" });
  assert.match(parts.body, /Couldn&#39;t work out what would be removed|Couldn't work out what would be removed/);
  assert.doesNotMatch(whole(parts), /No local cluster was found|removeFromList|uninstallStart/);
  assert.deepEqual(barActs(parts), ["uninstallBack", "openOutput", "uninstallReload"]);
});

test("a finished uninstall that left a record behind says which, and keeps the errno off the page", () => {
  const listed = uninstalledScreen({ removed: 4, kept: 0, followUpProblem: LIST_PROBLEM, stillListed: true, logsOpen: false }).body;
  assert.match(listed, /still in your clusters/);
  assert.match(listed, /Remove it from the list in the Clusters view\./);
  assert.doesNotMatch(listed, /EACCES|clusters\.yaml|\/Users\//, "detail belongs in the output, not the page");
  const recorded = uninstalledScreen({
    removed: 4, kept: 0, followUpProblem: "the cluster is off this machine, but the record of the install could not be removed", logsOpen: false,
  }).body;
  assert.doesNotMatch(recorded, /still in your clusters/, "the list entry went; only the record stayed");
  assert.match(recorded, /The MemQL Install output has the details\./);
});
