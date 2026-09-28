// A dispatcher that answers deploy-control requests the way the engine's
// bridge does, for driving a REAL DeployControlClient in a test.
//
// A real client rather than a fake port, because the defects this exists to
// pin were in what reached the SDK: an empty rollout name is refused by
// `DeployControlClient.rolloutAction` itself (`rollout is required`) before any
// message is sent, and a fake port would accept it and report success. Driving
// the SDK's own argument checks is the only way a test sees what an operator
// saw.
//
// Answers are keyed on the request's oneof field, and every request is kept in
// `sent` in the order it arrived, as the SDK put it on the wire.

import { DeployControlClient } from "@znasllc-io/memql-sdk-core/deploy";

/** One deploy-control request as it left the SDK: the oneof field and its arguments. */
export interface SentDeployRequest {
  field: string;
  args: Record<string, unknown>;
}

/**
 * The reply body for one oneof field. Returned as the `deployControlResult`
 * payload; `requestId` is filled in. An `errorCode` makes the SDK throw a
 * DeployControlError, as a gate refusal would.
 */
export type DeployReply = Record<string, unknown>;

export class DeployDispatcher {
  readonly sent: SentDeployRequest[] = [];
  private readonly replies = new Map<string, DeployReply>();

  /** Sets the answer for every later request carrying `field`. */
  answer(field: string, reply: DeployReply): this {
    this.replies.set(field, reply);
    return this;
  }

  /** The requests that carried `field`, in order. */
  calls(field: string): Record<string, unknown>[] {
    return this.sent.filter((request) => request.field === field).map((request) => request.args);
  }

  async sendAndWait(msg: Record<string, unknown>): Promise<Record<string, unknown>> {
    const envelope = msg["deployControl"] as Record<string, unknown> | undefined;
    if (envelope === undefined) throw new Error("DeployDispatcher: not a deployControl request");
    const { requestId, ...rest } = envelope;
    const [field, args] = Object.entries(rest)[0] ?? ["", {}];
    this.sent.push({ field, args: (args ?? {}) as Record<string, unknown> });
    const reply = this.replies.get(field) ?? { ok: true, action: { ok: true, message: "", auditEventId: "" } };
    return { correlateTo: msg["messageId"], deployControlResult: { requestId, ...reply } };
  }

  send(): string {
    throw new Error("DeployDispatcher: deploy control only uses sendAndWait");
  }

  registerStream(): () => void {
    return () => undefined;
  }

  /** A real SDK client over this dispatcher. */
  client(): DeployControlClient {
    return new DeployControlClient(this as unknown as ConstructorParameters<typeof DeployControlClient>[0]);
  }
}

/** A successful action result, audited as `auditEventId`. */
export function actionOk(auditEventId: string, message = ""): DeployReply {
  return { ok: true, action: { ok: true, message, auditEventId } };
}
