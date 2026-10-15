import { test } from "node:test";
import assert from "node:assert/strict";
import { PDFDocument } from "pdf-lib";
import { changePDF } from "../src/pdf.js";
import { Documents, isZip, newEmailResource, resourceFrom } from "../src/documents.js";
import { UserInputError } from "../src/problems.js";
import { readEmail } from "../src/emailDocument.js";
import type { EditorConnectionAPI } from "../../vscode/src/connection/api.js";

test("a failed upload explains recovery without requesting bytes; failed analysis remains readable", async () => {
  const lease = { domain: "client.example", name: "client", generation: 1 };
  const file = { name: "notes.md", status: "failed", blobUrl: "library/person/file/notes.md", versionNumber: 1 };
  let reads = 0;
  const api = { connect: async () => lease, execute: async (_lease: unknown, name: string) =>
    name === "libraryArtifactById" ? [{ kind: "file", sourceConceptRef: "file" }] : [file],
    readBytes: async () => { reads++; return new TextEncoder().encode("# Saved"); },
  } as unknown as EditorConnectionAPI;
  const documents = new Documents(api);
  await assert.rejects(documents.read("memql-file://client.example/artifacts/artifact/notes.md"), error =>
    error instanceof UserInputError && /Upload the original file again/.test(error.message));
  assert.equal(reads, 0);
  file.blobUrl = "https://storage.example/container/library/person/file/notes.md";
  const opened = await documents.read("memql-file://client.example/artifacts/artifact/notes.md");
  assert.equal(new TextDecoder().decode(opened.content), "# Saved");
  assert.equal(reads, 1);
});

test("creating a file resolves its indexed upload receipt without uploading twice", async () => {
  const lease = {domain:"client.example",name:"client",generation:1};
  let uploads = 0, reads = 0;
  const api = { uploadFile: async (actual: unknown) => { assert.equal(actual,lease); uploads++; return {fileId:"saved-file",artifactId:""}; },
    execute: async (actual: unknown, name: string, call: string) => {
      assert.equal(actual,lease); assert.equal(name,"libraryArtifactForFile"); assert.match(call,/saved-file/);
      return ++reads === 1 ? [] : [{id:"indexed-file"}];
    } } as unknown as EditorConnectionAPI;
  const uri = await new Documents(api).createFile(lease,"Figure 1.png","image/png",new Uint8Array([1]));
  assert.equal(uri,"memql-file://client.example/artifacts/indexed-file/Figure%201.png");
  assert.equal(uploads,1); assert.equal(reads,2);
});

test("ZIP detection handles misleading names and never extracts content", () => {
  assert.ok(isZip("backup.ZIP"));
  assert.ok(isZip("renamed.pdf", "application/zip"));
  assert.ok(isZip("renamed.txt", "", new Uint8Array([80, 75, 3, 4])));
  assert.ok(!isZip("document.pdf", "application/pdf", new Uint8Array([37, 80, 68, 70])));
});
test("only single, well-formed MemQL resources can be opened", () => {
  assert.equal(resourceFrom("memql-file://client.example/artifacts/abc/report.pdf").id, "abc");
  for (const value of ["https://client.example/artifacts/abc/report.pdf", "memql-file://user@client.example/artifacts/abc/a", "memql-file://client.example/artifacts/abc/a?token=secret", "memql-file://client.example/artifacts/abc/a%2Fb", "memql-file://client.example/artifacts/a%0A/a"]) assert.throws(() => resourceFrom(value));
});

test("a New email link names an organization and opens no file", async () => {
  const link = "memql-file://client.example/new-email/client-org/New%20email";
  assert.deepEqual(resourceFrom(link), { domain: "client.example", kind: "new-email", id: "client-org", name: "New email" });
  assert.equal(newEmailResource("Client.Example", "client-org"), link);
  for (const bad of [
    "memql-file://client.example/new/email?account=client-org&returnTo=email",
    "memql-file://client.example/new-email/client-org",
    "memql-file://client.example//new-email/client-org/New%20email",
    "memql-file://client.example/new-email/client-org/New%20email/",
    "memql-file://client.example/new-email/%61/New%20email",
    "memql-file://client.example:/new-email/client-org/New%20email",
    "memql-file://client.example/new-email/client-org/Bad%7f",

    "memql-file://client.example/new-email/v1:accounts:account:client-org/New%20email",
    "memql-file://client.example/new-email/client-org/New%20email?returnTo=email",
    "memql-file://client.example/new-email/client-org/a%2Fb",
  ]) assert.throws(() => resourceFrom(bad), bad);
  assert.throws(() => newEmailResource("client.example", "v1:accounts:account:x"));
  const api = { connect: async () => { throw new Error("must not connect"); } } as unknown as EditorConnectionAPI;
  await assert.rejects(new Documents(api).read(link), /opens a page, not a file/);
});

test("the New email builder returns the one canonical link or refuses", () => {
  // clients/os pins this same literal for its own builder; the OS cannot import editors/, so the string is the contract.
  assert.equal(newEmailResource("memql.example.com", "self"), "memql-file://memql.example.com/new-email/self/New%20email");
  const longest = "a".repeat(160);
  assert.equal(resourceFrom(newEmailResource("client.example", longest)).id, longest);
  const refusal = (error: unknown) => error instanceof UserInputError && error.message === "Invalid MemQL file link.";
  // An id is a bare id as written: resourceFrom decodes, so "%61" would read as "a" and "a%2Fb" as a path.
  for (const id of ["", "a".repeat(161), "a b", "a/b", "a?b", "a#b", "%61", "a%2Fb", "é", "v1:accounts:account:x"]) {
    assert.throws(() => newEmailResource("client.example", id), refusal, `id ${JSON.stringify(id)}`);
  }
  // A domain that reshapes the link (a slash, a port, a user, a query, a fragment, nothing at all) is refused, never reshaped.
  for (const domain of ["", "client.example/", "client.example/x", "client.example:8080", "user@client.example", "client.example?x=1", "client.example#x", "client example", "%61.example", "K.example"]) {
    assert.throws(() => newEmailResource(domain, "client-org"), refusal, `domain ${JSON.stringify(domain)}`);
  }
});

test("a New email link has no bytes to read or save", async () => {
  const link = newEmailResource("client.example", "client-org");
  const lease = { domain: "client.example", name: "client", generation: 1 };
  const calls: string[] = [];
  const untouched = (name: string) => async () => { calls.push(name); throw new Error(`${name} must not be called`); };
  const api = { connect: untouched("connect"), execute: untouched("execute"), readBytes: untouched("readBytes"),
    uploadFile: untouched("uploadFile"), uploadVersion: untouched("uploadVersion") } as unknown as EditorConnectionAPI;
  const documents = new Documents(api);
  await assert.rejects(documents.read(link), /opens a page, not a file/);
  await assert.rejects(documents.read(link, lease), /opens a page, not a file/);
  // A kind that would otherwise upload a new version: only the link's kind can be the reason for the refusal.
  const opened = { resource: resourceFrom(link), lease, content: new Uint8Array(), mime: "text/plain", sourceId: "file", kind: "file", version: 1 };
  await assert.rejects(documents.save(opened, new TextEncoder().encode("x")), /opens a page, not a file/);
  assert.deepEqual(calls, []);
  assert.equal(opened.version, 1);
  assert.equal(opened.content.length, 0);
});

test("PDF edits produce readable PDFs and leave the source untouched", async () => {
  const pdf = await PDFDocument.create(); pdf.addPage([400, 600]);
  const original = await pdf.save();
  const rotated = await changePDF(original, { kind: "rotate", page: 0 });
  assert.equal((await PDFDocument.load(rotated)).getPage(0).getRotation().angle, 90);
  assert.equal((await PDFDocument.load(original)).getPage(0).getRotation().angle, 0);
  const annotated = await changePDF(rotated, { kind: "text", page: 0, text: "Review", x: 36, y: 36 });
  assert.equal((await PDFDocument.load(annotated)).getPageCount(), 1);
  await assert.rejects(changePDF(original, { kind: "rotate", page: 2 }), /existing page/);
});
test("a file save uses its opened version and preserves edits on conflict", async () => {
  let requestedVersion = 0;
  const api = { uploadVersion: async (_lease: unknown, _id: unknown, _name: unknown, _mime: unknown, _content: unknown, version: number) => {
    requestedVersion = version; throw new Error("conflict");
  } } as unknown as EditorConnectionAPI;
  const doc = { resource: { domain: "client.example", kind: "artifacts" as const, id: "abc", name: "a.txt" },
    lease: { domain: "client.example", name: "client", generation: 1 }, content: new TextEncoder().encode("old"), mime: "text/plain", sourceId: "file", kind: "file", version: 3 };
  await assert.rejects(new Documents(api).save(doc, new TextEncoder().encode("new")), /conflict/);
  assert.equal(requestedVersion, 3); assert.equal(doc.version, 3);
  assert.equal(new TextDecoder().decode(doc.content), "old");
});

test("campaign template saves use the opened client and exact revision", async () => {
  const calls: {name:string;call:string}[] = [];
  const lease = { domain:"client.example", name:"client", generation:3 };
  let fail = false;
  const api = { connect: async () => lease, execute: async (_lease: unknown,name:string,call:string) => {
    calls.push({name,call});
    if (name === "templateById") return [{ id:"template",name:"Welcome",accountId:"client-org",subject:"Hi",textBody:"Hello",htmlBody:"<p>Hello</p>",status:"ready",createdAt:"2026-10-01T12:00:00Z" }];
    if (fail) throw new Error("template changed");
    return [{ saved:true,revision:"2026-10-01T12:00:01Z" }];
  } } as unknown as EditorConnectionAPI;
  const documents = new Documents(api);
  const opened = await documents.read("memql-file://client.example/templates/template/Welcome.email.json");
  assert.equal(opened.template?.accountId,"client-org");
  const email = JSON.stringify({ subject: "Thanks for subscribing", textBody: "Thanks for joining us.", htmlBody: "<p>Thanks for joining us.</p>" }, null, 2) + "\n";
  await documents.save(opened,new TextEncoder().encode(email));
  assert.equal(opened.revision,"2026-10-01T12:00:01Z");
  assert.equal(opened.template?.status,"draft");
  assert.match(calls[1].call,/accountId:\s*"client-org"/);
  assert.match(calls[1].call,/expectedRevision:\s*"2026-10-01T12:00:00Z"/);
  fail=true;
  await assert.rejects(documents.save(opened,new TextEncoder().encode(email.replace("Thanks for subscribing","Changed"))),/template changed/);
  assert.equal(readEmail(new TextDecoder().decode(opened.content)).subject,"Thanks for subscribing");
  assert.equal(opened.revision,"2026-10-01T12:00:01Z");
});

const emailTheme = { page: "#eef0f3", canvas: "#ffffff", primary: "#1f5f8b", secondary: "#5b8fb9", accent: "#d9534f", tint: "#f6f4ef", text: "#2b2b2b", muted: "#8a8f98", headingFont: "Georgia", bodyFont: "Arial" };
const openTemplate = async (row: Record<string, unknown>) => {
  const lease = { domain: "client.example", name: "client", generation: 3 };
  const api = { connect: async () => lease, execute: async (_lease: unknown, name: string) => name === "templateById" ? [row] : [] } as unknown as EditorConnectionAPI;
  return new Documents(api).read("memql-file://client.example/templates/template/Welcome.email.json");
};

test("a template row that carries a document opens as that document", async () => {
  const document = { subject: "Welcome", preheader: "Soon", layout: "builtin:letter", theme: emailTheme, blocks: [{ id: "headline", type: "heading", text: "Thanks for joining us" }, { id: "cta", type: "button", label: "Go", href: "{{confirmUrl}}" }] };
  const opened = await openTemplate({ id: "template", name: "Welcome", accountId: "client-org", subject: "Stored subject", textBody: "Stored text", htmlBody: "<p>Stored</p>", status: "draft", createdAt: "2026-10-01T12:00:00Z", document });
  const text = new TextDecoder().decode(opened.content);
  assert.deepEqual(JSON.parse(text), document);
  assert.equal(text, JSON.stringify(document, null, 2) + "\n");
  // It is written as the engine writes its files: the two separators JSON.stringify leaves raw are escaped.
  const separated = { ...document, subject: `One${String.fromCharCode(0x2028)}Two${String.fromCharCode(0x2029)}` };
  const written = new TextDecoder().decode((await openTemplate({ id: "template", name: "Welcome", accountId: "client-org", status: "draft", createdAt: "2026-10-01T12:00:00Z", document: separated })).content);
  assert.ok(written.includes('"subject": "One\\u2028Two\\u2029"'), written);
  assert.deepEqual(JSON.parse(written), separated);
  assert.equal(opened.kind, "campaign_template");
  assert.equal(opened.revision, "2026-10-01T12:00:00Z");
  assert.deepEqual(opened.template, { name: "Welcome", accountId: "client-org", status: "draft" });
  assert.equal(opened.accountIds, undefined, "a template's organization is its account, not a label");
  // A document that is not an object is not a document: the row opens as the custom email it also carries.
  for (const document of [null, "text", 7, ["x"]]) {
    const row = { id: "template", name: "Welcome", accountId: "client-org", subject: "Hi", textBody: "Hello", htmlBody: "", status: "draft", createdAt: "2026-10-01T12:00:00Z", document };
    assert.deepEqual(JSON.parse(new TextDecoder().decode((await openTemplate(row)).content)), { subject: "Hi", textBody: "Hello", htmlBody: "" });
  }
});

test("a template row without a document opens as a custom email", async () => {
  const row = { id: "template", name: "Welcome", accountId: "client-org", subject: "Hi", textBody: "Hello", htmlBody: "<p>Hello</p>", status: "ready", createdAt: "2026-10-01T12:00:00Z" };
  const plain = new TextDecoder().decode((await openTemplate(row)).content);
  assert.equal(plain, JSON.stringify({ subject: "Hi", textBody: "Hello", htmlBody: "<p>Hello</p>" }, null, 2) + "\n");
  const separated = new TextDecoder().decode((await openTemplate({ ...row, textBody: `a${String.fromCharCode(0x2028)}b` })).content);
  assert.ok(separated.includes('"textBody": "a\\u2028b"'), "a template without a document is written by the same writer");
  assert.ok(!plain.includes("preheader"), "an empty preheader is left out");
  const withPreheader = new TextDecoder().decode((await openTemplate({ ...row, preheader: "Soon" })).content);
  assert.equal(withPreheader, JSON.stringify({ subject: "Hi", preheader: "Soon", textBody: "Hello", htmlBody: "<p>Hello</p>" }, null, 2) + "\n");
  assert.deepEqual(Object.keys(JSON.parse(withPreheader)), ["subject", "preheader", "textBody", "htmlBody"]);
  assert.ok(!new TextDecoder().decode((await openTemplate({ ...row, preheader: "" })).content).includes("preheader"));
  assert.ok(!new TextDecoder().decode((await openTemplate({ ...row, preheader: 5 })).content).includes("preheader"), "a preheader that is not text is left out");
  const emptyBodies = new TextDecoder().decode((await openTemplate({ id: "template", name: "Welcome", accountId: "client-org", subject: "Hi", createdAt: "2026-10-01T12:00:00Z" })).content);
  assert.deepEqual(JSON.parse(emptyBodies), { subject: "Hi", textBody: "", htmlBody: "" });
});

test("an artifact's organization labels travel with the opened file", async () => {
  const lease = { domain: "client.example", name: "client", generation: 4 };
  let labels: unknown = ["client-org"];
  const api = {
    connect: async () => lease,
    readBytes: async () => new TextEncoder().encode("{}"),
    execute: async (_lease: unknown, name: string) => {
      if (name === "libraryArtifactById") return [{ id: "abc", kind: "file", sourceConceptRef: "file-1", mimeType: "application/json", accountIds: labels }];
      if (name === "libraryFileById") return [{ id: "file-1", name: "Welcome.email.json", mimeType: "application/json", versionNumber: 2 }];
      throw new Error(`unexpected ${name}`);
    },
  } as unknown as EditorConnectionAPI;
  const files = new Documents(api);
  const uri = "memql-file://client.example/artifacts/abc/Welcome.email.json";
  assert.deepEqual((await files.read(uri)).accountIds, ["client-org"]);
  labels = ["client-org", "other-org"];
  assert.deepEqual((await files.read(uri)).accountIds, ["client-org", "other-org"]);
  labels = ["client-org", 7, null, "other-org"];
  assert.deepEqual((await files.read(uri)).accountIds, ["client-org", "other-org"], "only text labels are kept");
  labels = [];
  assert.deepEqual((await files.read(uri)).accountIds, []);
  labels = undefined;
  assert.deepEqual((await files.read(uri)).accountIds, [], "a file with no labels reads as an empty list");
  labels = "client-org";
  assert.deepEqual((await files.read(uri)).accountIds, [], "a label that is not a list is no label");
});

test("revision requests and decisions stay on the document lease and refuse another artifact", async () => {
  const lease = { domain: "client.example", name: "client", generation: 9 };
  const calls: { name: string; call: string }[] = [];
  let wrong = false;
  let cancelRequested = false;
  const api = { execute: async (actual: unknown, name: string, call: string) => {
    assert.equal(actual, lease); calls.push({ name, call });
    if (name === "libraryRequestDocumentRevision") return [{ runId: "run", proposal: { artifactId: "abc" } }];
    if (name === "libraryDocumentRevisionStatus") return [{ status: "waiting", cancelRequested, approvalId: "approval", proposal: { artifactId: wrong ? "another" : "abc" } }];
    return [{ decision: "approved" }];
  } } as unknown as EditorConnectionAPI;
  const files = new Documents(api);
  const doc = { resource: { domain: "client.example", kind: "artifacts" as const, id: "abc", name: "document.md" }, lease,
    content: new TextEncoder().encode("Source"), mime: "text/markdown", sourceId: "source", kind: "file", version: 3, revision: "file:3" };
  await files.requestRevision(doc, ["comment"], "Clarify", "same-request");
  assert.match(calls[0].call, /expectedVersion: 3/);
  assert.match(calls[0].call, /requestId: "same-request"/);
  await files.decideRevision(doc, "same-request", "approval", "approved");
  assert.equal(calls.filter(call => call.name === "decideApproval").length, 1);
  cancelRequested = true;
  await assert.rejects(files.decideRevision(doc, "same-request", "approval", "approved"), /stopped/);
  assert.equal(calls.filter(call => call.name === "decideApproval").length, 1, "a stale button must not try to resume a cancelled job");
  wrong = true;
  await assert.rejects(files.decideRevision(doc, "same-request", "approval", "approved"), /does not belong/);
  assert.equal(calls.filter(call => call.name === "decideApproval").length, 1);
});

// Keep the real generated builder here: mocking removeAnnotation itself hid a
// wire regression that sent an empty builtin call despite the supplied IDs.
test("annotation deletion sends both IDs and requires a confirmed receipt", async () => {
  const lease = { domain: "client.example", name: "client", generation: 9 };
  let result: Record<string, unknown>[] = [{ removed: true }];
  let calls = 0;
  const api = { execute: async (actual: unknown, name: string, call: string) => {
    assert.equal(actual, lease);
    assert.equal(name, "libraryRemoveDocumentAnnotation");
    assert.equal(call, 'builtin libraryRemoveDocumentAnnotation(artifactId: "document-id", commentId: "annotation-id")');
    calls++;
    return result;
  } } as unknown as EditorConnectionAPI;
  const doc = { resource: { domain: "client.example", kind: "artifacts" as const, id: "document-id", name: "document.md" }, lease,
    content: new TextEncoder().encode("Source"), mime: "text/markdown", sourceId: "source", kind: "file", version: 3, revision: "file:3" };
  const files = new Documents(api);
  await files.removeAnnotation(doc, "annotation-id");
  result = [];
  await assert.rejects(files.removeAnnotation(doc, "annotation-id"), /Deletion was not confirmed/);
  result = [{ removed: false }];
  await assert.rejects(files.removeAnnotation(doc, "annotation-id"), /Deletion was not confirmed/);
  assert.equal(calls, 3);
  assert.equal(new TextDecoder().decode(doc.content), "Source");
});
