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
// ONE PERSISTENCE PATH
// -----------------------------------------------------------------------------
//
// Both sign-in shapes return the same `AuthFlowTokens`, and both land in
// persistSignIn (memql#3404) through `completeSignIn` below -- one call site,
// so a device sign-in cannot drift into storing its tokens somewhere the
// loopback sign-in does not, or skipping the client_id write that stops the
// next sign-in re-registering.

import { env, ProgressLocation, Uri, window, type CancellationToken, type Progress } from 'vscode';

import { upsertCluster } from '../clusters/file.js';
import type { ClusterConfig } from '../clusters/model.js';
import {
  runDeviceCodeFlow,
  signInWithDeviceCodeFallback,
  type DeviceAuthorization,
} from './deviceCode.js';
import { errorText, isAuthFlowError } from './errors.js';
import type { AuthFlowTokens } from './flow.js';
import { persistSignIn, type SecretStore } from './store.js';

export interface SignInUiDeps {
  /** Where clusters.yaml lives; the access token and client_id are written there. */
  clustersPath: string;
  /** VS Code's SecretStorage. Absent only in a host that has none. */
  secrets?: SecretStore;
}

/**
 * signInWithDeviceCode runs the device grant DELIBERATELY, skipping loopback.
 *
 * This is the command path (memql#3411): a user who already knows their
 * environment cannot do loopback should not have to sit through the
 * two-minute callback deadline to be told so.
 */
export async function signInWithDeviceCode(
  cluster: ClusterConfig,
  deps: SignInUiDeps,
): Promise<boolean> {
  return runSignIn(cluster, deps, `Signing in to "${cluster.name}" with a device code`, (flowDeps) =>
    runDeviceCodeFlow(cluster, flowDeps),
  );
}

/**
 * signInToCluster runs the loopback flow, falling back to the device grant when
 * this host cannot do loopback.
 *
 * The default sign-in path: the browser round trip when it works, and the
 * device code when the environment makes it impossible. Which one ran is
 * announced (`onFallback`) rather than silently substituted.
 */
export async function signInToCluster(
  cluster: ClusterConfig,
  deps: SignInUiDeps,
): Promise<boolean> {
  return runSignIn(cluster, deps, `Signing in to "${cluster.name}"`, (flowDeps, progress) =>
    signInWithDeviceCodeFallback(cluster, {
      ...flowDeps,
      resolveExternalUri: async (url) => (await env.asExternalUri(Uri.parse(url))).toString(true),
      openExternal: (url) => env.openExternal(Uri.parse(url)),
      onFallback: (reason) => {
        progress.report({
          message: 'This host cannot complete a browser sign-in; switching to a device code...',
        });
        window.showInformationMessage(
          `memQL: the browser sign-in could not complete (${reason.message}) Falling back to a device code.`,
        );
      },
    }),
  );
}

interface FlowDeps {
  /** Aborted by the progress notification's Cancel button. */
  signal: AbortSignal;
  onUserCode: (authorization: DeviceAuthorization) => void;
}

type SignInRunner = (
  deps: FlowDeps,
  progress: Progress<{ message?: string }>,
) => Promise<AuthFlowTokens>;

/**
 * runSignIn is the shared shell: a cancellable progress notification, the code
 * on screen, one persistence call, one report.
 *
 * The AbortController is what makes cancellation immediate rather than
 * eventual -- the poll loop's sleep rejects the moment it fires, instead of
 * finishing the interval it is in the middle of.
 */
async function runSignIn(
  cluster: ClusterConfig,
  deps: SignInUiDeps,
  title: string,
  run: SignInRunner,
): Promise<boolean> {
  return window.withProgress(
    { location: ProgressLocation.Notification, title, cancellable: true },
    async (progress, token: CancellationToken) => {
      const controller = new AbortController();
      const cancellation = token.onCancellationRequested(() => controller.abort());
      let settled = false;

      try {
        const tokens = await run(
          {
            signal: controller.signal,
            onUserCode: (authorization) => {
              progress.report({ message: codeLine(authorization) });
              showCodeActions(authorization, () => settled);
            },
          },
          progress,
        );
        settled = true;
        await completeSignIn(cluster.name, tokens, deps);
        window.showInformationMessage(`memQL: signed in to "${cluster.name}".`);
        return true;
      } catch (err) {
        settled = true;
        if (isAuthFlowError(err) && err.kind === 'cancelled') {
          // Deliberate. Nothing louder than an acknowledgement.
          window.showInformationMessage(`memQL: sign-in to "${cluster.name}" cancelled.`);
          return false;
        }
        window.showErrorMessage(`memQL: ${errorText(err)}`);
        return false;
      } finally {
        settled = true;
        cancellation.dispose();
      }
    },
  );
}

/**
 * completeSignIn is the ONLY place either sign-in shape stores anything.
 *
 * memql#3404 owns the split (refresh token to SecretStorage, access token and
 * client_id to the shared clusters.yaml); this call is what keeps the device
 * path from acquiring a second, divergent copy of that decision.
 */
async function completeSignIn(
  clusterName: string,
  tokens: AuthFlowTokens,
  deps: SignInUiDeps,
): Promise<void> {
  await persistSignIn(
    {
      secrets: deps.secrets,
      writeCluster: (update) => upsertCluster(deps.clustersPath, update),
    },
    clusterName,
    tokens,
  );
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
