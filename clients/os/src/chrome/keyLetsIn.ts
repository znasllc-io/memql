import { useEffect, useState } from "react";

import { useOsConnection } from "../live/connection";

// WHETHER AN API KEY LETS THIS PERSON PAST THE SETUP SCREEN (owner decision
// 2026-11-23; design record 2026-11-21-vendor-accounts-and-api-keys, "Decided
// by the owner after landing").
//
// The setup screen holds while the cluster reaches no model (the `ai` module
// is `unconfigured`). Model access on a cluster is bring-your-own, and an API
// key is a way in that the cluster's readiness does not count: it is one
// person's, and the readiness feed is the cluster's, the same for everybody.
// So the screen also asks, for this person, whether a key of theirs or one
// lent to them can pay on a route they choose. That is `inferenceStatus`'s
// `keyAccount` (component/memql, VendorDoor.Key), the answer Fleet and Ask
// already read.
//
// WHOSE SCREEN. An owner's or developer's: CoreGate asks for them alone. Since
// design record 2026-11-24-desktop-access everybody else enters on the
// per-person access check, which counts a key among their sources, and the
// cluster's verdict holds nobody else.
//
// WHEN IT IS ASKED. Only while the screen would show: the verdict is
// unconfigured and no window is open. A window open in front of the screen is
// one of its own acts at work (adding a key among them), and closing it is the
// moment the answer may have changed, so the question is asked again then.
// Never on a timer: a key is added by the person looking at this screen.
//
// WHAT A FAILED READ MEANS. Before any answer, nothing lets them in: the
// cluster's verdict stands, and the screen stays. After one, a read that fails
// keeps the answer it had, so a dropped connection does not throw somebody
// back out of a desk they were let into.
//
// Regression tests: test/chrome/coreGate.test.tsx, "an API key that can pay".

/** "unknown" until the first answer arrives. */
export type KeyAnswer = "unknown" | "yes" | "no";

export function useKeyLetsIn(asked: boolean, windowsOpen: boolean): KeyAnswer {
  const connection = useOsConnection();
  const [answer, setAnswer] = useState<KeyAnswer>("unknown");

  useEffect(() => {
    if (!asked || windowsOpen) return;
    if (connection === null) {
      setAnswer((prev) => (prev === "unknown" ? "no" : prev));
      return;
    }
    let stale = false;
    const abort = new AbortController();
    connection.query
      .inferenceStatus({}, { signal: abort.signal, priority: "background" })
      .then((result) => {
        if (stale) return;
        const row = result.single();
        setAnswer(row !== null && row["keyAccount"] === true ? "yes" : "no");
      })
      .catch(() => {
        if (!stale) setAnswer((prev) => (prev === "unknown" ? "no" : prev));
      });
    return () => {
      stale = true;
      abort.abort();
    };
  }, [asked, windowsOpen, connection]);

  return answer;
}
