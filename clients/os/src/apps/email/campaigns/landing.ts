import { useEffect, useRef, useState } from "react";

import type { ActSpec } from "../acts";

// A CHANGE ON ITS WAY (memql#6033).
//
// A write that changes what a campaign is -- sent, scheduled, cancelled, paused,
// resumed, repeating -- returns before the campaign's row does. In between, the
// bar would read the old state and offer its acts again, and a second press
// would be refused over a change that happened ("campaign is already sending"),
// with the page saying it did not.
//
// So from the press that confirmed it, the act stays BUSY and alone in the bar
// until the write is taken AND what it moves has moved -- the campaign's status,
// a series' revision. A write the cluster took whose row never arrives lets go
// after LANDING_MS: the page reads it again itself and stops holding the bar. A
// write refused, or whose answer was lost, lets go at once; the page says which.

/** How long a change the cluster took may take to arrive before the page reads it again itself. */
export const LANDING_MS = 10_000;

/** One change on its way. */
export interface Landing<A extends string> {
  act: A;
  /** The act pressed: held busy, and alone in the bar. */
  spec: ActSpec;
  /** What the change moves -- a status, a revision -- as it stood when it was pressed. */
  from: string;
  /** The cluster took the write; what is left is its row arriving. */
  taken: boolean;
}

export interface LandingControl<A extends string> {
  /** The change on its way, or null. */
  landing: Landing<A> | null;
  /** The confirming press: from here the act is busy and the bar offers nothing else. */
  begin: (act: A, spec: ActSpec) => void;
  /** The cluster took it: the act stays busy until the row moves, or LANDING_MS pass. */
  taken: () => void;
  /** Refused, or its answer lost: nothing is on its way that the bar should wait for. */
  drop: () => void;
}

export function useLanding<A extends string>({ marker, reread, onLanded }: {
  /** What a change of this act moves, as it stands now. */
  marker: (act: A) => string;
  /** Read again what a change of this act moves. */
  reread: (act: A) => void;
  /** The change arrived -- or was read again after LANDING_MS. */
  onLanded?: (act: A) => void;
}): LandingControl<A> {
  const [landing, setLanding] = useState<Landing<A> | null>(null);
  // The callbacks close over each render's rows; a timer set in an earlier one
  // calls the latest.
  const latest = useRef({ reread, onLanded });
  latest.current = { reread, onLanded };
  const arrived = landing !== null && landing.taken && marker(landing.act) !== landing.from;

  useEffect(() => {
    if (!arrived || landing === null) return;
    setLanding(null);
    latest.current.onLanded?.(landing.act);
  }, [arrived, landing]);

  useEffect(() => {
    if (landing === null || !landing.taken) return;
    const timer = setTimeout(() => {
      latest.current.reread(landing.act);
      setLanding(null);
      latest.current.onLanded?.(landing.act);
    }, LANDING_MS);
    return () => clearTimeout(timer);
  }, [landing]);

  return {
    landing,
    begin: (act, spec) => setLanding({ act, spec, from: marker(act), taken: false }),
    taken: () => setLanding((held) => (held === null ? null : { ...held, taken: true })),
    drop: () => setLanding(null),
  };
}
