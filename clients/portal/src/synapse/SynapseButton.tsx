import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";

import { useCluster } from "../cluster/ClusterProvider";
import { Button, TextInput, useReducedMotion } from "../ui";
import { coercePatches } from "./patches";
import { useSynapse } from "./SynapseProvider";
import { describeFloat, floatFor, recordUsage, type TokenFloat } from "./tokens";

// Synapse: one floating affordance, on every routed page (decisions D6-D8).
//
// ===========================================================================
// THE HARD RULES, WHICH ARE THE WHOLE DESIGN
// ===========================================================================
//   * It NEVER submits and NEVER navigates. The reply fills fields in a draft
//     the person then reads and sends themselves. Manual input stays primary
//     everywhere; this is a shortcut into a form, not a way past it.
//   * It acts ONLY on the active scope, and the popover names that scope in
//     words before you type, so the target is never a surprise.
//   * Every fire renders its cost. See tokens.ts for why.
//   * It IDLES STILL. The ring turns on hover, while listening and while a
//     request is running -- three states a person caused -- and at rest it is
//     a mark on a page, not a thing demanding attention.
//
// ===========================================================================
// THE IMPULSE GLYPH (D7)
// ===========================================================================
// A filled node with a single ring around it: one node of the product's own
// mark, with the pulse it would send. Not a sparkle, not a robot, not a
// wand -- the three shapes every other product uses for this, none of which
// say anything about what happens when you press it.

const HOLD_MS = 350;

type Phase = "idle" | "listening" | "transcribing" | "running";

export function SynapseButton(): ReactNode {
  const synapse = useSynapse();
  const { clients } = useCluster();
  const reduced = useReducedMotion();

  const [open, setOpen] = useState(false);
  const [prompt, setPrompt] = useState("");
  const [phase, setPhase] = useState<Phase>("idle");
  const [status, setStatus] = useState("");
  const [float, setFloat] = useState<{ key: number; value: TokenFloat } | null>(null);

  const holdTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const recorder = useRef<MediaRecorder | null>(null);
  const chunks = useRef<Blob[]>([]);
  const floatKey = useRef(0);

  const active = synapse?.active;
  const scopeLabel = active?.label ?? "";

  const stopHoldTimer = (): void => {
    if (holdTimer.current !== null) {
      clearTimeout(holdTimer.current);
      holdTimer.current = null;
    }
  };

  const run = useCallback(
    async (text: string) => {
      const scope = synapse?.active;
      if (scope === undefined || clients === null) return;
      const trimmed = text.trim();
      if (trimmed === "") return;

      // THE COST IS SHOWN ON FIRE, not on reply. It is what the person just
      // spent by pressing, and showing it only on success would hide the
      // spend of every call that failed.
      const request = {
        scope: {
          id: scope.id,
          label: scope.label,
          fields: scope.fields.map((field) => ({
            name: field.name,
            type: field.type,
            label: field.label ?? "",
            value: field.value ?? "",
            constraints: constraintsFor(field),
          })),
        },
        prompt: trimmed,
        page: scope.label,
      };
      const shown = floatFor(scope.id, JSON.stringify(request).length);
      floatKey.current += 1;
      setFloat({ key: floatKey.current, value: shown });
      setStatus(describeFloat(shown));

      setPhase("running");
      try {
        const reply = await clients.suggest("uiAssist", request as unknown as Record<string, unknown>);
        const result = (reply.result ?? {}) as Record<string, unknown>;
        const patches = coercePatches(result["patches"], scope.fields);

        const usage = Number((result as { usage?: { totalTokens?: unknown } }).usage?.totalTokens);
        if (Number.isFinite(usage) && usage > 0) recordUsage(scope.id, usage);

        // APPLY, and nothing else. No submit, no navigate -- there is nothing
        // here that could.
        scope.apply(patches);

        const note = typeof result["note"] === "string" ? result["note"].trim() : "";
        setStatus(
          patches.length === 0
            ? note || "Nothing in that could be filled in here."
            : `${patches.length} ${patches.length === 1 ? "field" : "fields"} filled${note === "" ? "." : `. ${note}`}`,
        );
        setPrompt("");
      } catch (error: unknown) {
        // The sentence, not the raw string: this is a status line inside a
        // popover, and it follows the same rule every other error render does.
        setStatus("That did not run. Nothing was filled in; try again.");
        void error;
      } finally {
        setPhase("idle");
      }
    },
    [synapse, clients],
  );

  const startListening = useCallback(async () => {
    if (clients === null) return;
    try {
      const media = await globalThis.navigator?.mediaDevices?.getUserMedia({ audio: true });
      if (media === undefined) return;
      const rec = new MediaRecorder(media);
      chunks.current = [];
      rec.ondataavailable = (event) => {
        if (event.data.size > 0) chunks.current.push(event.data);
      };
      rec.onstop = () => {
        for (const track of media.getTracks()) track.stop();
        void transcribeAndFill();
      };
      recorder.current = rec;
      rec.start();
      setOpen(true);
      setPhase("listening");
      setStatus(`Listening. Release to fill ${scopeLabel || "this page"}.`);
    } catch {
      // A refused microphone is a decision the person made, not a failure to
      // report as one. Typing is right there.
      setOpen(true);
      setStatus("No microphone available. Type instead.");
    }
    async function transcribeAndFill(): Promise<void> {
      if (clients === null) return;
      setPhase("transcribing");
      setStatus("Working out what you said…");
      try {
        const blob = new Blob(chunks.current, { type: chunks.current[0]?.type ?? "audio/webm" });
        const bytes = new Uint8Array(await blob.arrayBuffer());
        const { text } = await clients.transcribe(bytes, { mimeType: blob.type });
        // THE TRANSCRIPT IS SHOWN AND EDITABLE, always. Voice that ran
        // straight through would be a black box: a person could not tell a
        // bad fill from a misheard word, and the two have different remedies.
        setPrompt(text);
        setStatus(text.trim() === "" ? "Nothing was heard. Try again, or type it." : "Check it, then press Fill.");
      } catch {
        setStatus("That could not be transcribed. Type it instead.");
      } finally {
        setPhase("idle");
      }
    }
  }, [clients, scopeLabel]);

  const stopListening = useCallback(() => {
    const rec = recorder.current;
    recorder.current = null;
    if (rec !== null && rec.state !== "inactive") rec.stop();
  }, []);

  useEffect(() => () => stopHoldTimer(), []);

  // THE STATUS BELONGS TO ONE SCOPE. Found in the visual QA pass (memql#4660):
  // fill a form, navigate, and the popover still read "1 field filled." beside
  // "Nothing on this page can be filled in" -- a report about a section that
  // is no longer on screen, next to a sentence saying there is none.
  //
  // The PROMPT is deliberately kept. What a person typed is still what they
  // want, and the scope also changes just by moving the pointer between two
  // sections of one page -- clearing their words on a hover would be worse
  // than the staleness this fixes.
  useEffect(() => {
    setStatus("");
    setFloat(null);
  }, [synapse?.activeId]);

  // Nothing to offer and nothing to say: a page outside a cluster connection.
  if (clients === null) return null;

  const busy = phase === "running" || phase === "transcribing";

  return (
    // `self-end` on the button rather than `items-end` on this column. In a
    // flex COLUMN that alignment is horizontal, so the kit's items-end ban --
    // which is about the tallest child in a form ROW deciding where everyone
    // else's bottom edge lands -- does not apply here; expressing it per child
    // says which element actually needs it and keeps the guard's allowlist
    // from growing an entry that explains a rule not being broken.
    <div className="fixed right-6 bottom-6 z-30 flex flex-col gap-2">
      {open ? (
        <div
          // border-line-strong, matching the button: this is a card FLOATING
          // over the page rather than a panel sitting in it, and the kit has
          // no shadows by rule -- so the line is the only thing that can say
          // it is in front. A hairline read as a seam in the page behind it.
          className="w-80 self-end rounded-lg border border-line-strong bg-surface p-3"
          role="dialog"
          aria-label="Synapse"
        >
          <p className="mb-2 text-xs text-muted">
            {active === undefined ? (
              // The button never pretends. With no fillable section on the
              // page it says so, rather than offering to fill something.
              <>Nothing on this page can be filled in. Open the guide, or move to a form.</>
            ) : (
              <>
                Filling <span className="font-medium text-fg">{active.label}</span>
              </>
            )}
          </p>
          <TextInput
            value={prompt}
            onChange={setPrompt}
            placeholder={active === undefined ? "Nothing to fill here" : "Say what you want in it…"}
            ariaLabel="What to fill in"
            disabled={active === undefined || busy}
            onKeyDown={(event) => {
              if (event.key === "Enter") {
                event.preventDefault();
                void run(prompt);
              } else if (event.key === "Escape") {
                event.preventDefault();
                setOpen(false);
              }
            }}
          />
          <div className="mt-2 flex items-center justify-between gap-2">
            {/* The SAME sentence the float renders, for a reader who cannot
                see a number rise off a button. */}
            <p role="status" className="min-w-0 flex-1 truncate text-xs text-subtle">
              {status}
            </p>
            <Button
              size="xs"
              onClick={() => void run(prompt)}
              disabled={active === undefined || prompt.trim() === ""}
              busy={busy}
              busyLabel="Filling…"
            >
              Fill
            </Button>
          </div>
        </div>
      ) : null}

      <div className="relative self-end">
        {float === null ? null : (
          <span
            key={float.key}
            // aria-hidden: the popover's status line carries the same fact in
            // a sentence, and a number that flies off a button is not
            // something to announce twice.
            aria-hidden="true"
            className={
              "pointer-events-none absolute -top-6 right-2 text-xs font-semibold text-danger " +
              (reduced ? "synapse-float-static" : "synapse-float")
            }
            onAnimationEnd={() => setFloat(null)}
          >
            {float.value.firstRun ? "first run" : `${float.value.estimated ? "~" : ""}${float.value.tokens}`}
          </span>
        )}
        <button
          type="button"
          aria-label={
            active === undefined
              ? "Synapse -- nothing on this page can be filled in"
              : `Synapse -- fill ${active.label}`
          }
          aria-expanded={open}
          title="Click to type, hold to speak"
          data-synapse-phase={phase}
          onPointerDown={() => {
            stopHoldTimer();
            holdTimer.current = setTimeout(() => {
              holdTimer.current = null;
              void startListening();
            }, HOLD_MS);
          }}
          onPointerUp={() => {
            if (holdTimer.current !== null) {
              // Released before the hold threshold: a click, which opens the
              // popover to type.
              stopHoldTimer();
              setOpen((was) => !was);
              return;
            }
            stopListening();
          }}
          onPointerLeave={() => {
            stopHoldTimer();
            stopListening();
          }}
          // TUNED AT REAL SIZE in the visual QA pass (decision D7 defers this
          // here on purpose). Two things were wrong in the first cut and only
          // a browser could show them: a hairline `border-line` on white made
          // the button read as a smudge rather than a control at 44px, and a
          // 20px glyph inside it left the ring mostly empty air.
          className="synapse-button motion-wash flex h-11 w-11 items-center justify-center rounded-full border border-line-strong bg-surface text-accent hover:border-accent hover:bg-accent-subtle"
        >
          <Impulse />
        </button>
      </div>
    </div>
  );
}

// The Impulse: a filled node and the single ring it sends. The ring is a
// separate element so the stylesheet can animate it alone -- the node never
// moves, which is what "still at idle" means at this size.
function Impulse(): ReactNode {
  return (
    <svg viewBox="0 0 24 24" width={22} height={22} aria-hidden="true">
      <circle className="synapse-ring" cx="12" cy="12" r="8.5" fill="none" stroke="currentColor" strokeWidth="1.6" />
      <circle cx="12" cy="12" r="3.8" fill="currentColor" />
    </svg>
  );
}

// What the model is told about a field's allowed values, in one clause.
function constraintsFor(field: {
  options?: readonly string[];
  constraints?: string;
}): string {
  const parts: string[] = [];
  if (field.options !== undefined && field.options.length > 0) {
    parts.push(`one of: ${field.options.join(", ")}`);
  }
  if (field.constraints !== undefined && field.constraints !== "") parts.push(field.constraints);
  return parts.join("; ");
}
