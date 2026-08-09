// activate() brings up two INDEPENDENT surfaces, and this file pins that they
// stay independent (memql#3387).
//
// The defect: activate() resolved the memql-lsp binary first and returned when
// it was missing, so registerRuntimeSurface -- the tree data providers, the
// clusters.yaml watcher, every runtime command -- was never reached. The three
// views still rendered, because their `when` clause only asks for workspace
// trust, and sat permanently empty behind an error message naming a language
// server they need nothing from. It is the DEFAULT first-run state: a fresh
// clone has no bundled binary and nothing on PATH.
//
// The arrangement below is that state exactly -- no user setting, no bundled
// binary, an empty PATH -- in a TRUSTED workspace, which is the case the views
// are supposed to work in.
//
// The `vscode` module is test/support/vscodeStub.ts, aliased in by
// esbuild.test.js. See its header for why this one adapter is unit-tested at
// all.

import test from 'node:test';
import assert from 'node:assert/strict';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';

import type { ExtensionContext } from 'vscode';

import { activate } from '../src/extension.js';
import { constructed } from './support/languageClientStub.js';
import { recorded, settings, workspace } from './support/vscodeStub.js';

// A home directory of our own. registerRuntimeSurface reads (and mkdirs)
// ~/.memql, and a test has no business touching the developer's real one.
// os.homedir() consults $HOME on POSIX, so this redirects it.
const home = fs.mkdtempSync(path.join(os.tmpdir(), 'memql-activation-'));
process.env.HOME = home;
// An empty PATH is what "memql-lsp is not installed" looks like to
// resolveOnPath, and it cannot be faked any other way -- the resolver walks
// the real PATH entries.
process.env.PATH = '';

// asAbsolutePath points into an empty directory, so the bundled-binary
// candidate (bin/<platform>-<arch>/memql-lsp) does not exist either.
const context = {
  subscriptions: [] as { dispose(): unknown }[],
  asAbsolutePath: (relative: string) => path.join(home, 'extension', relative),
} as unknown as ExtensionContext;

// ACTIVATION HAPPENS ONCE, HERE. registerRuntimeSurface guards on module state
// and refuses a second registration (so the trust listener can never
// double-register), which makes a per-test activate() impossible in one
// process. Every case below reads the recording this single activation left.
// The trust-gate half of activate() lives in activationGates.test.ts, which
// node:test runs as its own process.
settings.clear();
workspace.isTrusted = true;
activate(context);

test('a missing memql-lsp does not take the runtime surface down with it', () => {
  // The whole point of the issue: all three views get a data provider even
  // though the language server never started.
  assert.deepEqual(recorded.treeViews, ['memqlClusters', 'memqlConcepts', 'memqlRuns']);
});

test('the runtime commands are registered, so a cluster can be selected and connected', () => {
  for (const id of [
    'memql.clusters.refresh',
    'memql.clusters.select',
    'memql.clusters.add',
    'memql.clusters.edit',
    'memql.clusters.disconnect',
    'memql.cluster.open',
    'memql.concepts.refresh',
    'memql.concepts.open',
    'memql.runs.refresh',
    'memql.runs.execute',
  ]) {
    assert.ok(recorded.commands.includes(id), `${id} was not registered`);
  }
});

test('clusters.yaml is watched, so an external edit still refreshes the tree', () => {
  assert.deepEqual(recorded.watched, [path.join(home, '.memql') + '/clusters.yaml']);
});

test('the missing-binary message names the language features, not the extension', () => {
  assert.equal(recorded.errors.length, 1, `unexpected errors: ${recorded.errors.join(' | ')}`);
  const message = recorded.errors[0] ?? '';

  // What is actually lost.
  assert.match(message, /language features/i);
  // The three ways out, unchanged from the original message.
  assert.match(message, /memql\.lsp\.serverPath/);
  assert.match(message, /PATH/);
  // And the correction the issue asks for: it must not read as "the extension
  // is dead" to someone staring at an empty Clusters view.
  assert.match(message, /Clusters, Concepts and Runs/);
});

test('no language client is started, which is the half that genuinely failed', () => {
  assert.deepEqual(constructed, []);
});

test('no workspace-level serverPath means no rejection notice', () => {
  assert.deepEqual(recorded.warnings, []);
});
