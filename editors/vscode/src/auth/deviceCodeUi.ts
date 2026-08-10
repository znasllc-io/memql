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
// WHAT THIS FILE IS AND IS NOT (memql#3515)
// -----------------------------------------------------------------------------
//
// PRESENTATION ONLY. It used to carry two full sign-in shells of its own --
// `signInWithDeviceCode` and an exported `signInToCluster` -- and the second was
// the bug memql#3515 was filed about: `memQL: Sign In` called a PRIVATE function
// of the same name in extension.ts that ran loopback with no fallback, so the
// capability here shipped with zero importers. A host that genuinely could not
// do loopback sat through the callback deadline and was told it had failed,
// while the code to hand it a device code sat right here.
//
// The resolution is one sign-in shell, in extension.ts, because that is where
// the rest of a sign-in lives: the tree refresh, the reconnect of the selected
// cluster, and the kind-based failure levels from src/auth/signin.ts. This file
// keeps the part that is genuinely about the DEVICE CODE and nothing else --
// putting it on screen and keeping it there.

import { env, Uri, window, type Progress } from 'vscode';

import type { AuthFlowError } from './errors.js';
import type { DeviceAuthorization } from './deviceCode.js';

/**
 * deviceCodeProgressLine is what the progress notification shows while polling.
 *
 * The notification carries the code AND the URL as text, so a user who closes
 * every other surface can still read both.
 */
export function deviceCodeProgressLine(authorization: DeviceAuthorization): string {
  return `Enter code ${authorization.userCode} at ${authorization.verificationUri}`;
}

/**
 * announceDeviceCodeFallback explains the switch when loopback proved
 * impossible. A flow that silently changes shape reads as a bug.
 */
export function announceDeviceCodeFallback(
  progress: Progress<{ message?: string }>,
  reason: AuthFlowError,
): void {
  progress.report({
    message: 'This host cannot complete a browser sign-in; switching to a device code...',
  });
  void window.showInformationMessage(
    `memQL: the browser sign-in could not complete (${reason.message}) Falling back to a device code.`,
  );
}

/**
 * showDeviceCodeActions puts the two buttons on screen and keeps them there for
 * as long as the user is using them. `isSettled` is read at each step so a flow
 * that finished (or was cancelled) while a notification sat open does not
 * re-summon it afterwards.
 */
export function showDeviceCodeActions(
  authorization: DeviceAuthorization,
  isSettled: () => boolean,
): void {
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
