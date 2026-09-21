import { useMemo } from "react";

import { useReading, type ReadingState } from "../../cluster/reading";
import { useOsConnection } from "../../live/connection";
import {
  languageDocsText,
  languageFactsFromRow,
  type LanguageDocsTopic,
  type LanguageFacts,
} from "./language";

// Settings -> Language, the reads (memql#5390).
//
// AN ON-DEMAND READ, NOT A LIVE ONE. What `languageStatus` answers changes only
// when the answering node loads a different tree or runs a different engine --
// a restart, never a write -- so there is no `graph.node.*` event to follow and
// a live collection over it would seed once and then never move. It reads once
// when the section opens, which is `src/cluster/reading`'s own rule.

export interface LanguageReading {
  /** `unread` until there is a connection to ask, then the reading's own state. */
  readonly state: ReadingState;
  /** The facts once a row this window reads has landed. Null before, on a refusal, and for a row it cannot read. */
  readonly facts: LanguageFacts | null;
  /** True when the read landed and the row was not one this window reads. */
  readonly unreadable: boolean;
  /** The engine's own sentence when it refused the read. "" otherwise. */
  readonly error: string;
}

export function useLanguage(): LanguageReading {
  const connection = useOsConnection();
  const reading = useReading<LanguageFacts | null>(
    "settings:language",
    connection === null
      ? null
      : async (signal) => {
          const result = await connection.query.languageStatus({}, { signal });
          return languageFactsFromRow(result.rows()[0] ?? null);
        },
  );
  return {
    state: reading.state,
    facts: reading.value,
    unreadable: reading.state === "read" && reading.value === null,
    error: reading.error,
  };
}

/**
 * The builtin each document comes from.
 *
 * `memqlGrammar` and `memqlVocabulary` are declared on this tree WITHOUT `@sdk`
 * and without a capability, and this leaves both exactly as they are: they are
 * read through `executeNamed`, the shell's path for an untyped builtin and the
 * one Deployables reads `siteHealthRead` through. Adding `@sdk` to a builtin
 * this section merely consumes would widen the generated SDK surface of two
 * calls that are not this section's to change, and nothing here needs it.
 */
const DOCS_BUILTIN: Record<LanguageDocsTopic, string> = {
  grammar: "memqlGrammar",
  vocabulary: "memqlVocabulary",
};

/**
 * Reads one of the engine's language documents as the text a copy puts on the
 * clipboard, or null when there is no connection to ask.
 *
 * ON CLICK, NEVER ON MOUNT. The grammar and the vocabulary are the largest
 * answers the engine gives about itself, and most visits to this section copy
 * neither. The promise rejects with the engine's own sentence when it refuses,
 * and with languageDocsText's when the answer is not the document asked for.
 */
export function useLanguageDocs(): ((topic: LanguageDocsTopic, signal: AbortSignal) => Promise<string>) | null {
  const connection = useOsConnection();
  return useMemo(() => {
    if (connection === null) return null;
    return async (topic: LanguageDocsTopic, signal: AbortSignal) => {
      const name = DOCS_BUILTIN[topic];
      const result = await connection.query.executeNamed(name, `builtin ${name}()`, { signal });
      return languageDocsText(topic, result.rows()[0] ?? null);
    };
  }, [connection]);
}
