import { rowArray, rowNumber, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

// The recommended set (epic memql#5146, D2).
//
// ===========================================================================
// THE ENGINE COMPUTES THE SET; THIS FILE ONLY READS IT
// ===========================================================================
// The page has the catalog and the machine and could work the recommendation
// out itself. It does not, because that would be a second implementation of
// RecommendedSet in a second language, and the two would drift -- presenting as
// a page that offers to pull a model the act then refuses, or lists a set the
// act does not take. `fleetRecommended` is the act's own answer without the
// act.

/** One entry in a machine's recommended set. */
export interface Recommendation {
  modelId: string;
  /** fast / strong / reasoning / embeddings. */
  level: string;
  runtime: string;
  category: string;
  params: number;
  sizeBytes: number;
  notes: string;
  /** Empty when the model can be pulled; otherwise the SENTENCE saying what is
   *  in the way, written once on the engine and rendered here verbatim. */
  blocked: string;
  pullable: boolean;
}

/** What `fleetRecommended` answers for one machine. */
export interface RecommendedSet {
  workerId: string;
  /** "" when the cockpit has not reported, "unsupported" below the floor,
   *  otherwise the rung. */
  machineClass: string;
  usableBytes: number;
  /** False when the cockpit has not reported its hardware. */
  reported: boolean;
  entries: Recommendation[];
  /** The runtimes the set NEEDS and the machine does not have, so the page can
   *  say "install Kokoro" once rather than once per blocked row. */
  runtimeGap: string[];
}

export const EMPTY_SET: RecommendedSet = {
  workerId: "",
  machineClass: "",
  usableBytes: 0,
  reported: false,
  entries: [],
  runtimeGap: [],
};

export function recommendedSetFrom(input: Row | null | undefined): RecommendedSet {
  if (!input) return EMPTY_SET;
  const row = input;
  const entries: Recommendation[] = (rowArray(row, "entries") ?? []).map((item) => {
    const entry = item as Row;
    return {
      modelId: rowString(entry, "modelId"),
      level: rowString(entry, "level"),
      runtime: rowString(entry, "runtime"),
      category: rowString(entry, "category"),
      params: rowNumber(entry, "params"),
      sizeBytes: rowNumber(entry, "size"),
      notes: rowString(entry, "notes"),
      blocked: rowString(entry, "blocked"),
      pullable: entry["pullable"] === true,
    };
  });
  const gap = (rowArray(row, "runtimeGap") ?? [])
    .map((v) => (typeof v === "string" ? v : ""))
    .filter((v) => v !== "");
  return {
    workerId: rowString(row, "workerId"),
    machineClass: rowString(row, "machineClass"),
    usableBytes: rowNumber(row, "usableBytes"),
    reported: row["reported"] === true,
    entries,
    runtimeGap: gap,
  };
}

/** A level, as the page writes it. */
export function levelLabel(level: string): string {
  switch (level) {
    case "fast":
      return "Fast";
    case "strong":
      return "Strong";
    case "reasoning":
      return "Reasoning";
    case "embeddings":
      return "Embeddings";
    default:
      return level;
  }
}
