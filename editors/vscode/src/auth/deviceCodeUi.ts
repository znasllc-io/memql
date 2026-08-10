// The editor-side half of the device-code sign-in: what the human sees.
//
// -----------------------------------------------------------------------------
// WHY THIS FILE EXISTS SEPARATELY
// -----------------------------------------------------------------------------
//
// deviceCode.ts carries the protocol and stays free of `vscode`, so it runs in
// the fast `node --test` lane. Everything below is API wiring with no logic
// worth unit-testing -- a progress notification, a clipboard write, an
// openExternal -- which is exactly the shape the import allowlist in
// cmd/memql-lsp/vscodeimportrule_test.go is for. Keeping it here rather than in
// extension.ts also keeps the activation file's edit down to an import and one
// registerCommand, which matters while memql#3403 is rewriting the same file on
// another branch.
//
// -----------------------------------------------------------------------------
// KEEPING THE CODE ON SCREEN
// -----------------------------------------------------------------------------
//
// The user has to read a code off this screen and type it into another one,
// which can take a minute -- so the code cannot be a toast that disappears
// while they are unlocking their phone. Two surfaces carry it, deliberately:
//
//   THE PROGRESS NOTIFICATION lives for exactly as long as the polling does and
//   cannot be dismissed by accident. It carries the code AND the verification
//   URL as text, so even a user who closes everything else can still read both.
//
//   AN INFORMATION MESSAGE carries the two ACTIONS -- Copy Code, and a button
//   that opens the verification page -- because a notification's progress
//   message renders no buttons and no links. It is re-shown after each click
//   (clicking it is not finishing with it), and NOT re-shown after a dismissal,
//   because re-summoning a notification a person just closed is nagging.
//
// -----------------------------------------------------------------------------
// WHAT THIS FILE IS, AND WHAT IT DELIBERATELY IS NOT (memql#3515)
// -----------------------------------------------------------------------------
//
// It exports FLOW RUNNERS, not sign-ins. Each one runs a flow and returns the
// tokens; what happens to those tokens -- the client_id write, the credential
// split, the tree refresh, the reconnect, which failure kinds get a red toast --
// belongs to the ONE sign-in shell in src/extension.ts, and both commands go
// through it.
//
// This file used to carry a second shell of its own, and with it a second
// exported function called `signInToCluster`. The one src/extension.ts actually
// ran was a private function of the same name that had NO fallback, so the
// device-code fallback below -- written, tested, and exported -- was reachable
// from nothing. Nothing at either call site said which one won. So there is now
// exactly one shell, it lives where the editor concerns are, and what is
// exported from here is the part that genuinely differs between the two
// commands: which flow runs, and what the human is told while it does.

import { env, Uri, window, type Progress } from 'vscode';

import type { ClusterConfig } from '../clusters/model.js';
import {
  runDeviceCodeFlow,
  signInWithDeviceCodeFallback,
  type DeviceAuthorization,
} from './deviceCode.js';
import type { AuthFlowTokens } from './flow.js';

export interface SignInFlowUi {
  /**
   * The sign-in command's progress notification. It carries the user code for
   * as long as the polling runs -- see KEEPING THE CODE ON SCREEN above.
   */
  progress: Progress<{ message?: string }>;
  /** Bridged from the notification's Cancel button by the shell. */
  signal?: AbortSignal;
}

/**
 * The default sign-in: the browser round trip, and the device grant when this
 * host cannot do one.
 *
 * Which one ran is ANNOUNCED (`onFallback`) rather than silently substituted --
 * a flow that changes shape without saying so reads as a bug. Only the failure
 * kinds that are evidence of an environment limitation switch; a cancellation
 * or a state mismatch does not (deviceCode.ts owns that rule).
 */
export async function runSignInWithFallback(
  cluster: ClusterConfig,
  ui: SignInFlowUi,
): Promise<AuthFlowTokens> {
  return withCodeOnScreen(ui, (flowDeps) => {
    ui.progress.report({ message: 'opening your browser...' });
    return signInWithDeviceCodeFallback(cluster, {
      ...flowDeps,
      // asExternalUri is not decoration: under Remote-SSH, Codespaces or a dev
      // container the browser runs on a different machine from this extension
      // host, and the loopback URL has to be rewritten into one that machine
      // can reach. toString(true) skips re-encoding -- the authorization URL's
      // query is already percent-encoded and encoding it twice corrupts the
      // PKCE challenge and the state.
      resolveExternalUri: async (url) => (await env.asExternalUri(Uri.parse(url))).toString(true),
      openExternal: (url) => env.openExternal(Uri.parse(url)),
      onFallback: (reason) => {
        ui.progress.report({
          message: 'This host cannot complete a browser sign-in; switching to a device code...',
        });
        window.showInformationMessage(
          `memQL: the browser sign-in could not complete: ${reason.message} Falling back to a device code.`,
        );
      },
    });
  });
}

/**
 * The device grant, run DELIBERATELY, skipping loopback.
 *
 * This is the second command (memql#3411): a user who already knows their
 * environment cannot do loopback should not have to sit through the two-minute
 * callback deadline to be told so.
 */
export async function runDeviceCodeSignIn(
  cluster: ClusterConfig,
  ui: SignInFlowUi,
): Promise<AuthFlowTokens> {
  return withCodeOnScreen(ui, (flowDeps) => {
    ui.progress.report({ message: 'requesting a device code...' });
    return runDeviceCodeFlow(cluster, flowDeps);
  });
}

interface FlowDeps {
  /** Aborted by the progress notification's Cancel button. */
  signal?: AbortSignal;
  onUserCode: (authorization: DeviceAuthorization) => void;
}

/**
 * withCodeOnScreen puts the user code in front of the human for exactly as long
 * as the flow is polling for it, and takes it away the moment the flow settles.
 *
 * `settled` is read at each step by showCodeActions rather than captured, so a
 * flow that finished (or was cancelled) while a notification sat open does not
 * re-summon it afterwards. The `finally` is what makes that true on the failure
 * path too.
 */
async function withCodeOnScreen(
  ui: SignInFlowUi,
  run: (deps: FlowDeps) => Promise<AuthFlowTokens>,
): Promise<AuthFlowTokens> {
  let settled = false;
  try {
    return await run({
      signal: ui.signal,
      onUserCode: (authorization) => {
        ui.progress.report({ message: codeLine(authorization) });
        showCodeActions(authorization, () => settled);
      },
    });
  } finally {
    settled = true;
  }
}

function codeLine(authorization: DeviceAuthorization): string {
  return `Enter code ${authorization.userCode} at ${authorization.verificationUri}`;
}

// showCodeActions puts the two buttons on screen and keeps them there for as
// long as the user is using them. `isSettled` is read at each step so a flow
// that finished (or was cancelled) while a notification sat open does not
// re-summon it afterwards.
function showCodeActions(authorization: DeviceAuthorization, isSettled: () => boolean): void {
  const COPY = 'Copy Code';
  const OPEN = 'Open Verification Page';
  const message = `memQL: enter code ${authorization.userCode} at ${authorization.verificationUri} to finish signing in.`;
  // The pre-filled form when the server offered one -- it saves the person
  // typing the code twice when they open the page on THIS machine.
  const target = authorization.verificationUriComplete || authorization.verificationUri;

  const show = (): void => {
    if (isSettled()) return;
    void Promise.resolve(window.showInformationMessage(message, COPY, OPEN)).then(
      async (choice) => {
        if (isSettled() || choice === undefined) return;
        if (choice === COPY) await env.clipboard.writeText(authorization.userCode);
        else await env.openExternal(Uri.parse(target));
        // Clicking a button is not finishing with the code: polling continues,
        // so the code goes back on screen.
        show();
      },
    );
  };
  show();
}
