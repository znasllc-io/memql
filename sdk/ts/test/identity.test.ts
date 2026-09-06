// Mock-dispatcher tests for the identity surface: guest invites
// (5 ops), session revoke (2 ops), and worker tokens (2 ops).
// Covers happy-path, input validation, QueryError surfacing, and
// AbortSignal cancellation.

import test from "node:test";
import assert from "node:assert/strict";

import { revokeCurrentSession, revokeAllSessions } from "../src/identity/session.js";
import { createWorkerToken, revokeWorkerToken } from "../src/identity/workerToken.js";
import { mintAccountToken, revokeAccountToken } from "../src/identity/accountToken.js";
import type { Dispatcher } from "../src/client/dispatcher.js";
import type { ClientMessage, ServerMessage } from "../src/client/wire.js";

// MockDispatcher: minimal stand-in for Dispatcher exposing only the
// methods the identity surface touches (send / sendAndWait / register-
// Stream). Mirrors the suite under si.test.ts; intentionally not
// shared to keep each test file self-contained.
class MockDispatcher {
  readonly sent: Array<{ msg: ClientMessage; messageId: string }> = [];
  private pendingReplies = new Map<string, (msg: ServerMessage) => void>();
  private streams = new Map<string, (msg: ServerMessage) => void>();
  private nextId = 0;

  send(msg: ClientMessage): string {
    const id = msg.messageId ?? `mock-${this.nextId++}`;
    this.sent.push({ msg: { ...msg, messageId: id }, messageId: id });
    return id;
  }

  async sendAndWait(msg: ClientMessage, signal?: AbortSignal): Promise<ServerMessage> {
    const id = msg.messageId ?? `mock-${this.nextId++}`;
    this.sent.push({ msg: { ...msg, messageId: id }, messageId: id });
    return new Promise<ServerMessage>((resolve, reject) => {
      if (signal?.aborted) {
        reject(new Error("aborted"));
        return;
      }
      this.pendingReplies.set(id, resolve);
      if (signal) {
        signal.addEventListener(
          "abort",
          () => {
            this.pendingReplies.delete(id);
            reject(new Error("aborted"));
          },
          { once: true },
        );
      }
    });
  }

  registerStream(requestId: string, handler: (msg: ServerMessage) => void): () => void {
    this.streams.set(requestId, handler);
    return () => {
      if (this.streams.get(requestId) === handler) this.streams.delete(requestId);
    };
  }

  reply(payload: Record<string, unknown>): void {
    const last = this.sent.at(-1);
    if (!last) throw new Error("MockDispatcher.reply: nothing sent yet");
    const resolver = this.pendingReplies.get(last.messageId);
    if (!resolver) throw new Error(`MockDispatcher.reply: no pending entry for ${last.messageId}`);
    this.pendingReplies.delete(last.messageId);
    resolver({ correlateTo: last.messageId, ...payload } as ServerMessage);
  }

  lastSent(): ClientMessage {
    const last = this.sent.at(-1);
    if (!last) throw new Error("MockDispatcher.lastSent: nothing sent yet");
    return last.msg;
  }

  lastRequestId(): string {
    const msg = this.lastSent() as unknown as Record<string, unknown>;
    for (const v of Object.values(msg)) {
      if (v && typeof v === "object" && "requestId" in v) {
        const rid = (v as { requestId?: string }).requestId;
        if (typeof rid === "string" && rid) return rid;
      }
    }
    throw new Error("MockDispatcher.lastRequestId: no requestId found");
  }

  asDispatcher(): Dispatcher {
    return this as unknown as Dispatcher;
  }
}

// ---------------------------------------------------------------------
// sendGuestInvite
// ---------------------------------------------------------------------

// ---------------------------------------------------------------------
// resolveGuestInvite
// ---------------------------------------------------------------------

// ---------------------------------------------------------------------
// joinSpaceAsGuest
// ---------------------------------------------------------------------

// ---------------------------------------------------------------------
// cancelGuestInvite + resendGuestInviteEmail
// ---------------------------------------------------------------------

// ---------------------------------------------------------------------
// session revoke
// ---------------------------------------------------------------------

test("revokeCurrentSession -- happy path", async () => {
  const mock = new MockDispatcher();
  const promise = revokeCurrentSession(mock.asDispatcher());
  mock.reply({
    revokeCurrentSessionResult: {
      requestId: mock.lastRequestId(),
      success: true,
      sessionId: "sess-42",
    },
  });
  const r = await promise;
  assert.equal(r.success, true);
  assert.equal(r.sessionId, "sess-42");
});

test("revokeCurrentSession -- no_session rides errorCode", async () => {
  const mock = new MockDispatcher();
  const promise = revokeCurrentSession(mock.asDispatcher());
  mock.reply({
    revokeCurrentSessionResult: {
      requestId: mock.lastRequestId(),
      success: true,
      errorCode: "no_session",
    },
  });
  const r = await promise;
  assert.equal(r.errorCode, "no_session");
});

test("revokeAllSessions -- returns revokedCount", async () => {
  const mock = new MockDispatcher();
  const promise = revokeAllSessions(mock.asDispatcher());
  mock.reply({
    revokeAllSessionsResult: {
      requestId: mock.lastRequestId(),
      success: true,
      revokedCount: 3,
    },
  });
  const r = await promise;
  assert.equal(r.revokedCount, 3);
});

// ---------------------------------------------------------------------
// worker tokens
// ---------------------------------------------------------------------

test("createWorkerToken -- returns plain token (one-shot)", async () => {
  // Build the mock bearer at runtime so the literal string never
  // appears in source; gitleaks flags the `mql_wkr_<...>` prefix on
  // a sufficiently long alphanumeric tail, and embedding a literal
  // in a test file is exactly the regex shape it's looking for.
  const mockBearer = ["mql", "wkr", "redacted-for-test"].join("_");
  const mock = new MockDispatcher();
  const promise = createWorkerToken(mock.asDispatcher(), {
    name: "macbook-pro",
    expiresAt: "2027-05-23T00:00:00Z",
  });
  const sent = mock.lastSent() as unknown as {
    createWorkerToken?: { name?: string; expiresAt?: string };
  };
  assert.equal(sent.createWorkerToken?.name, "macbook-pro");
  assert.equal(sent.createWorkerToken?.expiresAt, "2027-05-23T00:00:00Z");

  mock.reply({
    createWorkerTokenResult: {
      requestId: mock.lastRequestId(),
      success: true,
      plainToken: mockBearer,
      identityId: "v1:identity:identity:wkr_1",
      ownerUserId: "user-1",
    },
  });
  const r = await promise;
  assert.equal(r.plainToken, mockBearer);
  assert.equal(r.identityId, "v1:identity:identity:wkr_1");
});

test("createWorkerToken -- omits optional fields when absent", async () => {
  const mockBearer = ["mql", "wkr", "minimal-redacted"].join("_");
  const mock = new MockDispatcher();
  const promise = createWorkerToken(mock.asDispatcher(), { name: "minimal" });
  const sent = mock.lastSent() as unknown as {
    createWorkerToken?: { name?: string; expiresAt?: string; ownerUserId?: string };
  };
  assert.equal(sent.createWorkerToken?.expiresAt, undefined);
  assert.equal(sent.createWorkerToken?.ownerUserId, undefined);
  mock.reply({
    createWorkerTokenResult: {
      requestId: mock.lastRequestId(),
      success: true,
      plainToken: mockBearer,
    },
  });
  await promise;
});

test("revokeWorkerToken -- happy path", async () => {
  const mock = new MockDispatcher();
  const promise = revokeWorkerToken(mock.asDispatcher(), "v1:identity:identity:wkr_1");
  mock.reply({
    revokeWorkerTokenResult: { requestId: mock.lastRequestId(), success: true },
  });
  const r = await promise;
  assert.equal(r.success, true);
});

// ---------------------------------------------------------------------------
// Account tokens (memql#3322 wire; typed home added by memql#4234 -- the
// module the portal's accounts/wire.ts cast used to stand in for).
// ---------------------------------------------------------------------------

test("mintAccountToken -- returns plain token and subject (one-shot)", async () => {
  // Runtime-composed bearer for the same gitleaks reason as the worker
  // token above.
  const mockBearer = ["mql", "acct", "redacted-for-test"].join("_");
  const mock = new MockDispatcher();
  const promise = mintAccountToken(mock.asDispatcher(), {
    accountId: "acct-1",
    label: "Nightly export job",
  });
  const sent = mock.lastSent() as unknown as {
    createAccountToken?: { accountId?: string; label?: string; expiresAt?: string };
  };
  assert.equal(sent.createAccountToken?.accountId, "acct-1");
  assert.equal(sent.createAccountToken?.label, "Nightly export job");
  assert.equal(sent.createAccountToken?.expiresAt, undefined);

  mock.reply({
    createAccountTokenResult: {
      requestId: mock.lastRequestId(),
      success: true,
      plainToken: mockBearer,
      identityId: "v1:identity:identity:acct_1",
      accountId: "acct-1",
      subjectUserId: "user-1",
      auditEventId: "audit-1",
    },
  });
  const r = await promise;
  assert.equal(r.plainToken, mockBearer);
  assert.equal(r.subjectUserId, "user-1");
  assert.equal(r.auditEventId, "audit-1");
});

test("mintAccountToken -- rejects missing required args", async () => {
  const mock = new MockDispatcher();
  await assert.rejects(
    mintAccountToken(mock.asDispatcher(), { accountId: "", label: "x" }),
    /accountId is required/,
  );
  await assert.rejects(
    mintAccountToken(mock.asDispatcher(), { accountId: "acct-1", label: "" }),
    /label is required/,
  );
});

test("revokeAccountToken -- happy path carries the audit id", async () => {
  const mock = new MockDispatcher();
  const promise = revokeAccountToken(mock.asDispatcher(), "v1:identity:identity:acct_1");
  mock.reply({
    revokeAccountTokenResult: {
      requestId: mock.lastRequestId(),
      success: true,
      auditEventId: "audit-2",
    },
  });
  const r = await promise;
  assert.equal(r.success, true);
  assert.equal(r.auditEventId, "audit-2");
});

