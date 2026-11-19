import { useEffect, useRef } from "react";
import { useDroppable } from "@dnd-kit/core";
import { Plus } from "lucide-react";

import { surfaceHasContent } from "../system/desktop";
import { DESK_CAP, canAddDesk, type Desk } from "../system/desks";
import { useOs } from "./state";

// The desk pager (spec A): dots inside the dock, between Ask and the clock. Click switches; a window
// dragged onto a dot is thrown there; a full dot dims during a window
// drag; "+" adds a user desk, and is absent while MAX_EMPTY_DESKS desks
// are empty. The active desk is announced politely by the Desktop
// (aria-live), not here.

function PagerDot({ desk, index, active, draggingWindow }: {
  desk: Desk;
  index: number;
  active: boolean;
  draggingWindow: boolean;
}) {
  const { actions } = useOs();
  const full = desk.windows.length >= DESK_CAP;
  const { setNodeRef, isOver } = useDroppable({ id: `pagerdot:${desk.id}`, disabled: full });
  return (
    <button
      ref={setNodeRef}
      type="button"
      className="os-pager-dot"
      data-active={active || undefined}
      data-over={(isOver && !full) || undefined}
      data-refuses={(draggingWindow && full && !active) || undefined}
      aria-label={`Desk ${index + 1}${active ? ", current" : ""}${full ? ", full" : ""}`}
      aria-current={active || undefined}
      onClick={() => actions.switchDesk(desk.id)}
    />
  );
}

function NewDeskDrop({ visible }: { visible: boolean }) {
  const { setNodeRef, isOver } = useDroppable({ id: "pagerdot:new" });
  if (!visible) return null;
  return (
    <span ref={setNodeRef} className="os-pager-new" data-over={isOver || undefined} aria-hidden="true">
      <Plus size={10} aria-hidden />
    </span>
  );
}

// How near (px, from a dot's centre) the mouse starts to grow it.
const NEAR_PX = 44;

/** Grows each dot as the mouse comes near it (--os-near, 0 at NEAR_PX, 1 on
 * top of it), written straight to the dots so a mouse move never re-renders
 * the shell. Mouse only: touch has no hover to anticipate. */
function useNearDots(nav: React.RefObject<HTMLElement | null>) {
  useEffect(() => {
    let frame = 0;
    const measure = (x: number, y: number) => {
      nav.current?.querySelectorAll<HTMLElement>(".os-pager-dot").forEach((dot) => {
        const r = dot.getBoundingClientRect();
        const d = Math.hypot(x - (r.left + r.width / 2), y - (r.top + r.height / 2));
        dot.style.setProperty("--os-near", Math.max(0, 1 - d / NEAR_PX).toFixed(3));
      });
    };
    const move = (event: PointerEvent) => {
      if (event.pointerType !== "mouse") return;
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(() => measure(event.clientX, event.clientY));
    };
    const away = () => measure(-1e5, -1e5);
    window.addEventListener("pointermove", move);
    document.documentElement.addEventListener("pointerleave", away);
    return () => {
      cancelAnimationFrame(frame);
      window.removeEventListener("pointermove", move);
      document.documentElement.removeEventListener("pointerleave", away);
    };
  }, [nav]);
}

export function DeskPager({ draggingWindow }: { draggingWindow: boolean }) {
  const { state, actions } = useOs();
  const navRef = useRef<HTMLElement>(null);
  useNearDots(navRef);
  const { desks, activeDeskId } = state.shell;
  const addable = canAddDesk(state.shell, (deskId) => surfaceHasContent(state.surfaces[deskId]));
  return (
    <nav ref={navRef} className="os-pager" aria-label="Desks">
      {desks.map((desk, index) => (
        <PagerDot
          key={desk.id}
          desk={desk}
          index={index}
          active={desk.id === activeDeskId}
          draggingWindow={draggingWindow}
        />
      ))}
      {draggingWindow ? (
        <NewDeskDrop visible />
      ) : addable ? (
        <button
          type="button"
          className="os-pager-add"
          aria-label="New desk"
          onClick={() => actions.addDesk()}
        >
          <Plus size={10} aria-hidden />
        </button>
      ) : null}
    </nav>
  );
}
