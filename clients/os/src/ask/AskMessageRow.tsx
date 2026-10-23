import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { Check, Ellipsis } from "lucide-react";
import { ContextMenu } from "../chrome/ContextMenu";
import { IconButton } from "../kit/IconButton";
import type { AskTurn } from "./conversationSession";
import "./messageInteractions.css";

/** The visible message is copied independently of its sibling reply/receipt.
 * Events belong to the original request turn: its request, acknowledgment and
 * result share the recorded activity. A feed projection may omit those events,
 * so retain the live turn instead of inferring a finer message/event mapping. */
export function AskMessageRow({
  messageKey, turn, conversationId, role, kind, text, at, avatar, children, onActivity,
}: {
  messageKey: string;
  turn: AskTurn;
  conversationId: string | null;
  role: "user" | "assistant";
  kind: "request" | "acknowledgement" | "response";
  text: string;
  at?: string;
  avatar: ReactNode;
  children: ReactNode;
  onActivity: (turnId: string, messageKey: string) => void;
}) {
  const row = useRef<HTMLDivElement | null>(null);
  const [menu, setMenu] = useState<{ x: number; y: number } | null>(null);
  const [receipt, setReceipt] = useState<{ copied?: string; error?: string }>({});
  const attempt = useRef(0);
  const decay = useRef<ReturnType<typeof setTimeout> | null>(null);
  const closeMenu = useCallback(() => {
    setMenu(null);
    row.current?.querySelector<HTMLButtonElement>("[data-ask-message-menu]")?.focus({ preventScroll: true });
  }, []);

  useEffect(() => () => {
    attempt.current++;
    if (decay.current !== null) clearTimeout(decay.current);
  }, []);
  useEffect(() => {
    if (!menu) return;
    // A fixed portal must not be left behind when its message moves.
    const dismiss = closeMenu;
    window.addEventListener("resize", dismiss);
    window.addEventListener("scroll", dismiss, true);
    return () => {
      window.removeEventListener("resize", dismiss);
      window.removeEventListener("scroll", dismiss, true);
    };
  }, [menu, closeMenu]);

  function openMenu() {
    const rect = row.current?.querySelector("[data-ask-message-menu]")?.getBoundingClientRect();
    if (rect) setMenu({ x: rect.right, y: rect.bottom });
  }

  async function copy(value: string, label: string) {
    const current = ++attempt.current;
    if (decay.current !== null) clearTimeout(decay.current);
    setReceipt({});
    const clipboard = globalThis.navigator?.clipboard;
    if (!clipboard) {
      setReceipt({ error: "This browser offers no clipboard — nothing was copied." });
      return;
    }
    try {
      await clipboard.writeText(value);
      if (attempt.current !== current) return;
      setReceipt({ copied: label });
      decay.current = setTimeout(() => setReceipt({}), 2000);
    } catch {
      if (attempt.current === current) setReceipt({ error: "The browser refused the copy — nothing was copied." });
    }
  }

  function copyDetails() {
    void copy(JSON.stringify({
      conversationId, turnId: turn.id, messageKey, role, kind, text, at: at ?? null,
      state: turn.state, runId: turn.runId, goalId: turn.goalId,
      approvalId: turn.approvalId, route: turn.route,
      attachments: role === "user" ? turn.attachments : undefined,
      routed: turn.routed, activityScope: "turn", activity: turn.activity,
    }, null, 2), "details");
  }

  return <div ref={row} className="os-ask-message" data-menu-open={menu ? true : undefined}
    onKeyDown={event => {
      if (menu && event.key === "Tab") closeMenu();
    }}>
    {avatar}
    <div className="os-ask-message-body">{children}</div>
    <IconButton className="os-ask-message-more" label="Message actions"
      data-ask-message-menu={messageKey}
      aria-haspopup="menu" aria-expanded={menu !== null}
      data-copied={receipt.copied ? true : undefined}
      onClick={() => { if (menu) closeMenu(); else openMenu(); }}
      onKeyDown={event => {
        if (event.key === "ArrowDown" || event.key === "ArrowUp") {
          event.preventDefault(); openMenu();
        }
      }}>
      {receipt.copied ? <Check size={16} aria-hidden /> : <Ellipsis size={16} aria-hidden />}
    </IconButton>
    {menu ? <ContextMenu {...menu} label="Message actions" onClose={closeMenu}
      anchor={row.current?.querySelector<HTMLElement>("[data-ask-message-menu]")} entries={[
      { id: "copy-message", label: "Copy message", disabled: !text, onSelect: () => { void copy(text, "message"); } },
      { id: "copy-details", label: "Copy details", onSelect: copyDetails },
      { id: "activity", label: "View activity", onSelect: () => onActivity(turn.id, messageKey) },
    ]} /> : null}
    {receipt.copied ? <span className="os-sr-only" role="status">Copied {receipt.copied}</span> : null}
    {receipt.error ? <p className="os-copyvalue-note os-ask-copy-error" role="status">{receipt.error}</p> : null}
  </div>;
}
