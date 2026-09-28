import { Cloud, Cpu, Layers, Route, Server, Terminal } from "lucide-react";

import type { Readiness, SourceReading } from "./sources";
import type { SourceKind } from "./vocabulary";

// A source is drawn as a GLYPH before it is drawn as a word: a chip on a
// machine for a local model, a prompt for an app, a cloud for a vendor, a
// route for another route. The route list draws a whole chain this way --
// a strip of glyphs rather than a sentence -- so a person reads the shape of a
// route at a glance and opens it for the words.

export function SourceGlyph({ kind, size = 16 }: { kind: SourceKind; size?: number }) {
  switch (kind) {
    case "local":
      return <Cpu size={size} aria-hidden />;
    case "app":
      return <Terminal size={size} aria-hidden />;
    case "vendor":
      return <Cloud size={size} aria-hidden />;
    case "route":
      return <Route size={size} aria-hidden />;
    case "embeddings":
      return <Layers size={size} aria-hidden />;
    default:
      return <Server size={size} aria-hidden />;
  }
}

/** Readiness in the words a slot's accessible name carries. */
export function readinessWords(reading: Pick<SourceReading, "readiness" | "reason">): string {
  return reading.readiness === "ready" ? "ready" : reading.readiness === "unknown" ? "not checked" : reading.reason || "not ready";
}

/** The quiet dot beside a source: filled when ready, hollow when not, absent when unknown. */
export function ReadinessDot({ readiness }: { readiness: Readiness }) {
  if (readiness === "unknown") return null;
  return <span className="fleet-source-dot" data-ready={readiness === "ready" || undefined} aria-hidden />;
}

/**
 * A route's chain as a strip of glyphs, in try order. The serving source is
 * the one drawn in full ink; the ones that cannot serve are quiet. Each glyph
 * carries its source's name for hover and for assistive tech.
 */
export function GlyphStrip({ readings, servingIndex }: { readings: readonly SourceReading[]; servingIndex: number }) {
  return (
    <span className="fleet-glyph-strip" role="list" aria-label="Sources, in order">
      {readings.map((r, i) => (
        <span
          key={`${i}:${r.entry}`}
          role="listitem"
          className="fleet-glyph"
          data-serving={i === servingIndex || undefined}
          data-ready={r.readiness === "ready" || undefined}
          title={`${r.label}: ${readinessWords(r)}`}
          aria-label={`${r.label}, ${readinessWords(r)}`}
        >
          <SourceGlyph kind={r.kind} size={14} />
        </span>
      ))}
    </span>
  );
}
