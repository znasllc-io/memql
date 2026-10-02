import { test } from 'node:test';
import assert from 'node:assert/strict';
import { requestedResource, checkTools } from '../src/handoff.mjs';
const resource = 'memql-file://cluster.example/artifacts/file-1/Quarter%20one.md';
const handoff = value => `https://vscode.cluster.example/editor/?resource=${encodeURIComponent(value)}`;
test('opens only a single resource on this installation', () => {
  assert.equal(requestedResource(handoff(resource)), resource);
  assert.equal(requestedResource('https://vscode.cluster.example/editor/'), undefined);
  for (const invalid of [
    resource.replace('cluster.example', 'other.example'), 'file:///etc/passwd', 'command:evil',
    resource + '?token=secret', resource + '#fragment', resource.replace('file-1', 'v1:library:file:x'),
    resource.replace('Quarter%20one.md', '..%2Fsecret'), resource.replace('Quarter%20one.md', 'bad%00.md'),
  ]) assert.throws(() => requestedResource(handoff(invalid)));
  assert.throws(() => requestedResource(handoff(resource) + '&resource=another'));
});
test('requires successful activation of both tools and the shared connection API', async () => {
  for (const result of [undefined, {}, { coreActive: true }, { coreActive: true, productivityActive: true, connectionVersion: 2 }]) {
    await assert.rejects(checkTools(async () => result));
  }
  let called;
  await checkTools(async command => { called = command; return { coreActive: true, productivityActive: true, connectionVersion: 1 }; });
  assert.equal(called, 'memql.productivity.checkTools');
});
test('startup failure or a hung activation reaches the basic-mode choice', async () => {
  await assert.rejects(checkTools(async () => { throw new Error('extension disabled'); }), /extension disabled/);
  await assert.rejects(checkTools(() => new Promise(() => {}), 5), /taking too long/);
});
