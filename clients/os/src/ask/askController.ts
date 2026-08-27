// Ask's transport seam (spec C/D6). PR A ships the stub; the wire task
// binds sdk-core ai chat behind this exact interface, so the surface
// component never changes when the transport becomes real.

export interface AskCallbacks {
  delta: (text: string) => void;
  done: () => void;
  error: (message: string) => void;
}

export interface AskHandle {
  cancel: () => void;
}

export interface AskTransport {
  /**
   * Stream an answer. `context` is the surface's context tag
   * ("app:artifacts section:browse") or null from the desk/orb.
   */
  ask(prompt: string, context: string | null, on: AskCallbacks): AskHandle;
}

export const ASK_STUB_NOTICE =
  "Ask is not connected to the cluster yet -- the connected transport lands with the live substrate. This surface, its context tags and its streaming path are the real ones.";

/** PR A stand-in: streams the notice word by word so streaming UI is real. */
export class StubAskTransport implements AskTransport {
  constructor(private readonly tickMs = 24) {}

  ask(_prompt: string, context: string | null, on: AskCallbacks): AskHandle {
    const words = (context ? `(${context}) ` : "").concat(ASK_STUB_NOTICE).split(" ");
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
