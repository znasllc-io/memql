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

// asAbsolutePath points into an empty directory, so the bundled-binary
// candidate (bin/<platform>-<arch>/memql-lsp) does not exist either.
const context = {
  subscriptions: [] as { dispose(): unknown }[],
  asAbsolutePath: (relative: string) => path.join(home, 'extension', relative),
  globalState,
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
  // The whole point of the issue: every view gets a data provider even though
  // the language server never started.
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
});

test('the runtime commands are registered, so a cluster can be selected and connected', () => {
  for (const id of [
    'memql.clusters.refresh',
    'memql.clusters.select',
    'memql.clusters.add',
    'memql.clusters.edit',
    // memql#3403: sign-in is reachable from the palette and the Clusters view,
    // so it has to be registered at activation like every other cluster command.
    'memql.clusters.signIn',
    'memql.clusters.signOut',
    'memql.clusters.disconnect',
    'memql.clusters.connection',
    'memql.clusters.openPortal',
    'memql.data.refresh',
    'memql.data.open',
    'memql.runs.refresh',
    'memql.runs.execute',
  ]) {
    assert.ok(recorded.commands.includes(id), `${id} was not registered`);
  }
});

test('the memql-cluster scheme is claimed, so a cluster document has something to open with', () => {
  // memql#4248. A `memql-cluster:` uri with no registered content provider does
  // not fail loudly -- openTextDocument rejects with "cannot open", which reads
  // as the DOCUMENT being broken rather than as the registration being absent.
  // The host lane opens one for real, but that lane does not run in CI, so this
  // is the guard that travels with every pull request.
  assert.deepEqual(recorded.contentProviderSchemes, ['memql-cluster']);
});

test('every file the trees read is watched, so an external edit refreshes them', () => {
  // clusters.yaml appears TWICE, and that is the design rather than a slip:
  // the Clusters tree and the Deployments tree read the same registry and
  // refresh independently, so each holds its own watcher. One shared watcher
  // fanning out to both would make either view's lifetime decide the other's.
  const memql = path.join(home, '.memql');
  assert.deepEqual(recorded.watched, [
    `${memql}/clusters.yaml`,
    `${memql}/clusters.yaml`,
    `${memql}/install-receipt.json`,
    `${memql}/runs/*.json`,
  ]);
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
