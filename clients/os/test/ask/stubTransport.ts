import type { AskCallbacks, AskHandle, AskTransport } from "../../src/ask/askController";

const TEST_ANSWER = "This is a streamed answer from the test transport.";

/** Streams a fixture response through the real surface contract. */
export class StubAskTransport implements AskTransport {
  constructor(private readonly tickMs = 24) {}

  ask(_prompt: string, context: string | null, on: AskCallbacks): AskHandle {
    const words = (context ? `(${context}) ` : "").concat(TEST_ANSWER).split(" ");
    let i = 0;
    let cancelled = false;
    const step = () => {
      if (cancelled) return;
      if (i >= words.length) {
        on.done();
        return;
      }
      on.delta((i === 0 ? "" : " ") + words[i]);
      i += 1;
      setTimeout(step, this.tickMs);
    };
    setTimeout(step, this.tickMs);
    return { cancel: () => void (cancelled = true) };
  }
}
