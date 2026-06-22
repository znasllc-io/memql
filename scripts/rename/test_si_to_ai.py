#!/usr/bin/env python3
"""Self-test for the SI -> AI renamer. Run: python3 scripts/rename/test_si_to_ai.py

Proves the three properties issue 1.1 asks for:
  - denylist landmines (SIGTERM, SESSION, SIMLI, POSIX, SID, SIProvider's
    SIP-prefix collision, MEMQL_SI_* env vars, the "si" data value) are NEVER
    touched;
  - identifier boundaries are respected (isSI vs isSITyping);
  - the transform is idempotent (apply twice == apply once).
"""
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import si_to_ai as r  # noqa: E402

DATA = r.load_rules(r.DEFAULT_RULES)


def transform(text, ext, groups=None):
    rules = r.compiled_rules(DATA, groups or [], [])
    deny = r.compiled_denylist(DATA)
    new, _counts, violations = r.apply_rules_to_text(text, rules, ext, deny)
    assert not violations, f"unexpected denylist violation: {violations}"
    return new


FAILS = []


def check(name, got, want):
    if got != want:
        FAILS.append(f"{name}\n   got:  {got!r}\n   want: {want!r}")


# --- denylist landmines must pass through unchanged (Go) ---
for landmine in [
    "SIGTERM", "SIGINT", "syscall.SIGTERM", "POSIX", "VERSION",
    "MEMQL_SI_OPENAI_API_KEY", "MEMQL_GENESIS_B64",
    "IDENTITY_SIGNING_KEY_B64", "classifySIMILARITY", "SessionStore",
    "MEMQL_SAFETY_CLASSIFICATION_RETENTION_DAYS",
]:
    check(f"go landmine {landmine}", transform(landmine, ".go"), landmine)

# --- SIP telephony (LiveKit) must survive; SIProvider must rename ---
check("SIPInboundTrunkInfo kept", transform("SIPInboundTrunkInfo", ".go"),
      "SIPInboundTrunkInfo")
check("SIPDispatchRule kept", transform("SIPDispatchRule", ".go"), "SIPDispatchRule")
check("SIProvider renamed", transform("SIProvider", ".go"), "AIProvider")
check("ChatSIProvider renamed", transform("ChatSIProvider", ".go"), "ChatAIProvider")

# --- frontend denylist landmines ---
for landmine in [
    "isSIMILAR", "WINDOW_SIZE", "ACTIVE_REALTIME_SESSION_KEYS", "SIMLI",
    "VITE_SIMLI_API_KEY", "TRANSITION_DURATION_MS", "PROGRESSIVE_ID",
    "SID", "OUTSIDE", "MAX_FILE_SIZE_BYTES", "AutoTTSIndicator",
]:
    check(f"ts landmine {landmine}", transform(landmine, ".tsx"), landmine)

# --- identifier boundary correctness ---
check("isSI standalone", transform("isSI", ".tsx"), "isAI")
check("isSITyping not double-renamed", transform("isSITyping", ".tsx"), "isAITyping")
check("isSIMessage", transform("isSIMessage", ".tsx"), "isAIMessage")
check("isSITyping in expr", transform("if (isSITyping) {", ".tsx"), "if (isAITyping) {")

# --- proto family casing (Ai, not AI) + stem coverage of *H/*Payload variants ---
check("SIChatMsg -> AiChatMsg", transform("SIChatMsg", ".proto"), "AiChatMsg")
check("SIChatMsgH stem", transform("SIChatMsgH", ".go"), "AiChatMsgH")
check("fSIChatResult lowercased-prefix stem",
      transform("fSIChatResult", ".go"), "fAiChatResult")
check("SIForwardRequest -> Ai", transform("SIForwardRequest", ".go"), "AiForwardRequest")
check("SIForwardResponseSink stem",
      transform("SIForwardResponseSink", ".go"), "AiForwardResponseSink")
check("si_chat snake", transform("si_chat = 18;", ".proto"), "ai_chat = 18;")

# --- go-internal stems (AI all-caps) ---
check("ChatSIProvider infix", transform("ChatSIProvider", ".go"), "ChatAIProvider")
check("SIProviderRegistry infix", transform("SIProviderRegistry", ".go"), "AIProviderRegistry")
check("openSIProvider infix", transform("openSIProvider", ".go"), "openAIProvider")
check("convertSIExpr infix", transform("convertSIExpr", ".go"), "convertAIExpr")
check("buildSICacheKey infix", transform("buildSICacheKey", ".go"), "buildAICacheKey")
check("countActiveSIParticipantsForUser",
      transform("countActiveSIParticipantsForUser", ".go"),
      "countActiveAIParticipantsForUser")

# --- group ordering: InvokeSIChat is Go (AI), not proto (Ai) ---
check("InvokeSIChat -> InvokeAIChat (go beats proto)",
      transform("InvokeSIChat", ".go"), "InvokeAIChat")
check("InvokeSIStructured", transform("InvokeSIStructured", ".go"), "InvokeAIStructured")

# --- shared stems reach both Go and TSX ---
check("hasSIParticipant tsx", transform("hasSIParticipant", ".tsx"), "hasAIParticipant")
check("findSIParticipant go", transform("findSIParticipant", ".go"), "findAIParticipant")
check("autoJoinSI memql", transform("autoJoinSI", ".memql"), "autoJoinAI")
check("LogicAutoJoinSIArgs go", transform("LogicAutoJoinSIArgs", ".go"), "LogicAutoJoinAIArgs")

# --- DSL keyword: si( -> ai( in BOTH .memql (dsl-keyword) and .go literals (go-keyword-literals) ---
check("si() in memql", transform('x := si("p", a)', ".memql"), 'x := ai("p", a)')
check("si( literal renamed in .go (parse/serialize sites)",
      transform('WriteString("si(")', ".go"), 'WriteString("ai(")')

# --- "si" data value (NO paren) is left alone everywhere: no bare-si rule ---
check("participantType si value kept (.go)",
      transform('participantType == "si"', ".go"), 'participantType == "si"')
check("participantType si value kept (.memql)",
      transform('payload.participantType == "si"', ".memql"),
      'payload.participantType == "si"')

# --- idempotency: apply twice == once, across a mixed blob ---
blob = ('InvokeSI(); SIProvider{}; SIPInboundTrunkInfo; isSITyping; '
        'SIChatMsg; SIGTERM; si("p");')
once = transform(blob, ".go")
once = transform(once, ".memql")
twice = transform(transform(once, ".go"), ".memql")
check("idempotent", twice, once)

if FAILS:
    print("FAIL:")
    for f in FAILS:
        print(" -", f)
    sys.exit(1)
print(f"OK: all renamer self-tests passed")
