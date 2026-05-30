"""Centralized env-var loading for the voice-agent process."""

from __future__ import annotations

import json
import logging
import os
import urllib.error
import urllib.request
from dataclasses import dataclass

logger = logging.getLogger(__name__)

# voice_agent_bootstrap_http_timeout caps how long the Python
# self-bootstrap waits for the identity service to mint a token
# before giving up. Generous so slow-start identity in compose
# doesn't trigger a false negative, but bounded so a misconfigured
# identity endpoint doesn't block voice-agent startup forever.
# Mirrors `node.nodeBootstrapHTTPTimeout` (30s) on the Go side.
_VOICE_AGENT_BOOTSTRAP_HTTP_TIMEOUT_SEC = 30


@dataclass(frozen=True)
class Config:
    """Resolved runtime configuration. Read once at startup."""

    # LiveKit
    livekit_url: str
    livekit_api_key: str
    livekit_api_secret: str

    # Deepgram (STT + TTS)
    deepgram_api_key: str

    # Voice executor selection (Hybrid Realtime Voice epic #440, issue
    # #434). "cascade" (default) is the existing Deepgram STT -> memql
    # cognition -> Deepgram TTS path. "realtime" swaps in the OpenAI
    # gpt-realtime speech-to-speech model behind the MemqlLLM seam.
    # The default is deliberately "cascade" so there is no regression:
    # realtime is strictly opt-in. When "realtime" is selected but its
    # prerequisites are missing (no OpenAI key, plugin import fails,
    # session build errors), the selection logic falls back to cascade
    # automatically -- see voice_agent.realtime_executor.select_voice_executor.
    voice_executor: str  # 'cascade' | 'realtime'
    # OpenAI API key for the Realtime executor. Only required when
    # voice_executor == "realtime"; unused on the cascade path.
    openai_api_key: str | None
    # gpt-realtime model id. Overridable so a room can pin a specific
    # snapshot without a code change.
    realtime_model: str

    # Realtime session lifecycle + cost guardrails (Hybrid Realtime Voice
    # epic #440, issue #439). These bound a realtime session so an
    # always-on speech-to-speech model listening to a multi-human polyphon
    # room cannot burn audio tokens unbounded. All four are ceilings: a
    # session that trips any of them is torn down and the voice path
    # degrades to the cascade. Only consulted on the realtime path; the
    # cascade ignores them. See voice_agent.realtime_lifecycle.
    #
    # Idle timeout: how long the warm-but-muted realtime session may sit
    # without the conductor engaging it (no response.create) before it is
    # torn down. The chosen lifecycle is "warm but input-muted until the
    # conductor engages" (#432 option A turn_detection=None), so an idle
    # session is pure standing cost -- this bound caps it. <= 0 disables
    # the idle teardown.
    realtime_idle_timeout_sec: int
    # Hard cap on total realtime session wall-clock duration regardless of
    # activity. Backstop against a session that stays nominally "engaged"
    # forever. <= 0 disables the duration cap.
    realtime_max_session_sec: int
    # Per-session audio-token budget. The realtime model reports usage
    # (input + output audio tokens) per response; once the running total
    # crosses this ceiling the session is torn down and the voice path
    # degrades to the cascade. This is the cost guardrail the issue asks
    # for, surfaced like the existing plan token-budget caps. <= 0
    # disables the budget cap.
    realtime_max_audio_tokens: int

    # memql gRPC
    memql_grpc_addr: str
    # Identity-issued class="voice_agent" JWT bearer presented on
    # every MemqlService.Stream dial. Minted via
    # JWTIssuer.IssueVoiceAgentAccessToken on the cluster side; see
    # docs/auth/voice-agent-jwt.md.
    voice_agent_token: str

    # Avatar
    avatar_vendor: str  # 'anam' | 'simli' | 'none'
    anam_api_key: str | None
    simli_api_key: str | None
    # Default Anam face/persona to use when the agent record has no
    # `avatarPersonaId` stamped. Anam treats personas and avatars as
    # distinct entity types -- a persona bundles (face + voice + LLM
    # + system prompt) under one id; an avatar is just the face
    # model. Both fields are accepted; the persona-id path takes
    # precedence when both are set since a persona supplies more
    # context.
    anam_default_persona_id: str | None    # full Anam persona
    anam_default_avatar_id: str | None     # bare avatar face id
    anam_default_persona_name: str         # display name when using avatar-id path

    # Logging
    log_level: str

    # Deepgram tuning -- inherited from the shipped Bridge Agent's
    # production-tuned defaults so the LiveKit Agents path picks up
    # the same EOU semantics rather than the upstream defaults.
    dg_asr_model: str
    dg_tts_model: str
    dg_language: str
    dg_endpointing_ms: int
    dg_utterance_end_ms: int

    @property
    def avatar_enabled(self) -> bool:
        return self.avatar_vendor in ("anam", "simli")


def _get(env: str, default: str = "") -> str:
    return os.environ.get(env, default).strip()


def _get_required(env: str) -> str:
    val = os.environ.get(env, "").strip()
    if not val:
        raise RuntimeError(f"required env var {env} is unset")
    return val


def _get_int(env: str, default: int) -> int:
    raw = os.environ.get(env, "").strip()
    if not raw:
        return default
    try:
        return int(raw)
    except ValueError:
        logger.warning("env %s=%r is not an int, falling back to %d", env, raw, default)
        return default


def _maybe_bootstrap_voice_agent_token() -> str:
    """Self-bootstrap a class="voice_agent" JWT against the identity service.

    Returns the minted token when all preconditions are present + the call
    succeeds; returns "" when self-bootstrap isn't opted into (the operator
    is provisioning the token out-of-band via the documented flow).

    Preconditions (mirroring `component/node.maybeBootstrapNodeToken`):
      1. VOICE_AGENT_TOKEN env var is empty (operator-provisioned token wins).
      2. MEMQL_NODE_BOOTSTRAP_TOKEN env var is set (the shared bootstrap
         secret the identity service validates against; reused from #338
         so operators have one secret to rotate).
      3. IDENTITY_VERIFIER_BASE_URL env var is set (we need somewhere to POST).
      4. MEMQL_VOICE_AGENT_INSTANCE_ID env var is set (the operator-chosen
         instance label stamped onto the JWT claims for audit).

    Raises RuntimeError when the bootstrap call is configured but fails;
    the caller surfaces the error so the operator sees a clear diagnostic
    instead of a downstream auth failure. memql#342.
    """
    if os.environ.get("VOICE_AGENT_TOKEN", "").strip():
        # Operator-provisioned token wins.
        return ""
    secret = os.environ.get("MEMQL_NODE_BOOTSTRAP_TOKEN", "").strip()
    if not secret:
        # Self-bootstrap not opted into.
        return ""
    identity_base = os.environ.get("IDENTITY_VERIFIER_BASE_URL", "").strip().rstrip("/")
    if not identity_base:
        raise RuntimeError(
            "voice-agent bootstrap requested (MEMQL_NODE_BOOTSTRAP_TOKEN set) "
            "but IDENTITY_VERIFIER_BASE_URL is empty -- cannot reach identity service"
        )
    instance_id = os.environ.get("MEMQL_VOICE_AGENT_INSTANCE_ID", "").strip()
    if not instance_id:
        raise RuntimeError(
            "voice-agent bootstrap requested but MEMQL_VOICE_AGENT_INSTANCE_ID is empty "
            "(stamp an instance label so audit can correlate this process)"
        )

    endpoint = f"{identity_base}/node/bootstrap"
    payload = json.dumps(
        {"tokenClass": "voice_agent", "instanceId": instance_id}
    ).encode("utf-8")
    req = urllib.request.Request(
        endpoint,
        data=payload,
        method="POST",
        headers={
            "Content-Type": "application/json",
            "Authorization": f"Bootstrap {secret}",
        },
    )
    logger.info(
        "voice-agent bootstrap: requesting voice_agent JWT from identity endpoint=%s instance=%s",
        endpoint,
        instance_id,
    )
    try:
        with urllib.request.urlopen(req, timeout=_VOICE_AGENT_BOOTSTRAP_HTTP_TIMEOUT_SEC) as resp:
            body = resp.read()
            status = resp.status
    except urllib.error.HTTPError as e:
        body = e.read() if hasattr(e, "read") else b""
        status = e.code
    except urllib.error.URLError as e:
        raise RuntimeError(
            f"voice-agent bootstrap: POST {endpoint}: {e.reason}"
        ) from e

    try:
        decoded = json.loads(body) if body else {}
    except json.JSONDecodeError as e:
        raise RuntimeError(
            f"voice-agent bootstrap: identity returned {status} with non-JSON body: "
            f"{body[:512]!r}"
        ) from e

    if status != 200 or not decoded.get("success"):
        err_code = decoded.get("errorCode") or f"http_{status}"
        raise RuntimeError(
            "voice-agent bootstrap: identity rejected request "
            f"(status={status} errorCode={err_code}): {decoded.get('error', '').strip()}"
        )

    token = (decoded.get("plainToken") or "").strip()
    if not token:
        raise RuntimeError(
            "voice-agent bootstrap: identity returned 200 success but empty plainToken"
        )
    logger.info(
        "voice-agent bootstrap: minted voice_agent JWT identity_id=%s expires_at=%s",
        decoded.get("identityId"),
        decoded.get("expiresAt"),
    )
    return token


def _resolve_voice_agent_token() -> str:
    """Return the JWT to authenticate this voice-agent on MemqlService.Stream.

    Resolution order:
      1. VOICE_AGENT_TOKEN env var (operator-provisioned, the production path).
      2. Self-bootstrap via _maybe_bootstrap_voice_agent_token (the dev path
         introduced by memql#342 to keep `docker compose up` working out of
         the box).

    Raises RuntimeError when neither path yields a token -- matches the
    pre-#342 contract that `load_config` errors loudly when authentication
    can't be set up.
    """
    direct = os.environ.get("VOICE_AGENT_TOKEN", "").strip()
    if direct:
        return direct
    bootstrapped = _maybe_bootstrap_voice_agent_token()
    if bootstrapped:
        return bootstrapped
    raise RuntimeError(
        "required env var VOICE_AGENT_TOKEN is unset; either provision a "
        "voice-agent JWT out-of-band (see docs/auth/voice-agent-jwt.md) or "
        "set MEMQL_NODE_BOOTSTRAP_TOKEN + IDENTITY_VERIFIER_BASE_URL + "
        "MEMQL_VOICE_AGENT_INSTANCE_ID for the self-bootstrap path (memql#342)"
    )


def load_config() -> Config:
    """Load + validate config. Raises if required vars are missing."""
    cfg = Config(
        livekit_url=_get_required("LIVEKIT_URL"),
        livekit_api_key=_get_required("LIVEKIT_API_KEY"),
        livekit_api_secret=_get_required("LIVEKIT_API_SECRET"),
        deepgram_api_key=_get_required("MEMQL_DEEPGRAM_API_KEY"),
        voice_executor=_get("MEMQL_VOICE_EXECUTOR", "cascade").lower(),
        openai_api_key=_get("OPENAI_API_KEY") or None,
        realtime_model=_get("MEMQL_REALTIME_MODEL", "gpt-realtime"),
        # Realtime lifecycle + cost guardrails (#439). Defaults are
        # deliberately conservative so a misconfigured realtime room
        # cannot run unbounded: a 5-minute idle teardown, a 30-minute
        # hard duration cap, and a 1,000,000 audio-token per-session
        # budget. Operators dial these per deployment; <= 0 on any one
        # disables that specific guardrail (the others still apply).
        realtime_idle_timeout_sec=_get_int("MEMQL_REALTIME_IDLE_TIMEOUT_SEC", 300),
        realtime_max_session_sec=_get_int("MEMQL_REALTIME_MAX_SESSION_SEC", 1800),
        realtime_max_audio_tokens=_get_int("MEMQL_REALTIME_MAX_AUDIO_TOKENS", 1_000_000),
        memql_grpc_addr=_get_required("MEMQL_GRPC_ADDR"),
        voice_agent_token=_resolve_voice_agent_token(),
        avatar_vendor=_get("MEMQL_AVATAR_VENDOR", "anam").lower(),
        anam_api_key=_get("ANAM_API_KEY") or None,
        simli_api_key=_get("SIMLI_API_KEY") or None,
        anam_default_persona_id=_get("ANAM_DEFAULT_PERSONA_ID") or None,
        anam_default_avatar_id=_get("ANAM_DEFAULT_AVATAR_ID") or None,
        anam_default_persona_name=_get("ANAM_DEFAULT_PERSONA_NAME", "Assistant"),
        log_level=_get("VOICE_AGENT_LOG_LEVEL", "INFO").upper(),
        dg_asr_model=_get("POLYPHON_DEEPGRAM_ASR_MODEL", "nova-3"),
        dg_tts_model=_get("POLYPHON_DEEPGRAM_TTS_MODEL", "aura-2"),
        dg_language=_get("POLYPHON_DEEPGRAM_LANGUAGE", "en"),
        # Default intentionally errs on "let the user think." Voice
        # users pause mid-sentence to gather a thought; firing a final
        # transcript on a 500ms thinking gap (the old baseline) makes
        # the agent respond to a fragment and the user has to start
        # the sentence over. 1500ms of phrase-level silence gives
        # natural conversational pauses room to ride through; snappier
        # users can dial back via env. NB: the LK Deepgram plugin
        # (1.5) does not forward `utterance_end_ms` to the Deepgram
        # WebSocket -- the field below is parsed and logged for
        # future use (patching the plugin, or switching to a raw
        # Deepgram client) but is NOT honored on the wire today.
        # See docs/voice/eou-tuning.md.
        dg_endpointing_ms=_get_int("POLYPHON_DEEPGRAM_ENDPOINTING_MS", 2000),
        dg_utterance_end_ms=_get_int("POLYPHON_DEEPGRAM_UTTERANCE_END_MS", 0),
    )

    if cfg.voice_executor not in ("cascade", "realtime"):
        raise RuntimeError(
            f"MEMQL_VOICE_EXECUTOR={cfg.voice_executor!r} -- must be 'cascade' or 'realtime'"
        )
    if cfg.voice_executor == "realtime" and not cfg.openai_api_key:
        # Do not hard-fail: the realtime selection falls back to the
        # cascade at session-build time. Warn loudly so the operator
        # knows the realtime path will not engage.
        logger.warning(
            "MEMQL_VOICE_EXECUTOR=realtime but OPENAI_API_KEY is unset -- "
            "the realtime executor will fall back to the cascade path"
        )

    if cfg.avatar_vendor not in ("anam", "simli", "none"):
        raise RuntimeError(
            f"MEMQL_AVATAR_VENDOR={cfg.avatar_vendor!r} -- must be 'anam', 'simli', or 'none'"
        )
    if cfg.avatar_vendor == "anam" and not cfg.anam_api_key:
        logger.warning(
            "MEMQL_AVATAR_VENDOR=anam but ANAM_API_KEY is unset -- avatar will be disabled"
        )
    if cfg.avatar_vendor == "simli" and not cfg.simli_api_key:
        logger.warning(
            "MEMQL_AVATAR_VENDOR=simli but SIMLI_API_KEY is unset -- avatar will be disabled"
        )

    return cfg


def setup_logging(level: str) -> None:
    """Configure structured logging. Called once at startup."""
    logging.basicConfig(
        level=getattr(logging, level, logging.INFO),
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
        datefmt="%Y-%m-%dT%H:%M:%S",
    )
