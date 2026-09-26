import { LiveVoiceSession } from "./liveVoiceSession";
import { ConversationSession } from "./conversationSession";
import { createContext, useEffect, useCallback, useContext, useMemo, useRef, useState, useSyncExternalStore, type ReactNode } from "react";

import { CHECKING_ASK, type AskAvailability } from "./useAskReadiness";
import type { AskTransport } from "./askController";
import type { VoicePorts } from "./voiceSession";
import {
  LocalAskSettingsStore,
  type AskSettings,
  type AskSettingsStore,
} from "../apps/settings/askSettings";

// One Ask, three entry points (spec D6): the dock orb, the desk widget and
// every window's title bar all open the SAME surface. This context carries
// the transport and the sheet's open/context state; the widget renders the
// surface inline and needs only the transport.
//
// Voice (epic memql#4747) joins on the same terms: one set of ports built by
// the shell, shared by every entry point, and null in a harness that has no
// audio stack. Ask's own settings live here too rather than being re-read
// from storage by each surface -- the Settings window and the sheet are
// different windows, and a preference changed in one has to be true in the
// other without a reload.

export interface AskSheetState {
  open: boolean;
  context: string | null;
  contextLabel?: string;
}

interface AskContextValue {
  transport: AskTransport;
  conversation: ConversationSession;
  liveVoice: LiveVoiceSession;
  availability: AskAvailability;
  voice: VoicePorts | null;
  settings: AskSettings;
  updateSettings: (patch: Partial<AskSettings>) => void;
  sheet: AskSheetState;
  openAsk: (context?: string | null, contextLabel?: string) => void;
  closeAsk: () => void;
}

const Ctx = createContext<AskContextValue | null>(null);

export function useAsk(): AskContextValue {
  const value = useContext(Ctx);
  if (!value) throw new Error("useAsk outside AskProvider");
  return value;
}

const noActivity = () => null;
const noSubscription = () => () => {};
/** Optional in app harnesses; observes execution without issuing another action. */
export function useAskActivity() {
  const session = useContext(Ctx)?.conversation;
  return useSyncExternalStore(session?.subscribe ?? noSubscription, session ? () => session.getSnapshot().activity : noActivity);
}

export function AskProvider({
  transport,
  voice = null,
  availability = CHECKING_ASK,
  settingsStore,
  children,
}: {
  transport: AskTransport;
  availability?: AskAvailability;
  voice?: VoicePorts | null;
  settingsStore?: AskSettingsStore;
  children: ReactNode;
}) {
  const storeRef = useRef<AskSettingsStore | null>(null);
  if (!storeRef.current) storeRef.current = settingsStore ?? new LocalAskSettingsStore();
  const conversation = useMemo(() => new ConversationSession(transport), [transport]);
  useEffect(() => { void conversation.refresh(); return () => conversation.dispose(); }, [conversation]);
  const liveVoice = useMemo(() => new LiveVoiceSession(transport, conversation), [transport, conversation]);
  useEffect(() => () => liveVoice.dispose(), [liveVoice]);
  useEffect(() => { if (availability.state === "ready") void conversation.refresh(); }, [availability.state, conversation]);
  const [sheet, setSheet] = useState<AskSheetState>({ open: false, context: null });
  const [settings, setSettings] = useState<AskSettings>(() => storeRef.current!.load());

  const openAsk = useCallback((context: string | null = null, contextLabel?: string) => setSheet({ open: true, context, contextLabel }), []);
  const closeAsk = useCallback(() => setSheet((s) => ({ ...s, open: false })), []);
  const updateSettings = useCallback((patch: Partial<AskSettings>) => {
    setSettings((prev) => {
      const next = { ...prev, ...patch, version: 1 as const };
      storeRef.current?.save(next);
      return next;
    });
  }, []);

  const value = useMemo(
    () => ({ conversation, liveVoice, transport, availability, voice, settings, updateSettings, sheet, openAsk, closeAsk }),
    [conversation, liveVoice, transport, availability, voice, settings, updateSettings, sheet, openAsk, closeAsk],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}
