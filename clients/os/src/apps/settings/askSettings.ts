// Ask's own preferences, and the store that keeps them.
//
// A separate versioned key rather than a corner of DesktopDocument, for the
// reason apps/fleet/settings.ts states in full: the desktop document's
// `version: 1` gate rejects a document it does not know, so folding a
// preference in there would cost somebody their desks because Ask learned a
// checkbox. Ask is CHROME rather than an app, so these are not per-app
// settings -- but the discipline is identical and copying it is cheaper than
// inventing a second one.
//
// ===========================================================================
// WHAT IS DELIBERATELY NOT HERE: INPUT LANGUAGE
// ===========================================================================
// The epic asks for an input-language entry "if the engine surface offers it;
// otherwise omit -- do not invent". It does not offer it.
// AiTranscribeStreamStart carries `language_hint`, and
// component/grpc/ai_transcribe_stream.go discards it: the language is pinned
// cluster-wide from MEMQL_STT_LANGUAGE because an auto-detected language is
// the documented cause of the wrong-language hallucination on short, noisy
// audio. A picker here would be a control that changes nothing, which is
// worse than an absent one -- the person who sets it and keeps getting
// English has been told the fault is theirs.

/** Where a finished utterance goes. */
export type VoiceCommit = "send" | "review";

export interface AskSettings {
  version: 1;
  /**
   * `send` -- releasing the mic asks the question. `review` -- it lands in the
   * box for editing.
   *
   * `send` is the default, and the portal's own voice control (Synapse) reads
   * the opposite way: "the transcript is shown and editable, always. Voice
   * that ran straight through would be a black box." That rule was written
   * against a BATCH transcribe, where nothing is visible until it is over.
   * This path streams -- every delta replaces the field's contents while the
   * person is still speaking -- so by the time they release they have already
   * read what was heard. Nothing is hidden, so nothing needs reviewing, and
   * making the fast path the default is the point of talking to it.
   */
  commit: VoiceCommit;
  /**
   * Hold Space to talk while Ask is open and the caret is not in a field.
   *
   * On by default and narrow on purpose: the guard is "no editable element
   * has focus", so Space still types a space in the box it is aimed at. In
   * the sheet, which focuses the field on open, that means Space types until
   * the person Tabs away or presses the mic -- exactly right, and the reason
   * this cannot simply be "Space anywhere".
   */
  spaceToTalk: boolean;
}

export const ASK_SETTINGS_KEY = "memql-os-ask-v1";

export const DEFAULT_ASK_SETTINGS: AskSettings = {
  version: 1,
  commit: "send",
  spaceToTalk: true,
};

/**
 * Repair a parsed document to the defaults, field by field.
 *
 * A wrong version is wholesale -- this store cannot read it. Everything else
 * is independent: a garbage `commit` must not cost somebody their Space
 * preference.
 */
export function sanitizeAskSettings(raw: unknown): AskSettings {
  if (!raw || typeof raw !== "object") return { ...DEFAULT_ASK_SETTINGS };
  const doc = raw as Partial<AskSettings>;
  if (doc.version !== 1) return { ...DEFAULT_ASK_SETTINGS };
  return {
    version: 1,
    commit: doc.commit === "review" || doc.commit === "send" ? doc.commit : DEFAULT_ASK_SETTINGS.commit,
    spaceToTalk:
      typeof doc.spaceToTalk === "boolean" ? doc.spaceToTalk : DEFAULT_ASK_SETTINGS.spaceToTalk,
  };
}

export interface AskSettingsStore {
  load(): AskSettings;
  save(settings: AskSettings): void;
}

export class LocalAskSettingsStore implements AskSettingsStore {
  constructor(private readonly storage: Storage | null = safeStorage()) {}

  load(): AskSettings {
    try {
      const raw = this.storage?.getItem(ASK_SETTINGS_KEY);
      return sanitizeAskSettings(raw ? JSON.parse(raw) : null);
    } catch {
      return { ...DEFAULT_ASK_SETTINGS };
    }
  }

  save(settings: AskSettings): void {
    try {
      this.storage?.setItem(ASK_SETTINGS_KEY, JSON.stringify(settings));
    } catch {
      // A private window with storage disabled keeps working, with defaults.
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
