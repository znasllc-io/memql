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

const context = {
  subscriptions: [] as { dispose(): unknown }[],
  asAbsolutePath: (relative: string) => path.join(home, 'extension', relative),
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

  assert.deepEqual(recorded.treeViews, ['memqlClusters', 'memqlConcepts', 'memqlRuns']);
  assert.ok(recorded.commands.includes('memql.clusters.select'));

  // The listener disposes itself before calling in, and registerRuntimeSurface
  // guards as well. A second grant must be inert rather than a second tree.
  const registered = recorded.treeViews.length;
  grantWorkspaceTrust();
  assert.equal(recorded.treeViews.length, registered);
  assert.equal(isTrustListenerArmed(), false);
});
