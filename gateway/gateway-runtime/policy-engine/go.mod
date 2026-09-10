module github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine

go 1.26.5

require (
	github.com/andybalholm/brotli v1.2.0
	github.com/envoyproxy/go-control-plane/envoy v1.37.0
	github.com/go-viper/mapstructure/v2 v2.5.0
	github.com/google/cel-go v0.26.1
	github.com/google/uuid v1.6.0
	github.com/knadh/koanf/parsers/toml/v2 v2.2.0
	github.com/knadh/koanf/providers/confmap v1.0.0
	github.com/knadh/koanf/providers/file v1.2.1
	github.com/knadh/koanf/v2 v2.3.2
	github.com/moesif/moesifapi-go v1.1.5
	github.com/prometheus/client_golang v1.23.2
	github.com/stretchr/testify v1.11.1
	github.com/wso2/api-platform/common v0.0.0-20260326194347-3d85c50eae71
	github.com/wso2/api-platform/httpkit v0.0.0-local
	github.com/wso2/api-platform/sdk/core v0.4.0
	go.opentelemetry.io/otel v1.44.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.44.0
	go.opentelemetry.io/otel/sdk v1.44.0
	go.opentelemetry.io/otel/trace v1.44.0
	go.opentelemetry.io/proto/otlp v1.10.0
	google.golang.org/grpc v1.82.1
	google.golang.org/protobuf v1.36.11
	gopkg.in/yaml.v3 v3.0.1
)

require (
	cel.dev/expr v0.25.1 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cncf/xds/go v0.0.0-20260202195803-dba9d589def2 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/envoyproxy/protoc-gen-validate v1.3.3 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.29.0 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/knadh/koanf/maps v0.1.2 // indirect
	github.com/mitchellh/copystructure v1.2.0 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/pelletier/go-toml/v2 v2.2.4 // indirect
	github.com/planetscale/vtprotobuf v0.6.1-0.20240319094008-0393e58bdf10 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.19.2 // indirect
	github.com/stoewer/go-strcase v1.3.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.44.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.yaml.in/yaml/v2 v2.4.3 // indirect
	golang.org/x/exp v0.0.0-20260112195511-716be5621a96 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
)

replace github.com/wso2/api-platform/common => ../../../common

replace github.com/wso2/api-platform/httpkit => ../../../httpkit

// TEMPORARY, local-checkout only: this module now uses sdk/core fields (Body on
// DownstreamRequest, AttemptNumber/AttemptNumberHeader/CurrentAttemptNumber on SharedContext)
// added to the local api-platform/sdk/core checkout but not yet published as a tagged release
// (the require above still pins the last published v0.4.0). Outside the Docker build, go.work's
// own `use` block already resolves sdk/core to this same local checkout transparently, so this
// replace is redundant there — but the Docker build (gateway-runtime/Dockerfile's `sdk-core`
// build context, copied in specifically for this replace) has no go.work influence at all, so
// without it the image silently links the stale published SDK and fails with "undefined:
// policy.AttemptNumberHeader" etc. Remove this replace once a tagged sdk/core release carrying
// those additions is available and the require above is bumped to it.
replace github.com/wso2/api-platform/sdk/core => ../../../sdk/core

replace github.com/wso2/gateway-controllers/policies/model-failover => ../../dev-policies/model-failover

replace github.com/wso2/gateway-controllers/policies/openai-to-anthropic-transformer => ../../dev-policies/openai-to-anthropic-transformer

replace github.com/wso2/gateway-controllers/policies/openai-to-azure-openai-transformer => ../../dev-policies/openai-to-azure-openai-transformer

replace github.com/wso2/gateway-controllers/policies/openai-to-bedrock-transformer => ../../dev-policies/openai-to-bedrock-transformer

replace github.com/wso2/gateway-controllers/policies/openai-to-gemini-transformer => ../../dev-policies/openai-to-gemini-transformer

replace github.com/wso2/gateway-controllers/policies/openai-to-mistral-transformer => ../../dev-policies/openai-to-mistral-transformer

replace github.com/wso2/api-platform/gateway/system-policies/analytics => ../../system-policies/analytics
