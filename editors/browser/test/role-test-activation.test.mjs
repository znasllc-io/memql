import { test } from 'node:test';
import assert from 'node:assert/strict';
import { build } from 'esbuild';
import { pathToFileURL } from 'node:url';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';

// Exercise the worker entry with the serialized launch manifest. Undeclared
// settings are absent in the worker, as they were in the production failure.
const stub = `
export const state = { commands:new Map(), errors:[], configurationReads:0 };
export class EventEmitter { event=()=>({dispose(){}}); fire(){} dispose(){} }
export class ThemeIcon { constructor(name){this.id=name;} }
export const StatusBarAlignment={Left:1};
export const ProgressLocation={Notification:1};
export const env={openExternal:async()=>{throw new Error('unexpected sign-in');}};
export const Uri={parse:value=>value};
export const window={createStatusBarItem:()=>({show(){},dispose(){}}),registerTreeDataProvider:()=>({dispose(){}}),showErrorMessage:text=>state.errors.push(text)};
export const workspace={isTrusted:false,getConfiguration(){state.configurationReads++;return {get:()=>undefined};}};
export const commands={registerCommand:(name,fn)=>{state.commands.set(name,fn);return {dispose(){}};},executeCommand:async()=>{}};
`;
const built = await build({stdin:{contents:'export {activate} from "../vscode/src/webExtension.ts"; export {state} from "vscode";',resolveDir:path.resolve(import.meta.dirname,'..')},bundle:true,format:'esm',platform:'node',write:false,plugins:[{name:'worker-vscode',setup(b){b.onResolve({filter:/^vscode$/},()=>({path:'vscode',namespace:'stub'}));b.onLoad({filter:/.*/,namespace:'stub'},()=>({contents:stub,loader:'js'}));}}]});
const dir = await mkdtemp(path.join(tmpdir(),'memql-role-worker-'));
const file = path.join(dir,'worker.mjs');
await writeFile(file,built.outputFiles[0].contents);
const {activate,state} = await import(pathToFileURL(file));
await rm(dir,{recursive:true,force:true});
function context(testMode) {
 return {extension:{packageJSON:{memqlRoleTest:testMode}},subscriptions:[],globalState:{get(){throw new Error('owner profile read');},update(){throw new Error('owner profile write');}},secrets:{get:async()=>undefined,store:async()=>{},delete:async()=>{}}};
}
test('worker registers bounded handoff without reading settings or the owner profile', async()=>{
 const ctx=context(true); activate(ctx);
 assert.equal(state.configurationReads,0);
 assert.ok(state.commands.has('memql.web.roleTest'));
 await assert.rejects(state.commands.get('memql.web.roleTest')({token:'invalid',expiresAt:0}),/Invalid role-test handoff/);
 await state.commands.get('memql.clusters.signIn')();
 assert.match(state.errors.at(-1),/role test ended/);
 for(const disposable of ctx.subscriptions) disposable.dispose();
});
test('ordinary web extension does not expose a role-test handoff',()=>{
 state.commands.clear(); const ctx=context(false); activate(ctx);
 assert.equal(state.commands.has('memql.web.roleTest'),false);
 assert.equal(state.configurationReads,0);
 for(const disposable of ctx.subscriptions) disposable.dispose();
});
