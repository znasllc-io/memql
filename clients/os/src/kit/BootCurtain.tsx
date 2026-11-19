import { useEffect, useState, useSyncExternalStore } from "react";

// THE FIRST LOAD'S CURTAIN (Jesús, Nov 18 2026): the MemQL mark IS the loading
// bar. It arrives dark; each real step of the first load lights part of it, dot
// by dot along one walk through the mark, and the small dot outside lights last,
// when everything is ready. Then the curtain blooms open onto what is already
// drawn underneath. Under the mark, the current step reads in ink; finished
// steps slide down, shrink and turn muted.
//
// It moves only for real steps -- App reports the identity check, CoreGate the
// cluster's first facts and the drawn desktop -- and never invents progress. On
// a fast connection it catches up at its own pace and the whole thing takes
// about a second. Nothing waits for it: the destination is drawn underneath, and
// once ready the curtain lets clicks through while it blooms. It is the visual
// layer only (aria-hidden); EntryPending underneath keeps the role=status line
// for screen readers. It shows once per page load and never again.

export const BOOT_STEPS = ["Signing you in", "Reaching your cluster", "Loading your desktop", "Ready"] as const;

// The mark's geometry (same as Mark.tsx), its dots numbered in lighting order.
const N: readonly (readonly [number, number])[] = [
  [13.09, 2.52], [4.3, 5.32], [9.45, 6.83], [19.7, 8.76], [2.52, 14.24],
  [13.58, 16.07], [17.92, 17.85], [9.4, 20.7], [21.52, 21.48],
];
const E: readonly (readonly [number, number])[] = [
  [0, 1], [0, 2], [0, 3], [0, 4], [1, 2], [1, 3], [1, 4], [2, 3], [2, 4], [2, 5],
  [3, 5], [3, 6], [3, 7], [4, 5], [4, 7], [5, 6], [5, 7], [6, 7], [6, 8],
];
// One walk through every dot, ending at the outside dot (8), which only touches 6.
const PATH = [0, 1, 4, 2, 3, 5, 7, 6, 8];
// Three of the mark's lines run straight through a third dot; each waits for it too.
const THROUGH: Record<string, number> = { "0-4": 2, "1-3": 2, "3-7": 5 };
// How many dots each step has lit while it is the current one.
const LIT_BY_STEP = [1, 3, 6, 9];
const PACE_MS = 110;
const HOLD_MS = 180;
const BLOOM_MS = 650;
// A curtain nobody finishes must never hold the OS: past this it opens anyway.
const SAFETY_MS = 12_000;

let reached = 0;
const listeners = new Set<() => void>();
const subscribe = (listener: () => void) => { listeners.add(listener); return () => { listeners.delete(listener); }; };

/** A real step of the first load finished: 1 signed in, 2 cluster reached, 3 ready. */
export function bootReached(step: number) {
  const next = Math.min(step, 3);
  if (next <= reached) return;
  reached = next;
  listeners.forEach((listener) => listener());
}
export const bootFinished = () => bootReached(3);
export function resetBootCurtainForTests() { reached = 0; }

const order = PATH.reduce<Record<number, number>>((at, dot, index) => ({ ...at, [dot]: index }), {});
const pathKeys = new Set(PATH.slice(1).map((dot, i) => edgeKey(PATH[i]!, dot)));
function edgeKey(a: number, b: number) { return `${Math.min(a, b)}-${Math.max(a, b)}`; }

export function BootCurtain() {
  const step = useSyncExternalStore(subscribe, () => reached);
  const [reduced] = useState(() => window.matchMedia("(prefers-reduced-motion: reduce)").matches);
  const [gone, setGone] = useState(() => reached >= 3);
  const [lit, setLit] = useState(0);
  const [leaving, setLeaving] = useState(false);
  const target = LIT_BY_STEP[step]!;

  useEffect(() => {
    const timer = setTimeout(bootFinished, SAFETY_MS);
    return () => clearTimeout(timer);
  }, []);
  useEffect(() => {
    if (gone || lit >= target) return;
    const timer = setTimeout(() => setLit((held) => held + 1), lit === 0 || reduced ? 0 : PACE_MS);
    return () => clearTimeout(timer);
  }, [gone, lit, target, reduced]);
  useEffect(() => {
    if (gone || step < 3 || lit < 9) return;
    if (reduced) { setGone(true); return; }
    const bloom = setTimeout(() => setLeaving(true), HOLD_MS);
    const done = setTimeout(() => setGone(true), HOLD_MS + BLOOM_MS);
    return () => { clearTimeout(bloom); clearTimeout(done); };
  }, [gone, step, lit, reduced]);

  if (gone) return null;
  const isLit = (dot: number) => order[dot]! < lit;
  return (
    <div className="os-boot-curtain" data-leaving={leaving || undefined} aria-hidden="true">
      <div className="os-boot-curtain-content">
        <svg className="os-boot-mark" viewBox="0 0 24 24" width={43} height={43}>
          <g className="os-boot-mark-base">
            {E.map(([a, b]) => <line key={edgeKey(a, b)} x1={N[a]![0]} y1={N[a]![1]} x2={N[b]![0]} y2={N[b]![1]} />)}
            {N.map(([x, y], dot) => <circle key={dot} cx={x} cy={y} r={1.53} />)}
          </g>
          <g className="os-boot-mark-lit">
            {PATH.slice(1).map((dot, i) => {
              const from = PATH[i]!;
              return <line key={edgeKey(from, dot)} className="os-boot-path" data-on={i + 1 < lit || undefined}
                pathLength={1} x1={N[from]![0]} y1={N[from]![1]} x2={N[dot]![0]} y2={N[dot]![1]} />;
            })}
            {E.filter(([a, b]) => !pathKeys.has(edgeKey(a, b))).map(([a, b]) => {
              const through = THROUGH[edgeKey(a, b)];
              const on = isLit(a) && isLit(b) && (through === undefined || isLit(through));
              return <line key={edgeKey(a, b)} className="os-boot-edge" data-on={on || undefined}
                x1={N[a]![0]} y1={N[a]![1]} x2={N[b]![0]} y2={N[b]![1]} />;
            })}
            {N.map(([x, y], dot) => <circle key={dot} className="os-boot-dot" data-on={isLit(dot) || undefined} cx={x} cy={y} r={1.53} />)}
          </g>
        </svg>
        <div className="os-boot-steps">
          {BOOT_STEPS.slice(0, step + 1).map((label, index) => (
            <p key={label} className="os-boot-step" data-depth={Math.min(step - index, 3)}>{label}</p>
          ))}
        </div>
      </div>
    </div>
  );
}
