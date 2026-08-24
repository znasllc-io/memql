import assert from "node:assert/strict";
import { test } from "node:test";

import {
  AddClusterState,
  DERIVATION_PLACEHOLDER,
  connectDomainProblem,
  derivationLine,
  type ConnectField,
  type ConnectProbe,
  type ConnectProbeTargets,
} from "../src/state/addCluster.js";
import { DEFAULT_LOCAL_DOMAIN, installDomainProblem } from "../src/install/stackPin.js";
import { composeEndpointFromDomain } from "../src/connection/endpoint.js";

// connectForm.test.ts -- znasllc-io/memql#4431, memql#4432.
//
// "Add an existing cluster" asked four questions, of which the ENDPOINT was the
// derived one -- so the form asked for the answer (api.<domain>:443) and left
// the question it derives from (the domain) marked optional beside it. It now
// asks two, derives the rest, and puts the override behind a disclosure.
//
// And it refuses the localhost family by name. That flow is for clusters
// reachable over the network; a local install is the OTHER card, and it does far
// more than record an address -- hosts entries, an mkcert leaf, a k3d cluster,
// a bootstrapped owner. Typing `memql.localhost` here produced a registry entry
// pointing at a front door that does not exist, and the failure arrived later as
// a connection error naming a hostname the operator typed themselves.

function connectForm(values: Partial<Record<ConnectField, string>>): AddClusterState {
  const s = new AddClusterState();
  s.chooseAction("connect");
  for (const [field, text] of Object.entries(values)) {
    s.setConnectInput(field as ConnectField, text);
  }
  return s;
}

const messageFor = (s: AddClusterState, field: ConnectField): string | undefined =>
  s.connectErrors.find((e) => e.field === field)?.message;

// ---------------------------------------------------------------------------
// D6 -- the localhost family, refused by name
// ---------------------------------------------------------------------------

test("every spelling of the local machine is refused, and the message points at Install", () => {
  const family = [
    "localhost",
    "memql.localhost",
    "anything.at.all.localhost",
    "127.0.0.1",
    // The whole /8, not just the canonical address: 127.0.0.2 is just as local.
    "127.0.0.2",
    "127.255.255.254",
    "::1",
    "[::1]",
    "0:0:0:0:0:0:0:1",
    // Case is not a defence: DNS is case-insensitive and an operator may well
    // type LOCALHOST.
    "LocalHost",
    "MemQL.LOCALHOST",
    // Trailing dots are a legal absolute-name spelling that normalizeDomain
    // strips, so they must not be a way around the refusal.
    "memql.localhost.",
  ];
  for (const domain of family) {
    const problem = connectDomainProblem(domain);
    assert.notEqual(problem, undefined, `${domain} must be refused here`);
    assert.match(
      problem!,
      /Install a local cluster/,
      `${domain}'s refusal must name the other flow -- an operator who is told "no" and not "there" tries again`,
    );
  }
});

test("an ordinary public domain is not refused", () => {
  for (const domain of [
    "example.com",
    "memql.example.com",
    "localhost.example.com", // the LABEL appears, but the apex is public
    "notlocalhost",
    "10.0.0.5", // private, but routable -- a VPN-reachable cluster is legitimate
    "203.0.113.7",
  ]) {
    assert.equal(connectDomainProblem(domain), undefined, `${domain} must be accepted`);
  }
});

test("an empty domain is left to the required-field check to report", () => {
  // Two messages for one blank field is worse than one, and "a domain is
  // required" is the true sentence -- "that is a local install's domain" is not.
  assert.equal(connectDomainProblem(""), undefined);
  assert.equal(connectDomainProblem("   "), undefined);
});

test("the two flows diverge on memql.localhost, and each is right", () => {
  // THE SAME STRING, OPPOSITE ANSWERS, and this is the assertion that records
  // why. The INSTALL form is about to MAKE memql.localhost resolve -- it writes
  // the hosts entries and issues the certificate -- so it is that form's own
  // default. This form only RECORDS an address, so the same string names a front
  // door that will never answer.
  assert.equal(
    installDomainProblem(DEFAULT_LOCAL_DOMAIN),
    undefined,
    "the installer's own default must stay valid on the install form",
  );
  assert.notEqual(
    connectDomainProblem(DEFAULT_LOCAL_DOMAIN),
    undefined,
    "and must be refused on the form that only registers an address",
  );
});

// ---------------------------------------------------------------------------
// D4/D5 -- two primary answers, everything else derived
// ---------------------------------------------------------------------------

test("the domain is required, and its message says what is derived from it", () => {
  const s = connectForm({ name: "staging" });
  assert.equal(s.connectDraft(), undefined);
  assert.match(messageFor(s, "domain")!, /required/);
});

test("two answers are enough: the endpoint is derived", () => {
  const draft = connectForm({ name: "staging", domain: "example.com" }).connectDraft();
  assert.deepEqual(draft, {
    name: "staging",
    domain: "example.com",
    endpoint: "api.example.com:443",
  });
});

test("an Advanced endpoint wins over the derivation", () => {
  const draft = connectForm({
    name: "staging",
    domain: "example.com",
    endpoint: "gateway.example.com:8443",
  }).connectDraft();
  assert.equal(draft?.endpoint, "gateway.example.com:8443");
  assert.equal(draft?.domain, "example.com", "the domain still names the sign-in host");
});

test("the registration's SHAPE is unchanged: no local key, empty optionals omitted", () => {
  // `local: true` means "this cluster's data is disposable" -- it gates the
  // mutation confirmation and decides whether the tree row offers an uninstall.
  // Nothing about registering someone else's cluster knows that, so the key is
  // ABSENT rather than false: `local: false` is a key the Cockpit drops on its
  // next write, so writing it would make the two tools churn the file.
  //
  // And "" is not absent. They are the same file for a NEW entry and opposite
  // instructions to upsertCluster, where "" DELETES the key.
  const draft = connectForm({ name: "prod", domain: "prod.example.com", token: "  " }).connectDraft();
  assert.deepEqual(draft, {
    name: "prod",
    domain: "prod.example.com",
    endpoint: "api.prod.example.com:443",
  });
  assert.equal("local" in draft!, false);
  assert.equal("token" in draft!, false);
});

test("the derivation hint is composed by the real function, not re-implemented", () => {
  // THE TEMPLATE IS THE POINT (memql#4431). The webview updates this line as the
  // operator types, which means something on that side builds the sentence. It
  // is handed the composition ALREADY PERFORMED over a placeholder, so the
  // convention keeps exactly one spelling -- endpoint.ts records that it once
  // had three, and that the drift is invisible because every copy produces a
  // plausible hostname.
  const template = composeEndpointFromDomain(DERIVATION_PLACEHOLDER);
  assert.match(template, /%DOMAIN%/, "the placeholder must survive the composition");
  assert.equal(
    template.replace(DERIVATION_PLACEHOLDER, "example.com"),
    composeEndpointFromDomain("example.com"),
    "substituting into the template must equal composing directly, or the hint lies about what gets saved",
  );

  // EXACT, NOT A SUBSTRING MATCH. An unanchored /api\.example\.com:443/ passes
  // for "Will connect to evil.example.net/api.example.com:443." -- it asserts
  // that the endpoint appears SOMEWHERE in the sentence, which is not what the
  // hint promises. The hint's whole job is to be the thing the operator reads
  // instead of the endpoint box, so the whole sentence is the contract.
  // (CodeQL js/regex/missing-regexp-anchor flagged the loose form, and it was
  // right about the assertion even though this is a test.)
  assert.equal(derivationLine("example.com"), `Will connect to ${composeEndpointFromDomain("example.com")}.`);
  assert.doesNotMatch(derivationLine(""), /api\.:443/, "an empty domain must not compose a hole");
});

// ---------------------------------------------------------------------------
// D7 -- probe on save: warn, never block
// ---------------------------------------------------------------------------

const probeThat = (verdict: Awaited<ReturnType<ConnectProbe>>, seen?: ConnectProbeTargets[]): ConnectProbe =>
  async (targets) => {
    seen?.push(targets);
    return verdict;
  };

test("a passing probe saves silently, in one press", async () => {
  const s = connectForm({ name: "staging", domain: "example.com" });
  assert.equal(await s.prepareConnectSave(probeThat({ ok: true })), "write");
  assert.equal(s.connectProbe.state, "passed");
});

test("a failing probe WARNS and writes nothing -- and the second press writes", async () => {
  // A cluster that is stopped, behind a VPN, or mid-deploy is a cluster an
  // operator may legitimately register. Refusing would be wrong about all three.
  const s = connectForm({ name: "staging", domain: "example.com" });
  const probe = probeThat({ ok: false, reason: "getaddrinfo ENOTFOUND" });

  assert.equal(await s.prepareConnectSave(probe), "warned");
  assert.equal(s.connectProbe.state, "failed");
  assert.equal(
    s.connectProbe.state === "failed" ? s.connectProbe.reason : "",
    "getaddrinfo ENOTFOUND",
    "the reason must survive to the form -- 'could not reach it' sends nobody anywhere",
  );

  assert.equal(await s.prepareConnectSave(probe), "write", "Save anyway");
});

test("a probe that THROWS is a failed probe, never a failed save", async () => {
  // The probe is the panel's injected function. If it breaks, the operator must
  // still be able to register their cluster.
  const s = connectForm({ name: "staging", domain: "example.com" });
  const outcome = await s.prepareConnectSave(() => {
    throw new Error("socket hang up");
  });
  assert.equal(outcome, "warned");
  assert.match(s.connectProbe.state === "failed" ? s.connectProbe.reason : "", /socket hang up/);
});

test("an invalid form is never probed", async () => {
  // A probe is a network call against somebody's DNS. Spending one on a form
  // that cannot be saved anyway is both slow and rude.
  let called = false;
  const s = connectForm({ name: "", domain: "example.com" });
  const outcome = await s.prepareConnectSave(async () => {
    called = true;
    return { ok: true };
  });
  assert.equal(outcome, "invalid");
  assert.equal(called, false);
  assert.equal(s.connectProbe.state, "none");
});

test("the localhost ban is what keeps the mkcert false negative out of this path", async () => {
  // Node cannot verify a local mkcert leaf, so a memql.localhost probe would
  // fail for a reason that says nothing about the cluster. It never gets there:
  // validation refuses the domain first, and the probe is not called at all.
  let called = false;
  const s = connectForm({ name: "local", domain: DEFAULT_LOCAL_DOMAIN });
  const outcome = await s.prepareConnectSave(async () => {
    called = true;
    return { ok: false, reason: "self-signed certificate in certificate chain" };
  });
  assert.equal(outcome, "invalid");
  assert.equal(called, false, "the probe must not run against a domain the form already refuses");
});

test("editing any field retracts the verdict, and the Save-anyway it was offering", async () => {
  // A STALE PASS IS THE DANGEROUS DIRECTION. A verdict is about the values it
  // was given; once one changes it describes a cluster the operator is no longer
  // registering, and a pass carried over would let a corrected domain be written
  // on the strength of a check that ran against the typo.
  const s = connectForm({ name: "staging", domain: "exmaple.com" });
  assert.equal(await s.prepareConnectSave(probeThat({ ok: false, reason: "ENOTFOUND" })), "warned");

  s.setConnectInput("domain", "example.com");
  assert.equal(s.connectProbe.state, "none", "the old verdict must not survive the edit");

  const seen: ConnectProbeTargets[] = [];
  assert.equal(
    await s.prepareConnectSave(probeThat({ ok: true }, seen)),
    "write",
    "the corrected form is probed again rather than saved on the old warning",
  );
  assert.equal(seen[0]?.endpoint, "api.example.com:443");
});

test("the probe checks the front door AND the sign-in host, derived as the row will be", () => {
  // They are different hosts and they fail differently: api.<domain> serves the
  // gRPC front door, identity.<domain> serves the JWKS feed. A cluster whose
  // ingress routes one and not the other is a real half-configured state.
  const s = connectForm({ name: "staging", domain: "example.com" });
  assert.deepEqual(s.connectProbeTargets(), {
    jwksUrl: "https://identity.example.com/.well-known/jwks.json",
    endpoint: "api.example.com:443",
  });
});

test("an Advanced endpoint is what gets probed, not the one it replaced", () => {
  // Probing one endpoint and registering another is a green check mark over a
  // cluster that will not dial.
  const s = connectForm({
    name: "staging",
    domain: "example.com",
    endpoint: "gateway.example.com:8443",
  });
  assert.equal(s.connectProbeTargets().endpoint, "gateway.example.com:8443");
  assert.equal(
    s.connectProbeTargets().jwksUrl,
    "https://identity.example.com/.well-known/jwks.json",
    "the sign-in host still follows the DOMAIN, which is what names it",
  );
});
