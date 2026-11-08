import test from "node:test";
import assert from "node:assert/strict";
import { QueryClient, queryPage, buildSearchUsers, buildPendingUserInvitations } from "../src/client/index.js";
import type { Dispatcher } from "../src/client/dispatcher.js";
import type { ClientMessage } from "../src/client/wire.js";

test("queryPage sends generated search arguments with a bounded order, cursor and cancellation", async () => {
  const sent: Array<{message: ClientMessage; signal?: AbortSignal; priority?: string}> = [];
  const query = new QueryClient({ sendAndWait: async (message: ClientMessage, signal?: AbortSignal, priority?: string) => {
    sent.push({message,signal,priority});
    return {queryResult:{requestId:"r", result:{data:[],meta:{cursor:"next"}}}};
  }} as unknown as Dispatcher);
  const signal = new AbortController().signal;
  const result = await queryPage(query,"searchUsers",buildSearchUsers({search:'name"%_',role:"viewer",memberIds:["member"],showDeactivated:false}),{signal,cursor:"after",priority:"background"});
  assert.equal(sent.length,1);
  assert.equal(sent[0]?.signal,signal);
  assert.equal(sent[0]?.priority,"background");
  const request=(sent[0]?.message as {executeQuery?:{cursor?:string;query?:string}})?.executeQuery;
  assert.equal(request?.cursor,"after");
  assert.equal(request?.query,'paginate(query searchUsers(search: "name\\\"%_", role: "viewer", showDeactivated: false, memberIds: ["member"]), 10)');
  assert.equal(result.meta()?.cursor,"next");
  await queryPage(query,"pendingUserInvitations",buildPendingUserInvitations({search:"needle",unexpired:true}),{pageSize:10});
  assert.match((sent[1]?.message as {executeQuery?:{query?:string}})?.executeQuery?.query??"",/unexpired: true/);
});

test("queryPage rejects invalid bounds, mismatched builders and executable sort text before sending", () => {
  let reads=0;
  const query={executeNamed:async()=>{reads++;throw new Error("should not read");}};
  for (const size of [0,-1,1.5,501]) assert.throws(()=>queryPage(query,"searchUsers",buildSearchUsers({}),{pageSize:size}),/pageSize/);
  assert.throws(()=>queryPage(query,"searchUsers",buildPendingUserInvitations({})),/matching/);
  assert.throws(()=>queryPage(query,"searchUsers",buildSearchUsers({}),{sort:[["name\"),unsafe(","desc"]]}),/field paths/);
  assert.equal(reads,0);
});
