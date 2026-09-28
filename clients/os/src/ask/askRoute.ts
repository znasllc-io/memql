// An Ask conversation's route: WHERE its replies are answered and with how
// much EFFORT (design brief "routing in MemQL OS", section 6).
//
// ===========================================================================
// THE WIRE IS TWO STRINGS, AND EMPTY IS AUTO
// ===========================================================================
// AiChatMsg.provider names the source ("app:claude-code", "fleet:strongest",
// "federation:cheapest", ...) and AiChatMsg.level the level ("fast",
// "strong", "reasoning"). Either left empty means the cluster's own rules
// decide, which is what Auto IS -- so Auto is not a value this client
// invents, it is the absence of a choice, and the SDK omits it from the wire.
//
// The picker offers five rows, and three of them are exactly one selector.
// Local and Vendor are each ONE row over two selectors, and the Effort names
// which end serves: Fast asks for the fastest local model, and only Strong or
// Reasoning is worth the strongest (most expensive) vendor. The mapping lives
// here, once, so the pill, the picker and the store cannot disagree about it.
//
// ===========================================================================
// THE CHOICE IS A PER-VIEWER CONVENIENCE, KEPT PER CONVERSATION
// ===========================================================================
// Transcripts live on the cluster; which source this viewer asked a
// conversation to use is a preference of theirs, kept in this browser under a
// versioned key with every storage touch in a try/catch -- the discipline
// apps/settings/askSettings.ts and apps/fleet/settings.ts already follow. A
// conversation with nothing stored is on Auto, which is also where every new
// conversation starts.

export type AskLevel = "" | "fast" | "strong" | "reasoning";

export interface AskRouting {
  /** The source selector, sent as AiChatMsg.provider. "" is Auto. */
  source: string;
  /** The level, sent as AiChatMsg.level. "" is Auto. */
  level: AskLevel;
}

export const AUTO_ROUTING: AskRouting = Object.freeze({ source: "", level: "" }) as AskRouting;

/** The picker's Where rows, in the order it offers them. */
export type RouteWhere = "auto" | "local" | "claude-code" | "codex" | "vendor";
export const ROUTE_WHERE: readonly RouteWhere[] = ["auto", "local", "claude-code", "codex", "vendor"];

/** The Effort control's options, in order. */
export const ASK_LEVELS: readonly { value: AskLevel; label: string }[] = [
  { value: "", label: "Auto" },
  { value: "fast", label: "Fast" },
  { value: "strong", label: "Strong" },
  { value: "reasoning", label: "Reasoning" },
];

const LEVEL_VALUES = new Set<string>(ASK_LEVELS.map((level) => level.value));

export const WHERE_LABEL: Record<RouteWhere, string> = {
  auto: "Auto",
  local: "Local",
  "claude-code": "Claude Code",
  codex: "Codex",
  vendor: "Vendor",
};

/** The selector a Where row sends at this Effort. */
export function routingFor(where: RouteWhere, level: AskLevel): AskRouting {
  switch (where) {
    case "auto":
      return { source: "", level };
    case "claude-code":
      return { source: "app:claude-code", level };
    case "codex":
      return { source: "app:codex", level };
    case "local":
      return { source: level === "fast" ? "fleet:fastest" : "fleet:strongest", level };
    case "vendor":
      return {
        source: level === "strong" || level === "reasoning" ? "federation:strongest" : "federation:cheapest",
        level,
      };
  }
}

/** Which Where row a selector belongs to; null for one this picker never writes. */
export function whereOf(source: string): RouteWhere | null {
  if (source === "") return "auto";
  if (source === "app:claude-code") return "claude-code";
  if (source === "app:codex") return "codex";
  if (source.startsWith("fleet:")) return "local";
  if (source.startsWith("federation:")) return "vendor";
  return null;
}

function levelLabel(level: AskLevel): string {
  return ASK_LEVELS.find((option) => option.value === level)?.label ?? "Auto";
}

/** What the pill says: "Auto", or the source's short name, plus the level
 *  when it is not Auto ("Claude Code · Strong"). */
export function routeLabel(routing: AskRouting): string {
  const where = whereOf(routing.source);
  // A concrete model or a named route is its own short name -- the part
  // after the kind, which is what a person chose it by.
  const name = where === null || (where === "local" && !["fleet:strongest", "fleet:fastest"].includes(routing.source))
    ? routing.source.slice(routing.source.indexOf(":") + 1)
    : WHERE_LABEL[where];
  return routing.level ? `${name} · ${levelLabel(routing.level)}` : name;
}

/** Repair a parsed value to a routing, field by field; garbage reads as Auto. */
export function sanitizeRouting(raw: unknown): AskRouting {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return { ...AUTO_ROUTING };
  const value = raw as Record<string, unknown>;
  return {
    source: typeof value.source === "string" ? value.source.trim() : "",
    level: typeof value.level === "string" && LEVEL_VALUES.has(value.level) ? (value.level as AskLevel) : "",
  };
}

export function isAuto(routing: AskRouting): boolean {
  return routing.source === "" && routing.level === "";
}

export interface AskRouteStore {
  load(conversationId: string): AskRouting;
  save(conversationId: string, routing: AskRouting): void;
}

export const ASK_ROUTES_KEY = "memql-os-ask-routes-v1";
/** Enough for every conversation somebody is plausibly still in. */
const ASK_ROUTES_KEPT = 200;

interface RoutesDocument {
  version: 1;
  /** Insertion order is recency: a save moves its conversation to the end. */
  routes: Record<string, AskRouting>;
}

export class LocalAskRouteStore implements AskRouteStore {
  /**
   * What THIS page chose, Auto included, most recent last. Storage can refuse
   * a write (a private window, blocked site data, a full quota) and a reload
   * of the conversation must not then drop the choice back to Auto, so a
   * choice made here answers before whatever storage holds -- for as long as
   * the page lives.
   */
  private readonly session = new Map<string, AskRouting>();

  constructor(
    private readonly storage: Storage | null = safeStorage(),
    private readonly kept = ASK_ROUTES_KEPT,
  ) {}

  load(conversationId: string): AskRouting {
    const chosen = this.session.get(conversationId);
    if (chosen) return { ...chosen };
    const stored = this.read().routes[conversationId];
    return stored ? sanitizeRouting(stored) : { ...AUTO_ROUTING };
  }

  save(conversationId: string, routing: AskRouting): void {
    if (!conversationId) return;
    const choice = { source: routing.source, level: routing.level };
    this.session.delete(conversationId);
    this.session.set(conversationId, choice);
    for (const id of [...this.session.keys()].slice(0, Math.max(0, this.session.size - this.kept))) this.session.delete(id);
    const doc = this.read();
    delete doc.routes[conversationId];
    // Auto is the absence of a choice, so it is stored as nothing.
    if (!isAuto(choice)) doc.routes[conversationId] = choice;
    const ids = Object.keys(doc.routes);
    for (const id of ids.slice(0, Math.max(0, ids.length - this.kept))) delete doc.routes[id];
    try {
      this.storage?.setItem(ASK_ROUTES_KEY, JSON.stringify(doc));
    } catch {
      // Refused: the session map above still holds it for this page.
    }
  }

  private read(): RoutesDocument {
    try {
      const raw = this.storage?.getItem(ASK_ROUTES_KEY);
      const parsed = raw ? (JSON.parse(raw) as Partial<RoutesDocument>) : null;
      if (!parsed || parsed.version !== 1 || !parsed.routes || typeof parsed.routes !== "object") {
        return { version: 1, routes: {} };
      }
      return { version: 1, routes: { ...parsed.routes } };
    } catch {
      return { version: 1, routes: {} };
    }
  }
}

function safeStorage(): Storage | null {
  try {
    return globalThis.localStorage ?? null;
  } catch {
    return null;
  }
}
