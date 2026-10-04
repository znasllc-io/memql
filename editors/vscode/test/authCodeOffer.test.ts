// "Use a code instead" runs the device grant BESIDE the browser flow
// (src/auth/codeOffer.ts): the first to produce tokens wins, the other is
// stopped, and a late browser callback still completes a sign-in the person
// switched away from.

import test from "node:test";
import assert from "node:assert/strict";

import { BrowserOrCodeSignIn } from "../src/auth/codeOffer.js";
import { AuthFlowError } from "../src/auth/errors.js";
import type { AuthFlowTokens } from "../src/auth/flow.js";

function tokens(accessToken: string): AuthFlowTokens {
  return {
    accessToken,
    refreshToken: "R",
    expiresInSeconds: 900,
    expiresAtEpochSeconds: 900,
    clientId: "memql-vscode",
  };
}

interface Controlled {
  run: (signal: AbortSignal) => Promise<AuthFlowTokens>;
  resolve: (t: AuthFlowTokens) => void;
  reject: (err: unknown) => void;
  signal: () => AbortSignal | undefined;
  started: () => boolean;
}

/** A grant the test settles by hand; an abort rejects it as `cancelled`, as the real ones do. */
function controlled(): Controlled {
  let resolve!: (t: AuthFlowTokens) => void;
  let reject!: (err: unknown) => void;
  let seen: AbortSignal | undefined;
  let started = false;
  return {
    run: (signal) => {
      started = true;
      seen = signal;
      return new Promise<AuthFlowTokens>((res, rej) => {
        resolve = res;
        reject = rej;
        signal.addEventListener("abort", () => rej(new AuthFlowError("cancelled", "aborted")), { once: true });
      });
    },
    resolve: (t) => resolve(t),
    reject: (err) => reject(err),
    signal: () => seen,
    started: () => started,
  };
}

const tick = (): Promise<void> => new Promise((r) => setImmediate(r));

test("with no code taken, the browser's result is the sign-in's", async () => {
  const browser = controlled();
  const device = controlled();
  const signIn = new BrowserOrCodeSignIn({ runBrowser: browser.run, runDeviceCode: device.run });
  browser.resolve(tokens("FROM-BROWSER"));
  assert.equal((await signIn.result).accessToken, "FROM-BROWSER");
  assert.equal(device.started(), false, "the device grant runs only when the offer is taken");
});

test("taking the code starts the device grant WITHOUT stopping the browser", async () => {
  const browser = controlled();
  const device = controlled();
  const signIn = new BrowserOrCodeSignIn({ runBrowser: browser.run, runDeviceCode: device.run });
  assert.equal(signIn.useCode(), true);
  await tick();
  assert.equal(device.started(), true);
  assert.equal(browser.signal()?.aborted, false, "the loopback listener must stay up for a late callback");
  device.resolve(tokens("FROM-CODE"));
  assert.equal((await signIn.result).accessToken, "FROM-CODE");
  assert.equal(browser.signal()?.aborted, true, "the browser flow is stopped once the code won");
});

test("a late browser callback still wins after the code was chosen", async () => {
  const browser = controlled();
  const device = controlled();
  const signIn = new BrowserOrCodeSignIn({ runBrowser: browser.run, runDeviceCode: device.run });
  signIn.useCode();
  browser.resolve(tokens("LATE-BROWSER"));
  assert.equal((await signIn.result).accessToken, "LATE-BROWSER");
  assert.equal(device.signal()?.aborted, true, "the device polling stops");
});

test("one side failing does not end the sign-in while the other runs", async () => {
  const browser = controlled();
  const device = controlled();
  const signIn = new BrowserOrCodeSignIn({ runBrowser: browser.run, runDeviceCode: device.run });
  signIn.useCode();
  device.reject(new AuthFlowError("timeout", "the code expired"));
  await tick();
  assert.equal(signIn.settled, false);
  browser.resolve(tokens("BROWSER"));
  assert.equal((await signIn.result).accessToken, "BROWSER");
});

test("when both fail, the chosen code's error is the one reported", async () => {
  const browser = controlled();
  const device = controlled();
  const signIn = new BrowserOrCodeSignIn({ runBrowser: browser.run, runDeviceCode: device.run });
  signIn.useCode();
  browser.reject(new AuthFlowError("timeout", "browser gave up"));
  device.reject(new AuthFlowError("authorizationDenied", "declined on the phone"));
  await assert.rejects(() => signIn.result, (err: unknown) => (err as AuthFlowError).kind === "authorizationDenied");
});

test("a browser failure with no code taken is reported at once", async () => {
  const browser = controlled();
  const signIn = new BrowserOrCodeSignIn({ runBrowser: browser.run, runDeviceCode: controlled().run });
  browser.reject(new AuthFlowError("clientRefused", "refused"));
  await assert.rejects(() => signIn.result, (err: unknown) => (err as AuthFlowError).kind === "clientRefused");
  assert.equal(signIn.useCode(), false, "no code after the sign-in settled");
});

test("the outer cancel stops both grants", async () => {
  const browser = controlled();
  const device = controlled();
  const outer = new AbortController();
  const signIn = new BrowserOrCodeSignIn({ runBrowser: browser.run, runDeviceCode: device.run, signal: outer.signal });
  signIn.useCode();
  outer.abort();
  await assert.rejects(() => signIn.result, (err: unknown) => (err as AuthFlowError).kind === "cancelled");
  assert.equal(browser.signal()?.aborted, true);
  assert.equal(device.signal()?.aborted, true);
});

test("the code is offered once: a second take does nothing", async () => {
  const signIn = new BrowserOrCodeSignIn({ runBrowser: controlled().run, runDeviceCode: controlled().run });
  assert.equal(signIn.useCode(), true);
  assert.equal(signIn.useCode(), false);
  assert.equal(signIn.codeStarted, true);
});
