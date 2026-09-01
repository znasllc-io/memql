import { useEffect, useRef } from "react";

import { DEFAULT_FIELD, dotAt, generateField, type Field, type FieldOptions } from "./field";

// The wallpaper canvas. Reads its colors from the theme tokens at paint
// time (so a theme pack restyles it with zero code), stills itself under
// prefers-reduced-motion, and pauses entirely while the tab is hidden.
// Deliberately throttled HARD: every repaint dirties the frame beneath the
// glass chrome, and each dirty frame re-runs the backdrop blur for every
// window above it, so the repaint interval is a direct multiplier on GPU
// churn whenever windows are open. The drift is 0.05 rad/s -- one frame
// per second is already more temporal resolution than the motion has.

const FRAME_MS = 1000;

function fieldColors(el: HTMLElement): { dot: string; link: string } {
  const styles = getComputedStyle(el);
  return {
    dot: styles.getPropertyValue("--os-field-dot").trim() || "rgba(128,128,128,0.05)",
    link: styles.getPropertyValue("--os-field-link").trim() || "rgba(128,128,128,0.04)",
  };
}

function paint(canvas: HTMLCanvasElement, field: Field, t: number): void {
  let ctx: CanvasRenderingContext2D | null = null;
  try {
    ctx = canvas.getContext("2d");
  } catch {
    return; // jsdom (and some capture contexts) have no canvas backend
  }
  if (!ctx) return;
  const { dot, link } = fieldColors(canvas);
  const dpr = window.devicePixelRatio || 1;
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, canvas.width / dpr, canvas.height / dpr);

  ctx.strokeStyle = link;
  ctx.lineWidth = 1;
  for (const l of field.links) {
    const fromDot = field.dots[l.from];
    const toDot = field.dots[l.to];
    if (!fromDot || !toDot) continue;
    const a = dotAt(fromDot, t);
    const b = dotAt(toDot, t);
    ctx.beginPath();
    ctx.moveTo(a.x, a.y);
    ctx.lineTo(b.x, b.y);
    ctx.stroke();
  }

  ctx.fillStyle = dot;
  for (const d of field.dots) {
    const p = dotAt(d, t);
    ctx.beginPath();
    ctx.arc(p.x, p.y, d.r, 0, Math.PI * 2);
    ctx.fill();
  }
}

/**
 * The memory field.
 *
 * `seed` and `field` come from the ACTIVE THEME PACK since epic memql#4745 --
 * a theme is its token set plus its wallpaper parameters, and the wallpaper
 * is the largest surface any of them touches. The defaults are graphite's, so
 * a caller that passes nothing paints exactly what the foundation shipped.
 */
export function MemoryField({
  seed = 9,
  field: options = DEFAULT_FIELD,
}: {
  seed?: number;
  field?: FieldOptions;
}) {
  const canvasRef = useRef<HTMLCanvasElement | null>(null);

  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;

    let field: Field = { dots: [], links: [] };
    let raf = 0;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let disposed = false;
    const reduced = window.matchMedia("(prefers-reduced-motion: reduce)");

    const resize = () => {
      const parent = canvas.parentElement;
      if (!parent) return;
      const dpr = window.devicePixelRatio || 1;
      const w = parent.clientWidth;
      const h = parent.clientHeight;
      canvas.width = Math.max(1, Math.floor(w * dpr));
      canvas.height = Math.max(1, Math.floor(h * dpr));
      canvas.style.width = `${w}px`;
      canvas.style.height = `${h}px`;
      field = generateField(seed, w, h, options);
      paint(canvas, field, 0);
    };

    const loop = () => {
      if (disposed || document.hidden || reduced.matches) return;
      timer = setTimeout(() => {
        raf = requestAnimationFrame(() => {
          paint(canvas, field, performance.now() / 1000);
          loop();
        });
      }, FRAME_MS);
    };

    const wake = () => {
      if (disposed) return;
      paint(canvas, field, reduced.matches ? 0 : performance.now() / 1000);
      loop();
    };

    resize();
    loop();

    const onVisibility = () => wake();
    const observer = new ResizeObserver(resize);
    if (canvas.parentElement) observer.observe(canvas.parentElement);
    document.addEventListener("visibilitychange", onVisibility);
    reduced.addEventListener("change", wake);
    // Mode flips (data-theme) change the token colors under us.
    const themeWatch = new MutationObserver(() => paint(canvas, field, 0));
    themeWatch.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme", "data-os-theme"] });

    return () => {
      disposed = true;
      if (timer) clearTimeout(timer);
      cancelAnimationFrame(raf);
      observer.disconnect();
      themeWatch.disconnect();
      document.removeEventListener("visibilitychange", onVisibility);
      reduced.removeEventListener("change", wake);
    };
  }, [seed, options]);

  return <canvas ref={canvasRef} className="os-field" data-os-field aria-hidden="true" />;
}

/** The ghost desk numeral -- Squada One, one of its three appearances. */
export function DeskNumeral({ index }: { index: number }) {
  return (
    <div className="os-desk-numeral" data-os-desk-numeral aria-hidden="true">
      {String(index + 1).padStart(2, "0")}
    </div>
  );
}
