import { useDroppable } from "@dnd-kit/core";
import { Plus } from "lucide-react";

import { surfaceHasContent } from "../system/desktop";
import { DESK_CAP, canAddDesk, type Desk } from "../system/desks";
import { useOs } from "./state";

// The desk pager (spec A): dots above the dock. Click switches; a window
// dragged onto a dot is thrown there; a full dot dims during a window
// drag; "+" adds a user desk, and is greyed and inert while MAX_EMPTY_DESKS
// desks are empty. The active desk is announced politely by the Desktop
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

export function DeskPager({ draggingWindow }: { draggingWindow: boolean }) {
  const { state, actions } = useOs();
  const { desks, activeDeskId } = state.shell;
  const addable = canAddDesk(state.shell, (deskId) => surfaceHasContent(state.surfaces[deskId]));
  return (
    <nav className="os-pager" aria-label="Desks">
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
      ) : (
        // Always there, so the way to a new desk never disappears (Jesús, Nov
        // 2026); greyed and inert while MAX_EMPTY_DESKS desks are empty.
        <button
          type="button"
          className="os-pager-add"
          aria-label="New desk"
          aria-disabled={!addable || undefined}
          title={addable ? undefined : "Use an empty desk first"}
          onClick={() => { if (addable) actions.addDesk(); }}
        >
          <Plus size={10} aria-hidden />
        </button>
      )}
    </nav>
  );
}
