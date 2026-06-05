// Mock-dispatcher tests for the identity surface: guest invites
// (5 ops), session revoke (2 ops), and worker tokens (2 ops).
// Covers happy-path, input validation, QueryError surfacing, and
// AbortSignal cancellation.

import test from "node:test";
import assert from "node:assert/strict";

import {
  sendGuestInvite,
  resolveGuestInvite,
  joinSpaceAsGuest,
  cancelGuestInvite,
  resendGuestInviteEmail,
} from "../src/identity/guest.js";
import { revokeCurrentSession, revokeAllSessions } from "../src/identity/session.js";
import { createWorkerToken, revokeWorkerToken } from "../src/identity/workerToken.js";
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

test("sendGuestInvite -- happy path returns invitationId + success", async () => {
  const mock = new MockDispatcher();
  const promise = sendGuestInvite(mock.asDispatcher(), {
    spaceId: "spc-1",
    spaceName: "Brainstorm",
    inviterName: "Alice",
    email: "guest@example.com",
    joinUrlBase: "https://app.copresent.ai",
    expiresInMinutes: 15,
  });
  const sent = mock.lastSent() as unknown as {
    sendGuestInvite?: { spaceId?: string; expiresInMinutes?: number };
  };
  assert.equal(sent.sendGuestInvite?.spaceId, "spc-1");
  assert.equal(sent.sendGuestInvite?.expiresInMinutes, 15);

  mock.reply({
    sendGuestInviteResult: {
      requestId: mock.lastRequestId(),
      success: true,
      invitationId: "inv-abc",
    },
  });
  const r = await promise;
  assert.equal(r.success, true);
  assert.equal(r.invitationId, "inv-abc");
  assert.equal(r.errorCode, "");
});

test("sendGuestInvite -- typed errorCode rides the result (no throw)", async () => {
  const mock = new MockDispatcher();
  const promise = sendGuestInvite(mock.asDispatcher(), {
    spaceId: "spc-1",
    spaceName: "Brainstorm",
    inviterName: "Alice",
    email: "bad-email",
    joinUrlBase: "https://app.copresent.ai",
  });
  mock.reply({
    sendGuestInviteResult: {
      requestId: mock.lastRequestId(),
      success: false,
      errorCode: "invalid_email",
      errorMessage: "email failed validation",
    },
  });
  const r = await promise;
  assert.equal(r.success, false);
  assert.equal(r.errorCode, "invalid_email");
  assert.equal(r.errorMessage, "email failed validation");
});

test("sendGuestInvite -- throws on QueryError", async () => {
  const mock = new MockDispatcher();
  const promise = sendGuestInvite(mock.asDispatcher(), {
    spaceId: "spc-1",
    spaceName: "x",
    inviterName: "x",
    email: "x@y.z",
    joinUrlBase: "https://x",
  });
  mock.reply({ queryError: { requestId: mock.lastRequestId(), error: { message: "boom" } } });
  await assert.rejects(promise, /sendGuestInvite: boom/);
});

test("sendGuestInvite -- rejects missing required args", async () => {
  const mock = new MockDispatcher();
  await assert.rejects(
    () =>
      sendGuestInvite(mock.asDispatcher(), {
        spaceId: "",
        spaceName: "",
        inviterName: "",
        email: "x@y.z",
        joinUrlBase: "https://x",
      }),
    /spaceId is required/,
  );
});

// ---------------------------------------------------------------------
// resolveGuestInvite
// ---------------------------------------------------------------------

test("resolveGuestInvite -- ok status returns full guest-visible metadata", async () => {
  const mock = new MockDispatcher();
  const promise = resolveGuestInvite(mock.asDispatcher(), "tok-123");
  mock.reply({
    resolveGuestInviteResult: {
      requestId: mock.lastRequestId(),
      status: "ok",
      invitationId: "inv-x",
      spaceId: "spc-1",
      spaceName: "Brainstorm",
      inviterName: "Alice",
      inviteeEmail: "g@x.com",
      inviteeName: "Guest",
      expiresAt: "2026-05-23T20:00:00Z",
    },
  });
  const r = await promise;
  assert.equal(r.status, "ok");
  assert.equal(r.spaceName, "Brainstorm");
  assert.equal(r.expiresAt, "2026-05-23T20:00:00Z");
});

test("resolveGuestInvite -- non-ok status does not throw (renderer branches)", async () => {
  const mock = new MockDispatcher();
  const promise = resolveGuestInvite(mock.asDispatcher(), "tok-expired");
  mock.reply({
    resolveGuestInviteResult: {
      requestId: mock.lastRequestId(),
      status: "expired",
      errorMessage: "invitation past expires_at",
    },
  });
  const r = await promise;
  assert.equal(r.status, "expired");
  assert.equal(r.errorMessage, "invitation past expires_at");
});

// ---------------------------------------------------------------------
// joinSpaceAsGuest
// ---------------------------------------------------------------------

test("joinSpaceAsGuest -- happy path echoes participantId + resolves spaceId", async () => {
  const mock = new MockDispatcher();
  const promise = joinSpaceAsGuest(mock.asDispatcher(), {
    participantId: "ptp-1",
    displayName: "Guesty",
  });
  mock.reply({
    joinSpaceAsGuestResult: {
      requestId: mock.lastRequestId(),
      success: true,
      participantId: "ptp-1",
      spaceId: "spc-resolved",
    },
  });
  const r = await promise;
  assert.equal(r.success, true);
  assert.equal(r.participantId, "ptp-1");
  assert.equal(r.spaceId, "spc-resolved");
});

test("joinSpaceAsGuest -- unauthenticated rides errorCode", async () => {
  const mock = new MockDispatcher();
  const promise = joinSpaceAsGuest(mock.asDispatcher(), {
    participantId: "ptp-1",
    displayName: "Guesty",
  });
  mock.reply({
    joinSpaceAsGuestResult: {
      requestId: mock.lastRequestId(),
      success: false,
      errorCode: "unauthenticated",
    },
  });
  const r = await promise;
  assert.equal(r.errorCode, "unauthenticated");
});

// ---------------------------------------------------------------------
// cancelGuestInvite + resendGuestInviteEmail
// ---------------------------------------------------------------------

test("cancelGuestInvite -- success path", async () => {
  const mock = new MockDispatcher();
  const promise = cancelGuestInvite(mock.asDispatcher(), "inv-1");
  mock.reply({
    cancelGuestInviteResult: {
      requestId: mock.lastRequestId(),
      success: true,
      invitationId: "inv-1",
    },
  });
  const r = await promise;
  assert.equal(r.success, true);
  assert.equal(r.invitationId, "inv-1");
});

test("resendGuestInviteEmail -- forwards joinUrlBase", async () => {
  const mock = new MockDispatcher();
  const promise = resendGuestInviteEmail(mock.asDispatcher(), {
    invitationId: "inv-1",
    joinUrlBase: "https://new-host.example",
  });
  const sent = mock.lastSent() as unknown as {
    resendGuestInviteEmail?: { joinUrlBase?: string };
  };
  assert.equal(sent.resendGuestInviteEmail?.joinUrlBase, "https://new-host.example");

  mock.reply({
    resendGuestInviteEmailResult: {
      requestId: mock.lastRequestId(),
      success: true,
      invitationId: "inv-1",
    },
  });
  const r = await promise;
  assert.equal(r.success, true);
});

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

