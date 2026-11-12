import { act } from "@testing-library/react";
import { useEffect, type ReactNode } from "react";
import { createRoot } from "react-dom/client";

/**
 * Renders `ui` the way a component a read brings in is rendered -- on React's
 * own scheduler, not under act() -- and resolves on the microtask straight
 * after React commits it, before that commit's passive effects have run.
 *
 * THAT MOMENT IS REAL. Testing Library turns act off while a `findBy*` waits,
 * so a list that arrives from a read is committed in one scheduler task and
 * runs its effects in a later one, and whatever happens in between acts on
 * components whose mount effects are still to come. A mount effect that sets
 * state then lands after it. A `findBy*` used to hand a test back in between
 * about once in 300 runs on an idle machine, and far more often on a loaded
 * runner. It now waits for those effects first (test/setup.ts), so this
 * helper is how a test gets there, on purpose and every time. Under act() the
 * effects run with the commit, and the moment does not exist.
 *
 * It proves the moment was reached: a probe committed with `ui` must not have
 * run its effect yet, or this throws rather than let a test pass by never
 * reaching what it is about.
 */
export async function renderBeforeEffects(ui: ReactNode): Promise<{ unmount: () => void }> {
  const environment = globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean };
  const previous = environment.IS_REACT_ACT_ENVIRONMENT;
  const container = document.body.appendChild(document.createElement("div"));
  const root = createRoot(container);
  let effectsRan = false;
  function Probe() {
    useEffect(() => { effectsRan = true; }, []);
    return null;
  }
  environment.IS_REACT_ACT_ENVIRONMENT = false;
  try {
    await new Promise<void>((committed) => {
      const observer = new MutationObserver(() => { observer.disconnect(); committed(); });
      observer.observe(container, { childList: true, subtree: true });
      root.render(<><Probe />{ui}</>);
    });
  } finally {
    environment.IS_REACT_ACT_ENVIRONMENT = previous;
  }
  const unmount = () => { act(() => root.unmount()); container.remove(); };
  if (effectsRan) {
    unmount();
    throw new Error("The commit's effects had already run, so this test no longer reaches the moment it is about.");
  }
  return { unmount };
}
