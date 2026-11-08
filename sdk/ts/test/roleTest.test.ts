import test from "node:test";
import assert from "node:assert/strict";
import { IdentityAdminClient } from "../src/identityadmin/identityAdmin.js";
import type { Dispatcher } from "../src/client/dispatcher.js";

test("role testing's list carries the roles the server offers, each with its labels", async () => {
  // protojson omits empty strings, so a global role arrives with no account
  // and no organization at all.
  const dispatcher = { sendAndWait: async () => ({ identityAdminResult: { ok: true, roleTestRoles: [
    { role: "reader", name: "Viewer" },
    { role: "site-moderator", name: "Site moderator", accountId: "org-fixture", organization: "Acme" },
  ] } }) } as unknown as Dispatcher;
  const result = await new IdentityAdminClient(dispatcher).roleTest("list");
  assert.deepEqual(result.roles, [
    { role: "reader", name: "Viewer", accountId: "", organization: "" },
    { role: "site-moderator", name: "Site moderator", accountId: "org-fixture", organization: "Acme" },
  ]);
  assert.deepEqual(result.sessions, []);
});

test("role testing's list offers nothing when the server offers nothing", async () => {
  const dispatcher = { sendAndWait: async () => ({ identityAdminResult: { ok: true } }) } as unknown as Dispatcher;
  assert.deepEqual((await new IdentityAdminClient(dispatcher).roleTest("list")).roles, []);
});
