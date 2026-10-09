// Cluster setup must not start the offline workspace compiler. Exercise the
// real activation adapter with a resolvable server, so a missing binary cannot
// accidentally satisfy the assertion that the language client stayed down.
import test from 'node:test';
import assert from 'node:assert/strict';
import type { ExtensionContext } from 'vscode';
import { activate } from '../src/extension.js';
import { constructed, instances } from './support/languageClientStub.js';
import { openTextDocument, recorded, settings, Uri, workspace } from './support/vscodeStub.js';

const document = (uri: string, languageId: string) => ({
  uri: Uri.parse(uri), languageId, getText: () => '', isDirty: false,
});

test('only opening a local MemQL document starts the language client, once', () => {
  // No runtime here: this also proves language features still work in
  // Restricted Mode. activation.test.ts covers the trusted runtime surface.
  workspace.isTrusted = false;
  workspace.workspaceFolders = [{ uri: Uri.file('/workspace') }];
  workspace.textDocuments.push(document('file:///workspace/readme.md', 'markdown'));
  settings.set('memql.lsp.serverPath', { globalValue: '/bundled/memql-lsp' });
  const context = {
    subscriptions: [],
    globalState: { get: () => undefined, update: async () => {} },
  } as unknown as ExtensionContext;
  activate(context);
  assert.deepEqual(constructed, []);
  assert.deepEqual(recorded.watched, []);
  assert.deepEqual(recorded.errors, []);

  for (const doc of [
    document('file:///workspace/main.go', 'go'),
    document('file:///workspace/memql.toml', 'toml'),
    document('memql-cluster://local/core/queries.memql', 'memql'),
    document('memql-artifact://local/example.memql', 'memql'),
    document('untitled:example.memql', 'memql'),
    document('git:/workspace/example.memql', 'memql'),
  ]) openTextDocument(doc);
  assert.deepEqual(constructed, [], 'unrelated and cluster-served documents must not compile the local workspace');
  assert.deepEqual(recorded.watched, []);

  openTextDocument(document('file:///workspace/example.memql', 'memql'));
  openTextDocument(document('file:///workspace/second.memql', 'memql'));
  assert.deepEqual(constructed, ['memql']);
  assert.deepEqual(recorded.watched, ['**/*.memql', '**/memql.toml']);
  assert.deepEqual((instances[0].serverOptions as { run: { args: string[] } }).run.args,
    ['--stdio', '--root', '/workspace']);
  assert.deepEqual((instances[0].clientOptions as { documentSelector: unknown }).documentSelector,
    [{ language: 'memql', scheme: 'file' }]);
});
