import { persistSignIn } from "../src/auth/store.js";
import { test } from "node:test";
import assert from "node:assert/strict";
import { EditorConnection, validateEditorDomain, type EditorSession } from "../src/connection/api.js";
import { BrowserRegistry } from "../src/clusters/browserRegistry.js";
import { generateWebPkcePair } from "../src/auth/webPkce.js";
import { createHash } from "node:crypto";

test("a retained file cannot read or write through a switched or reconnected session", async () => {
  let writes = 0;
  const query = { executeNamed: async () => { writes++; return { rows: () => [] }; } } as unknown as EditorSession["query"];
  let session: EditorSession | undefined = { cluster: { name: "a", domain: "a.example", endpoint: "api.a.example:443" }, query, bearer: "private" };
  const api = new EditorConnection({ session: () => session, connect: async () => {} });
  const original = api.current()!;
  session = undefined; api.changed();
  session = { cluster: { name: "a", domain: "a.example", endpoint: "api.a.example:443" }, query, bearer: "another-user" }; api.changed();
  await assert.rejects(api.execute(original, "save", "mutation save()"), /connection changed/);
  assert.equal(writes, 0);
  await api.execute(api.current()!, "read", "query read()");
  assert.equal(writes, 1);
});

test("a read that finishes after a cluster switch cannot populate the next cluster's editor", async () => {
  let finish!: (value: { rows(): Record<string, unknown>[] }) => void;
  const query = { executeNamed: () => new Promise(resolve => { finish = resolve; }) } as unknown as EditorSession["query"];
  let session: EditorSession | undefined = { cluster: { name: "a", domain: "a.example", endpoint: "api.a.example:443" }, query, bearer: "private" };
  const api = new EditorConnection({ session: () => session, connect: async () => {} });
  const reading = api.execute(api.current()!, "read", "query read()");
  session = undefined; api.changed(); finish({ rows: () => [{ content: "client A" }] });
  await assert.rejects(reading, /connection changed/);
});

test("browser registry never stores bearer or refresh credentials in profile state", async () => {
  const profile = new Map<string, unknown>(); const secrets = new Map<string, string>();
  const registry = new BrowserRegistry({ get: <T>(key: string, fallback: T) => (profile.get(key) ?? fallback) as T,
    update: async (key, value) => { profile.set(key, value); } }, {
    get: async key => secrets.get(key), store: async (key, value) => { secrets.set(key, value); }, delete: async key => { secrets.delete(key); },
  });
  await registry.select("client.example");
  await persistSignIn({ secrets: {
    get: async key => secrets.get(key), store: async (key, value) => { secrets.set(key, value); }, delete: async key => { secrets.delete(key); },
  }, writeCluster: registry.write }, "client.example", { accessToken: "bearer-secret", refreshToken: "refresh-secret", expiresAtEpochSeconds: 0, clientId: "memql-vscode" });
  assert.equal((await registry.read("client.example")).token, "bearer-secret");
  assert.doesNotMatch(JSON.stringify([...profile]), /bearer-secret|refresh-secret/);
  await registry.remove("client.example");
  assert.equal((await registry.read("client.example")).token, undefined);
});

test("browser PKCE uses S256 with a fresh secret verifier", async () => {
  const a = await generateWebPkcePair(); const b = await generateWebPkcePair();
  assert.match(a.verifier, /^[A-Za-z0-9_-]{43}$/);
  assert.notEqual(a.verifier, b.verifier);
  assert.equal(a.challenge, createHash("sha256").update(a.verifier).digest("base64url"));
});

test("file links cannot supply an alternate credential destination", () => {
  for (const domain of ["https://evil.test", "a.example:80", "user@a.example", "a.example/path", "a.example?x", "", "a..example"]) {
    assert.throws(() => validateEditorDomain(domain));
  }
  assert.equal(validateEditorDomain("Client.Example."), "client.example");
});

test("reference uploads use the selected cluster and do not return a receipt after switching", async () => {
  let session: EditorSession | undefined = { cluster: { name: "client", domain: "client.example", endpoint: "api.client.example:443" }, query: {} as EditorSession["query"], bearer: "private" };
  let finish!: (response: Response) => void;
  const api = new EditorConnection({ session: () => session, connect: async () => {}, fetch: async (url, options) => {
    assert.equal(url, "https://api.client.example/artifacts");
    assert.equal(options?.credentials, "omit"); assert.equal(options?.redirect, "error");
    const body = options?.body as FormData;
    assert.equal(body.get("name"), "examples.zip");
    assert.equal(body.has("targetArtifactId"), false);
    return new Promise(resolve => { finish = resolve; });
  } });
  const upload = api.uploadFile(api.current()!, "examples.zip", "application/zip", new Uint8Array([80, 75]));
  session = undefined; api.changed();
  finish(new Response(JSON.stringify({ fileId: "one", artifactId: "two" }), { status: 201 }));
  await assert.rejects(upload, /connection changed/);
});
