"""Custom Anam AvatarSession that supports `personaId` directly.

Background
----------
`livekit-plugins-anam` 1.5 only supports the "ephemeral" personaConfig
shape on Anam's session-token endpoint: it always builds
`{"type": "ephemeral", "name": ..., "avatarId": ..., "llmId": ...}`.
That's the path for clients minting an ad-hoc persona from a bare
avatar (face-model) id.

Anam also exposes a "persona" entity that bundles (face + voice + LLM
+ system prompt) under one id -- the personaId path. Configuring a
persona in the dashboard and referring to it by id is the natural way
to use Anam for production avatars, but the LK plugin doesn't support
it. Calling the API with a persona id in the ephemeral payload's
`avatarId` slot fails with:

    "The value passed as avatarId is a persona ID. Pass it as
    personaId instead, or supply a valid avatar ID."

This module subclasses `anam.AvatarSession` and overrides `start()` to
POST the persona-id shape (`{"personaConfig": {"personaId": "..."}}`)
when given a persona id, while keeping every other piece of the
plugin's lifecycle (LiveKit token mint, engine-session start, audio
output reassignment) intact.

Pre-flight before editing this file: read
`/usr/local/lib/python3.12/site-packages/livekit/plugins/anam/avatar.py`
inside the running container to confirm the base class's start()
shape hasn't drifted in a newer release.
"""

from __future__ import annotations

import logging
from typing import Any

import aiohttp

from livekit import api, rtc
from livekit.agents import APIConnectOptions, APIStatusError
from livekit.agents.types import NotGivenOr, NOT_GIVEN
from livekit.agents.voice.avatar import DataStreamAudioOutput
from livekit.agents.job import get_job_context
from livekit.plugins import anam
from livekit.plugins.anam.avatar import (
    _AVATAR_AGENT_IDENTITY,
    _AVATAR_AGENT_NAME,
    SAMPLE_RATE,
    ATTRIBUTE_PUBLISH_ON_BEHALF,
)
from livekit.plugins.anam.errors import AnamException

logger = logging.getLogger(__name__)


class AnamPersonaSession(anam.AvatarSession):
    """`anam.AvatarSession` variant that takes a `personaId` instead of an
    avatarId + ephemeral PersonaConfig.

    Inherits the base lifecycle (LK participant identity, audio output
    reassignment, error wrapping) and overrides only the session-token
    creation step.
    """

    def __init__(
        self,
        *,
        persona_id: str,
        api_url: NotGivenOr[str] = NOT_GIVEN,
        api_key: NotGivenOr[str] = NOT_GIVEN,
        avatar_participant_identity: NotGivenOr[str] = NOT_GIVEN,
        avatar_participant_name: NotGivenOr[str] = NOT_GIVEN,
        conn_options: APIConnectOptions = APIConnectOptions(
            max_retry=3, retry_interval=2.0, timeout=10.0
        ),
    ) -> None:
        # The base class requires a PersonaConfig. Build a stub one --
        # it's never used because we override start() before any code
        # path that reads `self._persona_config`.
        stub_config = anam.PersonaConfig(name="memql", avatarId="__unused__")
        super().__init__(
            persona_config=stub_config,
            api_url=api_url,
            api_key=api_key,
            avatar_participant_identity=avatar_participant_identity,
            avatar_participant_name=avatar_participant_name,
            conn_options=conn_options,
        )
        self._persona_id = persona_id

    async def start(
        self,
        agent_session: Any,
        room: rtc.Room,
        *,
        livekit_url: NotGivenOr[str] = NOT_GIVEN,
        livekit_api_key: NotGivenOr[str] = NOT_GIVEN,
        livekit_api_secret: NotGivenOr[str] = NOT_GIVEN,
    ) -> None:
        # Call the base class's parent (BaseAvatarSession) init -- we
        # need its bookkeeping for the audio reassignment below but we
        # are bypassing the anam.AvatarSession.start() implementation
        # entirely since it hardcodes the ephemeral payload shape.
        from livekit.agents.voice.avatar._types import AvatarSession as BaseAvatarSession

        await BaseAvatarSession.start(self, agent_session, room)

        import os

        # Anam's engine is a cloud service and must dial INTO the
        # LiveKit server using a publicly-reachable URL. The
        # voice-agent's own LIVEKIT_URL is usually the internal
        # Docker hostname (ws://livekit:7880) -- perfect for our
        # process but unreachable from outside. LIVEKIT_PUBLIC_URL
        # takes precedence when set so Anam gets the external face.
        lk_url = (
            livekit_url
            if (livekit_url is not NOT_GIVEN and livekit_url)
            else os.getenv("LIVEKIT_PUBLIC_URL") or os.getenv("LIVEKIT_URL")
        )
        lk_api_key = (
            livekit_api_key
            if (livekit_api_key is not NOT_GIVEN and livekit_api_key)
            else os.getenv("LIVEKIT_API_KEY")
        )
        lk_api_secret = (
            livekit_api_secret
            if (livekit_api_secret is not NOT_GIVEN and livekit_api_secret)
            else os.getenv("LIVEKIT_API_SECRET")
        )
        if not lk_url or not lk_api_key or not lk_api_secret:
            raise AnamException(
                "livekit_url, livekit_api_key, and livekit_api_secret must be set "
                "by arguments or environment variables"
            )

        job_ctx = get_job_context()
        local_participant_identity = job_ctx.local_participant_identity
        livekit_token = (
            api.AccessToken(
                api_key=lk_api_key,
                api_secret=lk_api_secret,
            )
            .with_kind("agent")
            .with_identity(self._avatar_participant_identity)
            .with_name(self._avatar_participant_name)
            .with_grants(api.VideoGrants(room_join=True, room=room.name))
            .with_attributes({ATTRIBUTE_PUBLISH_ON_BEHALF: local_participant_identity})
            .to_jwt()
        )

        async with aiohttp.ClientSession() as session:
            session_token = await self._create_session_token_with_persona_id(
                session=session,
                livekit_url=lk_url,
                livekit_token=livekit_token,
            )
            logger.debug("Anam persona session token created")

            engine_url = f"{self._api_url}/v1/engine/session"
            async with session.post(
                engine_url,
                headers={
                    "Authorization": f"Bearer {session_token}",
                    "Content-Type": "application/json",
                },
                json={},
                timeout=aiohttp.ClientTimeout(sock_connect=self._conn_options.timeout),
            ) as resp:
                if not resp.ok:
                    body = await resp.text()
                    raise APIStatusError(
                        f"Anam engine session failed: {resp.status}",
                        status_code=resp.status,
                        body=body,
                    )
                session_details = await resp.json()
                self.session_id = session_details.get("sessionId")
                logger.info(
                    "anam persona avatar started session_id=%s persona_id=%s",
                    self.session_id, self._persona_id,
                )

        agent_session.output.audio = DataStreamAudioOutput(
            room=room,
            destination_identity=self._avatar_participant_identity,
            sample_rate=SAMPLE_RATE,
        )

    async def _create_session_token_with_persona_id(
        self,
        *,
        session: aiohttp.ClientSession,
        livekit_url: str,
        livekit_token: str,
    ) -> str:
        """POST to /v1/auth/session-token, going through the ephemeral
        PersonaConfig path so OUR TTS audio is what the avatar
        lip-syncs to.

        Anam's personaConfig has two shapes:
          - `{personaId: "..."}` -- references a fully-bundled persona
            (face + voice + LLM + system prompt). Anam owns the audio;
            anything we publish into the DataStreamAudioOutput is
            ignored because Anam's own LLM/TTS is running.
          - `{type: "ephemeral", name: ..., avatarId: ..., llmId:
            "CUSTOMER_CLIENT_V1"}` -- the avatar is just the face;
            client (us) supplies the audio. Anam runs lip-sync on
            our PCM frames and republishes both audio + lip-synced
            video on the avatar participant.

        We want the ephemeral path. To use the persona's face without
        its bundled voice, fetch the persona record from Anam's API
        and pull its `avatarId`, then build the ephemeral config with
        that avatarId + our display name + the client-audio LLM. End
        result: Anam shows the user's chosen face but speaks with
        Sofia's Aura-2 voice -- one audio stream for voice-only AND
        voice+video sessions.
        """
        avatar_id = await self._fetch_avatar_id_for_persona(session)
        payload: dict[str, Any] = {
            "personaConfig": {
                "type": "ephemeral",
                "name": self._avatar_participant_name,
                "avatarId": avatar_id,
                "llmId": "CUSTOMER_CLIENT_V1",
            },
            "environment": {
                "livekitUrl": livekit_url,
                "livekitToken": livekit_token,
            },
        }
        async with session.post(
            f"{self._api_url}/v1/auth/session-token",
            headers={
                "Authorization": f"Bearer {self._api_key}",
                "Content-Type": "application/json",
            },
            json=payload,
            timeout=aiohttp.ClientTimeout(sock_connect=self._conn_options.timeout),
        ) as resp:
            if not resp.ok:
                body = await resp.text()
                raise APIStatusError(
                    f"Anam session-token (personaId) failed: {resp.status}",
                    status_code=resp.status,
                    body=body,
                )
            data = await resp.json()
            token = data.get("sessionToken")
            if not token:
                raise AnamException(
                    "Anam session-token response missing sessionToken"
                )
            return token

    async def _fetch_avatar_id_for_persona(
        self,
        session: aiohttp.ClientSession,
    ) -> str:
        """GET the persona record from Anam's API and return its
        avatarId. The personaId the user configured in
        ANAM_DEFAULT_PERSONA_ID names a bundled persona ("Liv",
        etc.); the avatarId inside it is the face-model id we want
        for the ephemeral PersonaConfig.

        Cached on the instance after the first fetch so a session
        that reconnects mid-room doesn't re-hit the persona endpoint.
        """
        cached = getattr(self, "_cached_avatar_id", None)
        if cached:
            return cached

        url = f"{self._api_url}/v1/personas/{self._persona_id}"
        async with session.get(
            url,
            headers={
                "Authorization": f"Bearer {self._api_key}",
                "Accept": "application/json",
            },
            timeout=aiohttp.ClientTimeout(sock_connect=self._conn_options.timeout),
        ) as resp:
            if not resp.ok:
                body = await resp.text()
                raise APIStatusError(
                    f"Anam persona lookup failed: {resp.status}",
                    status_code=resp.status,
                    body=body,
                )
            data = await resp.json()
            # Anam's persona JSON shape: top-level `avatarId` field on
            # the persona record. Some accounts surface it nested under
            # `personaConfig.avatarId`; probe both for robustness.
            avatar_id = data.get("avatarId")
            if not avatar_id:
                nested = data.get("personaConfig") or {}
                avatar_id = nested.get("avatarId")
            if not avatar_id:
                raise AnamException(
                    f"Anam persona {self._persona_id} has no avatarId "
                    f"in the response: keys={sorted(data.keys())}"
                )
            self._cached_avatar_id = avatar_id
            logger.info(
                "anam: resolved persona %s -> avatar %s",
                self._persona_id, avatar_id,
            )
            return avatar_id
