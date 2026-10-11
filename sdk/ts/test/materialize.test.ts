// The Materializer front door: the one client entry point for creating a file
// (docs/superpowers/specs/2026-10-11-materializer-front-door-design.md, section 2).
//
// Every test runs against a fake `execute`, the way the OS and the editors
// will supply it, and asserts on the CALL STRINGS it receives. The properties
// worth pinning are the ones a green happy path hides:
//
//   * the hand-off request ids must satisfy the Library's own rules
//     (integrations/library/review.go reviewRequestID, ^[a-zA-Z0-9_-]{8,120}$),
//     or every real hand-off is refused while every fake-backed test passes;
//   * a follow must not wait forever on a run that has died, and must not
//     leave an abort listener behind per poll;
//   * the hand-off never throws, because the file exists and is good.

import test from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { getEventListeners } from "node:events";

import {
  followComposition,
  handOffDescription,
  isDescribedMarkdown,
  materialize,
  startMaterialize,
  MaterializeCancelled,
  MaterializeFailed,
  type MaterializeExecute,
  type MaterializeRequest,
} from "../src/client/materialize.js";
import {
  buildComposeMaterialize,
  buildLibraryAddDocumentComment,
  buildLibraryRequestDocumentRevision,
} from "../src/client/generated_builtins.js";
import * as client from "../src/client/index.js";

type Rows = Record<string, unknown>[];
// A queue answers call after call (the last entry repeats); a function answers
// from the call string, so a test need not depend on the order calls are made.
type Answer = Rows[] | ((call: string) => Rows);

function fake(script: Record<string, Answer>, limit = 300) {
  const calls: { name: string; call: string }[] = [];
  const execute: MaterializeExecute = async (name, call) => {
    calls.push({ name, call });
    // A loop that never ends is the failure several tests exist to catch; end
    // it with a message instead of hanging the run.
    if (calls.length > limit) throw new Error(`more than ${limit} calls: the code under test polls without end`);
    const answer = script[name];
    if (answer === undefined) return [];
    if (typeof answer === "function") return answer(call);
    return answer.length > 1 ? (answer.shift() ?? []) : (answer[0] ?? []);
  };
  return { calls, execute, named: (name: string) => calls.filter(c => c.name === name) };
}

const fileIdIn = (call: string) => /fileId:\s*"([^"]*)"/.exec(call)?.[1] ?? "";
const requestIdIn = (call: string) => /requestId:\s*"([^"]*)"/.exec(call)?.[1] ?? "";

// What the Library accepts as a request id (integrations/library/review.go).
const LIBRARY_REQUEST_ID = /^[a-zA-Z0-9_-]{8,120}$/;

// ---------------------------------------------------------------------------
// Starting and following a composition
// ---------------------------------------------------------------------------

test("materialize starts once, follows to ready, and resolves the artifact", async () => {
  const f = fake({
    composeMaterialize: [[{ compositionId: "c1" }]],
    compositionById: [[{ status: "composing" }], [{ status: "ready", outputFileId: "f1", name: "Notes" }]],
    libraryArtifactForFile: [[], [{ id: "a1", title: "Notes.md" }]],
  });
  const seen: string[] = [];
  const out = await materialize({ execute: f.execute, onStatus: s => seen.push(s), intervalMs: 1 }, { name: "Notes", format: "markdown", requestId: "r1" });
  assert.deepEqual(out, { compositionId: "c1", fileId: "f1", artifactId: "a1", name: "Notes.md" });
  assert.equal(f.named("composeMaterialize").length, 1);
  assert.match(f.calls[0]?.call ?? "", /requestId:\s*"r1"/);
  assert.ok(seen.includes("composing") && seen.includes("Saved; waiting for its Files entry"));
});

test("a failed composition throws its reason", async () => {
  const f = fake({ compositionById: [[{ status: "failed", failureReason: "No access to notes.md" }]] });
  await assert.rejects(followComposition({ execute: f.execute, intervalMs: 1 }, "c1"), (e: Error) => e instanceof MaterializeFailed && e.message === "No access to notes.md");
});

test("a failed composition with no reason still says something", async () => {
  const f = fake({ compositionById: [[{ status: "failed" }]] });
  await assert.rejects(followComposition({ execute: f.execute, intervalMs: 1 }, "c1"), (e: Error) => e instanceof MaterializeFailed && e.message === "Materializer could not finish this file.");
});

test("a composition cancelled elsewhere ends the follow without cancelling it again", async () => {
  const f = fake({ compositionById: [[{ status: "cancelled" }]] });
  await assert.rejects(followComposition({ execute: f.execute, intervalMs: 1 }, "c1"), MaterializeCancelled);
  assert.equal(f.named("composeCancel").length, 0);
});

test("a composition that can no longer be read is an explicit failure", async () => {
  const f = fake({ compositionById: [[]] });
  await assert.rejects(followComposition({ execute: f.execute, intervalMs: 1 }, "c1"),
    (e: Error) => e instanceof MaterializeFailed && /no longer readable/.test(e.message));
});

test("no compositionId in the reply is an explicit error", async () => {
  const f = fake({ composeMaterialize: [[{}]] });
  await assert.rejects(materialize({ execute: f.execute, intervalMs: 1 }, { name: "x", format: "txt", requestId: "r" }), /did not confirm a composition/);
});

test("followComposition resolves a ready composition and never hands anything off", async () => {
  // The composition keeps the person's statement, but handing it to the
  // Library is the caller's business: the OS follows its own live feed and
  // calls handOffDescription itself.
  const f = fake({
    compositionById: [[{ status: "ready", outputFileId: "f1", name: "Brief", statement: "A one page brief" }]],
    libraryArtifactForFile: [[{ id: "a1", title: "Brief.md" }]],
  });
  const out = await followComposition({ execute: f.execute, intervalMs: 1 }, "c1");
  assert.deepEqual(out, { compositionId: "c1", fileId: "f1", artifactId: "a1", name: "Brief.md" });
  assert.deepEqual(f.calls.map(c => c.name), ["compositionById", "libraryArtifactForFile"]);
});

// ---------------------------------------------------------------------------
// Stopping
// ---------------------------------------------------------------------------

test("aborting cancels the composition once and stops polling", async () => {
  const controller = new AbortController();
  const f = fake({ compositionById: [[{ status: "ready", outputFileId: "f1" }]], libraryArtifactForFile: [[]] });
  const run = followComposition({ execute: f.execute, signal: controller.signal, intervalMs: 5 }, "c1");
  setTimeout(() => controller.abort(), 12);
  await assert.rejects(run, (e: Error) => e instanceof MaterializeCancelled);
  const before = f.calls.length;
  await new Promise(r => setTimeout(r, 20));
  assert.equal(f.calls.filter(c => c.name === "composeCancel").length, 1);
  assert.equal(f.calls.length, before, "polling continued after abort");
});

test("an already stopped signal cancels the composition without reading it", async () => {
  const controller = new AbortController();
  controller.abort();
  const f = fake({});
  await assert.rejects(followComposition({ execute: f.execute, signal: controller.signal, intervalMs: 1 }, "c1"), MaterializeCancelled);
  assert.deepEqual(f.calls.map(c => c.name), ["composeCancel"]);
  assert.match(f.calls[0]?.call ?? "", /compositionId:\s*"c1"/);
  assert.match(f.calls[0]?.call ?? "", /reason:\s*"Stopped by the person"/);
});

test("stopping before the request is sent starts nothing", async () => {
  const controller = new AbortController();
  controller.abort();
  const f = fake({ composeMaterialize: [[{ compositionId: "c1" }]] });
  await assert.rejects(materialize({ execute: f.execute, signal: controller.signal, intervalMs: 1 }, { name: "Notes", format: "markdown", requestId: "req-0001" }), MaterializeCancelled);
  assert.deepEqual(f.calls, []);
});

test("a long follow leaves no abort listener behind: each lives only as long as its wait", async () => {
  const controller = new AbortController();
  const polls = 25;
  const queue: Rows[] = Array.from({ length: polls }, () => [{ status: "composing" }]);
  queue.push([{ status: "ready", outputFileId: "f1" }]);
  const base = fake({ compositionById: queue, libraryArtifactForFile: [[{ id: "a1", title: "Long.md" }]] });
  const heldAtCall: number[] = [];
  const execute: MaterializeExecute = (name, call) => {
    heldAtCall.push(getEventListeners(controller.signal, "abort").length);
    return base.execute(name, call);
  };
  const out = await followComposition({ execute, signal: controller.signal, intervalMs: 1 }, "c1");
  assert.equal(out.artifactId, "a1");
  assert.ok(heldAtCall.length > polls, "the follow was too short to prove anything");
  assert.equal(Math.max(...heldAtCall), 0, "a listener outlived the wait that added it");
  assert.equal(getEventListeners(controller.signal, "abort").length, 0);
});

// ---------------------------------------------------------------------------
// A run that has stopped must not leave the follow waiting
// ---------------------------------------------------------------------------

function unfinished(extra: Record<string, unknown> = {}): Rows[] {
  return [[{ status: "composing", runId: "run1", ...extra }]];
}

test("a run that stopped before the file was finished fails the follow with the run's message", async () => {
  const f = fake({
    compositionById: unfinished(),
    workRunForOwner: [[{ id: "run1", status: "abandoned", errorMessage: "The cluster lost the node making this file." }]],
  });
  await assert.rejects(followComposition({ execute: f.execute, intervalMs: 1 }, "c1"),
    (e: Error) => e instanceof MaterializeFailed && e.message === "The cluster lost the node making this file.");
  assert.match(f.named("workRunForOwner")[0]?.call ?? "", /runId:\s*"run1"/);
});

for (const status of ["failed", "cancelled", "abandoned"]) {
  test(`a run in state "${status}" with no message still fails the follow, in plain words`, async () => {
    const f = fake({ compositionById: unfinished(), workRunForOwner: [[{ id: "run1", status }]] });
    await assert.rejects(followComposition({ execute: f.execute, intervalMs: 1 }, "c1"),
      (e: Error) => e instanceof MaterializeFailed && e.message === "The run stopped before the file was finished.");
  });
}

test("a run that is still working does not stop the follow", async () => {
  const f = fake({
    compositionById: [...unfinished(), ...unfinished(), [{ status: "ready", outputFileId: "f1", runId: "run1" }]],
    workRunForOwner: [[{ id: "run1", status: "running" }]],
    libraryArtifactForFile: [[{ id: "a1", title: "Slow.md" }]],
  });
  const out = await followComposition({ execute: f.execute, intervalMs: 1 }, "c1");
  assert.equal(out.artifactId, "a1");
  assert.equal(f.named("workRunForOwner").length, 2, "the run is read while the composition is unfinished, not after it is ready");
});

test("a ready composition keeps its file even when its run is recorded as stopped", async () => {
  const f = fake({
    compositionById: [[{ status: "ready", outputFileId: "f1", runId: "run1" }]],
    workRunForOwner: [[{ id: "run1", status: "failed", errorMessage: "late failure" }]],
    libraryArtifactForFile: [[{ id: "a1", title: "Kept.md" }]],
  });
  const out = await followComposition({ execute: f.execute, intervalMs: 1 }, "c1");
  assert.equal(out.fileId, "f1");
  assert.equal(f.named("workRunForOwner").length, 0);
});

test("only the run that carries this composition can explain its stop", async () => {
  const f = fake({
    compositionById: [...unfinished(), [{ status: "ready", outputFileId: "f1", runId: "run1" }]],
    workRunForOwner: [[{ id: "some-other-run", status: "failed", errorMessage: "not ours" }]],
    libraryArtifactForFile: [[{ id: "a1", title: "Ours.md" }]],
  });
  const out = await followComposition({ execute: f.execute, intervalMs: 1 }, "c1");
  assert.equal(out.artifactId, "a1");
});

test("a run that cannot be read does not stop the follow", async () => {
  // The run read is a safety net. If it is unavailable the composition's own
  // status still decides, as it did before the net existed.
  const f = fake({
    compositionById: [...unfinished(), [{ status: "ready", outputFileId: "f1", runId: "run1" }]],
    workRunForOwner: () => { throw new Error("not permitted"); },
    libraryArtifactForFile: [[{ id: "a1", title: "Quiet.md" }]],
  });
  const out = await followComposition({ execute: f.execute, intervalMs: 1 }, "c1");
  assert.equal(out.artifactId, "a1");
});

// ---------------------------------------------------------------------------
// What startMaterialize sends
// ---------------------------------------------------------------------------

test("startMaterialize sends only what the person set", async () => {
  const f = fake({ composeMaterialize: [[{ compositionId: "c1" }]] });
  const id = await startMaterialize(f.execute, {
    name: "Plan", format: "docx", requestId: "req-0001", statement: "   ", sources: [], accountIds: [], templateId: "", folderId: "fold1",
  });
  assert.equal(id, "c1");
  assert.equal(f.calls[0]?.call, buildComposeMaterialize({ name: "Plan", format: "docx", requestId: "req-0001", folderId: "fold1" }));
});

test("startMaterialize carries every field the request declares", async () => {
  const f = fake({ composeMaterialize: [[{ compositionId: "c1" }]] });
  await startMaterialize(f.execute, {
    name: "Welcome", format: "json", requestId: "req-0002", statement: "A welcome email", outputKind: "email_template",
    sources: [{ kind: "library_file", ref: "f9", label: "Brand notes", content: true }], templateId: "t1", folderId: "fold1",
    accountIds: ["acct1"], recipeId: "rc1", deployableKind: "spa",
  });
  assert.equal(f.calls[0]?.call, buildComposeMaterialize({
    name: "Welcome", format: "json", requestId: "req-0002", statement: "A welcome email", outputKind: "email_template",
    sources: [{ kind: "library_file", ref: "f9", label: "Brand notes", content: true }], templateId: "t1", folderId: "fold1",
    accountIds: ["acct1"], recipeId: "rc1", deployableKind: "spa",
  }));
});

test("startMaterialize never forwards a field the request type does not declare", async () => {
  // A draft or section keys would change what the server treats the request
  // as, and isDescribedMarkdown could no longer say what the server will do.
  const f = fake({ composeMaterialize: [[{ compositionId: "c1" }]] });
  const sneaky = { name: "x", format: "markdown", requestId: "req-0003", draft: "text", sectionKeys: ["a"], ceilings: { maxModelCalls: 99 } } as MaterializeRequest;
  await startMaterialize(f.execute, sneaky);
  assert.equal(f.calls[0]?.call, buildComposeMaterialize({ name: "x", format: "markdown", requestId: "req-0003" }));
});

test("a request with no request id is refused before anything is sent", async () => {
  // Without an id a double click makes two files; that is what the id is for.
  const f = fake({ composeMaterialize: [[{ compositionId: "c1" }]] });
  await assert.rejects(startMaterialize(f.execute, { name: "x", format: "txt", requestId: "  " }), /request id/);
  assert.deepEqual(f.calls, []);
});

// ---------------------------------------------------------------------------
// handOffDescription: the person's client gives the description to the Library
// ---------------------------------------------------------------------------

const REVISION_REPLY = { runId: "run9", goalId: "goal9", requestId: "ignored", proposal: { artifactId: "a1" } };

function handOffScript(extra: Record<string, Answer> = {}): Record<string, Answer> {
  return {
    libraryAddDocumentComment: [[{ saved: true, commentId: "cm1" }]],
    libraryRequestDocumentRevision: [[REVISION_REPLY]],
    ...extra,
  };
}

test("handOffDescription comments on the whole document, then asks the Library to write it", async () => {
  const f = fake(handOffScript());
  const out = await handOffDescription(f.execute, { artifactId: "a1", statement: "A one page brief for the launch", requestId: "req-0001" });
  assert.deepEqual(out, { revisionId: "req-0001-revision" });
  assert.deepEqual(f.calls.map(c => c.name), ["libraryAddDocumentComment", "libraryRequestDocumentRevision"]);
  assert.equal(f.calls[0]?.call, buildLibraryAddDocumentComment({
    artifactId: "a1", expectedVersion: 1, expectedRevision: "file:1",
    anchor: { kind: "document", quote: "Entire document" },
    body: "A one page brief for the launch", requestId: "req-0001-comment", attachments: [],
  }));
  assert.equal(f.calls[1]?.call, buildLibraryRequestDocumentRevision({
    artifactId: "a1", expectedVersion: 1, expectedRevision: "file:1",
    commentIds: ["cm1"], instruction: "", requestId: "req-0001-revision",
  }));
});

test("the request ids it derives are ones the Library accepts", async () => {
  // integrations/library/review.go refuses any id outside [a-zA-Z0-9_-]{8,120}.
  // A separator such as ":" would make every real hand-off fail.
  const f = fake(handOffScript());
  const requestId = randomUUID();
  const out = await handOffDescription(f.execute, { artifactId: "a1", statement: "Describe it", requestId });
  assert.equal(out.revisionFailure, undefined);
  const sent = f.calls.map(c => requestIdIn(c.call));
  assert.equal(sent.length, 2);
  for (const id of sent) assert.match(id, LIBRARY_REQUEST_ID);
  assert.equal(out.revisionId, sent[1]);
  assert.notEqual(sent[0], sent[1]);
});

test("a retried hand-off reuses its request ids, so the Library returns the first receipts", async () => {
  const f = fake(handOffScript());
  const request = { artifactId: "a1", statement: "A one page brief", requestId: "req-0001" };
  const first = await handOffDescription(f.execute, request);
  const second = await handOffDescription(f.execute, request);
  assert.deepEqual(second, first);
  assert.deepEqual(f.calls.map(c => requestIdIn(c.call)), ["req-0001-comment", "req-0001-revision", "req-0001-comment", "req-0001-revision"]);
});

test("handOffDescription attaches each library file source once, at the version the Library holds", async () => {
  const artifacts: Record<string, Rows> = {
    "f-notes": [{ id: "att-notes" }],
    "f-logo": [{ id: "att-logo" }],
    "f-nofile": [{ id: "att-nofile" }],
    "f-archived": [{ id: "att-archived", archived: true }],
  };
  const files: Record<string, Rows> = {
    "f-notes": [{ name: "notes.md", versionNumber: 3 }],
    "f-logo": [{ name: "logo.png" }],
    "f-archived": [{ name: "old.md", versionNumber: 2 }],
  };
  const f = fake(handOffScript({
    libraryArtifactForFile: call => artifacts[fileIdIn(call)] ?? [],
    libraryFileById: call => files[fileIdIn(call)] ?? [],
  }));
  const out = await handOffDescription(f.execute, {
    artifactId: "a1", statement: "Summarise these", requestId: "req-0002",
    sources: [
      { kind: "library_file", ref: "f-notes", label: "Notes" },
      { kind: "concept_row", ref: "v1:crm:contact#c1", label: "A contact" },
      { kind: "query", ref: "contacts()", label: "All contacts" },
      { kind: "library_file", ref: "f-logo", label: "Logo" },
      { kind: "library_file", ref: "f-notes", label: "Notes, listed twice" },
      { kind: "library_file", ref: "f-gone", label: "Nothing resolves" },
      { kind: "library_file", ref: "f-nofile", label: "No file row" },
      { kind: "library_file", ref: "f-archived", label: "Archived" },
    ],
  });
  assert.deepEqual(out, { revisionId: "req-0002-revision" });
  assert.equal(f.named("libraryAddDocumentComment")[0]?.call, buildLibraryAddDocumentComment({
    artifactId: "a1", expectedVersion: 1, expectedRevision: "file:1",
    anchor: { kind: "document", quote: "Entire document" }, body: "Summarise these", requestId: "req-0002-comment",
    attachments: [
      { artifactId: "att-notes", version: 3, revision: "file:3" },
      { artifactId: "att-logo", version: 1, revision: "file:1" },
    ],
  }));
  // Sources that are not files are not attachments and are never looked up.
  assert.equal(f.calls.some(c => /contact/.test(c.call)), false, "a source that is not a file was looked up");
});

const FAILURE_PREFIX = "The description could not be sent for writing: ";

const failures: { label: string; script: Record<string, Answer>; request?: Partial<Parameters<typeof handOffDescription>[1]>; message: RegExp; calls: string[] }[] = [
  {
    label: "the Library refuses the comment",
    script: handOffScript({ libraryAddDocumentComment: () => { throw new Error("the document changed; reopen its current revision before adding feedback"); } }),
    message: /^the document changed; reopen its current revision/,
    calls: ["libraryAddDocumentComment"],
  },
  {
    label: "the comment is not confirmed",
    script: handOffScript({ libraryAddDocumentComment: [[{ saved: false }]] }),
    message: /did not confirm the description/,
    calls: ["libraryAddDocumentComment"],
  },
  {
    label: "the Library cannot start the revision",
    script: handOffScript({ libraryRequestDocumentRevision: () => { throw new Error("document review jobs are unavailable on this node"); } }),
    message: /^document review jobs are unavailable/,
    calls: ["libraryAddDocumentComment", "libraryRequestDocumentRevision"],
  },
  {
    label: "the revision is not confirmed",
    script: handOffScript({ libraryRequestDocumentRevision: [[{ requestId: "req-0003-revision" }]] }),
    message: /did not confirm the writing request/,
    calls: ["libraryAddDocumentComment", "libraryRequestDocumentRevision"],
  },
  {
    label: "the request id is not one the Library accepts",
    script: handOffScript(),
    request: { requestId: "campaign:42" },
    message: /request ids .* letters, digits/,
    calls: [],
  },
  {
    label: "there is nothing to send",
    script: handOffScript(),
    request: { statement: "   " },
    message: /no description to send/,
    calls: [],
  },
];

for (const { label, script, request, message, calls } of failures) {
  test(`handOffDescription reports a failure instead of throwing: ${label}`, async () => {
    const f = fake(script);
    const out = await handOffDescription(f.execute, { artifactId: "a1", statement: "A one page brief", requestId: "req-0003", ...request });
    assert.equal(out.revisionId, undefined);
    const failure = out.revisionFailure ?? "";
    assert.ok(failure.startsWith(FAILURE_PREFIX), `got ${JSON.stringify(out)}`);
    assert.match(failure.slice(FAILURE_PREFIX.length), message);
    assert.equal("revisionId" in out, false);
    assert.deepEqual(f.calls.map(c => c.name), calls);
  });
}

// ---------------------------------------------------------------------------
// materialize: the hand-off for a described Markdown request
// ---------------------------------------------------------------------------

function describedScript(extra: Record<string, Answer> = {}): Record<string, Answer> {
  return handOffScript({
    composeMaterialize: [[{ compositionId: "c1" }]],
    compositionById: [[{ status: "ready", outputFileId: "f1", name: "Brief" }]],
    libraryArtifactForFile: call => {
      const id = fileIdIn(call);
      return id === "f1" ? [{ id: "a1", title: "Brief.md" }] : id === "f-notes" ? [{ id: "att-notes" }] : [];
    },
    libraryFileById: call => fileIdIn(call) === "f-notes" ? [{ name: "notes.md", versionNumber: 2 }] : [],
    ...extra,
  });
}

const described: MaterializeRequest = {
  name: "Brief", format: "markdown", requestId: "req-0004", statement: "A one page brief for the launch",
  sources: [{ kind: "library_file", ref: "f-notes", label: "Notes" }],
};

test("materialize on a described Markdown request returns the revision once the file is in Files", async () => {
  const f = fake(describedScript());
  const out = await materialize({ execute: f.execute, intervalMs: 1 }, described);
  assert.deepEqual(out, { compositionId: "c1", fileId: "f1", artifactId: "a1", name: "Brief.md", revisionId: "req-0004-revision" });
  const names = f.calls.map(c => c.name);
  assert.deepEqual(names.slice(0, 3), ["composeMaterialize", "compositionById", "libraryArtifactForFile"]);
  assert.deepEqual(names.slice(-2), ["libraryAddDocumentComment", "libraryRequestDocumentRevision"]);
  assert.equal(f.named("libraryAddDocumentComment")[0]?.call, buildLibraryAddDocumentComment({
    artifactId: "a1", expectedVersion: 1, expectedRevision: "file:1",
    anchor: { kind: "document", quote: "Entire document" }, body: "A one page brief for the launch", requestId: "req-0004-comment",
    attachments: [{ artifactId: "att-notes", version: 2, revision: "file:2" }],
  }));
  // The request the server sees is the person's own words, filed blank there.
  assert.match(f.calls[0]?.call ?? "", /statement:\s*"A one page brief for the launch"/);
});

test("materialize waits for the Files entry before it hands the description over", async () => {
  const f = fake(describedScript({
    libraryArtifactForFile: [[], [{ id: "a1", title: "Brief.md" }]],
    libraryFileById: [[]],
  }));
  const out = await materialize({ execute: f.execute, intervalMs: 1 }, { ...described, sources: [] });
  assert.equal(out.revisionId, "req-0004-revision");
  const names = f.calls.map(c => c.name);
  assert.ok(names.indexOf("libraryAddDocumentComment") > names.lastIndexOf("libraryArtifactForFile"));
});

test("a failed hand-off leaves the file and says why, and materialize still resolves", async () => {
  const f = fake(describedScript({ libraryRequestDocumentRevision: () => { throw new Error("document review jobs are unavailable on this node"); } }));
  const out = await materialize({ execute: f.execute, intervalMs: 1 }, described);
  assert.equal(out.artifactId, "a1");
  assert.equal(out.fileId, "f1");
  assert.equal(out.revisionId, undefined);
  assert.equal(out.revisionFailure, FAILURE_PREFIX + "document review jobs are unavailable on this node");
});

test("stopping after the file exists but before the description is sent sends nothing", async () => {
  const controller = new AbortController();
  const f = fake(describedScript({
    libraryArtifactForFile: call => { controller.abort(); return fileIdIn(call) === "f1" ? [{ id: "a1", title: "Brief.md" }] : []; },
  }));
  await assert.rejects(materialize({ execute: f.execute, signal: controller.signal, intervalMs: 1 }, described), MaterializeCancelled);
  assert.equal(f.named("libraryAddDocumentComment").length, 0);
  assert.equal(f.named("composeCancel").length, 0, "a ready composition is not cancelled");
});

const notDescribed: [string, MaterializeRequest][] = [
  ["a blank Markdown file", { name: "Notes", format: "markdown", requestId: "req-0005" }],
  ["a statement of only spaces", { name: "Notes", format: "markdown", requestId: "req-0005", statement: "   " }],
  ["Markdown through a template", { name: "Notes", format: "markdown", requestId: "req-0005", statement: "Brief", templateId: "t1" }],
  ["Markdown from a recipe", { name: "Notes", format: "markdown", requestId: "req-0005", statement: "Brief", recipeId: "rc1" }],
  ["a described email template", { name: "Notes", format: "markdown", requestId: "req-0005", statement: "Brief", outputKind: "email_template" }],
  ["a package", { name: "Notes", format: "markdown", requestId: "req-0005", statement: "Brief", deployableKind: "spa" }],
  ["a described file that is not Markdown", { name: "Notes", format: "txt", requestId: "req-0005", statement: "Brief" }],
  ["a described email JSON", { name: "Notes", format: "json", requestId: "req-0005", statement: "Brief", outputKind: "email_template" }],
];

for (const [label, request] of notDescribed) {
  test(`materialize makes no Library hand-off call for ${label}`, async () => {
    const f = fake(describedScript());
    const out = await materialize({ execute: f.execute, intervalMs: 1 }, request);
    assert.deepEqual(out, { compositionId: "c1", fileId: "f1", artifactId: "a1", name: "Brief.md" });
    assert.equal(isDescribedMarkdown(request), false);
    for (const name of ["libraryAddDocumentComment", "libraryRequestDocumentRevision", "libraryFileById", "libraryDocumentReview"]) {
      assert.equal(f.named(name).length, 0, `${name} was called`);
    }
  });
}

test("isDescribedMarkdown matches the server's rule for a request filed blank", () => {
  assert.equal(isDescribedMarkdown(described), true);
  assert.equal(isDescribedMarkdown({ format: "markdown", statement: " Write it " }), true);
  assert.equal(isDescribedMarkdown({ format: "markdown", statement: "Write it", templateId: "  " }), true, "a template of only spaces is no template, as on the server");
  assert.equal(isDescribedMarkdown({ format: "html", statement: "Write it" }), false);
});

// ---------------------------------------------------------------------------
// The package surface
// ---------------------------------------------------------------------------

test("the client barrel exports the materialize surface", () => {
  for (const name of ["materialize", "startMaterialize", "followComposition", "handOffDescription", "isDescribedMarkdown"] as const) {
    assert.equal(typeof client[name], "function", `${name} is not exported from the client barrel`);
  }
  assert.equal(client.MaterializeCancelled, MaterializeCancelled);
  assert.equal(client.MaterializeFailed, MaterializeFailed);
});
