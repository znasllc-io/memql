import { beforeEach, expect, it, vi } from "vitest";
import { LiveVoiceSession } from "../../src/ask/liveVoiceSession";
import { ConversationSession } from "../../src/ask/conversationSession";
import type { AskTransport } from "../../src/ask/askController";

const rtc = vi.hoisted(() => ({
  handlers: new Map<string, () => void>(),
  connect: vi.fn<() => Promise<void>>(),
  disconnect: vi.fn(),
  microphone: vi.fn<() => Promise<void>>(),
}));
vi.mock("livekit-client", () => ({
  Room: class {
    canPlaybackAudio = true;
    localParticipant = { setMicrophoneEnabled: rtc.microphone };
    connect = rtc.connect;
    disconnect = rtc.disconnect;
    startAudio = async () => {};
    on(event: string, handler: () => void) { rtc.handlers.set(event, handler); return this; }
  },
  RoomEvent: { Disconnected: "disconnected", TrackSubscribed: "track", TrackUnsubscribed: "untrack", AudioPlaybackStatusChanged: "playback", DataReceived: "data" },
  Track: { Kind: { Audio: "audio" } },
}));

beforeEach(() => {
  rtc.handlers.clear();
  rtc.connect.mockReset().mockResolvedValue();
  rtc.microphone.mockReset().mockResolvedValue();
  rtc.disconnect.mockReset().mockImplementation(() => rtc.handlers.get("disconnected")?.());
});

function session() {
  const transport: AskTransport = {
    ask: () => ({ cancel() {} }),
    conversations: { list: async () => [], create: async () => ({ id: "conversation", title: "Voice" }), read: async () => [] },
    startVoice: async () => ({ url: "wss://voice.example.com", token: "test-token", room: "test-room" }),
  };
  const conversation = new ConversationSession(transport);
  return { voice: new LiveVoiceSession(transport, conversation), conversation };
}

it("preserves the connect failure when LiveKit emits Disconnected before rejecting", async () => {
  rtc.connect.mockImplementation(async () => {
    rtc.handlers.get("disconnected")?.();
    throw new Error("Could not connect to the signaling server");
  });
  const { voice, conversation } = session();
  await voice.start(null, "female");
  expect(voice.getSnapshot()).toMatchObject({ phase: "off", error: "Could not connect to the signaling server" });
  expect(rtc.microphone).not.toHaveBeenCalled();
  expect(conversation.getSnapshot().voiceActive).toBe(false);
});

it("ends an established call on disconnection", async () => {
  const { voice, conversation } = session();
  await voice.start(null, "male");
  expect(voice.getSnapshot().phase).toBe("listening");
  rtc.handlers.get("disconnected")?.();
  expect(voice.getSnapshot()).toMatchObject({ phase: "off", error: "Voice connection ended. You can reconnect." });
  expect(conversation.getSnapshot().voiceActive).toBe(false);
});

it("does not report an error when the person ends the call", async () => {
  const { voice } = session();
  await voice.start(null, "female");
  voice.stop();
  expect(voice.getSnapshot()).toMatchObject({ phase: "off", error: "" });
});
