// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/ci-design.md, section D3.
module github.com/znasllc-io/memql/component/server

go 1.26.1

toolchain go1.26.5

require (
	github.com/google/uuid v1.6.0
	github.com/stretchr/testify v1.11.1
	github.com/znasllc-io/memql/component/auth v0.0.0
	github.com/znasllc-io/memql/component/automations v0.0.0
	github.com/znasllc-io/memql/component/bus v0.0.0
	github.com/znasllc-io/memql/component/database v0.0.0
	github.com/znasllc-io/memql/component/events v0.0.0
	github.com/znasllc-io/memql/component/grpc/gen v0.0.0
	github.com/znasllc-io/memql/component/language v0.0.0
	github.com/znasllc-io/memql/component/memql v0.0.0
	github.com/znasllc-io/memql/component/polyphon v0.0.0
	github.com/znasllc-io/memql/core v0.0.0
	github.com/znasllc-io/memql/integrations/stt v0.0.0
	google.golang.org/grpc v1.83.0
	google.golang.org/protobuf v1.36.11
	nhooyr.io/websocket v1.8.17
)

require (
	buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go v1.36.11-20260415201107-50325440f8f2.1 // indirect
	buf.build/go/protovalidate v1.2.0 // indirect
	buf.build/go/protoyaml v0.7.0 // indirect
	cel.dev/expr v0.25.2 // indirect
	github.com/anthropics/anthropic-sdk-go v1.61.0 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/benbjohnson/clock v1.3.5 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/buger/jsonparser v1.1.2 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/dennwc/iters v1.2.2 // indirect
	github.com/dgraph-io/ristretto v0.2.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/frostbyte73/core v0.1.1 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/gammazero/deque v1.2.1 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/cel-go v0.29.0 // indirect
	github.com/invopop/jsonschema v0.14.0 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jxskiss/base62 v1.1.0 // indirect
	github.com/klauspost/compress v1.19.1 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/lib/pq v1.12.3 // indirect
	github.com/lithammer/shortuuid/v4 v4.2.0 // indirect
	github.com/livekit/mageutil v0.0.0-20250511045019-0f1ff63f7731 // indirect
	github.com/livekit/protocol v1.49.0 // indirect
	github.com/livekit/psrpc v0.7.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/nats-io/nats.go v1.52.0 // indirect
	github.com/nats-io/nkeys v0.4.16 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/pb33f/ordered-map/v2 v2.3.1 // indirect
	github.com/pion/datachannel v1.6.2 // indirect
	github.com/pion/dtls/v3 v3.1.5 // indirect
	github.com/pion/ice/v4 v4.4.0 // indirect
	github.com/pion/interceptor v0.1.47 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/mdns/v2 v2.1.0 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.17 // indirect
	github.com/pion/rtp v1.10.5 // indirect
	github.com/pion/sctp v1.11.1 // indirect
	github.com/pion/sdp/v3 v3.0.19 // indirect
	github.com/pion/srtp/v3 v3.0.12 // indirect
	github.com/pion/stun/v3 v3.1.6 // indirect
	github.com/pion/transport/v4 v4.0.2 // indirect
	github.com/pion/turn/v5 v5.0.12 // indirect
	github.com/pion/webrtc/v4 v4.2.18 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/client_golang v1.24.1 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	github.com/puzpuzpuz/xsync/v3 v3.5.1 // indirect
	github.com/puzpuzpuz/xsync/v4 v4.5.0 // indirect
	github.com/redis/go-redis/v9 v9.20.0 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
	github.com/santhosh-tekuri/jsonschema/v5 v5.3.1 // indirect
	github.com/sashabaranov/go-openai v1.42.0 // indirect
	github.com/standard-webhooks/standard-webhooks/libraries v0.0.1 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/tmthrgd/go-hex v0.0.0-20190904060850-447a3041c3bc // indirect
	github.com/twitchtv/twirp v8.1.3+incompatible // indirect
	github.com/uptrace/bun v1.2.18 // indirect
	github.com/uptrace/bun/dialect/pgdialect v1.2.18 // indirect
	github.com/uptrace/bun/driver/pgdriver v1.2.18 // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	github.com/zeozeozeo/gomplerate v0.0.0-20250404113140-0fbb236df825 // indirect
	github.com/znasllc-io/memql/component/actions v0.0.0 // indirect
	github.com/znasllc-io/memql/component/bus/gen v0.0.0 // indirect
	github.com/znasllc-io/memql/component/config v0.0.0 // indirect
	github.com/znasllc-io/memql/component/genesis v0.0.0 // indirect
	github.com/znasllc-io/memql/component/harness v0.0.0 // indirect
	github.com/znasllc-io/memql/component/language/annotations v0.0.0 // indirect
	github.com/znasllc-io/memql/component/language/ast v0.0.0 // indirect
	github.com/znasllc-io/memql/component/language/dslclause v0.0.0 // indirect
	github.com/znasllc-io/memql/component/provenance v0.0.0 // indirect
	github.com/znasllc-io/memql/component/safety v0.0.0 // indirect
	github.com/znasllc-io/memql/component/secret v0.0.0 // indirect
	github.com/znasllc-io/memql/docs v0.0.0 // indirect
	github.com/znasllc-io/memql/dsl v0.0.0 // indirect
	github.com/znasllc-io/memql/integrations/openai v0.0.0 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.28.0 // indirect
	go.uber.org/zap/exp v0.3.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.2 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/exp v0.0.0-20260603202125-055de637280b // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	mellium.im/sasl v0.3.2 // indirect
)

replace github.com/znasllc-io/memql/component/actions => ../actions

replace github.com/znasllc-io/memql/component/auth => ../auth

replace github.com/znasllc-io/memql/component/automations => ../automations

replace github.com/znasllc-io/memql/component/bus => ../bus

replace github.com/znasllc-io/memql/component/bus/gen => ../bus/gen

replace github.com/znasllc-io/memql/component/config => ../config

replace github.com/znasllc-io/memql/component/database => ../database

replace github.com/znasllc-io/memql/component/events => ../events

replace github.com/znasllc-io/memql/component/genesis => ../genesis

replace github.com/znasllc-io/memql/component/grpc/gen => ../grpc/gen

replace github.com/znasllc-io/memql/component/harness => ../harness

replace github.com/znasllc-io/memql/component/language => ../language

replace github.com/znasllc-io/memql/component/language/annotations => ../language/annotations

replace github.com/znasllc-io/memql/component/language/ast => ../language/ast

replace github.com/znasllc-io/memql/component/language/dslclause => ../language/dslclause

replace github.com/znasllc-io/memql/component/memql => ../memql

replace github.com/znasllc-io/memql/component/polyphon => ../polyphon

replace github.com/znasllc-io/memql/component/provenance => ../provenance

replace github.com/znasllc-io/memql/component/safety => ../safety

replace github.com/znasllc-io/memql/component/secret => ../secret

replace github.com/znasllc-io/memql/core => ../../core

replace github.com/znasllc-io/memql/docs => ../../docs

replace github.com/znasllc-io/memql/dsl => ../../dsl

replace github.com/znasllc-io/memql/integrations/openai => ../../integrations/openai

replace github.com/znasllc-io/memql/integrations/stt => ../../integrations/stt

replace github.com/znasllc-io/memql/component/deploycontrol => ../deploycontrol

replace github.com/znasllc-io/memql/component/healing => ../healing

replace github.com/znasllc-io/memql/component/identity => ../identity

replace github.com/znasllc-io/memql/component/metrics => ../metrics
