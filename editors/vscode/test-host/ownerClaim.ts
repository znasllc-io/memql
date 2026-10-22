// The owner of a freshly installed LOCAL cluster claims it with a passkey, and
// the editor signs in, connects, signs out and signs in again -- against a real
// cluster, through the extension's own code.
//
// WHY THIS EXISTS. Every lane that installs a cluster stopped at the front door,
// so nothing ever claimed one, and the defect that locked every local owner out
// (a claim with no organization name, refused AFTER the passkey was saved) went
// out green. This drives the same functions the editor's commands call --
// runAuthorizationFlow (Register owner passkey / Sign In), DesktopTrust (the
// local CA), ConnectionManager (connect / disconnect) and revokeRefreshToken
// (Sign Out) -- and stands a Chromium with a CDP virtual authenticator in for
// the person in the browser. Nothing here imports `vscode`.
//
// WHAT IT LEAVES BEHIND. The virtual authenticator becomes the cluster's owner,
// so a cluster this has claimed is a test cluster: uninstall it afterwards.
//
// Run it bundled (esbuild, `--external:playwright`), with node_modules from
// editors/vscode on NODE_PATH:
//
//   OWNER_EMAIL=ci@example.com DOMAIN=memql.localhost OUT=/tmp/shots node owner-claim.js
//
// Exits 0 only when every step held; each step prints one line.

import * as fs from "node:fs";
import * as path from "node:path";
import { chromium, type CDPSession, type Page } from "playwright";
import { runAuthorizationFlow, type AuthFlowTokens } from "../src/auth/flow.js";
import { revokeRefreshToken } from "../src/auth/revoke.js";
import { readOwnerSetupState } from "../src/clusters/claimState.js";
import type { ClusterConfig } from "../src/clusters/model.js";
import { DesktopTrust } from "../src/connection/desktopTrust.js";
import { ConnectionManager, desktopDialWithTrust } from "../src/connection/manager.js";

const email = process.env.OWNER_EMAIL ?? "";
const domain = process.env.DOMAIN ?? "memql.localhost";
const out = process.env.OUT ?? "";
const cluster = { name: "memql", domain, endpoint: `api.${domain}:443`, issuer: `https://identity.${domain}`, local: true } as ClusterConfig;
const trust = new DesktopTrust(async () => [cluster]);
let shots = 0;
/** What the ownership page's email field held when it first appeared, before anything was typed. */
let arrivedWith: string | undefined;

function step(message: string): void {
  console.log(`[x] ${message}`);
}

async function snap(page: Page, label: string): Promise<void> {
  if (out === "") return;
  fs.mkdirSync(out, { recursive: true });
  await page.screenshot({ path: path.join(out, `${String(++shots).padStart(2, "0")}-${label}.png`), fullPage: true }).catch(() => undefined);
}

/** Answers whatever page the browser is on, as the owner would, until the editor's loopback answers. */
async function playTheOwner(page: Page, url: string, done: { settled: boolean }): Promise<void> {
  await page.goto(url, { waitUntil: "domcontentloaded" });
  const answered = new Set<string>();
  for (let tick = 0; tick < 240 && !done.settled; tick++) {
    await page.waitForTimeout(500);
    if (page.url().startsWith("http://127.0.0.1")) return;
    const text = (await page.locator("body").innerText().catch(() => "")).replace(/\s+/g, " ");
    // The whole text, not a prefix: the owner-email and passkey steps open with
    // the same heading and lead, and the OS follows identity's redirect without
    // changing the URL, so a prefix reads the passkey step as already answered.
    const key = `${page.url().replace(/#.*/, "")}|${text}`;
    if (answered.has(key)) continue;
    const press = async (name: RegExp, label: string): Promise<boolean> => {
      const button = page.getByRole("button", { name });
      if ((await button.count()) === 0 || !(await button.first().isEnabled())) return false;
      await snap(page, label);
      answered.add(key);
      await button.first().click();
      return true;
    };
    const ownerEmail = page.getByLabel("Owner email");
    if ((await ownerEmail.count()) > 0) {
      const value = await ownerEmail.first().inputValue();
      arrivedWith ??= value;
      if (value !== email) await ownerEmail.first().fill(email);
    }
    if (await press(/^(Create passkey|Finish setup)$/, "create-passkey")) continue;
    if (await press(/^(Sign in with a passkey|Try passkey again)$/, "passkey-sign-in")) continue;
  }
}

async function signIn(page: Page, ownerSetup: boolean): Promise<AuthFlowTokens> {
  const done = { settled: false };
  let owner: Promise<void> = Promise.resolve();
  try {
    return await runAuthorizationFlow(cluster, {
      ownerSetup,
      ...(ownerSetup ? { loginHint: email } : {}),
      fetch: trust.fetch as never,
      timeoutMs: 180_000,
      resolveExternalUri: (url) => url,
      openExternal: (url) => {
        owner = playTheOwner(page, url, done);
        return true;
      },
    });
  } finally {
    done.settled = true;
    await owner;
  }
}

async function connect(tokens: AuthFlowTokens): Promise<number> {
  const manager = new ConnectionManager(desktopDialWithTrust((url) => trust.certificateFor(url)));
  await manager.connect({ ...cluster, token: tokens.accessToken } as ClusterConfig);
  const state = manager.state;
  if (state.status !== "connected") {
    throw new Error(`connect failed: ${state.status === "error" ? `${state.reason}: ${state.message}` : state.status}`);
  }
  const concepts = await manager.query!.listConcepts();
  await manager.disconnect();
  return concepts.length;
}

async function addVirtualAuthenticator(cdp: CDPSession): Promise<void> {
  await cdp.send("WebAuthn.enable");
  await cdp.send("WebAuthn.addVirtualAuthenticator", {
    options: {
      protocol: "ctap2", transport: "internal", hasResidentKey: true,
      hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true,
    },
  });
}

async function main(): Promise<void> {
  if (email === "") throw new Error("OWNER_EMAIL is required: the email the install named as owner");
  const before = await readOwnerSetupState(cluster, trust.fetch as never);
  if (before !== "unclaimed") throw new Error(`the cluster reads ${before}, not unclaimed: claim a fresh install`);
  step("the fresh cluster is unclaimed");

  const browser = await chromium.launch();
  try {
    const context = await browser.newContext();
    const page = await context.newPage();
    await addVirtualAuthenticator(await context.newCDPSession(page));

    const claim = await signIn(page, true);
    if (arrivedWith !== email) throw new Error(`the ownership page arrived with ${JSON.stringify(arrivedWith ?? "no email field")}, not the install's owner email`);
    step("the ownership page arrived with the install's owner email filled in");
    step("Register owner passkey: the passkey claimed the cluster and the editor received its tokens");
    const after = await readOwnerSetupState(cluster, trust.fetch as never);
    if (after !== "claimed") throw new Error(`the claim completed but the cluster reads ${after}`);
    step("the cluster reads claimed");
    step(`connected over the local CA and listed ${await connect(claim)} concepts, then disconnected`);

    const revoked = await revokeRefreshToken(cluster.issuer!, claim.refreshToken, trust.fetch as never);
    if (!revoked.attempted || !revoked.revoked) throw new Error(`sign out did not revoke the session: ${JSON.stringify(revoked)}`);
    step("Sign Out revoked the session on the cluster");

    await context.clearCookies();
    const again = await signIn(page, false);
    step("Sign In with the passkey alone, no browser session left");
    step(`connected again and listed ${await connect(again)} concepts`);
  } finally {
    await browser.close();
    trust.dispose();
  }
}

main().catch((err: unknown) => {
  const e = err as { kind?: string; message?: string };
  console.error(`[ ] ERROR: ${e.kind ? `${e.kind}: ` : ""}${e.message ?? String(err)}`);
  process.exitCode = 1;
});
