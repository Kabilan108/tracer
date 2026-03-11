module github.com/tracer-ai/tracer-cli

// When updating the Go version, also update:
//   - README.md
//   - .golangci.yml (run.go)
//   - ../.github/workflows/ci.yml (setup-go)
//   - ../.github/workflows/release.yml (setup-go)
go 1.26.0

// To check for outdated direct dependencies:
// `go list -m -u -json all | jq -r 'select(.Indirect != true) | select(.Update != null) | "\(.Path) \(.Version) -> \(.Update.Version)"'`
require (
	github.com/BurntSushi/toml v1.6.0 // TOML parsing for configuration files
	github.com/fsnotify/fsnotify v1.9.0 // Cross-platform file system event notifications
	github.com/google/uuid v1.6.0 // indirect; Generates and inspects UUIDs
	github.com/spf13/cobra v1.10.2 // Command-line interface framework
	github.com/xeipuuv/gojsonschema v1.2.0 // JSON document validation against a JSON schema
	go.opentelemetry.io/otel v1.40.0 // send observability data to observability platforms
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc v1.40.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.40.0
	go.opentelemetry.io/otel/metric v1.40.0
	go.opentelemetry.io/otel/sdk v1.40.0
	go.opentelemetry.io/otel/sdk/metric v1.40.0
	go.opentelemetry.io/otel/trace v1.40.0
	golang.org/x/text v0.34.0 // Text processing and Unicode normalization
	modernc.org/sqlite v1.46.1 // Pure Go SQLite database driver
)

require github.com/spf13/pflag v1.0.9

require (
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.27.7 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/xeipuuv/gojsonpointer v0.0.0-20180127040702-4e3ac2762d5f // indirect
	github.com/xeipuuv/gojsonreference v0.0.0-20180127040603-bd5ef7bd5415 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.40.0 // indirect
	go.opentelemetry.io/proto/otlp v1.9.0 // indirect
	golang.org/x/exp v0.0.0-20251023183803-a4bb9ffd2546 // indirect
	golang.org/x/net v0.49.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260128011058-8636f8732409 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260128011058-8636f8732409 // indirect
	google.golang.org/grpc v1.78.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	modernc.org/libc v1.67.6 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
