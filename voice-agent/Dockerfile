# memql voice-agent (LiveKit Agents 1.5)
#
# Built from the memql repo root so we can copy the canonical proto
# alongside the Python source:
#   docker build -f voice-agent/Dockerfile -t memql-voice-agent .
#
# Runtime env vars (see voice-agent/README.md):
#   LIVEKIT_URL, LIVEKIT_API_KEY, LIVEKIT_API_SECRET
#   MEMQL_DEEPGRAM_API_KEY
#   MEMQL_GRPC_ADDR, VOICE_AGENT_TOKEN
#   MEMQL_AVATAR_VENDOR (anam | simli | none), ANAM_API_KEY / SIMLI_API_KEY

FROM python:3.12-slim AS builder

ENV PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PYTHONDONTWRITEBYTECODE=1

WORKDIR /app

# Build deps for any wheels we have to compile from source.
RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential \
    && rm -rf /var/lib/apt/lists/*

COPY voice-agent/pyproject.toml ./
COPY voice-agent/voice_agent ./voice_agent

# Proto generation. Keep stubs inside the image so the runtime stage
# doesn't need grpcio-tools.
COPY component/grpc/memql.proto /tmp/memql.proto
RUN pip install --upgrade pip setuptools wheel \
    && pip install grpcio-tools protobuf \
    && python -m grpc_tools.protoc \
        -I/tmp \
        --python_out=voice_agent/proto \
        --grpc_python_out=voice_agent/proto \
        /tmp/memql.proto \
    && sed -i 's/^import memql_pb2/from voice_agent.proto import memql_pb2/' \
        voice_agent/proto/memql_pb2_grpc.py

# Pinned to anam extras by default; rebuild with --build-arg AVATAR=simli for that vendor.
ARG AVATAR=anam
RUN pip install ".[${AVATAR}]"


FROM python:3.12-slim AS runtime

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

WORKDIR /app

# Pull installed site-packages + binary plus the local source tree.
COPY --from=builder /usr/local/lib/python3.12/site-packages /usr/local/lib/python3.12/site-packages
COPY --from=builder /usr/local/bin /usr/local/bin
COPY --from=builder /app /app

USER 1000:1000

ENTRYPOINT ["memql-voice-agent"]
CMD ["start"]
