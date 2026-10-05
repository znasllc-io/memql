import test from "node:test";
import assert from "node:assert/strict";
import { IdentityAdminClient, IdentityAdminError } from "../src/identityadmin/identityAdmin.js";
import type { Dispatcher } from "../src/client/dispatcher.js";
import type { ClientMessage } from "../src/client/wire.js";

test("access review carries the decision and preserves delivery failures and server refusals", async () => {
  let sent: ClientMessage | undefined;
  let refused = false;
  const dispatcher = { sendAndWait: async (message: ClientMessage) => {
    sent = message;
    return { identityAdminResult: refused
      ? { ok: false, errorCode: 9, errorMessage: "Already reviewed", auditEventId: "audit-2" }
      : { ok: true, message: "Invitation created", auditEventId: "audit-1", invitationUrl: "https://identity.test/invite", invitationEmailSent: false, invitationEmailError: "mail unavailable" } };
  } } as unknown as Dispatcher;
  const client = new IdentityAdminClient(dispatcher);
  const result = await client.reviewAccessRequest("request-1", "approve", "reader", "Approved");
  const request = sent && "identityAdmin" in sent ? sent.identityAdmin : null;
  assert.ok(request && "reviewAccessRequest" in request);
  assert.deepEqual(request.reviewAccessRequest, { requestId: "request-1", decision: "approve", role: "reader", note: "Approved" });
  assert.equal(result.emailSent, false);
  assert.equal(result.emailError, "mail unavailable");
  assert.equal(result.url, "https://identity.test/invite");
  refused = true;
  await assert.rejects(client.reviewAccessRequest("request-1", "reject", "", "Closed"), error => error instanceof IdentityAdminError && error.code === 9);
});
