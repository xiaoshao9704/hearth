module hearth/server

go 1.27.0

require (
	github.com/go-chi/chi/v5 v5.3.2
	github.com/go-sql-driver/mysql v1.10.0
	github.com/huin/goupnp v1.3.0
	github.com/jackc/pgx/v5 v5.10.0
	github.com/livekit/livekit-server v1.13.6
	github.com/livekit/protocol v1.51.1-0.20260903060125-0cf5ba018b8e
	github.com/livekit/server-sdk-go/v2 v2.18.2-0.20260903135950-af5ddd69d657
	github.com/pion/ice/v4 v4.4.0
	github.com/pion/rtcp v1.2.17
	github.com/pion/rtp v1.10.5
	github.com/pion/sdp/v3 v3.0.19
	github.com/pion/stun/v3 v3.1.7
	github.com/pion/webrtc/v4 v4.2.19
	github.com/uptrace/bun v1.2.18
	github.com/uptrace/bun/dialect/mysqldialect v1.2.18
	github.com/uptrace/bun/dialect/pgdialect v1.2.18
	github.com/uptrace/bun/dialect/sqlitedialect v1.2.18
	golang.org/x/crypto v0.55.0
	golang.org/x/net v0.57.0
	golang.org/x/sys v0.47.0
	modernc.org/sqlite v1.57.0
	nhooyr.io/websocket v1.8.17
)

require (
	buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go v1.36.11-20260709200747-435963d16310.1 // indirect
	buf.build/go/protovalidate v1.2.0 // indirect
	buf.build/go/protoyaml v0.7.0 // indirect
	cel.dev/expr v0.25.2 // indirect
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/benbjohnson/clock v1.3.5 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bep/debounce v1.2.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/d5/tengo/v2 v2.17.0 // indirect
	github.com/dennwc/iters v1.2.2 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/elliotchance/orderedmap/v3 v3.1.1 // indirect
	github.com/florianl/go-tc v0.4.8 // indirect
	github.com/frostbyte73/core v0.1.1 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/gammazero/deque v1.2.1 // indirect
	github.com/gammazero/workerpool v1.2.1 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/cel-go v0.30.0 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/google/wire v0.7.0 // indirect
	github.com/gorilla/websocket v1.5.4-0.20250319132907-e064f32e3674 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	github.com/hashicorp/go-retryablehttp v0.7.8 // indirect
	github.com/hashicorp/go-version v1.9.0 // indirect
	github.com/hashicorp/golang-lru v1.0.2 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jellydator/ttlcache/v3 v3.4.1 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/josharian/native v1.1.0 // indirect
	github.com/jxskiss/base62 v1.1.0 // indirect
	github.com/klauspost/compress v1.19.1 // indirect
	github.com/klauspost/cpuid/v2 v2.4.0 // indirect
	github.com/lithammer/shortuuid/v4 v4.2.0 // indirect
	github.com/livekit/mageutil v0.0.0-20250511045019-0f1ff63f7731 // indirect
	github.com/livekit/mediatransportutil v0.0.0-20260821083140-f234b534b095 // indirect
	github.com/livekit/psrpc v0.7.5 // indirect
	github.com/mackerelio/go-osstat v0.2.8 // indirect
	github.com/magefile/mage v1.17.2 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mdlayher/netlink v1.11.2 // indirect
	github.com/mdlayher/socket v0.6.1 // indirect
	github.com/mitchellh/go-homedir v1.1.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/nats-io/nats.go v1.52.0 // indirect
	github.com/nats-io/nkeys v0.4.16 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/nyaruka/phonenumbers v1.8.1 // indirect
	github.com/petermattis/goid v0.0.0-20260725062400-500c67a39b75 // indirect
	github.com/pion/datachannel v1.6.2 // indirect
	github.com/pion/dtls/v3 v3.1.5 // indirect
	github.com/pion/interceptor v0.1.47 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/mdns/v2 v2.1.0 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/sctp v1.11.1 // indirect
	github.com/pion/srtp/v3 v3.0.13 // indirect
	github.com/pion/transport/v4 v4.1.0 // indirect
	github.com/pion/turn/v5 v5.0.13 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/prometheus/client_golang v1.24.1 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	github.com/puzpuzpuz/xsync/v3 v3.5.1 // indirect
	github.com/puzpuzpuz/xsync/v4 v4.5.0 // indirect
	github.com/redis/go-redis/v9 v9.22.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rs/cors v1.11.1 // indirect
	github.com/thoas/go-funk v0.9.3 // indirect
	github.com/tmthrgd/go-hex v0.0.0-20190904060850-447a3041c3bc // indirect
	github.com/tomnomnom/linkheader v0.0.0-20250811210735-e5fe3b51442e // indirect
	github.com/twitchtv/twirp v8.1.3+incompatible // indirect
	github.com/ua-parser/uap-go v0.0.0-20260529044130-17c35e68e58c // indirect
	github.com/urfave/cli/v3 v3.10.1 // indirect
	github.com/urfave/negroni/v3 v3.1.1 // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.45.0 // indirect
	go.opentelemetry.io/otel/metric v1.45.0 // indirect
	go.opentelemetry.io/otel/trace v1.45.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.28.0 // indirect
	go.uber.org/zap/exp v0.3.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/exp v0.0.0-20260727155853-b88d891fe743 // indirect
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260803160001-6ac0973c030d // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260803160001-6ac0973c030d // indirect
	google.golang.org/grpc v1.83.1 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	modernc.org/libc v1.74.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

replace github.com/livekit/livekit-server => github.com/xiaoshao9704/livekit v1.13.6-hearth.1
