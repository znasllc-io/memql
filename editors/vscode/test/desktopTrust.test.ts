import test from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:https";
import { mkdtempSync, readFileSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { execFileSync } from "node:child_process";
import { WebSocketServer } from "ws";
import { desktopWebSocketFactory } from "../src/connection/manager.js";
import { DesktopTrust } from "../src/connection/desktopTrust.js";
import { readOwnerSetupState } from "../src/clusters/claimState.js";
import { runAuthorizationFlow } from "../src/auth/flow.js";

// Real TLS sockets: no global CA install, TLS bypass, or live account changes.
test("the installed CA covers discovery, owner setup, OAuth and WebSocket on the local cluster only", async () => {
  const dir = mkdtempSync(join(tmpdir(), "memql-editor-tls-"));
  const openssl = (...args: string[]) => execFileSync("openssl", args, { cwd: dir, stdio: "pipe" });
  openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", "ca.key", "-out", "ca.pem", "-days", "1", "-subj", "/CN=MemQL test CA");
  openssl("req", "-newkey", "rsa:2048", "-nodes", "-keyout", "leaf.key", "-out", "leaf.csr", "-subj", "/CN=127.0.0.1");
  writeFileSync(join(dir, "ext"), "subjectAltName=IP:127.0.0.1\nextendedKeyUsage=serverAuth\n");
  openssl("x509", "-req", "-in", "leaf.csr", "-CA", "ca.pem", "-CAkey", "ca.key", "-CAcreateserial", "-out", "leaf.pem", "-days", "1", "-extfile", "ext");
  const ca = readFileSync(join(dir, "ca.pem"), "utf8");
  let issuer = "";
  let ownerState = "unclaimed";
  let exchanges = 0;
  const server = createServer({ key: readFileSync(join(dir, "leaf.key")), cert: readFileSync(join(dir, "leaf.pem")) }, (req, res) => {
    res.setHeader("content-type", "application/json");
    if (req.url?.startsWith("/.well-known/")) res.end(JSON.stringify({ issuer, authorization_endpoint: issuer + "/authorize", token_endpoint: issuer + "/oauth/token" }));
    else if (req.url === "/auth/setup/state") res.end(JSON.stringify({ state: ownerState }));
    else if (req.url === "/oauth/token") { exchanges++; res.end(JSON.stringify({ access_token: "test-access", expires_in: 300 })); }
    else if (req.url === "/redirect") { res.writeHead(302, { location: "https://localhost:1/" }); res.end(); }
    else res.end("{}");
  });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  const port = (server.address() as import("node:net").AddressInfo).port;
  issuer = `https://127.0.0.1:${port}`;
  const cluster = { name: "fixture", issuer, endpoint: `127.0.0.1:${port}`, local: true };
  let local = true;
  let selectedCA = ca;
  const trust = new DesktopTrust(async () => [{ ...cluster, local }], async () => selectedCA);
  const wss = new WebSocketServer({ server });
  wss.on("connection", socket => socket.close());
  try {
    await assert.rejects(fetch(issuer), /fetch failed/);
    assert.equal(await readOwnerSetupState(cluster, trust.fetch), "unclaimed");
    const tokens = await runAuthorizationFlow(cluster, {
      ownerSetup: true, fetch: trust.fetch, timeoutMs: 3000,
      resolveExternalUri: url => url,
      openExternal: async url => {
        const setup = new URL(url);
        assert.equal(setup.pathname, "/setup");
        assert.equal(setup.searchParams.get("code_challenge_method"), "S256");
        assert.equal(exchanges, 0, "no token before the owner ceremony finishes");
        ownerState = "claimed";
        const callback = new URL(setup.searchParams.get("redirect_uri")!);
        callback.searchParams.set("state", setup.searchParams.get("state")!);
        callback.searchParams.set("code", "fixture-registration-complete");
        // The listener answers after the token exchange, so don't await here.
        void fetch(callback).then(response => response.text());
      },
    });
    assert.equal(tokens.accessToken, "test-access");
    assert.equal(exchanges, 1);
    assert.equal(await readOwnerSetupState(cluster, trust.fetch), "claimed");
    await new Promise<void>((resolve, reject) => {
      const socket = desktopWebSocketFactory(ca)(`wss://127.0.0.1:${port}`);
      socket.onerror = reject; socket.onclose = () => resolve();
    });
    assert.equal(await trust.certificateFor(`wss://127.0.0.1:${port}`), ca);
    assert.equal(await trust.certificateFor(`https://127.0.0.1:${port + 1}`), undefined);
    assert.equal(await trust.certificateFor("https://example.com"), undefined);
    await assert.rejects(trust.fetch(issuer + "/redirect"), /fetch failed/);
    assert.equal((await trust.fetch(issuer + "/redirect", { redirect: "manual" })).status, 302);
    local = false;
    await assert.rejects(trust.fetch(issuer), /fetch failed/);
    local = true;
    selectedCA = "invalid certificate";
    await assert.rejects(trust.fetch(issuer), /fetch failed/);
    selectedCA = ca;
    assert.equal((await trust.fetch(issuer)).status, 200, "CA rotation is read without restarting VS Code");
    const wrongHost = new DesktopTrust(async () => [{ ...cluster, issuer: `https://localhost:${port}` }], async () => ca);
    try { await assert.rejects(wrongHost.fetch(`https://localhost:${port}`), (error: unknown) => {
      assert.equal((error as { cause?: { code?: string } }).cause?.code, "ERR_TLS_CERT_ALTNAME_INVALID");
      return true;
    }); }
    finally { wrongHost.dispose(); }
  } finally {
    trust.dispose();
    wss.close();
    server.closeAllConnections();
    await new Promise<void>(resolve => server.close(() => resolve()));
    rmSync(dir, { recursive: true, force: true });
  }
});
