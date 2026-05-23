// polyphonRoomToken wraps MemqlClientMessage.polyphon_room_token.
// Mints a short-lived LiveKit room token for the caller to join a
// space's voice room as the named participant. The reply carries
// the LiveKit URL alongside the room name so the consumer can hand
// both straight to LiveKit's client SDK without a second config
// call.

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload } from "../client/wire.js";

export interface PolyphonRoomTokenArgs {
  spaceId: string;
  participantId: string;
  displayName: string;
  signal?: AbortSignal;
}

export interface PolyphonRoomTokenResult {
  token: string;
  roomName: string;
  livekitUrl: string;
  // Unix seconds; 0 when the server didn't stamp an expiry. The
  // wire type accepts int64 as string-or-number; this field is
  // already decoded.
  expiresAt: number;
}

export async function polyphonRoomToken(
  dispatcher: Dispatcher,
  args: PolyphonRoomTokenArgs,
): Promise<PolyphonRoomTokenResult> {
  if (!dispatcher) throw new Error("polyphonRoomToken: dispatcher is required");
  if (!args.spaceId) throw new Error("polyphonRoomToken: spaceId is required");
  if (!args.participantId) throw new Error("polyphonRoomToken: participantId is required");
  if (!args.displayName) throw new Error("polyphonRoomToken: displayName is required");

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    {
      polyphonRoomToken: {
        requestId,
        spaceId: args.spaceId,
        participantId: args.participantId,
        displayName: args.displayName,
      },
    },
    args.signal,
  );

  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`polyphonRoomToken: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "polyphonRoomTokenResult") {
    throw new Error("polyphonRoomToken: unexpected reply envelope");
  }
  return {
    token: payload.value.token ?? "",
    roomName: payload.value.roomName ?? "",
    livekitUrl: payload.value.livekitUrl ?? "",
    expiresAt: numberFromWire(payload.value.expiresAt),
  };
}

function numberFromWire(v: string | number | undefined): number {
  if (typeof v === "number") return v;
  if (typeof v === "string" && v) {
    const n = Number(v);
    return Number.isFinite(n) ? n : 0;
  }
  return 0;
}
