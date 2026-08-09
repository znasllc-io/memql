import type { Dispatcher } from "@znasllc-io/memql-sdk-core/client";
import { newShortId, renderMemQLValue } from "@znasllc-io/memql-sdk-core/client";

// The two account-token envelopes, and the one place the portal reaches past
// the SDK's typed wire union.
//
// ===========================================================================
// WHY THE MINT IS AN ENVELOPE AT ALL
// ===========================================================================
// Everything else an operator does to a customer -- create, edit, archive,
// list the credentials -- is an ordinary named query or mutation, run through
// QueryClient.executeNamed like every other read in this app. Two operations
// are not:
//
//   createAccountToken  the plaintext credential exists in EXACTLY ONE PLACE,
//                       the reply. Routing it through anything else is another
//                       place it can be logged, cached or retried. So the
//                       browser calls the mint RPC directly and the value
//                       lives in one React state field until the operator
//                       dismisses it. Same precedent as CreateWorkerTokenMsg.
//   revokeAccountToken  audited, and the audit id comes back on the reply.
//
// ===========================================================================
// WHY THE CAST, AND WHY IT IS EXACTLY HERE
// ===========================================================================
// sdk/ts/src/client/wire.ts is a HAND-MIRRORED closed union of the envelopes
// the SDK models, and `Dispatcher.sendAndWait` is typed against it. The two
// messages below were added to component/grpc/memql.proto by memql#3322 and
// their typed SDK home (a wire.ts union entry plus an
// sdk/ts/src/identity/accountToken.ts wrapper, beside the worker-token one) is
// a follow-up in that package.
//
// Until then the boundary is crossed HERE, in one module, behind two typed
// functions -- rather than at each call site, which is how an untyped
// dispatcher call ends up copy-pasted into a component. The dispatcher itself
// is envelope-agnostic: it correlates purely on messageId / correlateTo and
// hands the reply back verbatim, so an unmodelled payload round-trips
// correctly. What is missing is the compiler's opinion about the field names,
// which is what the interfaces below restate.

export interface AccountTokenMintResult {
  success: boolean;
  // "mql_acct_<43 base64url>". Present only on the mint reply, only once.
  // Never write this to storage, a URL, or anything that outlives the
  // component that received it.
  plainToken: string;
  identityId: string;
  accountId: string;
  // The credential's authenticated SUBJECT: the operator user. Never the
  // account -- nothing authenticates as an account. The server sends it so a
  // client cannot render otherwise without contradicting a field it was
  // handed.
  subjectUserId: string;
  auditEventId: string;
  errorCode: string;
  errorMessage: string;
}

export interface AccountTokenRevokeResult {
  success: boolean;
  auditEventId: string;
  errorCode: string;
  errorMessage: string;
}

// Loose views of the two envelopes. `unknown`-keyed rather than `any` so
// reading a field is a deliberate cast at the read, not an implicit one.
type LooseEnvelope = Record<string, unknown>;

function replyPayload(reply: unknown, key: string): LooseEnvelope | null {
  if (reply === null || typeof reply !== "object") return null;
  const value = (reply as LooseEnvelope)[key];
  if (value === null || typeof value !== "object") return null;
  return value as LooseEnvelope;
}

function str(payload: LooseEnvelope | null, key: string): string {
  const value = payload?.[key];
  return typeof value === "string" ? value : "";
}

async function send(
  dispatcher: Dispatcher,
  envelope: LooseEnvelope,
  signal?: AbortSignal,
): Promise<unknown> {
  // The single cast. See the header.
  return dispatcher.sendAndWait(
    envelope as unknown as Parameters<Dispatcher["sendAndWait"]>[0],
    signal,
  );
}

export async function mintAccountToken(
  dispatcher: Dispatcher,
  args: { accountId: string; label: string; expiresAt?: string; signal?: AbortSignal },
): Promise<AccountTokenMintResult> {
  const requestId = newShortId();
  const reply = await send(
    dispatcher,
    {
      createAccountToken: {
        requestId,
        accountId: args.accountId,
        label: args.label,
        ...(args.expiresAt ? { expiresAt: args.expiresAt } : {}),
      },
    },
    args.signal,
  );

  const error = replyPayload(reply, "queryError");
  if (error) {
    throw new Error(str(replyPayload(error, "error"), "message") || "the cluster refused the mint");
  }
  const payload = replyPayload(reply, "createAccountTokenResult");
  if (payload === null) {
    throw new Error("unexpected reply to createAccountToken");
  }
  return {
    success: payload["success"] === true,
    plainToken: str(payload, "plainToken"),
    identityId: str(payload, "identityId"),
    accountId: str(payload, "accountId"),
    subjectUserId: str(payload, "subjectUserId"),
    auditEventId: str(payload, "auditEventId"),
    errorCode: str(payload, "errorCode"),
    errorMessage: str(payload, "errorMessage"),
  };
}

export async function revokeAccountToken(
  dispatcher: Dispatcher,
  identityId: string,
  signal?: AbortSignal,
): Promise<AccountTokenRevokeResult> {
  const requestId = newShortId();
  const reply = await send(
    dispatcher,
    { revokeAccountToken: { requestId, identityId } },
    signal,
  );

  const error = replyPayload(reply, "queryError");
  if (error) {
    throw new Error(str(replyPayload(error, "error"), "message") || "the cluster refused the revoke");
  }
  const payload = replyPayload(reply, "revokeAccountTokenResult");
  if (payload === null) {
    throw new Error("unexpected reply to revokeAccountToken");
  }
  return {
    success: payload["success"] === true,
    auditEventId: str(payload, "auditEventId"),
    errorCode: str(payload, "errorCode"),
    errorMessage: str(payload, "errorMessage"),
  };
}

// namedCall composes a MemQL named-primitive invocation from typed args.
//
// renderMemQLValue is the SDK's own literal renderer -- the same one the
// generated typed methods use -- so quoting and escaping are not reimplemented
// here. Absent / empty args are dropped rather than sent as "": an omitted
// field on updateAccount keeps its current value (partial read-merge), while an
// empty string would BLANK it, and "the operator cleared the field by not
// typing in it" is the kind of data loss nobody reports as a bug.
export function namedCall(
  kind: "query" | "mutation",
  name: string,
  args: Record<string, string | undefined>,
): string {
  const parts: string[] = [];
  for (const [key, value] of Object.entries(args)) {
    if (value === undefined || value === "") continue;
    parts.push(`${key}: ${renderMemQLValue(value)}`);
  }
  return `${kind} ${name}(${parts.join(", ")})`;
}

// newAccountId mints the opaque shortId half of a new account's row id. The
// canonical `{concept}:{shortId}` composition is the engine's job -- clients
// never compose canonical ids (docs/public/concepts/identifiers.md).
export function newAccountId(): string {
  return newShortId();
}
