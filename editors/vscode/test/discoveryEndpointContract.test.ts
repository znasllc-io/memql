// The TypeScript half of the discovery-endpoint contract (memql#3399).
//
// The cluster advertises a dial address in GET /.well-known/memql-config.json.
// This extension is one of the clients that reads it, and webSocketUrlFor is
// where that value becomes a connection. Nothing in the build links the two, so
// the live cluster spent the whole life of memql#3399 publishing
// "https://bff.memql.localhost" -- a host with no ingress -- while both sides'
// own tests stayed green.
//
// So both sides are asserted against ONE file. The Go half is
// component/identity/discovery_endpoint_contract_test.go, and it reads the same
// fixture: a case added there is a case this must satisfy, and a change to what
// the parser accepts fails there too.
//
// The fixture is read at runtime rather than imported, because tsconfig.test.json
// roots this package at editors/vscode and the fixture deliberately lives outside
// it -- it belongs to neither half.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import { webSocketUrlFor } from "../src/connection/endpoint.js";

interface ContractCase {
  name: string;
  configured: string;
  identityUrl: string;
  advertised: string;
  webSocketUrl: string;
}

interface Contract {
  cases: ContractCase[];
  rejected: string[];
}

// repoRoot walks up from this file until it finds the workspace marker. Walking
// beats a fixed "../../.." because the test runs from dist-test/ (esbuild output),
// not from test/, and a hard-coded depth silently reads the wrong tree the moment
// either layout moves.
function repoRoot(): string {
  let dir = __dirname;
  for (;;) {
    if (fs.existsSync(path.join(dir, "go.work"))) return dir;
    const parent = path.dirname(dir);
    if (parent === dir) {
      throw new Error(`could not locate the repository root (no go.work above ${__dirname})`);
    }
    dir = parent;
  }
}

function loadContract(): Contract {
  const file = path.join(repoRoot(), "test", "fixtures", "discovery-endpoint-contract.json");
  return JSON.parse(fs.readFileSync(file, "utf8")) as Contract;
}

const contract = loadContract();

test("the shared contract fixture carries cases", () => {
  assert.ok(contract.cases.length > 0, "fixture carries no cases");
  assert.ok(contract.rejected.length > 0, "fixture names no rejected forms");
});

for (const c of contract.cases) {
  test(`advertised value is usable: ${c.name}`, () => {
    assert.equal(
      webSocketUrlFor({ name: "contract", endpoint: c.advertised }),
      c.webSocketUrl,
      `discovery advertised "${c.advertised}" for configured "${c.configured}"`,
    );
  });
}

// THE DOCUMENT MUST NEVER PUBLISH THESE; A CLIENT MUST STILL SURVIVE MEETING
// ONE. The Go half proves the discovery handler cannot emit a rejected spelling.
// This half used to prove the parser refused them outright -- and that is what
// left a signed-in editor unable to dial the owner's local cluster, because the
// Cockpit writes the same front door as `https://api.memql.localhost` into the
// shared cluster list. So a web-URL spelling now dials EXACTLY the bridge its
// host:port form dials (same host, same port, the socket twin of its scheme),
// and any other scheme is still refused.
function hostPortForm(spelling: string): string | undefined {
  const m = /^(https?):\/\/([^/]+)\/?$/i.exec(spelling);
  if (m === null) return undefined;
  const authority = m[2];
  const hasPort = /:\d+$/.test(authority);
  const port = m[1].toLowerCase() === "https" ? "443" : "80";
  return hasPort ? authority : `${authority}:${port}`;
}

for (const spelling of contract.rejected) {
  const equivalent = hostPortForm(spelling);
  if (equivalent === undefined) {
    test(`rejected form stays rejected: ${spelling}`, () => {
      assert.throws(
        () => webSocketUrlFor({ name: "contract", endpoint: spelling }),
        `"${spelling}" must not be accepted -- the discovery document is forbidden from publishing it`,
      );
    });
    continue;
  }
  test(`a web-URL spelling dials the front door its host:port form names: ${spelling}`, () => {
    const dialed = webSocketUrlFor({ name: "contract", endpoint: spelling });
    const viaHostPort = new URL(webSocketUrlFor({ name: "contract", endpoint: equivalent }));
    const url = new URL(dialed);
    assert.equal(url.hostname, viaHostPort.hostname);
    assert.equal(url.pathname, "/memql/ws");
    assert.equal(url.protocol, spelling.toLowerCase().startsWith("https") ? "wss:" : "ws:");
  });
}
