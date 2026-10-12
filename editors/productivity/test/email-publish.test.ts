import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { readEmail } from "../src/emailDocument.js";
import { UserInputError } from "../src/problems.js";
import {
  EmailPublisher, chooseOrganization, editorBar, organizations, resolveOrganization, templateFor,
  type BarInput, type EmailTarget, type Organization, type PublishPorts, type TemplateState,
} from "../src/emailPublish.js";

// -- Fixtures -----------------------------------------------------------------

type Rows = Record<string, unknown>[];
type Answer = (call: string) => Rows | Promise<Rows>;

/**
 * Fake ports. `answers` is one function per builtin or query name; each call is recorded as
 * {name, call} before it is answered, and a name nobody answers is an error, so a call the code
 * should not make fails the test. `state` is the Map behind the memento, which like VS Code's
 * forgets a key that is updated to undefined, and `writes` is every update it was told of.
 */
function ports(answers: Record<string, Answer> = {}) {
  const calls: { name: string; call: string }[] = [];
  const state = new Map<string, unknown>();
  const writes: { key: string; value: unknown }[] = [];
  const port: PublishPorts = {
    async execute(name, call) {
      calls.push({ name, call });
      const answer = answers[name];
      if (!answer) throw new Error(`unexpected call: ${name}`);
      return answer(call);
    },
    state: {
      get: <T>(key: string) => state.get(key) as T | undefined,
      update: async (key, value) => { writes.push({ key, value }); if (value === undefined) state.delete(key); else state.set(key, value); },
    },
  };
  return { calls, state, writes, ports: port };
}

const names = (calls: { name: string }[]) => calls.map(call => call.name);

// Task 2's layout() helper: a layout-mode email file with the blocks given.
const theme = { page: "#eef0f3", canvas: "#ffffff", primary: "#1f5f8b", secondary: "#5b8fb9", accent: "#d9534f", tint: "#f6f4ef", text: "#2b2b2b", muted: "#8a8f98", headingFont: "Georgia", bodyFont: "Arial" };
const layout = (blocks: unknown[]) => JSON.stringify({ subject: "Hello", preheader: "Soon", layout: "builtin:article", theme, blocks }, null, 2) + "\n";
const welcome = readEmail(layout([
  { id: "headline", type: "heading", text: "Welcome" },
  { id: "cta", type: "button", label: "Get started", href: "https://example.com/start" },
]));
const written = readEmail(JSON.stringify({ subject: "Plain", textBody: "Hello", htmlBody: "<p>Hello</p>" }));

const CLIENT: Organization = { id: "client-org", name: "Client" };
const OTHER: Organization = { id: "other-org", name: "Other" };
// clientAccountsAll lists the person's organizations unsorted; the archived one is not for emails.
const ACCOUNTS: Rows = [
  { id: "other-org", name: "Other", status: "active" },
  { id: "old-org", name: "Archived", status: "archived" },
  { id: "client-org", name: "Client", status: "active" },
];

const file = (change: Partial<Extract<EmailTarget, { kind: "file" }>> = {}): EmailTarget =>
  ({ kind: "file", domain: "client.example", artifactId: "a1", fileId: "f1", name: "Welcome.email.json", labels: [], ...change });
const template = (change: Partial<Extract<EmailTarget, { kind: "template" }>> = {}): Extract<EmailTarget, { kind: "template" }> =>
  ({ kind: "template", domain: "client.example", templateId: "t9", name: "Monthly", accountId: "client-org", status: "draft", revision: "2026-09-01T12:00:00Z", ...change });
const local: EmailTarget = { kind: "local", name: "welcome.email.json" };

/** The receipt campaignSaveTemplate answers a save or a publish with. */
const saved = (revision: string, status = "draft", issues: unknown[] = [], templateId = "t1") => ({ templateId, revision, status, saved: true, issues });

// -- Organization -------------------------------------------------------------

test("organizations lists the active ones by name, whatever order the cluster answers in", async () => {
  const f = ports({ clientAccountsAll: () => ACCOUNTS });
  assert.deepEqual(await organizations(f.ports), [CLIENT, OTHER]);
  assert.deepEqual(names(f.calls), ["clientAccountsAll"]);
  assert.equal(f.calls[0]!.call, "query clientAccountsAll()", "archived organizations are not asked for, and any that come back are left out all the same");
  const unnamed = ports({ clientAccountsAll: () => [{ id: "b-org", name: "", status: "active" }, { id: "a-org", name: "Alpha", status: "active" }, { name: "No id", status: "active" }, { id: "x", name: "Paused", status: "suspended" }] });
  assert.deepEqual(await organizations(unnamed.ports), [{ id: "a-org", name: "Alpha" }, { id: "b-org", name: "b-org" }], "a name that is missing is the id; a row with no id or not active is no organization");
});

test("a file the Materializer wrote knows its organization without asking", async () => {
  const f = ports({
    clientAccountsAll: () => ACCOUNTS,
    compositionForOutputFile: () => [{ id: "c1", outputFileId: "f1", accountIds: ["client-org"] }],
  });
  const answer = await resolveOrganization(file(), f.ports);
  assert.deepEqual(answer, { organization: { id: "client-org", name: "Client" }, choices: [] });
  const asked = f.calls.find(call => call.name === "compositionForOutputFile")!;
  assert.match(asked.call, /fileId: "f1"/, "the composition is looked up by the Library file, not by its artifact");

  // The composition wins over the file's labels: first answer wins.
  const labelled = await resolveOrganization(file({ labels: ["other-org"] }), f.ports);
  assert.deepEqual(labelled.organization, CLIENT);

  // A composition for an organization the person no longer has, or for several, decides nothing.
  const gone = ports({ clientAccountsAll: () => ACCOUNTS, compositionForOutputFile: () => [{ accountIds: ["old-org"] }] });
  assert.deepEqual(await resolveOrganization(file({ labels: ["other-org"] }), gone.ports), { organization: OTHER, choices: [] });
  const several = ports({ clientAccountsAll: () => ACCOUNTS, compositionForOutputFile: () => [{ accountIds: ["client-org", "other-org"] }] });
  assert.deepEqual(await resolveOrganization(file(), several.ports), { organization: undefined, choices: [CLIENT, OTHER] });
});

test("a template knows its organization", async () => {
  const f = ports({ clientAccountsAll: () => ACCOUNTS });
  assert.deepEqual(await resolveOrganization(template(), f.ports), { organization: CLIENT, choices: [] });
  assert.deepEqual(names(f.calls), ["clientAccountsAll"], "a template has no composition to ask, and the name is all the list is needed for");
  // The template's organization is the answer even where it is not one the person lists: the render says why it cannot be shown.
  assert.deepEqual(await resolveOrganization(template({ accountId: "old-org" }), f.ports), { organization: { id: "old-org", name: "old-org" }, choices: [] });
  assert.deepEqual(await resolveOrganization(template(), f.ports, "other-org"), { organization: CLIENT, choices: [] }, "a remembered choice never overrides what the template says");
});

test("one label or one organization is enough; otherwise the person chooses", async () => {
  const many = ports({ clientAccountsAll: () => ACCOUNTS, compositionForOutputFile: () => [] });
  // Exactly one label is the organization, and nothing is asked of the person.
  assert.deepEqual(await resolveOrganization(file({ labels: ["other-org"] }), many.ports), { organization: OTHER, choices: [] });
  // Several labels, or none, with several organizations: the person chooses among the active ones, by name.
  assert.deepEqual(await resolveOrganization(file({ labels: ["client-org", "other-org"] }), many.ports), { organization: undefined, choices: [CLIENT, OTHER] });
  assert.deepEqual(await resolveOrganization(file(), many.ports), { organization: undefined, choices: [CLIENT, OTHER] });
  assert.deepEqual(await resolveOrganization(local, many.ports), { organization: undefined, choices: [CLIENT, OTHER] });
  // A remembered id among them resolves, and the list stays so the choice can be changed.
  assert.deepEqual(await resolveOrganization(file(), many.ports, "other-org"), { organization: OTHER, choices: [CLIENT, OTHER] });
  assert.deepEqual(await resolveOrganization(local, many.ports, "client-org"), { organization: CLIENT, choices: [CLIENT, OTHER] });
  // A remembered id that is not one of the active organizations resolves nothing.
  assert.deepEqual(await resolveOrganization(file(), many.ports, "old-org"), { organization: undefined, choices: [CLIENT, OTHER] });
  assert.deepEqual(await resolveOrganization(file(), many.ports, "nobody"), { organization: undefined, choices: [CLIENT, OTHER] });
  // The labels of organizations the person does not have decide nothing either.
  assert.deepEqual(await resolveOrganization(file({ labels: ["old-org"] }), many.ports), { organization: undefined, choices: [CLIENT, OTHER] });
  // The same organization twice is one organization.
  assert.deepEqual(await resolveOrganization(file({ labels: ["other-org", "other-org"] }), many.ports), { organization: OTHER, choices: [] });
  const twice = ports({ clientAccountsAll: () => ACCOUNTS, compositionForOutputFile: () => [{ accountIds: ["client-org", "client-org"] }] });
  assert.deepEqual(await resolveOrganization(file(), twice.ports), { organization: CLIENT, choices: [] });

  // The only active organization is enough, with or without labels, and a local file takes it too.
  const one = ports({ clientAccountsAll: () => [ACCOUNTS[2]!, ACCOUNTS[1]!], compositionForOutputFile: () => [] });
  assert.deepEqual(await resolveOrganization(file(), one.ports), { organization: CLIENT, choices: [] });
  assert.deepEqual(await resolveOrganization(file({ labels: ["client-org", "old-org"] }), one.ports), { organization: CLIENT, choices: [] });
  assert.deepEqual(await resolveOrganization(local, one.ports), { organization: CLIENT, choices: [] });

  // No organization at all: nothing to resolve and nothing to choose from.
  const none = ports({ clientAccountsAll: () => [], compositionForOutputFile: () => [] });
  assert.deepEqual(await resolveOrganization(file(), none.ports), { organization: undefined, choices: [] });
});

test("a file's organizations and its composition are asked for together", async () => {
  // The organizations answer on a later turn. By then the composition must already have been asked for: neither read waits for the other.
  let asked: string[] = [];
  const f = ports({
    clientAccountsAll: async () => { await new Promise(resolve => setImmediate(resolve)); asked = names(f.calls); return ACCOUNTS; },
    compositionForOutputFile: () => [{ accountIds: ["client-org"] }],
  });
  assert.deepEqual(await resolveOrganization(file(), f.ports), { organization: CLIENT, choices: [] });
  assert.deepEqual(asked, ["clientAccountsAll", "compositionForOutputFile"]);
  // A template has no composition to ask for.
  const g = ports({ clientAccountsAll: () => ACCOUNTS });
  await resolveOrganization(template(), g.ports);
  assert.deepEqual(names(g.calls), ["clientAccountsAll"]);
});

test("a read that fails fails the resolution, and the read left over is not an unhandled rejection", async () => {
  const unhandled: unknown[] = [];
  const onUnhandled = (reason: unknown) => { unhandled.push(reason); };
  process.on("unhandledRejection", onUnhandled);
  try {
    for (const [accounts, composition] of [["organizations failed", ""], ["", "composition failed"], ["organizations failed", "composition failed"]] as const) {
      const f = ports({
        clientAccountsAll: async () => { await new Promise(resolve => setImmediate(resolve)); if (accounts) throw new Error(accounts); return ACCOUNTS; },
        compositionForOutputFile: async () => { if (composition) throw new Error(composition); return []; },
      });
      await assert.rejects(resolveOrganization(file(), f.ports), new RegExp(`${composition || accounts}`));
    }
    // Rejections that nobody handled are reported on a later turn.
    await new Promise(resolve => setImmediate(resolve));
    await new Promise(resolve => setImmediate(resolve));
  } finally {
    process.off("unhandledRejection", onUnhandled);
  }
  assert.deepEqual(unhandled, []);
});

test("choosing labels an unlabelled Library file once", async () => {
  const f = ports({ setArtifactAccounts: () => [{ id: "a1" }] });
  assert.equal(await chooseOrganization(file(), CLIENT, f.ports), "labelled");
  assert.deepEqual(names(f.calls), ["setArtifactAccounts"]);
  assert.match(f.calls[0]!.call, /artifactId: "a1"/);
  assert.match(f.calls[0]!.call, /accountIds: \["client-org"\]/);

  // A labelled file keeps its labels, a local file has none to write and a template needs none: the choice is only remembered.
  const quiet = ports();
  assert.equal(await chooseOrganization(file({ labels: ["client-org"] }), OTHER, quiet.ports), "remembered");
  assert.equal(await chooseOrganization(file({ labels: ["a-org", "b-org"] }), OTHER, quiet.ports), "remembered");
  assert.equal(await chooseOrganization(local, CLIENT, quiet.ports), "remembered");
  assert.equal(await chooseOrganization(template(), CLIENT, quiet.ports), "remembered");
  assert.deepEqual(quiet.calls, [], "and nothing is written");
});

// -- The action bar -----------------------------------------------------------

const BASE: BarInput = { connected: true, domain: "client.example", invalid: false, local: false, organization: CLIENT, mode: "layout", dirty: false, issues: 0, unchanged: true };
const READY: TemplateState = { templateId: "t1", status: "ready", revision: "r1" };
const bar = (change: Partial<BarInput>) => editorBar({ ...BASE, ...change });

test("the action bar follows the email's state", () => {
  // 1. Busy: the work in words, no acts, whatever else is true.
  assert.deepEqual(bar({ busy: "publish" }), { state: "Publishing", acts: [] });
  assert.deepEqual(bar({ busy: "layout" }), { state: "Saving the layout", acts: [] });
  assert.deepEqual(bar({ busy: "copy" }), { state: "Saving to MemQL", acts: [] });
  assert.deepEqual(bar({ busy: "publish", connected: false, invalid: true, organization: undefined, issues: 2, dirty: true }), { state: "Publishing", acts: [] });

  // 2. Not connected: Connect, and the domain where there is one.
  assert.deepEqual(bar({ connected: false }), { state: "Not connected to client.example", acts: ["connect"] });
  assert.deepEqual(bar({ connected: false, local: true, domain: undefined }), { state: "Not connected", acts: ["connect"] });
  assert.deepEqual(bar({ connected: false, local: true }), { state: "Not connected", acts: ["connect"] });
  assert.deepEqual(bar({ connected: false, domain: undefined }), { state: "Not connected", acts: ["connect"] }, "and no placeholder shows where there is no domain to name");
  assert.deepEqual(bar({ connected: false, invalid: true, organization: undefined, dirty: true, issues: 3 }), { state: "Not connected to client.example", acts: ["connect"] });

  // 3. Unreadable: Open Source.
  assert.deepEqual(bar({ invalid: true }), { state: "Can’t read this email", acts: ["openSource"] });
  assert.deepEqual(bar({ invalid: true, organization: undefined, local: true, dirty: true }), { state: "Can’t read this email", acts: ["openSource"] });

  // 4. No organization yet: the inspector asks, the bar offers nothing.
  assert.deepEqual(bar({ organization: undefined }), { state: "Choose the organization", acts: [] });
  assert.deepEqual(bar({ organization: undefined, local: true, dirty: true, issues: 1 }), { state: "Choose the organization", acts: [] });

  // 5. A local file is saved to MemQL before anything else.
  assert.deepEqual(bar({ local: true, domain: undefined }), { state: "On this computer", acts: ["saveToMemQL"] });
  assert.deepEqual(bar({ local: true, dirty: true, issues: 2, mode: "custom" }), { state: "On this computer", acts: ["saveToMemQL"] });

  // 6. Issues: fix them first. Publish is absent, and Save as layout stays for a layout.
  assert.deepEqual(bar({ issues: 1 }), { state: "Fix 1 issue to publish", acts: ["saveLayout"] });
  assert.deepEqual(bar({ issues: 3, dirty: true }), { state: "Fix 3 issues to publish", acts: ["saveLayout"] });
  assert.deepEqual(bar({ issues: 2, template: READY }), { state: "Fix 2 issues to publish", acts: ["saveLayout"] });
  assert.deepEqual(bar({ issues: 1, mode: "custom" }), { state: "Fix 1 issue to publish", acts: [] });

  // 7. Unsaved changes: save a layout or publish, which saves first.
  assert.deepEqual(bar({ dirty: true }), { state: "Unsaved changes", acts: ["saveLayout", "publish"] });
  assert.deepEqual(bar({ dirty: true, template: READY, unchanged: true }), { state: "Unsaved changes", acts: ["saveLayout", "publish"] });
  assert.deepEqual(bar({ dirty: true, mode: "custom" }), { state: "Unsaved changes", acts: ["publish"] });

  // 8. Published and unchanged: nothing left to publish.
  assert.deepEqual(bar({ template: READY }), { state: "Published to Client", acts: ["saveLayout"] });
  assert.deepEqual(bar({ template: READY, organization: OTHER }), { state: "Published to Other", acts: ["saveLayout"] });
  assert.deepEqual(bar({ template: READY, mode: "custom" }), { state: "Published to Client", acts: [] });

  // 9. Published, then changed.
  assert.deepEqual(bar({ template: READY, unchanged: false }), { state: "Changes since publishing", acts: ["saveLayout", "publish"] });
  assert.deepEqual(bar({ template: READY, unchanged: false, mode: "custom" }), { state: "Changes since publishing", acts: ["publish"] });

  // 10. Otherwise a draft: no template yet, a template that is a draft, or one that was archived.
  assert.deepEqual(bar({ unchanged: false }), { state: "Draft", acts: ["saveLayout", "publish"] });
  assert.deepEqual(bar({}), { state: "Draft", acts: ["saveLayout", "publish"] });
  assert.deepEqual(bar({ template: { ...READY, status: "draft" } }), { state: "Draft", acts: ["saveLayout", "publish"] });
  assert.deepEqual(bar({ template: { ...READY, status: "archived" }, unchanged: false }), { state: "Draft", acts: ["saveLayout", "publish"] });
  assert.deepEqual(bar({ mode: "custom" }), { state: "Draft", acts: ["publish"] });

  // A layout that was just saved replaces the state of rows 6 to 10, and only those.
  assert.deepEqual(bar({ layoutSaved: true }), { state: "Saved as a layout", acts: ["saveLayout", "publish"] });
  assert.deepEqual(bar({ layoutSaved: true, template: READY }), { state: "Saved as a layout", acts: ["saveLayout"] });
  assert.deepEqual(bar({ layoutSaved: true, dirty: true }), { state: "Saved as a layout", acts: ["saveLayout", "publish"] });
  assert.deepEqual(bar({ layoutSaved: true, issues: 1 }), { state: "Saved as a layout", acts: ["saveLayout"] });
  assert.deepEqual(bar({ layoutSaved: true, busy: "layout" }), { state: "Saving the layout", acts: [] });
  assert.deepEqual(bar({ layoutSaved: true, connected: false }), { state: "Not connected to client.example", acts: ["connect"] });
  assert.deepEqual(bar({ layoutSaved: true, invalid: true }), { state: "Can’t read this email", acts: ["openSource"] });
  assert.deepEqual(bar({ layoutSaved: true, organization: undefined }), { state: "Choose the organization", acts: [] });
  assert.deepEqual(bar({ layoutSaved: true, local: true }), { state: "On this computer", acts: ["saveToMemQL"] });

  // Every combination: at most three acts, none twice, and an act only where it is legal. An act that is not legal is absent.
  const templates: (TemplateState | undefined)[] = [undefined, READY, { ...READY, status: "draft" }, { ...READY, status: "archived" }];
  let combinations = 0;
  for (const connected of [true, false]) for (const invalid of [false, true]) for (const isLocal of [false, true]) for (const organization of [undefined, CLIENT])
    for (const mode of ["layout", "custom"] as const) for (const dirty of [false, true]) for (const issues of [0, 1, 4]) for (const current of templates)
      for (const unchanged of [true, false]) for (const busy of [undefined, "publish", "layout", "copy"] as const) for (const layoutSaved of [false, true]) {
        const input: BarInput = { connected, domain: isLocal ? undefined : "client.example", invalid, local: isLocal, organization, mode, dirty, issues, template: current, unchanged, busy, layoutSaved };
        const { state, acts } = editorBar(input);
        const where = JSON.stringify(input);
        combinations++;
        assert.ok(state !== "" && !/[{}]/.test(state), `a state in words with no placeholder left: ${where} gave "${state}"`);
        assert.ok(acts.length <= 3 && new Set(acts).size === acts.length, `at most three acts, none twice: ${where}`);
        const usable = connected && !invalid && organization !== undefined && !busy;
        if (busy) assert.deepEqual(acts, [], `busy has no acts: ${where}`);
        if (acts.includes("connect")) assert.ok(!connected && !busy, where);
        if (acts.includes("openSource")) assert.ok(connected && invalid && !busy, where);
        if (acts.includes("saveToMemQL")) assert.ok(usable && isLocal, where);
        if (acts.includes("publish")) assert.ok(usable && !isLocal && issues === 0, `publish only for an email that can be published: ${where}`);
        if (acts.includes("saveLayout")) assert.ok(usable && !isLocal && mode === "layout", `custom HTML and local files have no Save as layout: ${where}`);
        if (/^Published to /.test(state)) assert.ok(!acts.includes("publish"), `nothing to publish when published and unchanged: ${where}`);
        if (acts.includes("publish") && !dirty && issues === 0) assert.ok(!(current?.status === "ready" && unchanged), where);
        if (layoutSaved && usable && !isLocal) assert.equal(state, "Saved as a layout", where);
      }
  assert.equal(combinations, 2 * 2 * 2 * 2 * 2 * 2 * 3 * 4 * 2 * 4 * 2);
});

test("the acts the bar can name are the acts the page draws", () => {
  // emailView.ts runs in the page and keeps its own list of acts; an act the page does not know is dropped without a word.
  const view = readFileSync(join(__dirname, "..", "src", "emailView.ts"), "utf8");
  const drawn = /^type BarAct = ([^;]+);/m.exec(view)?.[1];
  assert.ok(drawn, "the page declares the acts it draws");
  const produced = new Set<string>();
  const templates: (TemplateState | undefined)[] = [undefined, READY, { ...READY, status: "draft" }];
  for (const connected of [true, false]) for (const invalid of [false, true]) for (const isLocal of [false, true]) for (const organization of [undefined, CLIENT])
    for (const mode of ["layout", "custom"] as const) for (const dirty of [false, true]) for (const issues of [0, 1]) for (const current of templates)
      for (const unchanged of [true, false]) for (const act of editorBar({ connected, invalid, local: isLocal, organization, mode, dirty, issues, template: current, unchanged }).acts) produced.add(act);
  assert.deepEqual([...produced].sort(), [...drawn.matchAll(/"(\w+)"/g)].map(match => match[1]!).sort());
});

// -- Publishing ---------------------------------------------------------------

test("first publish saves then publishes the exact revision, keyed by the file", async () => {
  const f = ports({
    templateForSource: () => [],
    campaignSaveTemplate: call => /action: "publish"/.test(call)
      ? [{ templateId: "t1", revision: "2026-10-11T10:00:01Z", status: "ready", saved: true, issues: [] }]
      : [{ templateId: "t1", revision: "2026-10-11T10:00:00Z", status: "draft", saved: true, issues: [] }],
  });
  const target = { kind: "file", domain: "client.example", artifactId: "a1", fileId: "f1", name: "Welcome.email.json", labels: [] } as const;
  await new EmailPublisher(f.ports).publish({ target: () => target, organization: { id: "client-org", name: "Client" }, email: () => welcome, save: async () => true, stillCurrent: () => true });
  assert.deepEqual(f.calls.map(c => c.name), ["templateForSource", "campaignSaveTemplate", "campaignSaveTemplate"]);
  assert.match(f.calls[1].call, /action: "save"/);
  assert.match(f.calls[1].call, /sourceArtifactId: "a1"/);
  assert.match(f.calls[1].call, /expectedRevision: ""/);
  assert.doesNotMatch(f.calls[1].call, /templateId:/);
  assert.match(f.calls[1].call, /name: "Welcome"/);
  assert.match(f.calls[2].call, /templateId: "t1"/);
  assert.match(f.calls[2].call, /expectedRevision: "2026-10-11T10:00:00Z"/);
  assert.ok([...f.state.keys()].every(key => !key.startsWith("memql.campaignPublication")), "no local publication mapping");
});

/** What publish needs from the editor, with each part replaceable. */
function publishing(change: Partial<Parameters<EmailPublisher["publish"]>[0]> = {}) {
  return { target: () => file(), organization: CLIENT, email: () => welcome, save: async () => true, stillCurrent: () => true, ...change };
}

test("a publication asks for the file's template in the organization, and sends the email as it is", async () => {
  const before = JSON.stringify(welcome);
  // The first save of a file makes its template under an id the cluster chooses; the publication names that one.
  const f = ports({ templateForSource: () => [], campaignSaveTemplate: call => [saved("2026-10-11T10:00:00Z", /action: "publish"/.test(call) ? "ready" : "draft", [], "d41d8cd98f00")] });
  await new EmailPublisher(f.ports).publish(publishing());
  assert.match(f.calls[0]!.call, /^query templateForSource\(accountId: "client-org", sourceArtifactId: "a1"\)$/);
  for (const index of [1, 2]) {
    const call = f.calls[index]!.call;
    assert.match(call, /accountId: "client-org"/);
    assert.match(call, /name: "Welcome"/);
    assert.match(call, /content: \{blocks: \[/, "the whole email is the content, sent as the document it is");
    assert.ok(call.includes('label: "Get started"') && call.includes('subject: "Hello"') && call.includes('layout: "builtin:article"'), call);
  }
  assert.equal(JSON.stringify(welcome), before, "the email is read, never changed");
  assert.match(f.calls[2]!.call, /templateId: "d41d8cd98f00"/, "a publication names the template the save answered with");
  assert.doesNotMatch(f.calls[2]!.call, /sourceArtifactId/, "and needs no file to find it by");
});

test("the template is named after the file, whatever the case of its suffix", async () => {
  for (const [name, expected] of [["Welcome.email.json", "Welcome"], ["Spring sale.EMAIL.JSON", "Spring sale"], ["notes.email.json.email.json", "notes.email.json"], ["plain", "plain"], [".email.json", ".email.json"]] as const) {
    const f = ports({ templateForSource: () => [], campaignSaveTemplate: () => [saved("r1")] });
    // stillCurrent answers false so the run ends after the save, which is the call that names the template.
    await assert.rejects(new EmailPublisher(f.ports).publish(publishing({ target: () => file({ name }), stillCurrent: () => false })), /changed while publishing/);
    assert.ok(f.calls[1]!.call.includes(`name: ${JSON.stringify(expected)}`), f.calls[1]!.call.slice(0, 200));
  }
});

test("republishing updates the template found for the file", async () => {
  const f = ports({
    templateForSource: () => [{ id: "t1", createdAt: "2026-10-11T10:00:00Z" }],
    campaignSaveTemplate: call => /action: "publish"/.test(call) ? [saved("2026-10-11T10:00:02Z", "ready")] : [saved("2026-10-11T10:00:01Z")],
  });
  const state = await new EmailPublisher(f.ports).publish(publishing());
  assert.deepEqual(names(f.calls), ["templateForSource", "campaignSaveTemplate", "campaignSaveTemplate"]);
  assert.match(f.calls[1]!.call, /expectedRevision: "2026-10-11T10:00:00Z"/, "the found row's createdAt is the revision the save expects");
  assert.doesNotMatch(f.calls[1]!.call, /templateId:/, "the server knows which template the file is");
  assert.match(f.calls[2]!.call, /templateId: "t1"/);
  assert.match(f.calls[2]!.call, /expectedRevision: "2026-10-11T10:00:01Z"/, "the publication expects the revision the save made");
  assert.match(f.calls[2]!.call, /action: "publish"/);
  assert.deepEqual(state, { templateId: "t1", status: "ready", revision: "2026-10-11T10:00:02Z", document: JSON.parse(JSON.stringify(welcome)) }, "what is now published, with the email it holds");
  assert.notEqual(state.document, welcome, "a copy: an edit to the email afterwards does not change what was published");
});

test("a lost response is recovered without a second template", async () => {
  let stored: { id: string; createdAt: string } | undefined;
  let lose = true;
  const f = ports({
    templateForSource: () => stored ? [{ ...stored }] : [],
    campaignSaveTemplate: call => {
      if (/action: "publish"/.test(call)) return [saved("2026-10-11T10:00:02Z", "ready")];
      if (/expectedRevision: ""/.test(call)) {
        // The cluster creates the template, then the answer is lost.
        stored = { id: "t1", createdAt: "2026-10-11T10:00:00Z" };
        if (lose) { lose = false; throw new Error("connection lost"); }
        return [saved("2026-10-11T10:00:00Z")];
      }
      return [saved("2026-10-11T10:00:00Z")];
    },
  });
  const publisher = new EmailPublisher(f.ports);
  await assert.rejects(publisher.publish(publishing()), /connection lost/);
  const state = await publisher.publish(publishing());
  assert.equal(state.templateId, "t1");
  assert.equal(state.status, "ready");
  const saves = f.calls.filter(call => call.name === "campaignSaveTemplate" && /action: "save"/.test(call.call));
  assert.equal(saves.filter(call => /expectedRevision: ""/.test(call.call)).length, 1, "only one save ever asks to create");
  assert.equal(saves.length, 2);
  assert.match(saves[1]!.call, /expectedRevision: "2026-10-11T10:00:00Z"/, "the retry saves with the revision it found");
  assert.deepEqual(names(f.calls), ["templateForSource", "campaignSaveTemplate", "templateForSource", "campaignSaveTemplate", "campaignSaveTemplate"]);
  assert.equal(f.state.size, 0, "nothing is remembered between the two attempts");
});

test("a double click publishes once", async () => {
  const f = ports({
    templateForSource: () => [],
    campaignSaveTemplate: call => [saved("r1", /action: "publish"/.test(call) ? "ready" : "draft")],
  });
  const publisher = new EmailPublisher(f.ports);
  let saves = 0;
  const input = publishing({ save: async () => { saves++; return true; } });
  const [first, second] = await Promise.all([publisher.publish(input), publisher.publish(input)]);
  assert.deepEqual(second, first);
  assert.equal(saves, 1, "one buffer save");
  assert.deepEqual(names(f.calls), ["templateForSource", "campaignSaveTemplate", "campaignSaveTemplate"], "one save and one publication");
  // The flight ends with the publication: the next click is a new one.
  await publisher.publish(input);
  assert.equal(f.calls.filter(call => call.name === "campaignSaveTemplate").length, 4);
  // Another file is another flight, and so is the same file in another organization or on another cluster, and so is a template.
  const other = ports({ templateForSource: () => [], campaignSaveTemplate: call => [saved("r1", /action: "publish"/.test(call) ? "ready" : "draft")] });
  const together = new EmailPublisher(other.ports);
  await Promise.all([
    together.publish(input),
    together.publish(publishing({ target: () => file({ artifactId: "a2" }) })),
    together.publish(publishing({ organization: OTHER })),
    together.publish(publishing({ target: () => file({ domain: "other.example" }) })),
    together.publish(publishing({ target: () => template() })),
    together.publish(publishing({ target: () => template({ templateId: "t10" }) })),
    together.publish(publishing({ target: () => template({ domain: "other.example" }) })),
    // A file and a template that happen to share an id are still two things.
    together.publish(publishing({ target: () => file({ artifactId: "t9" }) })),
  ]);
  assert.equal(other.calls.filter(call => call.name === "campaignSaveTemplate").length, 5 * 2 + 3, "five files with a save and a publication each, and three templates with a publication");
});

test("a publication that fails ends its flight, so the next click tries again", async () => {
  let fail = true;
  const f = ports({
    templateForSource: () => [],
    campaignSaveTemplate: call => { if (fail) { fail = false; throw new Error("connection lost"); } return [saved("r1", /action: "publish"/.test(call) ? "ready" : "draft")]; },
  });
  const publisher = new EmailPublisher(f.ports);
  await assert.rejects(Promise.all([publisher.publish(publishing()), publisher.publish(publishing())]), /connection lost/);
  assert.equal(f.calls.filter(call => call.name === "campaignSaveTemplate").length, 1, "both clicks shared the one attempt");
  assert.equal((await publisher.publish(publishing())).status, "ready");
});

test("an edit while publishing stops before the publish call", async () => {
  let current = true;
  const f = ports({
    templateForSource: () => [],
    // The person types while the save is on its way.
    campaignSaveTemplate: () => { current = false; return [saved("r1")]; },
  });
  await assert.rejects(new EmailPublisher(f.ports).publish(publishing({ stillCurrent: () => current })),
    (error: unknown) => error instanceof UserInputError && error.message === "The email changed while publishing. Publish again.");
  assert.deepEqual(names(f.calls), ["templateForSource", "campaignSaveTemplate"], "the draft is saved, and nothing is published");
  assert.doesNotMatch(f.calls[1]!.call, /action: "publish"/);

  // A template resource has no save of its own to wait for: the edit that lands while the buffer is saved stops it too.
  let typed = false;
  const target = template();
  const g = ports({ campaignSaveTemplate: () => [saved("r1", "ready", [], "t9")] });
  await assert.rejects(new EmailPublisher(g.ports).publish(publishing({ target: () => target, save: async () => { typed = true; return true; }, stillCurrent: () => !typed })), /changed while publishing/);
  assert.deepEqual(g.calls, []);
  assert.equal(target.status, "draft", "the template is as it was");
});

test("issues from the save stop the publish", async () => {
  const issues = [{ code: "image_missing", block: "hero", message: "Choose a picture for “Hero”." }];
  const f = ports({ templateForSource: () => [], campaignSaveTemplate: () => [saved("r1", "draft", issues)] });
  await assert.rejects(new EmailPublisher(f.ports).publish(publishing()),
    (error: unknown) => error instanceof UserInputError && error.message === "Fix the issues shown in the editor, then publish.");
  assert.deepEqual(names(f.calls), ["templateForSource", "campaignSaveTemplate"]);
  assert.match(f.calls[1]!.call, /action: "save"/);
});

test("a save that does not answer as a save is not a publication", async () => {
  for (const answer of [[], [{}], [{ saved: false, templateId: "t1", revision: "r1" }], [{ saved: true, revision: "r1" }], [{ saved: true, templateId: "t1" }], [{ saved: true, templateId: "t1", revision: "" }]]) {
    const f = ports({ templateForSource: () => [], campaignSaveTemplate: () => answer });
    await assert.rejects(new EmailPublisher(f.ports).publish(publishing()), (error: unknown) => error instanceof Error && !(error instanceof UserInputError) && /did not confirm/.test(error.message), JSON.stringify(answer));
    assert.deepEqual(names(f.calls), ["templateForSource", "campaignSaveTemplate"], "and nothing is published on it");
  }
  // Nor is a publication that does not say it is published.
  for (const answer of [[], [{ saved: true, templateId: "t1", revision: "r2" }], [{ saved: false, templateId: "t1", revision: "r2", status: "ready" }]]) {
    const f = ports({ templateForSource: () => [], campaignSaveTemplate: call => /action: "publish"/.test(call) ? answer : [saved("r1")] });
    await assert.rejects(new EmailPublisher(f.ports).publish(publishing()), (error: unknown) => error instanceof Error && !(error instanceof UserInputError) && /did not confirm/.test(error.message), JSON.stringify(answer));
  }
  // A template found without its revision cannot be saved against.
  const f = ports({ templateForSource: () => [{ id: "t1" }], campaignSaveTemplate: () => [saved("r1")] });
  await assert.rejects(new EmailPublisher(f.ports).publish(publishing()), /did not return the template revision/);
  assert.deepEqual(names(f.calls), ["templateForSource"]);
});

test("publishing needs the buffer saved, and a local email is published from MemQL", async () => {
  const f = ports();
  await assert.rejects(new EmailPublisher(f.ports).publish(publishing({ save: async () => false })),
    (error: unknown) => error instanceof UserInputError && error.message === "Save the email before publishing.");
  assert.deepEqual(f.calls, [], "nothing is asked of the cluster for a buffer that could not be saved");
  let saves = 0;
  await assert.rejects(new EmailPublisher(f.ports).publish(publishing({ target: () => local, save: async () => { saves++; return true; } })), (error: unknown) => error instanceof Error && !(error instanceof UserInputError));
  assert.equal(saves, 0, "a file that cannot be published is not saved for it");
  assert.deepEqual(f.calls, []);
});

test("a template resource publishes with its opened revision", async () => {
  const target = template();
  const f = ports({ campaignSaveTemplate: () => [saved("2026-10-11T10:00:01Z", "ready", [], "t9")] });
  // The editor's organization is OTHER here, as if it were stale: a template is published as the organization it belongs to.
  const state = await new EmailPublisher(f.ports).publish(publishing({ target: () => target, email: () => written, organization: OTHER }));
  assert.deepEqual(names(f.calls), ["campaignSaveTemplate"], "no lookup by file: the template is the target");
  const call = f.calls[0]!.call;
  assert.match(call, /templateId: "t9"/);
  assert.match(call, /accountId: "client-org"/);
  assert.match(call, /name: "Monthly"/);
  assert.match(call, /expectedRevision: "2026-09-01T12:00:00Z"/);
  assert.match(call, /action: "publish"/);
  assert.match(call, /htmlBody: "<p>Hello<\/p>"/);
  assert.doesNotMatch(call, /sourceArtifactId/);
  assert.equal(target.status, "ready");
  assert.equal(target.revision, "2026-10-11T10:00:01Z");
  assert.deepEqual(state, { templateId: "t9", status: "ready", revision: "2026-10-11T10:00:01Z", document: JSON.parse(JSON.stringify(written)) });
  assert.equal(f.state.size, 0);
});

test("a template resource publishes the revision its save left", async () => {
  // Saving the buffer is what moves a template's revision (Documents.save writes a draft), so the target is read afterwards.
  let target = template({ status: "ready" });
  const f = ports({ campaignSaveTemplate: () => [saved("2026-10-11T10:00:09Z", "ready", [], "t9")] });
  const order: string[] = [];
  await new EmailPublisher(f.ports).publish(publishing({
    target: () => { order.push("target"); return target; },
    email: () => { order.push("email"); return welcome; },
    save: async () => { order.push("save"); target = template({ revision: "2026-10-11T10:00:05Z", status: "draft" }); return true; },
    stillCurrent: () => { order.push("current"); return true; },
  }));
  assert.deepEqual(order, ["target", "save", "target", "email", "current"], "the first read only names the flight; the target and the email that are published are read after the save");
  assert.match(f.calls[0]!.call, /expectedRevision: "2026-10-11T10:00:05Z"/);
  assert.equal(target.status, "ready", "the target that was published is the one updated");
  assert.equal(target.revision, "2026-10-11T10:00:09Z");
});

// -- Templates ----------------------------------------------------------------

test("a file's template is the one published from it, read whole by id", async () => {
  const stored = { welcome: { subject: "Hello", blocks: [] } };
  const f = ports({
    templateForSource: () => [{ id: "t1", createdAt: "2026-10-11T10:00:00Z" }],
    templateById: () => [{ id: "t1", status: "ready", createdAt: "2026-10-11T10:00:03Z", document: stored.welcome, htmlBody: "<p>…</p>" }],
  });
  assert.deepEqual(await templateFor(file(), CLIENT, f.ports), { templateId: "t1", status: "ready", revision: "2026-10-11T10:00:03Z", document: stored.welcome });
  assert.deepEqual(names(f.calls), ["templateForSource", "templateById"], "the lookup is light and carries no document; the document is read by id");
  assert.match(f.calls[0]!.call, /accountId: "client-org", sourceArtifactId: "a1"/);
  assert.match(f.calls[1]!.call, /templateId: "t1"/);

  // A file never published has no template, and one published before documents have no document to compare with.
  const none = ports({ templateForSource: () => [] });
  assert.equal(await templateFor(file(), CLIENT, none.ports), undefined);
  assert.deepEqual(names(none.calls), ["templateForSource"]);
  const legacy = ports({ templateForSource: () => [{ id: "t2", createdAt: "r" }], templateById: () => [{ id: "t2", status: "draft", createdAt: "r2", subject: "Old" }] });
  assert.deepEqual(await templateFor(file(), CLIENT, legacy.ports), { templateId: "t2", status: "draft", revision: "r2" });
  const gone = ports({ templateForSource: () => [{ id: "t3", createdAt: "r" }], templateById: () => [] });
  assert.equal(await templateFor(file(), CLIENT, gone.ports), undefined, "a template that went between the two reads is no template");

  // A template resource is its own state, and a local file has none; neither asks the cluster.
  const quiet = ports();
  for (const status of ["ready", "draft", "archived"]) {
    assert.deepEqual(await templateFor(template({ status, revision: "r9" }), CLIENT, quiet.ports), { templateId: "t9", status, revision: "r9" });
  }
  assert.equal(await templateFor(local, CLIENT, quiet.ports), undefined);
  assert.deepEqual(quiet.calls, []);
});

// -- Layouts ------------------------------------------------------------------

const BUILTINS: Rows = [
  { id: "builtin:confirmation", kind: "builtin", name: "Confirmation", status: "active" },
  { id: "builtin:article", kind: "builtin", name: "Article", status: "active" },
];
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const UNRELATED: Record<string, unknown> = { id: "org:colleague", layoutId: "colleague", kind: "organization", name: "Autumn letter", description: "A colleague’s layout", revision: "r0", status: "active" };

test("save as layout creates once with a stable id and updates the layout saved from the same file", async () => {
  // A new layout is created under an id that is kept until the cluster has answered, so a lost answer retries the same layout.
  let lose = true;
  const f = ports({
    emailLayouts: () => BUILTINS,
    campaignSaveLayout: call => {
      if (lose) { lose = false; throw new Error("connection lost"); }
      return [{ layoutId: /layoutId: "([^"]+)"/.exec(call)![1], revision: "2026-10-11T11:00:00Z", status: "active", saved: true }];
    },
  });
  const publisher = new EmailPublisher(f.ports);
  const ask = { target: file(), organization: CLIENT, email: welcome, name: "Monthly", key: "k1" };
  await assert.rejects(publisher.saveLayout(ask), /connection lost/);
  const pending = f.state.get("memql.emailLayoutPending:k1");
  assert.match(String(pending), UUID, "the id is made and kept before the request");
  assert.match(f.calls[1]!.call, new RegExp(`layoutId: "${pending}"`));

  const result = await publisher.saveLayout(ask);
  assert.deepEqual(result, { layoutId: pending, revision: "2026-10-11T11:00:00Z" });
  const saves = f.calls.filter(call => call.name === "campaignSaveLayout");
  assert.equal(saves.length, 2);
  assert.match(saves[1]!.call, new RegExp(`layoutId: "${pending}"`), "the retry carries the stored id");
  for (const save of saves) {
    assert.match(save.call, /accountId: "client-org"/);
    assert.match(save.call, /name: "Monthly"/);
    assert.match(save.call, /sourceArtifactId: "a1"/, "the layout remembers the file it was saved from");
    assert.match(save.call, /expectedRevision: ""/);
    assert.match(save.call, /action: "save"/);
    assert.match(save.call, /document: \{[^]*"?blocks"?: \[/, "the whole email is the layout");
    assert.doesNotMatch(save.call, /description/);
  }
  assert.equal(f.state.has("memql.emailLayoutPending:k1"), false, "cleared once the cluster answered");
  assert.deepEqual(names(f.calls), ["emailLayouts", "campaignSaveLayout", "emailLayouts", "campaignSaveLayout"]);
  assert.match(f.calls[0]!.call, /^builtin emailLayouts\(accountId: "client-org"\)$/);

  // Another email key is another request. This fake lists no layout from the file, so it creates another, under a new id; a cluster
  // that had kept the first would list it by its file, and the next test shows that save updating it.
  const again = await publisher.saveLayout({ ...ask, key: "k2" });
  assert.notEqual(again.layoutId, pending);

  // With a layout saved from this file the call updates it: its layoutId and revision, its description, and the file it came from.
  const g = ports({
    emailLayouts: () => [...BUILTINS, { id: "org:l1", layoutId: "l1", sourceArtifactId: "a2", revision: "r0", name: "Other file", kind: "organization" },
      { id: "org:l2", layoutId: "l2", sourceArtifactId: "a1", revision: "r1", name: "Monthly", description: "For the monthly letter", kind: "organization" }],
    campaignSaveLayout: () => [{ layoutId: "l2", revision: "r2", status: "active", saved: true }],
  });
  const updated = await new EmailPublisher(g.ports).saveLayout({ ...ask, name: "Monthly letter" });
  assert.deepEqual(updated, { layoutId: "l2", revision: "r2" });
  const call = g.calls[1]!.call;
  assert.match(call, /layoutId: "l2"/);
  assert.match(call, /expectedRevision: "r1"/);
  assert.match(call, /description: "For the monthly letter"/);
  assert.match(call, /name: "Monthly letter"/);
  assert.match(call, /sourceArtifactId: "a1"/);
  assert.doesNotMatch(call, /org:/, "the client never builds the opaque id");
  assert.equal(g.state.size, 0, "an update needs no id kept");

  // A lost answer on an update that follows a lost creation finds the layout that creation made, and leaves no id behind.
  const h = ports({
    emailLayouts: () => [{ id: "org:made", layoutId: "made", sourceArtifactId: "a1", revision: "r5", name: "Monthly", description: "", kind: "organization" }],
    campaignSaveLayout: () => [{ layoutId: "made", revision: "r5", status: "active", saved: true }],
  });
  h.state.set("memql.emailLayoutPending:k1", "made");
  await new EmailPublisher(h.ports).saveLayout(ask);
  assert.match(h.calls[1]!.call, /layoutId: "made"/);
  assert.match(h.calls[1]!.call, /expectedRevision: "r5"/);
  assert.equal(h.state.has("memql.emailLayoutPending:k1"), false);
});

test("publishing an email and saving it as a layout are separate jobs", async () => {
  const f = ports({
    templateForSource: () => [],
    emailLayouts: () => BUILTINS,
    campaignSaveTemplate: call => [saved("r1", /action: "publish"/.test(call) ? "ready" : "draft")],
    campaignSaveLayout: call => [{ layoutId: /layoutId: "([^"]+)"/.exec(call)![1], revision: "r7", status: "active", saved: true }],
  });
  const publisher = new EmailPublisher(f.ports);
  const [published, layout] = await Promise.all([
    publisher.publish(publishing()),
    publisher.saveLayout({ target: file(), organization: CLIENT, email: welcome, name: "Monthly", key: "memql-file://client.example/artifacts/a1/Welcome.email.json" }),
  ]);
  assert.equal(published.templateId, "t1");
  assert.match(layout.layoutId, UUID);
  assert.equal(layout.revision, "r7");
});

/**
 * The cluster as saveLayout meets it. A layout is made under the id it is sent. An update must name the revision it read. Saving
 * what is already stored answers with the receipt already given. `lose` is how many answers are lost after the layout is stored.
 */
function layoutCluster(lose: number) {
  const stored = new Map<string, { revision: string; content: string }>();
  let revisions = 0;
  let lost = lose;
  const answers: Record<string, Answer> = {
    // A layout nobody here made, saved by a colleague since (newest first), is listed ahead of the one this email made: it has no file either.
    emailLayouts: () => [...BUILTINS, UNRELATED, ...[...stored].map(([layoutId, row]) => ({ id: `org:${layoutId}`, layoutId, kind: "organization", name: "Monthly", description: "", revision: row.revision, status: "active" }))],
    campaignSaveLayout: call => {
      const layoutId = /layoutId: "([^"]+)"/.exec(call)![1]!;
      const expected = /expectedRevision: "([^"]*)"/.exec(call)![1]!;
      const content = call.replace(/expectedRevision: "[^"]*"/, "");
      const row = stored.get(layoutId);
      if (row && row.content === content) return [{ layoutId, revision: row.revision, status: "active", saved: true }];
      if (row && row.revision !== expected) throw new Error("this layout changed; compare the latest revision before saving");
      if (!row && expected !== "") throw new Error("That layout isn’t available. Choose another layout or save a new one.");
      const revision = `r${++revisions}`;
      stored.set(layoutId, { revision, content });
      if (lost > 0) { lost--; throw new Error("connection lost"); }
      return [{ layoutId, revision, status: "active", saved: true }];
    },
  };
  return { stored, answers };
}

test("a lost creation, then an edit, updates the layout the creation made", async () => {
  // A template opened as a file has no Library file to find its layout by, so the id kept for it is what finds it again.
  const cluster = layoutCluster(1);
  const f = ports(cluster.answers);
  const publisher = new EmailPublisher(f.ports);
  const ask = { target: template(), organization: CLIENT, email: welcome, name: "Monthly", key: "k1" };

  await assert.rejects(publisher.saveLayout(ask), /connection lost/);
  assert.deepEqual(names(f.calls), ["campaignSaveLayout"], "a first save makes one call: there is nothing yet to look for");
  assert.equal(cluster.stored.size, 1, "the creation reached the cluster, and its answer was lost");
  const kept = String(f.state.get("memql.emailLayoutPending:k1"));
  assert.match(kept, UUID);

  // The person changes the email and saves it as a layout again.
  const edited = readEmail(layout([
    { id: "headline", type: "heading", text: "Welcome back" },
    { id: "cta", type: "button", label: "Get started", href: "https://example.com/start" },
  ]));
  const result = await publisher.saveLayout({ ...ask, email: edited });
  assert.deepEqual(names(f.calls), ["campaignSaveLayout", "emailLayouts", "campaignSaveLayout"]);
  const update = f.calls[2]!.call;
  assert.match(update, new RegExp(`layoutId: "${kept}"`), "the layout the creation made");
  assert.match(update, /expectedRevision: "r1"/, "at the revision it was listed at");
  assert.ok(update.includes("Welcome back"), "with the edited email");
  assert.deepEqual(result, { layoutId: kept, revision: "r2" });
  assert.equal(cluster.stored.size, 1, "one layout, not two");
  assert.ok(f.calls.every(call => !call.call.includes("colleague")), "a layout that is not this email's is never touched");
  assert.equal(f.state.has("memql.emailLayoutPending:k1"), false, "cleared once the cluster has answered");
  assert.deepEqual(f.writes, [{ key: "memql.emailLayoutPending:k1", value: kept }, { key: "memql.emailLayoutPending:k1", value: undefined }], "kept once, before the first request, and cleared when the cluster answered");

  // The same without an edit finds the layout all the same, and updates it rather than making another.
  const same = layoutCluster(1);
  const g = ports(same.answers);
  await assert.rejects(new EmailPublisher(g.ports).saveLayout(ask), /connection lost/);
  const again = await new EmailPublisher(g.ports).saveLayout(ask);
  assert.equal(again.layoutId, String(Array.from(same.stored.keys())[0]));
  assert.equal(same.stored.size, 1);
});

test("a creation that never reached the cluster is retried under the same id", async () => {
  let reached = false;
  const f = ports({
    emailLayouts: () => [...BUILTINS, UNRELATED],
    campaignSaveLayout: call => {
      if (!reached) { reached = true; throw new Error("connection lost"); }
      return [{ layoutId: /layoutId: "([^"]+)"/.exec(call)![1], revision: "r1", status: "active", saved: true }];
    },
  });
  const publisher = new EmailPublisher(f.ports);
  const ask = { target: template(), organization: CLIENT, email: welcome, name: "Monthly", key: "k1" };
  await assert.rejects(publisher.saveLayout(ask), /connection lost/);
  const kept = String(f.state.get("memql.emailLayoutPending:k1"));
  const result = await publisher.saveLayout(ask);
  assert.deepEqual(names(f.calls), ["campaignSaveLayout", "emailLayouts", "campaignSaveLayout"], "the kept id is looked for, and nothing is there");
  assert.equal(result.layoutId, kept);
  assert.match(f.calls[2]!.call, new RegExp(`layoutId: "${kept}"`));
  assert.match(f.calls[2]!.call, /expectedRevision: ""/, "so it creates, and under the id it already tried");
  assert.ok(!f.calls[2]!.call.includes("colleague"), "and does not take another email's layout for its own");
  assert.equal(f.state.size, 0);
  assert.deepEqual(f.writes, [{ key: "memql.emailLayoutPending:k1", value: kept }, { key: "memql.emailLayoutPending:k1", value: undefined }], "the id is kept once: the retry reuses it without writing it again");

  // Whatever is kept that is not an id is not used: the cluster would refuse it, and there is nothing to look for.
  for (const notAnId of [{ not: "an id" }, "not an id", "", 7]) {
    const h = ports({ emailLayouts: () => BUILTINS, campaignSaveLayout: call => [{ layoutId: /layoutId: "([^"]+)"/.exec(call)![1], revision: "r1", status: "active", saved: true }] });
    h.state.set("memql.emailLayoutPending:k1", notAnId);
    const fresh = await new EmailPublisher(h.ports).saveLayout(ask);
    assert.match(fresh.layoutId, UUID, JSON.stringify(notAnId));
    assert.deepEqual(names(h.calls), ["campaignSaveLayout"], JSON.stringify(notAnId));
    assert.equal(h.state.size, 0);
  }
});

test("a layout saved from a template or a local file is always a new layout", async () => {
  for (const target of [template(), local]) {
    const f = ports({ campaignSaveLayout: call => [{ layoutId: /layoutId: "([^"]+)"/.exec(call)![1], revision: "r1", status: "active", saved: true }] });
    const result = await new EmailPublisher(f.ports).saveLayout({ target, organization: CLIENT, email: welcome, name: "Monthly", key: "k" });
    assert.deepEqual(names(f.calls), ["campaignSaveLayout"], "there is no file to look a layout up by");
    assert.match(result.layoutId, UUID);
    assert.match(f.calls[0]!.call, /expectedRevision: ""/);
    assert.doesNotMatch(f.calls[0]!.call, /sourceArtifactId/);
    assert.equal(f.state.size, 0);
  }
});

test("saving a layout twice at once makes one layout", async () => {
  const f = ports({
    emailLayouts: () => BUILTINS,
    campaignSaveLayout: call => [{ layoutId: /layoutId: "([^"]+)"/.exec(call)![1], revision: "r1", status: "active", saved: true }],
  });
  const publisher = new EmailPublisher(f.ports);
  const ask = { target: file(), organization: CLIENT, email: welcome, name: "Monthly", key: "k1" };
  const [first, second] = await Promise.all([publisher.saveLayout(ask), publisher.saveLayout(ask)]);
  assert.deepEqual(second, first);
  assert.equal(f.calls.filter(call => call.name === "campaignSaveLayout").length, 1);
  // Another email is another layout.
  const [third, fourth] = await Promise.all([publisher.saveLayout({ ...ask, key: "k2" }), publisher.saveLayout({ ...ask, key: "k3" })]);
  assert.notEqual(third.layoutId, fourth.layoutId);
  assert.equal(f.calls.filter(call => call.name === "campaignSaveLayout").length, 3);
});

test("an email written in HTML cannot be a layout", async () => {
  const f = ports();
  await assert.rejects(new EmailPublisher(f.ports).saveLayout({ target: file(), organization: CLIENT, email: written, name: "Monthly", key: "k1" }),
    (error: unknown) => error instanceof UserInputError && error.message === "An email written in HTML can’t be a layout.");
  assert.deepEqual(f.calls, [], "nothing is asked of the cluster, and no id is kept");
  assert.equal(f.state.size, 0);
});

test("a layout name is one line of up to 200 characters", async () => {
  const sentence = "Enter a layout name up to 200 characters.";
  const f = ports({
    emailLayouts: () => BUILTINS,
    campaignSaveLayout: call => [{ layoutId: /layoutId: "([^"]+)"/.exec(call)![1], revision: "r1", status: "active", saved: true }],
  });
  const publisher = new EmailPublisher(f.ports);
  const save = (name: string) => publisher.saveLayout({ target: file(), organization: CLIENT, email: welcome, name, key: `k${name.length}` });
  for (const name of ["", "   ", "x".repeat(201), "Monthly\nletter", "Monthly\u0007", "Monthly\u007f", "Monthly\u0085", "😀".repeat(201)]) {
    await assert.rejects(save(name), (error: unknown) => error instanceof UserInputError && error.message === sentence, JSON.stringify(name));
  }
  assert.equal(f.calls.length, 0, "a name that cannot be saved is not sent");
  await save("x".repeat(200));
  await save("😀".repeat(200));
  await save("  Monthly letter  ");
  const sent = f.calls.filter(call => call.name === "campaignSaveLayout").map(call => /name: ("(?:[^"\\]|\\.)*")/.exec(call.call)![1]);
  assert.deepEqual(sent.map(name => (JSON.parse(name!) as string).length), [200, 400, "Monthly letter".length], "characters are counted as the cluster counts them, and the name is trimmed");
});

test("a layout that does not answer as saved is not saved", async () => {
  for (const answer of [[], [{}], [{ saved: false, layoutId: "l1", revision: "r1" }], [{ saved: true, revision: "r1" }], [{ saved: true, layoutId: "l1" }]]) {
    const f = ports({ emailLayouts: () => BUILTINS, campaignSaveLayout: () => answer });
    await assert.rejects(new EmailPublisher(f.ports).saveLayout({ target: file(), organization: CLIENT, email: welcome, name: "Monthly", key: "k1" }),
      (error: unknown) => error instanceof Error && !(error instanceof UserInputError) && /did not confirm/.test(error.message), JSON.stringify(answer));
    assert.match(String(f.state.get("memql.emailLayoutPending:k1")), UUID, "the id stays, so the retry recovers the layout");
  }
  const f = ports({ emailLayouts: () => [{ id: "org:l1", layoutId: "l1", sourceArtifactId: "a1", name: "Monthly", kind: "organization" }] });
  await assert.rejects(new EmailPublisher(f.ports).saveLayout({ target: file(), organization: CLIENT, email: welcome, name: "Monthly", key: "k1" }), /did not return the layout revision/);
  assert.deepEqual(names(f.calls), ["emailLayouts"]);
});
