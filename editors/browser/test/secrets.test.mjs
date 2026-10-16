import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import ts from 'typescript';
import { indexedDB } from 'fake-indexeddb';
globalThis.indexedDB = indexedDB;
const source = await readFile(new URL('../src/secrets.ts', import.meta.url), 'utf8');
const code = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText;
const { BrowserSecrets, MemorySecrets } = await import('data:text/javascript;base64,' + Buffer.from(code).toString('base64'));
test('concurrent tabs share a persistent key without overwriting credentials', async () => {
  const first = new BrowserSecrets(), second = new BrowserSecrets();
  await Promise.all([first.set('one','first-token'), second.set('two','second-token')]);
  const reopened = new BrowserSecrets();
  assert.equal(await reopened.get('one'), 'first-token');
  assert.equal(await reopened.get('two'), 'second-token');
  await reopened.delete('one');
  assert.equal(await first.get('one'), undefined);
  assert.deepEqual(await reopened.keys(), ['two']);
});
test('stored records contain encrypted bytes and an unexportable key', async () => {
  const db = await new Promise((resolve,reject) => { const r=indexedDB.open('memql-editor-secrets'); r.onsuccess=()=>resolve(r.result); r.onerror=()=>reject(r.error); });
  const read = (store,key) => new Promise(resolve => { const r=db.transaction(store).objectStore(store).get(key); r.onsuccess=()=>resolve(r.result); });
  const key = await read('key','encryption');
  assert.equal(key.extractable,false);
  const record=await read('secrets','two');
  assert.equal(record.iv.length,12);
  assert.notEqual(new TextDecoder().decode(record.bytes),'second-token');
  await assert.rejects(crypto.subtle.exportKey('raw', key));
  db.close();
});

test('test profiles neither read nor change the normal editor credentials', async () => {
  const normal = new BrowserSecrets();
  await normal.set('operator', 'original-session');
  const testProfile = new MemorySecrets();
  assert.equal(await testProfile.get('operator'), undefined);
  await testProfile.set('operator', 'test-session');
  assert.equal(await normal.get('operator'), 'original-session');
  const anotherTest = new MemorySecrets();
  assert.equal(await anotherTest.get('operator'), undefined);
  await testProfile.delete('operator');
  assert.equal(await normal.get('operator'), 'original-session');
});
