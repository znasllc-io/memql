"""Tests for the Realtime MCP tool bridge (#435).

These cover the risk-tier boundary and the cognition-awareness mirror
without standing up a real OpenAI Realtime session or the memql gRPC
stream:

* low-risk read tools (web search, knowledge lookup, recent-chat) ARE
  exposed to the model;
* privileged / side-effecting tools (client_execution, write/navigate
  scopes, computer_use) are NOT exposed -- default-deny;
* a tool nobody has tiered is privileged-by-default;
* every model-driven call is mirrored into cognition (call + result);
* a mirror failure never fails the tool call the model awaits;
* tool-call errors are surfaced to the model rather than swallowed.

The bridge builds livekit ``function_tool`` objects; the OpenAI plugin
is installed in CI via ``make voice-agent`` so ``function_tool`` is
importable. The memql gRPC transport is faked.
"""

from __future__ import annotations

from typing import Any

import pytest

from voice_agent.mcp_tool_bridge import (
    LOW_RISK_TOOL_ALLOWLIST,
    PRIVILEGED_SCOPES,
    CognitionMirror,
    McpToolBridge,
    ToolDef,
    attach_mcp_tool_bridge,
    is_low_risk_tool,
    tool_defs_from_proto,
)


# --- fixtures / helpers ----------------------------------------------------


def _read_tool(name: str, scopes: list[str] | None = None) -> ToolDef:
    return ToolDef(
        name=name,
        description=f"{name} description",
        input_schema='{"type":"object","properties":{"q":{"type":"string"}}}',
        client_execution=False,
        scopes=scopes if scopes is not None else ["read"],
    )


def _privileged_tool(
    name: str, *, client_execution: bool = False, scopes: list[str] | None = None
) -> ToolDef:
    return ToolDef(
        name=name,
        description=f"{name} description",
        input_schema='{"type":"object"}',
        client_execution=client_execution,
        scopes=scopes or [],
    )


class _RecordingMirror:
    """A mirror sink that records every record it receives."""

    def __init__(self) -> None:
        self.records: list[dict[str, Any]] = []

    async def __call__(self, record: dict[str, Any]) -> None:
        self.records.append(record)


def _bridge(tool_definitions: list[ToolDef]) -> McpToolBridge:
    """A bridge with an inert ok-transport + recording mirror.

    Used by the synchronous tier-boundary tests that only inspect which
    tools are exposed vs withheld (they never invoke a handler).
    """

    async def _ok_transport(name: str, args: dict[str, Any]) -> tuple[str, bool]:
        return (f"result for {name}", False)

    mirror = CognitionMirror(
        sink=_RecordingMirror(), space_id="space-1", agent_id="space-1-ga"
    )
    return McpToolBridge(
        transport=_ok_transport,
        mirror=mirror,
        tool_definitions=tool_definitions,
    )


# --- tier boundary: is_low_risk_tool --------------------------------------


@pytest.mark.parametrize("name", sorted(LOW_RISK_TOOL_ALLOWLIST))
def test_allowlisted_read_tool_is_low_risk(name: str) -> None:
    assert is_low_risk_tool(name, client_execution=False, scopes=["read"]) is True


def test_unlisted_tool_is_privileged_by_default() -> None:
    # Default-deny: a tool nobody tiered is never low-risk, even with a
    # benign-looking read scope.
    assert is_low_risk_tool("someNewTool", client_execution=False, scopes=["read"]) is False


def test_client_execution_tool_is_never_low_risk() -> None:
    # A client_execution tool drives the browser UI (CoPresent Operator)
    # and is privileged even if its name were allowlisted.
    assert is_low_risk_tool("webSearch", client_execution=True, scopes=["read"]) is False


@pytest.mark.parametrize("scope", sorted(PRIVILEGED_SCOPES))
def test_privileged_scope_denies_even_allowlisted_tool(scope: str) -> None:
    # Belt-and-suspenders: an allowlisted tool that grows a side-effecting
    # scope is denied.
    assert is_low_risk_tool("webSearch", client_execution=False, scopes=[scope]) is False


def test_no_scopes_allowlisted_tool_is_low_risk() -> None:
    # A tool with no declared scopes (implicitly read-only) on the
    # allowlist is fine.
    assert is_low_risk_tool("recentChat", client_execution=False, scopes=None) is True


# --- bridge: which tools reach the model ----------------------------------


def test_only_low_risk_tools_are_exposed() -> None:
    defs = [
        _read_tool("webSearch"),
        _read_tool("knowledgeLookup"),
        _read_tool("recentChat", scopes=None),
        _privileged_tool("copresentControl", client_execution=True, scopes=["navigate"]),
        _privileged_tool("createSpace", scopes=["create"]),
        _privileged_tool("workerComputer", scopes=["computer_use"]),
        _privileged_tool("someUntieredTool", scopes=["read"]),
    ]
    bridge = _bridge(defs)
    tools = bridge.build_function_tools()

    assert sorted(bridge.exposed_tool_names) == [
        "knowledgeLookup",
        "recentChat",
        "webSearch",
    ]
    # The privileged + untiered tools are all withheld.
    assert "copresentControl" in bridge.denied_tool_names
    assert "createSpace" in bridge.denied_tool_names
    assert "workerComputer" in bridge.denied_tool_names
    assert "someUntieredTool" in bridge.denied_tool_names
    # One function_tool object per exposed tool.
    assert len(tools) == 3


def test_privileged_tools_are_never_handed_to_the_model() -> None:
    defs = [
        _privileged_tool("copresentControl", client_execution=True, scopes=["navigate"]),
        _privileged_tool("workerHost", scopes=["execute"]),
        _privileged_tool("mutationCreateSpace", scopes=["create"]),
    ]
    bridge = _bridge(defs)
    tools = bridge.build_function_tools()
    assert tools == []
    assert bridge.exposed_tool_names == []
    assert sorted(bridge.denied_tool_names) == [
        "copresentControl",
        "mutationCreateSpace",
        "workerHost",
    ]


# --- mirror: every model-driven call is mirrored into cognition -----------


async def _invoke_first_tool(bridge: McpToolBridge, **args: Any) -> str:
    """Build the tools and invoke the first one's raw handler directly."""
    bridge.build_function_tools()
    # The handler is the bound coroutine the function_tool wraps; reach it
    # via the bridge's own factory so we exercise the exact production path
    # (build_function_tools wires the same _make_handler).
    td = next(
        t
        for t in bridge.tool_definitions
        if is_low_risk_tool(t.name, client_execution=t.client_execution, scopes=t.scopes)
    )
    handler = bridge._make_handler(td)  # noqa: SLF001 -- exercising the real handler
    return await handler(args)


@pytest.mark.asyncio
async def test_model_driven_call_is_mirrored_into_cognition() -> None:
    recorder = _RecordingMirror()

    async def _transport(name: str, args: dict[str, Any]) -> tuple[str, bool]:
        return (f"hits for {args.get('q')}", False)

    mirror = CognitionMirror(sink=recorder, space_id="space-1", agent_id="space-1-ga")
    bridge = McpToolBridge(
        transport=_transport,
        mirror=mirror,
        tool_definitions=[_read_tool("webSearch")],
    )

    out = await _invoke_first_tool(bridge, q="memql")

    assert out == "hits for memql"
    assert len(recorder.records) == 1
    rec = recorder.records[0]
    assert rec["kind"] == "realtime_mcp_tool_call"
    assert rec["tool_name"] == "webSearch"
    assert rec["space_id"] == "space-1"
    assert rec["agent_id"] == "space-1-ga"
    assert rec["is_error"] is False
    assert "memql" in rec["arguments_json"]
    assert rec["result_text"] == "hits for memql"


@pytest.mark.asyncio
async def test_tool_error_is_mirrored_and_surfaced_to_model() -> None:
    recorder = _RecordingMirror()

    async def _err_transport(name: str, args: dict[str, Any]) -> tuple[str, bool]:
        return ("upstream search unavailable", True)

    mirror = CognitionMirror(sink=recorder, space_id="space-1", agent_id="space-1-ga")
    bridge = McpToolBridge(
        transport=_err_transport,
        mirror=mirror,
        tool_definitions=[_read_tool("webSearch")],
    )

    out = await _invoke_first_tool(bridge, q="x")

    # The error is surfaced to the model (so it can retry / pivot) ...
    assert out == "[tool error] upstream search unavailable"
    # ... and still mirrored into cognition with is_error=True.
    assert len(recorder.records) == 1
    assert recorder.records[0]["is_error"] is True


@pytest.mark.asyncio
async def test_mirror_failure_never_fails_the_tool_call() -> None:
    async def _failing_sink(record: dict[str, Any]) -> None:
        raise RuntimeError("cognition stream down")

    async def _transport(name: str, args: dict[str, Any]) -> tuple[str, bool]:
        return ("the result", False)

    mirror = CognitionMirror(sink=_failing_sink, space_id="s", agent_id="a")
    bridge = McpToolBridge(
        transport=_transport,
        mirror=mirror,
        tool_definitions=[_read_tool("webSearch")],
    )

    # The model still receives its result even though the mirror blew up.
    out = await _invoke_first_tool(bridge, q="x")
    assert out == "the result"


# --- proto adaptation + registration --------------------------------------


def test_tool_defs_from_proto_reads_all_fields() -> None:
    class _ProtoTool:
        def __init__(self) -> None:
            self.name = "webSearch"
            self.description = "Search the web"
            self.input_schema = '{"type":"object"}'
            self.client_execution = False
            self.scopes = ["read"]

    defs = tool_defs_from_proto([_ProtoTool()])
    assert len(defs) == 1
    assert defs[0].name == "webSearch"
    assert defs[0].scopes == ["read"]
    assert defs[0].client_execution is False


@pytest.mark.asyncio
async def test_attach_registers_only_low_risk_tools_on_model() -> None:
    class _FakeRealtimeModel:
        def __init__(self) -> None:
            self.registered: list[Any] = []

        def update_tools(self, tools: list[Any]) -> None:
            self.registered = tools

    model = _FakeRealtimeModel()

    async def _transport(name: str, args: dict[str, Any]) -> tuple[str, bool]:
        return ("ok", False)

    mirror = CognitionMirror(
        sink=_RecordingMirror(), space_id="space-1", agent_id="space-1-ga"
    )
    bridge = await attach_mcp_tool_bridge(
        model,
        transport=_transport,
        mirror=mirror,
        tool_definitions=[
            _read_tool("webSearch"),
            _privileged_tool("copresentControl", client_execution=True, scopes=["navigate"]),
        ],
    )

    assert bridge.exposed_tool_names == ["webSearch"]
    assert bridge.denied_tool_names == ["copresentControl"]
    # Exactly the one low-risk tool was registered on the model.
    assert len(model.registered) == 1


@pytest.mark.asyncio
async def test_attach_tolerates_model_without_setter() -> None:
    # A plugin-version skew where the model exposes no setter must not
    # crash session build -- the bridge still reports what it would expose.
    class _BareModel:
        pass

    async def _transport(name: str, args: dict[str, Any]) -> tuple[str, bool]:
        return ("ok", False)

    mirror = CognitionMirror(
        sink=_RecordingMirror(), space_id="space-1", agent_id="space-1-ga"
    )
    bridge = await attach_mcp_tool_bridge(
        _BareModel(),
        transport=_transport,
        mirror=mirror,
        tool_definitions=[_read_tool("webSearch")],
    )
    assert bridge.exposed_tool_names == ["webSearch"]
