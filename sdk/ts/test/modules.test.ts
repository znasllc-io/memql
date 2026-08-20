// Mock-dispatcher tests for ModulesClient (epic memql#4183): the three
// module-registry envelopes round-trip, wire optionals normalize into the
// SDK-owned value types, payload-level errorCode throws, and the secret
// contract survives normalization -- a secret env var's value stays ""
// even when a (hypothetically misbehaving) server sent one, because the
// UI must never have a value to render for a secret.

import test from "node:test";
import assert from "node:assert/strict";

import { ModulesClient } from "../src/client/modules.js";
import type { Dispatcher } from "../src/client/dispatcher.js";
import type { ClientMessage, ServerMessage } from "../src/client/wire.js";

class MockDispatcher {
  readonly sent: Array<{ msg: ClientMessage; messageId: string }> = [];
  private pendingReplies = new Map<string, (msg: ServerMessage) => void>();
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
    });
  }

  addEventListener(_handler: (msg: ServerMessage) => void): () => void {
    return () => {};
  }

  registerStream(_requestId: string, _handler: (msg: ServerMessage) => void): () => void {
    return () => {};
  }

  reply(payload: Record<string, unknown>): void {
    const last = this.sent.at(-1);
    if (!last) throw new Error("MockDispatcher.reply: nothing sent yet");
    const resolver = this.pendingReplies.get(last.messageId);
    if (!resolver) throw new Error(`MockDispatcher.reply: no pending entry for ${last.messageId}`);
    this.pendingReplies.delete(last.messageId);
    resolver({ correlateTo: last.messageId, ...payload } as ServerMessage);
  }

  asDispatcher(): Dispatcher {
    return this as unknown as Dispatcher;
  }
}

test("listModules -- sends modulesList and normalizes rows", async () => {
  const mock = new MockDispatcher();
  const mc = new ModulesClient(mock.asDispatcher());

  const promise = mc.listModules();
  const sent = mock.sent.at(-1)!.msg as unknown as { modulesList?: { requestId?: string } };
  assert.ok(sent.modulesList, "sends a modulesList payload");
  assert.ok(sent.modulesList.requestId, "carries a request id");

  mock.reply({
    modulesListResult: {
      modules: [
        {
          kind: "pack",
          name: "harness",
          state: "enabled",
          scope: "cluster",
          fqnPrefixes: ["integration.harnessRecall.", "integration.harnessTrace."],
        },
        { kind: "component", name: "identity" },
      ],
      reportingNodeId: "bff-abc",
      reportingNodeType: "bff",
    },
  });

  const inv = await promise;
  assert.equal(inv.modules.length, 2);
  assert.equal(inv.modules[0]!.name, "harness");
  assert.equal(inv.modules[0]!.scope, "cluster");
  assert.deepEqual(inv.modules[1]!.fqnPrefixes, []);
  assert.equal(inv.modules[1]!.stateDetail, "");
  assert.equal(inv.reportingNodeId, "bff-abc");
  assert.equal(inv.reportingNodeType, "bff");
});

test("listModules -- payload-level error throws with the engine's message", async () => {
  const mock = new MockDispatcher();
  const mc = new ModulesClient(mock.asDispatcher());

  const promise = mc.listModules();
  mock.reply({
    modulesListResult: { errorCode: 7, errorMessage: "modules: requires the owner or admin role" },
  });

  await assert.rejects(promise, /requires the owner or admin role/);
});

test("getModuleDetail -- sends (kind, name) and normalizes env vars", async () => {
  const mock = new MockDispatcher();
  const mc = new ModulesClient(mock.asDispatcher());

  const promise = mc.getModuleDetail("integration", "email");
  const sent = mock.sent.at(-1)!.msg as unknown as {
    moduleDetail?: { kind?: string; name?: string };
  };
  assert.equal(sent.moduleDetail?.kind, "integration");
  assert.equal(sent.moduleDetail?.name, "email");

  mock.reply({
    moduleDetailResult: {
      module: { kind: "integration", name: "email", state: "active" },
      envVars: [
        { name: "SMTP_HOST", secret: false, set: true, value: "mail.example.test" },
        { name: "SMTP_PASSWORD", secret: true, set: true },
      ],
      reportingNodeId: "bff-abc",
      reportingNodeType: "bff",
    },
  });

  const detail = await promise;
  assert.equal(detail.module.name, "email");
  assert.equal(detail.envVars.length, 2);
  assert.equal(detail.envVars[0]!.value, "mail.example.test");
  const secret = detail.envVars[1]!;
  assert.equal(secret.secret, true);
  assert.equal(secret.set, true);
  assert.equal(secret.value, "", "a secret entry never carries a value");
});

test("setPackEnabled -- round-trips the flip and keeps restartRequired honest", async () => {
  const mock = new MockDispatcher();
  const mc = new ModulesClient(mock.asDispatcher());

  const promise = mc.setPackEnabled("harness", false, "maintenance window");
  const sent = mock.sent.at(-1)!.msg as unknown as {
    setPackEnabled?: { packDomain?: string; enabled?: boolean; reason?: string };
  };
  assert.equal(sent.setPackEnabled?.packDomain, "harness");
  assert.equal(sent.setPackEnabled?.enabled, false);
  assert.equal(sent.setPackEnabled?.reason, "maintenance window");

  mock.reply({
    setPackEnabledResult: {
      packDomain: "harness",
      priorEnabled: true,
      enabled: false,
      restartRequired: true,
    },
  });

  const outcome = await promise;
  assert.equal(outcome.priorEnabled, true);
  assert.equal(outcome.enabled, false);
  assert.equal(outcome.restartRequired, true);
});

test("setPackEnabled -- owner refusal throws", async () => {
  const mock = new MockDispatcher();
  const mc = new ModulesClient(mock.asDispatcher());

  const promise = mc.setPackEnabled("harness", false, "");
  mock.reply({
    setPackEnabledResult: { errorCode: 7, errorMessage: "set pack enabled: owner only" },
  });

  await assert.rejects(promise, /owner only/);
});
