import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { AskSurface, ASK_VOICE_FINISHING, ASK_VOICE_HOLD, ASK_VOICE_LATCHED } from "../../src/ask/AskSurface";
import { LATCH_BELOW_MS } from "../../src/ask/voiceSession";
import { StubAskTransport } from "../../src/ask/askController";
import { MicError } from "../../src/ask/micCapture";
import { DEFAULT_ASK_SETTINGS } from "../../src/apps/settings/askSettings";
import type { VoiceCapture, VoicePorts, VoiceTranscriber } from "../../src/ask/voiceSession";

// The surface half: what a person sees and does. The decisions are the pure
// session's (test/ask/voiceSession.test.ts); this pins the wiring -- which
// gesture reaches which method, where the transcript lands, and which
// sentence stands under the input.

class FakeCapture implements VoiceCapture {
  readonly audio = new ReadableStream<Uint8Array>({ start() {} });
  readonly sampleRate = 16000;
  level() {
    return 0.5;
  }
  async stop() {}
}

class FakeWire implements VoiceTranscriber {
  partial: ((text: string) => void) | null = null;
  private settle: ((text: string) => void) | null = null;
  run(
    _audio: ReadableStream<Uint8Array>,
    opts: { sampleRate: number; onPartial: (text: string) => void; signal: AbortSignal },
  ) {
    this.partial = opts.onPartial;
    return new Promise<string>((resolve) => {
      this.settle = resolve;
    });
  }
  complete(text: string) {
    this.settle?.(text);
  }
}

function mount(opts: { commit?: "send" | "review"; ports?: Partial<VoicePorts> } = {}) {
  const wire = new FakeWire();
  // An injected clock, so "held long enough to send" is a decision the test
  // makes rather than a race with the machine it runs on.
  let now = 1000;
  const ports: VoicePorts = {
    openMicrophone: async () => new FakeCapture(),
    transcriber: wire,
    now: () => now,
    ...opts.ports,
  };
  render(
    <AskSurface
      transport={new StubAskTransport(1)}
      voicePorts={ports}
      settings={{ ...DEFAULT_ASK_SETTINGS, commit: opts.commit ?? "send" }}
      variant="sheet"
    />,
  );
  return {
    wire,
    mic: screen.getByRole("button", { name: "Ask by voice" }),
    advance: (ms: number) => {
      now += ms;
    },
  };
}

/** press-and-hold, then let go. jsdom has no pointer capture; the code copes. */
function hold(mic: HTMLElement) {
  fireEvent.pointerDown(mic, { pointerId: 1 });
}
function letGo(mic: HTMLElement) {
  fireEvent.pointerUp(mic, { pointerId: 1 });
}

describe("Ask voice, at the surface", () => {
  it("puts the live transcript in the box and sends it on release", async () => {
    const { wire, mic, advance } = mount({ commit: "send" });
    hold(mic);
    await waitFor(() => expect(screen.getByText(ASK_VOICE_HOLD)).toBeTruthy());

    const field = screen.getByRole("textbox", { name: "Ask" }) as HTMLInputElement;
    act(() => wire.partial?.("show me"));
    act(() => wire.partial?.("show me the fleet"));
    await waitFor(() => expect(field.value).toBe("show me the fleet"));
    // Being written by the microphone, not by the person -- read-only, never
    // disabled, so it stays focusable and selectable.
    expect(field.readOnly).toBe(true);

    advance(LATCH_BELOW_MS + 200);
    letGo(mic);
    expect(screen.getByText(ASK_VOICE_FINISHING)).toBeTruthy();

    act(() => wire.complete("show me the fleet"));
    // The question is asked, so it appears in the log and leaves the box.
    await waitFor(() => expect(screen.getByText("show me the fleet")).toBeTruthy());
    await waitFor(() => expect(field.value).toBe(""));
    expect(field.readOnly).toBe(false);
  });

  it("leaves it in the box when that is the preference", async () => {
    const { wire, mic, advance } = mount({ commit: "review" });
    hold(mic);
    await waitFor(() => expect(screen.getByText(/put it in the box/i)).toBeTruthy());
    advance(LATCH_BELOW_MS + 200);
    letGo(mic);
    act(() => wire.complete("draft this carefully"));

    const field = screen.getByRole("textbox", { name: "Ask" }) as HTMLInputElement;
    await waitFor(() => expect(field.value).toBe("draft this carefully"));
    // Nothing was asked -- the log is still empty and the field is editable.
    expect(screen.queryByText(/Ask is not connected/)).toBeNull();
    expect(field.readOnly).toBe(false);
  });

  it("a tap keeps listening and says how to stop", async () => {
    const { mic } = mount();
    hold(mic);
    await waitFor(() => expect(screen.getByText(ASK_VOICE_HOLD)).toBeTruthy());
    // Released inside the latch window (no clock advanced), so it latches.
    letGo(mic);
    await waitFor(() => expect(screen.getByText(ASK_VOICE_LATCHED)).toBeTruthy());
  });

  it("an interrupted gesture abandons instead of latching a hot microphone", async () => {
    // `pointercancel` means the browser took the gesture over -- on touch,
    // a finger sliding off the mic into a scroll. Treating it as a release
    // would latch, and leave the device open behind a gesture the person
    // abandoned.
    const { wire, mic } = mount();
    hold(mic);
    await waitFor(() => expect(screen.getByText(ASK_VOICE_HOLD)).toBeTruthy());
    act(() => wire.partial?.("never mind"));

    fireEvent.pointerCancel(mic, { pointerId: 1 });

    await waitFor(() => expect(screen.queryByText(ASK_VOICE_HOLD)).toBeNull());
    expect(screen.queryByText(ASK_VOICE_LATCHED)).toBeNull();
    expect(screen.queryByText(ASK_VOICE_FINISHING)).toBeNull();
    expect((mic as HTMLButtonElement).getAttribute("data-voice")).toBe("idle");
  });

  it("a blocked microphone explains itself and leaves typing alone", async () => {
    const { mic } = mount({
      ports: {
        openMicrophone: () => Promise.reject(new MicError("denied", "no")),
        transcriber: new FakeWire(),
      },
    });
    hold(mic);
    await waitFor(() =>
      expect(screen.getByText(/browser is blocking the microphone/i)).toBeTruthy(),
    );

    // The whole point: text still works, and the note is a warning rather
    // than a caption lost in the muted grey.
    const field = screen.getByRole("textbox", { name: "Ask" }) as HTMLInputElement;
    expect(field.readOnly).toBe(false);
    fireEvent.change(field, { target: { value: "type it instead" } });
    expect(field.value).toBe("type it instead");
    expect(
      screen.getByText(/browser is blocking the microphone/i).getAttribute("data-note"),
    ).toBe("problem");
  });

  it("Send while the mic is live finishes the utterance instead of sending half of it", async () => {
    const { wire, mic } = mount();
    hold(mic);
    await waitFor(() => expect(screen.getByText(ASK_VOICE_HOLD)).toBeTruthy());
    act(() => wire.partial?.("half a thought"));

    fireEvent.click(screen.getByRole("button", { name: "Finish" }));
    expect(screen.getByText(ASK_VOICE_FINISHING)).toBeTruthy();
    // Nothing has been asked yet -- the microphone is closing first.
    expect(screen.queryByText(/Ask is not connected/)).toBeNull();
  });
});
