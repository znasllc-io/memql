// The two gates activate() applies, in the one process each of them needs
// (memql#3387).
//
// 1. WORKSPACE TRUST gates the runtime surface and nothing else. An untrusted
//    window arms a one-shot listener; granting trust lights the surface up
//    without a reload.
// 2. SETTING SCOPE gates memql.lsp.serverPath. A workspace-scoped value is
//    refused -- it comes out of the opened folder, so honouring it would let a
//    repository name any executable on the machine -- and the refusal is now
//    SAID rather than silent.
//
// Separate file from activation.test.ts because registerRuntimeSurface refuses
// a second registration within a process, so each activation scenario needs its
// own. node:test runs every file in its own child process.

import test from 'node:test';
import assert from 'node:assert/strict';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';

import type { ExtensionContext } from 'vscode';

import { activate } from '../src/extension.js';
import {
  grantWorkspaceTrust,
  isTrustListenerArmed,
  recorded,
  settings,
  workspace,
} from './support/vscodeStub.js';

const home = fs.mkdtempSync(path.join(os.tmpdir(), 'memql-activation-gates-'));
process.env.HOME = home;
process.env.PATH = '';

// registerRuntimeSurface takes a parked portal handoff out of globalState on
// its way in (memql#4251), so the fake context has to model one. In-memory and
// empty: this lane is about activation ORDER, and a replay would drag a uri
// handler's whole decision into a test about which surface came up first.
const globalState = {
  store: new Map<string, unknown>(),
  get<T>(key: string): T | undefined {
    return globalState.store.get(key) as T | undefined;
  },
  update(key: string, value: unknown): Promise<void> {
    if (value === undefined) globalState.store.delete(key);
    else globalState.store.set(key, value);
    return Promise.resolve();
  },
};

const context = {
  subscriptions: [] as { dispose(): unknown }[],
  asAbsolutePath: (relative: string) => path.join(home, 'extension', relative),
  globalState,
} as unknown as ExtensionContext;

// A workspace that tries to redirect the language server at itself, in a
// window the user has NOT trusted.
settings.clear();
settings.set('memql.lsp.serverPath', {
  workspaceValue: path.join(home, 'workspace-supplied-memql-lsp'),
});
workspace.isTrusted = false;
activate(context);

test('an untrusted workspace registers no runtime surface', () => {
  assert.deepEqual(recorded.treeViews, []);
  assert.deepEqual(recorded.commands, []);
  assert.deepEqual(recorded.watched, []);
  assert.equal(recorded.fileDecorationProviders, 0);

  // ...but the URI HANDLER IS REGISTERED, and that contrast is the premise the
  // portal handoff rests on (memql#4251). `onUri` is what ACTIVATES the
  // extension, so a handler put behind the trust gate would not exist for the
  // very link that woke the editor -- VS Code delivers the uri to whatever
  // handler is there when activate() returns. The gate protects the ACTION
  // instead: handleOpenUri finds no runtime surface and answers `untrusted`.
  assert.equal(recorded.uriHandlers, 1);
});

test('an untrusted workspace still arms the one-shot trust listener', () => {
  assert.ok(isTrustListenerArmed());
});

test('a workspace-level serverPath is refused, and the refusal is visible', () => {
  assert.equal(recorded.warnings.length, 1, `unexpected warnings: ${recorded.warnings.join(' | ')}`);
  const message = recorded.warnings[0] ?? '';
  assert.match(message, /memql\.lsp\.serverPath/);
  assert.match(message, /IGNORED/);
  assert.match(message, /user settings/i);
});

test('the workspace value is not used as the server path either', () => {
  // If it had been honoured, resolveServerPath would have returned it and the
  // "no binary found" error would never have fired.
  assert.equal(recorded.errors.length, 1, `unexpected errors: ${recorded.errors.join(' | ')}`);
  assert.match(recorded.errors[0] ?? '', /language features/i);
});

test('granting trust registers the runtime surface, exactly once', () => {
  grantWorkspaceTrust();

  // REGISTRATION order, which is not the display order: Deployments sits above
  // Clusters in the panel, and that is decided by contributes.views in the
  // manifest. Here it comes second because it is built from the presence memo,
  // which activation resolves after the Clusters tree.
  assert.deepEqual(recorded.treeViews, [
    'memqlClusters',
    'memqlDeployments',
    'memqlConstructs',
    'memqlData',
    'memqlRuns',
  ]);
  assert.ok(recorded.commands.includes('memql.clusters.select'));

  // The read-only badge (memql#3762) comes up with the runtime surface and not
  // before it. It reads the connected cluster's catalog, so registering it in a
  // window that has not been trusted would mean an untrusted workspace could
  // provoke the fetch that feeds it.
  assert.equal(recorded.fileDecorationProviders, 1);

  // The listener disposes itself before calling in, and registerRuntimeSurface
  // guards as well. A second grant must be inert rather than a second tree.
  const registered = recorded.treeViews.length;
  grantWorkspaceTrust();
  assert.equal(recorded.treeViews.length, registered);
  assert.equal(recorded.fileDecorationProviders, 1);
  assert.equal(isTrustListenerArmed(), false);
});
