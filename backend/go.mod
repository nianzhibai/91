module github.com/video-site/backend

go 1.27.3

require (
	github.com/OpenListTeam/wopan-sdk-go v0.2.0 // Newer releases require prerelease resty.dev/v3.
	github.com/SheltonZhu/115driver v1.3.5
	github.com/aliyun/aliyun-oss-go-sdk v3.0.2+incompatible
	github.com/andybalholm/brotli v1.2.6
	github.com/go-chi/chi/v5 v5.5.0
	github.com/go-resty/resty/v2 v2.17.2
	github.com/google/uuid v1.6.0
	github.com/gotd/td v0.163.0
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e
	golang.org/x/crypto v0.58.0
	golang.org/x/image v0.47.0
	golang.org/x/net v0.61.0
	golang.org/x/sys v0.49.0
	golang.org/x/text v0.43.0
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.61.0
)

require (
	github.com/aead/ecdh v0.2.0 // indirect
	github.com/andreburgaud/crypt2go v1.1.0 // indirect
	github.com/cenkalti/backoff/v4 v4.3.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/coder/websocket v1.8.15 // indirect
	github.com/dlclark/regexp2 v1.12.0 // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/fatih/color v1.19.0 // indirect
	github.com/ghodss/yaml v1.0.0 // indirect
	github.com/go-faster/errors v0.8.0 // indirect
	github.com/go-faster/jx v1.2.0 // indirect
	github.com/go-faster/xor v1.0.0 // indirect
	github.com/go-faster/yaml v0.4.6 // indirect
	github.com/gotd/ige v0.3.0 // indirect
	github.com/gotd/log v0.1.0 // indirect
	github.com/gotd/neo v0.1.5 // indirect
	github.com/klauspost/compress v1.19.1 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/ogen-go/ogen v1.23.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.17 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/refraction-networking/utls v1.8.2 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	github.com/yuin/goldmark v1.8.5 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.28.0 // indirect
	golang.org/x/exp v0.0.0-20230725093048-515e97ebf090 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/time v0.14.0 // indirect
	golang.org/x/tools v0.52.0 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	modernc.org/libc v1.79.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	rsc.io/qr v0.2.0 // indirect
)

// 依赖已通过 go mod vendor 打包进 backend/vendor/ 并入库，支持离线构建。
// 升级 SDK 请使用标准流程：
//   go get github.com/SheltonZhu/115driver@<版本>
//   go mod tidy
//   go mod vendor

ignore (
	./data
	./tmp
)
