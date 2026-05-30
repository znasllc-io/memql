"""MCP tool bridge for the Realtime voice session (#435).

This module exposes a curated set of **low-risk read tools** (web search,
knowledge/domain lookup, recent-chat, etc.) to the OpenAI ``gpt-realtime``
session so the model can call them directly via async function calling --
the conversation does not freeze while a tool runs -- while **mirroring
every model-driven tool call + result back into cognition** so cognition
is never blind to what the model did.

Risk-tier split (the heart of #435)
------------------------------------
memql already speaks MCP on the bidirectional ``MemqlService.Stream`` RPC
(``ListToolsMsg`` / ``CallToolMsg``; see ``component/grpc/memql.proto``).
The full tool registry contains both harmless read tools and **privileged /
side-effecting tools** (``computer_use`` / ``workerHost`` /
``workerComputer`` / ``copresent_control`` UI takeover / graph mutations)
that carry memql's authorization model -- role-locks,
``v1:copresent:agentAuthorization`` scope, and the computer-use kill
switch. Letting the *model* pick those is unacceptable: the model is not a
party to that auth model.

So this bridge applies a **default-deny tier boundary**:

* A tool is exposed to the Realtime model ONLY when it is on the
  explicit low-risk allowlist (:data:`LOW_RISK_TOOL_ALLOWLIST`) AND its
  registry ``ToolDefinition`` carries no side-effecting signal --
  ``client_execution`` is false and none of its declared ``scopes`` are
  in :data:`PRIVILEGED_SCOPES`.
* Every other tool -- including any tool the registry grows in the
  future that nobody has tiered yet -- is **privileged by default** and
  is NOT handed to the model. Privileged tools stay on the
  cognition-mediated, authorized tool loop (role-locks /
  agentAuthorization / kill switch enforced before dispatch), exactly as
  they are on the text path.

Awareness mirror
----------------
When the model calls a low-risk tool, the bridge:

1. executes it through memql's MCP surface (``CallToolMsg`` ->
   ``CallToolResult``) -- the same authorized backend path the text loop
   uses, so server-side tiering is still the source of truth; and
2. emits a **mirror** of the call + result into the cognition stream via
   :class:`CognitionMirror`, so transcripts, turn/utterance history, the
   conductor's state, and the chat/canvas UI all stay coherent (e.g. a
   "searched the web" affordance + citations). Nothing happens behind
   cognition's back.

Wiring is behind the ``MemqlLLM`` seam: ``build_realtime_executor`` calls
:func:`attach_mcp_tool_bridge` at the ``TODO(#435)`` seam. The model is
handed the filtered tool set; everything else is unchanged.

Out of scope (sibling issues): persona / graph grounding injection
(#436), output capture / utterances / citations rendering (#437), avatar
drive (#438), session lifecycle / cost guardrails (#439). This module
references those seams but does not implement them.
"""

from __future__ import annotations

import json
import logging
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from typing import Any

logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Risk-tier policy
# ---------------------------------------------------------------------------

# The low-risk allowlist. A tool reaches the Realtime model ONLY if its
# registered name is in this set AND it carries no side-effecting signal
# (see is_low_risk_tool). These are read-only / no-auth-gate lookups:
# web search, knowledge/domain retrieval, recent-chat recall. They are
# safe for the model to pick because a wrong pick wastes a call -- it
# cannot mutate state, drive the UI, or touch a user's machine.
#
# DEFAULT-DENY: anything not listed here is privileged-by-default and is
# never exposed to the model. Adding a tool here is a deliberate act that
# asserts "this tool is read-only and carries no authorization gate."
LOW_RISK_TOOL_ALLOWLIST: frozenset[str] = frozenset(
    {
        "webSearch",
        "web_search",
        "knowledgeLookup",
        "knowledge_lookup",
        "domainLookup",
        "recentChat",
        "recent_chat",
    }
)

# Scopes that mark a tool as side-effecting / privileged. A tool whose
# ToolDefinition declares ANY of these is denied to the model even if its
# name somehow appears on the allowlist -- a belt-and-suspenders guard so
# a registry change that adds a write scope to a previously-read tool
# cannot silently leak it to the model. "read" is intentionally absent:
# it is the only non-privileged scope.
PRIVILEGED_SCOPES: frozenset[str] = frozenset(
    {
        "navigate",
        "highlight",
        "create",
        "update",
        "delete",
        "identity",
        "billing",
        "execute",
        "write",
        "computer_use",
        "control",
    }
)


def is_low_risk_tool(
    name: str,
    *,
    client_execution: bool,
    scopes: list[str] | None,
) -> bool:
    """Return True iff ``name`` may be exposed to the Realtime model.

    The tier boundary, made explicit and default-deny:

    * The tool name must be on :data:`LOW_RISK_TOOL_ALLOWLIST`.
    * ``client_execution`` must be false -- a client-executed tool drives
      the browser UI (the CoPresent Operator surface) and is privileged
      by construction.
    * None of ``scopes`` may be in :data:`PRIVILEGED_SCOPES` -- any
      side-effecting / auth-gated scope denies the tool.

    Any tool that fails any clause is privileged-by-default and stays on
    the cognition-mediated authorized tool loop.
    """
    if name not in LOW_RISK_TOOL_ALLOWLIST:
        return False
    if client_execution:
        logger.debug("mcp bridge: deny %s -- client_execution tool is privileged", name)
        return False
    declared = set(scopes or ())
    privileged = declared & PRIVILEGED_SCOPES
    if privileged:
        logger.debug(
            "mcp bridge: deny %s -- declares privileged scope(s) %s",
            name,
            sorted(privileged),
        )
        return False
    return True


# ---------------------------------------------------------------------------
# Cognition awareness mirror
# ---------------------------------------------------------------------------

# A mirror sink receives one record per model-driven tool call + result.
# The default sink threads it into the cognition stream; tests substitute
# a recording sink. The record is a plain dict so it is trivially
# serializable onto the wire and inspectable in tests.
MirrorSink = Callable[[dict[str, Any]], Awaitable[None]]


@dataclass
class CognitionMirror:
    """Mirrors model-driven low-risk tool calls into the cognition stream.

    Every call the model makes via the MCP bridge is emitted here so
    cognition (history + conductor + chat/canvas UI) is aware of it.
    Mirroring is best-effort: a mirror failure must never fail the tool
    call the model is waiting on (the model still gets its result; we
    only lose the awareness breadcrumb, which we log). This mirrors the
    "do not let observability break the hot path" posture the rest of the
    voice-agent uses.
    """

    sink: MirrorSink
    space_id: str
    agent_id: str

    async def record_call(
        self,
        *,
        tool_name: str,
        arguments: dict[str, Any],
        result_text: str,
        is_error: bool,
    ) -> None:
        record = {
            "kind": "realtime_mcp_tool_call",
            "space_id": self.space_id,
            "agent_id": self.agent_id,
            "tool_name": tool_name,
            # JSON-encode so the wire shape matches CallToolMsg semantics
            # (arguments-as-JSON) and the mirror is self-describing.
            "arguments_json": json.dumps(arguments, default=str, sort_keys=True),
            "result_text": result_text,
            "is_error": is_error,
        }
        try:
            await self.sink(record)
        except Exception:  # noqa: BLE001 -- awareness must never break the call
            logger.exception(
                "mcp bridge: cognition mirror failed tool=%s space=%s "
                "(tool result still delivered to the model)",
                tool_name,
                self.space_id,
            )


# ---------------------------------------------------------------------------
# The bridge
# ---------------------------------------------------------------------------

# A tool-call transport executes a low-risk tool against memql's MCP
# surface and returns (result_text, is_error). The production transport
# sends a CallToolMsg on the MemqlService.Stream and awaits the
# CallToolResult; tests inject a fake. Kept as a narrow callable so this
# module does not import the generated proto at module scope (the stubs
# are built by `make voice-agent`, are absent in the Go-only CI, and the
# unit tests must not require them).
ToolCallTransport = Callable[[str, dict[str, Any]], Awaitable[tuple[str, bool]]]


@dataclass
class McpToolBridge:
    """Builds the low-risk Realtime tool set and mirrors every call.

    Construction takes the raw registry ``ToolDefinition`` list (already
    fetched via ``ListToolsMsg``), the transport that runs a tool via
    ``CallToolMsg``, and the cognition mirror. :meth:`build_function_tools`
    returns the filtered, model-facing tools; each tool's handler runs the
    call through ``transport`` and mirrors it via ``mirror`` before
    returning the result string to the model.
    """

    transport: ToolCallTransport
    mirror: CognitionMirror
    tool_definitions: list["ToolDef"]
    _exposed: list[str] = field(default_factory=list)
    _denied: list[str] = field(default_factory=list)

    @property
    def exposed_tool_names(self) -> list[str]:
        """Names handed to the model (set after build_function_tools)."""
        return list(self._exposed)

    @property
    def denied_tool_names(self) -> list[str]:
        """Names withheld from the model (privileged-by-default tier)."""
        return list(self._denied)

    def _low_risk_definitions(self) -> list["ToolDef"]:
        low_risk: list[ToolDef] = []
        self._exposed = []
        self._denied = []
        for td in self.tool_definitions:
            if is_low_risk_tool(
                td.name,
                client_execution=td.client_execution,
                scopes=td.scopes,
            ):
                low_risk.append(td)
                self._exposed.append(td.name)
            else:
                self._denied.append(td.name)
        return low_risk

    def build_function_tools(self) -> list[Any]:
        """Return livekit ``function_tool`` objects for the low-risk set.

        Each tool is a raw-schema function tool (name + description + JSON
        input schema straight from the registry ``ToolDefinition``) whose
        handler (a) runs the call through the memql MCP transport, (b)
        mirrors the call + result into cognition, and (c) returns the
        result text to the model. Privileged tools are excluded -- they
        never appear in the returned list.

        The import of ``function_tool`` is lazy so this module is
        importable without livekit installed (Go-only CI / docs builds).
        """
        from livekit.agents.llm import function_tool  # type: ignore

        low_risk = self._low_risk_definitions()
        tools: list[Any] = []
        for td in low_risk:
            tools.append(function_tool(self._make_handler(td), raw_schema=td.raw_schema()))

        logger.info(
            "mcp bridge: exposed %d low-risk tool(s) to the realtime model "
            "(%s); withheld %d privileged-by-default tool(s) (%s)",
            len(self._exposed),
            ", ".join(self._exposed) or "none",
            len(self._denied),
            ", ".join(self._denied) or "none",
        )
        return tools

    def _make_handler(self, td: "ToolDef") -> Callable[[dict[str, Any]], Awaitable[str]]:
        tool_name = td.name

        async def _handle(raw_arguments: dict[str, Any]) -> str:
            args = raw_arguments or {}
            result_text, is_error = await self.transport(tool_name, args)
            # Mirror BEFORE returning so cognition sees the call even if
            # the model immediately barges on. Mirror failure is swallowed
            # inside record_call -- it never fails the call.
            await self.mirror.record_call(
                tool_name=tool_name,
                arguments=args,
                result_text=result_text,
                is_error=is_error,
            )
            if is_error:
                # Surface the error to the model as the tool output so it
                # can decide to retry / pick another tool, matching the
                # text loop's behavior. Do not raise: a raised ToolError
                # would be opaque to the awareness mirror semantics above.
                return f"[tool error] {result_text}"
            return result_text

        return _handle


@dataclass(frozen=True)
class ToolDef:
    """The bridge's minimal view of a registry ``ToolDefinition``.

    Decouples the bridge + tests from the generated proto type: callers
    adapt the proto ``ToolDefinition`` (or any equivalent) into this shape
    via :func:`tool_defs_from_proto`. Carries exactly the fields the tier
    decision and the raw schema need.
    """

    name: str
    description: str
    input_schema: str  # JSON Schema as a JSON string (proto input_schema)
    client_execution: bool = False
    scopes: list[str] = field(default_factory=list)

    def raw_schema(self) -> dict[str, Any]:
        """The OpenAI/livekit raw-function-tool schema for this tool."""
        try:
            parameters = json.loads(self.input_schema) if self.input_schema else {}
        except json.JSONDecodeError:
            logger.warning(
                "mcp bridge: tool %s has non-JSON input_schema -- "
                "exposing with empty parameters",
                self.name,
            )
            parameters = {}
        if not isinstance(parameters, dict):
            parameters = {}
        # livekit's raw function tool requires a "parameters" key (an
        # object schema). Default to an empty object schema for tools that
        # take no arguments rather than omitting it (which raises).
        if "type" not in parameters:
            parameters = {"type": "object", "properties": {}}
        return {
            "name": self.name,
            "description": self.description or "",
            "parameters": parameters,
        }


def tool_defs_from_proto(proto_tools: list[Any]) -> list[ToolDef]:
    """Adapt generated-proto ``ToolDefinition`` messages into :class:`ToolDef`.

    Tolerant of either the generated proto message (fields ``name`` /
    ``description`` / ``input_schema`` / ``client_execution`` / ``scopes``)
    or any duck-typed stand-in, so this module need not import the proto
    stubs at module scope.
    """
    out: list[ToolDef] = []
    for t in proto_tools:
        out.append(
            ToolDef(
                name=getattr(t, "name", ""),
                description=getattr(t, "description", ""),
                input_schema=getattr(t, "input_schema", "") or "",
                client_execution=bool(getattr(t, "client_execution", False)),
                scopes=list(getattr(t, "scopes", []) or []),
            )
        )
    return out


async def attach_mcp_tool_bridge(
    realtime_model: Any,
    *,
    transport: ToolCallTransport,
    mirror: CognitionMirror,
    tool_definitions: list[ToolDef],
) -> McpToolBridge:
    """Register the low-risk MCP tool bridge on a Realtime model.

    Called from ``build_realtime_executor`` at the ``TODO(#435)`` seam.
    Builds the filtered low-risk tool set from ``tool_definitions``,
    registers it on ``realtime_model`` (via ``update_tools`` /
    ``set_tools`` -- whichever the installed livekit plugin exposes), and
    returns the :class:`McpToolBridge` so the caller can inspect what was
    exposed vs withheld.

    Registration is best-effort against the plugin surface: if the model
    object exposes neither setter the tools are still built (and the
    returned bridge reports them) so a plugin-version skew degrades to "no
    tools wired" rather than crashing the realtime session build.
    """
    bridge = McpToolBridge(
        transport=transport,
        mirror=mirror,
        tool_definitions=tool_definitions,
    )
    tools = bridge.build_function_tools()

    registered = False
    for setter_name in ("update_tools", "set_tools"):
        setter = getattr(realtime_model, setter_name, None)
        if callable(setter):
            result = setter(tools)
            if hasattr(result, "__await__"):
                await result
            registered = True
            logger.debug("mcp bridge: registered %d tool(s) via %s", len(tools), setter_name)
            break

    if not registered:
        logger.warning(
            "mcp bridge: realtime model exposes no update_tools/set_tools setter -- "
            "%d low-risk tool(s) built but not registered (plugin version skew?)",
            len(tools),
        )

    return bridge


# ---------------------------------------------------------------------------
# gRPC glue (production wiring from main.py)
# ---------------------------------------------------------------------------


def _result_text(call_tool_result: Any) -> str:
    """Flatten a CallToolResult's content list into a single text string."""
    parts: list[str] = []
    for item in getattr(call_tool_result, "content", []) or []:
        text = getattr(item, "text", "")
        if text:
            parts.append(text)
    return "\n".join(parts)


async def wire_realtime_mcp_tools(
    realtime_model: Any,
    *,
    client: Any,
    space_id: str,
    agent_id: str,
) -> McpToolBridge | None:
    """List memql tools, filter to low-risk, and bridge them to the model.

    This is the production glue called from ``main.py`` on the realtime
    path. It:

    1. fetches the registry via ``ListToolsMsg`` over the gRPC stream;
    2. adapts the proto ``ToolDefinition`` list into :class:`ToolDef`;
    3. builds a :class:`ToolCallTransport` backed by ``CallToolMsg`` and a
       :class:`CognitionMirror` whose sink emits the awareness breadcrumb
       back to memql; and
    4. registers the filtered low-risk tools on ``realtime_model``.

    Returns the :class:`McpToolBridge` (so the caller can log what was
    exposed) or ``None`` if the registry could not be fetched -- in which
    case the realtime session simply comes up with no model-driven tools,
    which is a safe degradation (privileged tools were never exposed
    anyway, and the cognition tool loop is unaffected).

    The generated proto is imported lazily here so this module stays
    importable without the stubs (Go-only CI / unit tests).
    """
    from voice_agent.proto import memql_pb2  # type: ignore

    try:
        envelope = await client.send_request(
            "list_tools",
            memql_pb2.ListToolsMsg(request_id=_new_request_id()),
        )
    except Exception:  # noqa: BLE001 -- a failed list must not abort session build
        logger.exception(
            "mcp bridge: ListToolsMsg failed space=%s -- realtime session "
            "comes up with no model-driven tools",
            space_id,
        )
        return None

    result = getattr(envelope, "list_tools_result", None)
    proto_tools = list(getattr(result, "tools", []) or []) if result is not None else []
    tool_definitions = tool_defs_from_proto(proto_tools)

    transport = _grpc_call_tool_transport(client, memql_pb2)
    mirror = CognitionMirror(
        sink=_default_mirror_sink,
        space_id=space_id,
        agent_id=agent_id,
    )
    return await attach_mcp_tool_bridge(
        realtime_model,
        transport=transport,
        mirror=mirror,
        tool_definitions=tool_definitions,
    )


def _grpc_call_tool_transport(client: Any, memql_pb2: Any) -> ToolCallTransport:
    """Build a ToolCallTransport backed by ``CallToolMsg`` on the stream.

    Running the model-driven call through memql's MCP surface (rather than
    any direct provider tool) is itself part of the awareness contract:
    the call traverses the same authorized backend path the text loop
    uses, so server-side tiering remains the source of truth even for the
    low-risk tools the model picks.
    """

    async def _transport(name: str, arguments: dict[str, Any]) -> tuple[str, bool]:
        from google.protobuf import struct_pb2  # type: ignore

        args_struct = struct_pb2.Struct()
        args_struct.update(arguments)
        reply = await client.send_request(
            "call_tool",
            memql_pb2.CallToolMsg(
                request_id=_new_request_id(),
                name=name,
                arguments=args_struct,
            ),
        )
        call_result = getattr(reply, "call_tool_result", None)
        if call_result is None:
            return ("tool returned no CallToolResult", True)
        return (_result_text(call_result), bool(getattr(call_result, "is_error", False)))

    return _transport


async def _default_mirror_sink(record: dict[str, Any]) -> None:
    """Default cognition-awareness sink for a model-driven tool call.

    Emits a structured ``voiceTrace``-shaped log line (the same convention
    the rest of the voice path uses, see
    ``docs/voice/432-conductor-response-gate.md`` section 5) carrying the
    full call + result. This is the awareness breadcrumb cognition reads:
    because the tool itself executed through memql's ``CallToolMsg`` path
    (see :func:`_grpc_call_tool_transport`), the backend already saw the
    call; this record makes the call legible in the per-space trace that
    history / the conductor / the chat-canvas "searched the web" affordance
    consume.

    A dedicated cognition event message (so the breadcrumb threads into the
    ``v1:cognition`` utterance/turn history as first-class structured data,
    rather than a parallel ``VoiceAgentFinalTranscript`` that would
    mis-attribute the call as a *human* utterance) is the integration seam
    left for the cognition-side follow-up; this sink is swappable via
    :class:`CognitionMirror.sink` without touching the bridge.
    """
    logger.info(
        "voiceTrace voice:%s stage=realtime.mcp.tool.call tool=%s is_error=%s "
        "agent=%s arguments=%s result=%s",
        record["space_id"],
        record["tool_name"],
        record["is_error"],
        record["agent_id"],
        record["arguments_json"],
        record["result_text"][:500],
    )


def _new_request_id() -> str:
    import uuid

    return uuid.uuid4().hex
