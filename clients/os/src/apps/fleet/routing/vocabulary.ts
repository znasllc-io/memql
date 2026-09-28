// The routing vocabulary (design brief, section 1), in ONE place.
//
// Four nouns, and only four, reach a person on a routing surface:
//
//   Source   something that can answer a model call -- a local model on a
//            machine, an app on a machine, or a vendor
//   Route    an ordered list of sources, tried in order until one can serve
//   Rule     "when work looks like X, take route Y"
//   Level    how much intelligence a call needs: Fast, Strong, Reasoning
//
// The engine keeps its own names -- `policy`, `door`, `lane`, `federation` --
// in ids, query names and the DSL, and those names are not wrong there. They
// are wrong on the screen, where the same thing used to wear three of them.
// So every string a routing surface draws from an engine id goes through a
// function in this file, and `retiredWordsIn` is what the suite sweeps every
// rendered routing surface with.

import type { RuleRow } from "../../settings/rulesFacts";
import { appLabel } from "../rows";

/**
 * The engine's words that must never be rendered on a routing surface, each
 * with the test that finds it. Ids leak in camelCase ("fastLane",
 * "localPolicy"), so a word glued to the end of another counts -- and
 * "planes" does not, because a lane there is part of an English word.
 */
export const RETIRED_ROUTING_WORDS: readonly (readonly [string, (text: string) => boolean])[] = [
  ["policy", (t) => /(^|[^a-z])polic(y|ies)\b/i.test(t) || /[a-z]Polic(y|ies)\b/.test(t)],
  ["door", (t) => /(^|[^a-z])doors?\b/i.test(t) || /[a-z]Doors?\b/.test(t)],
  ["task rule", (t) => /\btask\s+rules?\b/i.test(t)],
  ["federation", (t) => /federation/i.test(t)],
  ["lane", (t) => /(^|[^a-z])lanes?\b/i.test(t) || /[a-z]Lanes?\b/.test(t)],
];

/** Every retired word the text carries, by its canonical spelling. */
export function retiredWordsIn(text: string): string[] {
  return RETIRED_ROUTING_WORDS.filter(([, found]) => found(text)).map(([word]) => word);
}

/** An engine id as words: "planningStaysLocal" -> "Planning stays local". */
export function humanize(id: string): string {
  const words = id
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .replace(/[_-]+/g, " ")
    .trim()
    .split(/\s+/)
    .filter(Boolean)
    .map((w) => w.toLowerCase());
  if (words.length === 0) return "";
  const [first, ...rest] = words;
  return [first!.charAt(0).toUpperCase() + first!.slice(1), ...rest].join(" ");
}

/**
 * The id a route gets from the name a person typed.
 *
 * The engine takes `^[a-z][A-Za-z0-9]*$` and nothing else, so "Night shift"
 * becomes `nightShift` -- and `routeTitle` turns it back into the words that
 * were typed. A name with no letters in it yields "", which the wizard reads
 * as "not a name yet".
 */
export function routeIdFrom(text: string): string {
  const words = text
    .replace(/[^A-Za-z0-9]+/g, " ")
    .trim()
    .split(/\s+/)
    .filter(Boolean);
  const id = words
    .map((w, i) => (i === 0 ? w.toLowerCase() : w.charAt(0).toUpperCase() + w.slice(1).toLowerCase()))
    .join("");
  return /^[a-z][A-Za-z0-9]*$/.test(id) ? id : "";
}

/**
 * The shipped routes' names in words. Custom routes are humanized: the person
 * typed words, and `routeIdFrom` kept them.
 *
 * `federationStrongest` is spelled out rather than humanized because the
 * humanized id carries a retired word.
 */
const SHIPPED_TITLES: Record<string, string> = {
  localFirst: "Local first",
  fastLocalFirst: "Fast local first",
  localOnly: "Local only",
  federationStrongest: "Local, then best vendor",
  embeddingsBinding: "Embeddings",
};

export function routeTitle(name: string): string {
  return SHIPPED_TITLES[name] ?? humanize(name);
}

/** What kind of thing a chain entry names; drives its glyph and its group. */
export type SourceKind = "local" | "app" | "vendor" | "route" | "embeddings" | "provider";

export function sourceKind(entry: string): SourceKind {
  const scheme = entry.includes(":") ? entry.slice(0, entry.indexOf(":")) : "";
  if (scheme === "fleet") return "local";
  if (scheme === "app") return "app";
  if (scheme === "federation") return "vendor";
  if (scheme === "policy") return "route";
  if (scheme === "embedder") return "embeddings";
  return "provider";
}

/** A chain entry in words. Model ids stay ids: they are the model's own name. */
export function sourceLabel(entry: string): string {
  const scheme = entry.includes(":") ? entry.slice(0, entry.indexOf(":")) : "";
  const rest = scheme === "" ? entry : entry.slice(scheme.length + 1);
  switch (scheme) {
    case "fleet":
      if (rest === "strongest") return "Strongest local model";
      if (rest === "fastest") return "Fastest local model";
      return rest;
    case "app": {
      if (rest === "*") return "Any signed-in app";
      const [appId, model] = [rest.split(":")[0] ?? rest, rest.includes(":") ? rest.slice(rest.indexOf(":") + 1) : ""];
      return model ? `${appLabel(appId)} · ${model}` : appLabel(appId);
    }
    case "federation":
      if (rest === "cheapest") return "Cheapest vendor";
      if (rest === "strongest") return "Strongest vendor";
      return rest;
    case "policy":
      return routeTitle(rest);
    case "embedder":
      return "Active embeddings";
    default:
      return entry;
  }
}

/** The three levels a person picks from; Embeddings is internal. */
export const LEVEL_TITLES: Record<string, string> = {
  fast: "Fast",
  strong: "Strong",
  reasoning: "Reasoning",
  embeddings: "Embeddings",
};

export function levelTitle(level: string): string {
  return LEVEL_TITLES[level] ?? humanize(level);
}

/** The kinds of call a rule can match, in the reader's words. */
export const MODALITY_TITLES: Record<string, string> = {
  chat: "Chat",
  streamingChat: "Streaming chat",
  tools: "Tool calls",
  streamingTools: "Streaming tool calls",
  structured: "Structured output",
  vision: "Vision",
  speech: "Speech",
  transcribe: "Transcription",
};

const TAG_TITLES: Record<string, string> = {
  background: "Background work",
  backgroundEscalation: "Background escalations",
};

function lower(text: string): string {
  return text.charAt(0).toLowerCase() + text.slice(1);
}

/**
 * A rule's conditions as the work they match: the "When" half of the sentence.
 *
 * A rule with no conditions reads "Everything else" when it is the shipped
 * floor (it catches what nothing above it matched) and "Every call" when a
 * person wrote it.
 */
export function whenWords(rule: Pick<RuleRow, "when" | "locked">): string {
  const parts: string[] = [];
  const w = rule.when;
  if ("level" in w) parts.push(w.level ? (w.level === "embeddings" ? "Embeddings" : `${levelTitle(w.level)} work`) : "No level");
  if ("modality" in w) parts.push(w.modality ? MODALITY_TITLES[w.modality] ?? humanize(w.modality) : "No kind of call");
  if ("prompt" in w) parts.push(w.prompt ? `${humanize(w.prompt)} prompt` : "No prompt");
  if ("role" in w) parts.push(w.role ? `${w.role} acting` : "No role acting");
  if ("actorRole" in w) parts.push(w.actorRole ? `${w.actorRole} watching` : "No role watching");
  if ("tag" in w) parts.push(w.tag ? TAG_TITLES[w.tag] ?? `Tagged ${lower(humanize(w.tag))}` : "No tag");
  if ("touches" in w) parts.push(w.touches ? `Touches ${w.touches}` : "Touches nothing");
  if (parts.length === 0) return rule.locked ? "Everything else" : "Every call";
  return parts.map((p, i) => (i === 0 ? p.charAt(0).toUpperCase() + p.slice(1) : lower(p))).join(" · ");
}

/** What else a rule does, beyond taking its route, in as few words as it takes. */
export function ruleExtras(rule: Pick<RuleRow, "level" | "onUnavailable">): string {
  const parts: string[] = [];
  if (rule.level) parts.push(`asks for ${levelTitle(rule.level)}`);
  if (rule.onUnavailable === "park") parts.push("waits when nothing is ready");
  return parts.join(" · ");
}
