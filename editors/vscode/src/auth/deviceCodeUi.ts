// The editor-side half of the device-code sign-in: what the human sees.
//
// -----------------------------------------------------------------------------
// WHY THIS FILE EXISTS SEPARATELY
// -----------------------------------------------------------------------------
//
// deviceCode.ts carries the protocol AND the words (deviceCodeActionMessage,
// deviceCodeOpenTarget) and stays free of `vscode`, so both run in the fast
// `node --test` lane. Everything below is API wiring with no logic worth
// unit-testing -- a progress notification, a clipboard write, an openExternal
// -- which is exactly the shape the import allowlist in
// cmd/memql-lsp/vscodeimportrule_test.go is for.
//
// -----------------------------------------------------------------------------
// ONE NOTIFICATION PER FLOW (memql#4595)
// -----------------------------------------------------------------------------
//
// The 2026-08-25 field storm was this file's old shape compounding: a
// "falling back" toast, then a code notification that RE-SUMMONED itself
// after every button click -- copy the code, and the message you just acted
// on comes straight back. Two rules replace it:
//
//   THE PROGRESS NOTIFICATION stays the undismissable copy of the code and
//   the URL, alive exactly as long as the polling (deviceCodeProgressLine).
//   A person who closes everything else can still read both there.
//
//   ONE ACTION MESSAGE per flow. It is shown once, carries the two actions a
//   progress line cannot render (Copy Code / Open Approval Page), and -- when
//   the flow got here by fallback -- the explanation that used to be its own
//   toast. A click performs its action and does NOT re-show the message: the
//   code is still on the progress line, and re-summoning a notification a
//   person just acted on teaches them to dismiss MemQL toasts unread.
//
// The approval page is also OPENED up front, best effort, at the pre-filled
// URL (the /device page reads `?user_code=`), so on any host with a browser
// the whole dance is "click Approve". A host that cannot open one -- the
// genuinely headless box the device grant exists for -- fails that open
// quietly into the Connection output and leans on the code + URL.

import { env, Uri, window, type Progress } from 'vscode';

import type { AuthFlowError } from './errors.js';
import { errorText } from './errors.js';
import { recordDiagnostic, type DiagnosticSink } from '../state/diagnostics.js';
import {
  deviceCodeActionMessage,
  deviceCodeOpenTarget,
  type DeviceAuthorization,
  type DeviceCodeVia,
} from './deviceCode.js';

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
 * announceDeviceCodeFallback records the switch when loopback proved
 * impossible: the progress line changes and the reason lands in the MemQL
 * Connection channel (memql#4194, audit 6).
 *
 * NO TOAST OF ITS OWN (memql#4595). The explanation the old information
 * message carried now rides the single action message
 * (deviceCodeActionMessage via="fallback"), which arrives moments later with
 * the code -- one notification instead of two stacked ones.
 */
export function announceDeviceCodeFallback(
  progress: Progress<{ message?: string }>,
  reason: AuthFlowError,
  diagnostics?: DiagnosticSink,
): void {
  progress.report({
    message: 'This host cannot complete a browser sign-in; switching to a device code...',
  });
  if (diagnostics !== undefined) {
    recordDiagnostic(
      diagnostics,
      'browser sign-in is not possible on this host; using a device code',
      reason.message,
      new Date().toISOString(),
    );
  }
}

/**
 * showDeviceCodeActions opens the approval page and puts the one action
 * message on screen. `isSettled` is read before any action so a flow that
 * finished (or was cancelled) while the notification sat open does not act
 * afterwards.
 */
export function showDeviceCodeActions(
  authorization: DeviceAuthorization,
  isSettled: () => boolean,
  via: DeviceCodeVia,
  diagnostics?: DiagnosticSink,
): void {
  const COPY = 'Copy Code';
  const OPEN = 'Open Approval Page';
  const target = deviceCodeOpenTarget(authorization);

  // Best effort, up front: on a host with a browser this lands the person on
  // the pre-filled approval page before they have read the toast. A failure
  // is the headless case the code + URL exist for -- recorded, not shown.
  void Promise.resolve(env.openExternal(Uri.parse(target))).then(undefined, (err: unknown) => {
    if (diagnostics !== undefined) {
      recordDiagnostic(
        diagnostics,
        'the device approval page could not be opened here',
        errorText(err),
        new Date().toISOString(),
      );
    }
  });

  void Promise.resolve(
    window.showInformationMessage(deviceCodeActionMessage(authorization, via), COPY, OPEN),
  ).then(async (choice) => {
    if (isSettled() || choice === undefined) return;
    if (choice === COPY) await env.clipboard.writeText(authorization.userCode);
    else await env.openExternal(Uri.parse(target));
    // Deliberately NOT re-shown: the progress line still carries the code,
    // and the click was the person USING the notification, not dismissing it
    // unserved. Re-summoning here is the "friend copy code" storm.
  });
}
