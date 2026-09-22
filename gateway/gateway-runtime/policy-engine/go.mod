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
	github.com/stretchr/testify v1.12.1
	github.com/wso2/api-platform/common v0.0.0-20260326194347-3d85c50eae71
	github.com/wso2/api-platform/gateway/system-policies/analytics v0.0.0-00010101000000-000000000000
	github.com/wso2/api-platform/httpkit v0.0.0-local
	github.com/wso2/api-platform/sdk/core v0.4.0
	github.com/wso2/gateway-controllers/policies/advanced-ratelimit v1.2.0
	github.com/wso2/gateway-controllers/policies/analytics-header-filter v1.0.1
	github.com/wso2/gateway-controllers/policies/api-key-auth v1.2.1
	github.com/wso2/gateway-controllers/policies/aws-authentication v0.10.0
	github.com/wso2/gateway-controllers/policies/aws-bedrock-guardrail v1.2.0
	github.com/wso2/gateway-controllers/policies/azure-content-safety-content-moderation v1.0.2
	github.com/wso2/gateway-controllers/policies/backend-jwt v1.0.1
	github.com/wso2/gateway-controllers/policies/basic-auth v1.0.3
	github.com/wso2/gateway-controllers/policies/basic-ratelimit v1.1.1
	github.com/wso2/gateway-controllers/policies/content-length-guardrail v1.0.1
	github.com/wso2/gateway-controllers/policies/cors v1.0.2
	github.com/wso2/gateway-controllers/policies/dynamic-endpoint v1.0.1
	github.com/wso2/gateway-controllers/policies/host-rewrite v1.0.1
	github.com/wso2/gateway-controllers/policies/interceptor-service v1.0.1
	github.com/wso2/gateway-controllers/policies/json-schema-guardrail v1.0.1
	github.com/wso2/gateway-controllers/policies/json-xml-mediator v1.0.4
	github.com/wso2/gateway-controllers/policies/jwt-auth v1.3.1
	github.com/wso2/gateway-controllers/policies/llm-cost v1.1.0
	github.com/wso2/gateway-controllers/policies/llm-cost-based-ratelimit v1.1.1
	github.com/wso2/gateway-controllers/policies/llm-header-router v0.9.0
	github.com/wso2/gateway-controllers/policies/log-message v1.0.3
	github.com/wso2/gateway-controllers/policies/mcp-acl-list v1.0.3
	github.com/wso2/gateway-controllers/policies/mcp-auth v1.3.0
	github.com/wso2/gateway-controllers/policies/mcp-authz v1.2.0
	github.com/wso2/gateway-controllers/policies/mcp-ratelimit v1.1.2
	github.com/wso2/gateway-controllers/policies/mcp-rewrite v1.0.3
	github.com/wso2/gateway-controllers/policies/model-failover v0.0.0-00010101000000-000000000000
	github.com/wso2/gateway-controllers/policies/model-round-robin v1.1.1
	github.com/wso2/gateway-controllers/policies/model-weighted-round-robin v1.1.1
	github.com/wso2/gateway-controllers/policies/oauth2-generator v0.9.0
	github.com/wso2/gateway-controllers/policies/opaque-token-auth v1.0.1
	github.com/wso2/gateway-controllers/policies/openai-to-anthropic-transformer v0.9.1
	github.com/wso2/gateway-controllers/policies/openai-to-azure-openai-transformer v0.9.0
	github.com/wso2/gateway-controllers/policies/openai-to-bedrock-transformer v0.9.1
	github.com/wso2/gateway-controllers/policies/openai-to-gemini-transformer v0.9.1
	github.com/wso2/gateway-controllers/policies/openai-to-mistral-transformer v0.9.0
	github.com/wso2/gateway-controllers/policies/pii-masking-regex v1.0.4
	github.com/wso2/gateway-controllers/policies/prompt-decorator v1.0.2
	github.com/wso2/gateway-controllers/policies/prompt-template v1.0.1
	github.com/wso2/gateway-controllers/policies/redirect v0.9.1
	github.com/wso2/gateway-controllers/policies/regex-guardrail v1.0.1
	github.com/wso2/gateway-controllers/policies/remove-headers v1.0.1
	github.com/wso2/gateway-controllers/policies/request-rewrite v1.0.2
	github.com/wso2/gateway-controllers/policies/respond v1.0.1
	github.com/wso2/gateway-controllers/policies/semantic-cache v1.1.0
	github.com/wso2/gateway-controllers/policies/semantic-prompt-guard v1.0.1
	github.com/wso2/gateway-controllers/policies/semantic-tool-filtering v1.0.2
	github.com/wso2/gateway-controllers/policies/sentence-count-guardrail v1.0.2
	github.com/wso2/gateway-controllers/policies/set-headers v1.1.0
	github.com/wso2/gateway-controllers/policies/subscription-validation v1.0.3
	github.com/wso2/gateway-controllers/policies/token-based-ratelimit v1.1.1
	github.com/wso2/gateway-controllers/policies/url-guardrail v1.0.1
	github.com/wso2/gateway-controllers/policies/word-count-guardrail v1.0.2
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
	github.com/aws/aws-sdk-go-v2 v1.42.1 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.6.3 // indirect
	github.com/aws/aws-sdk-go-v2/config v1.32.29 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.19.28 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.30 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.30 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.30 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.31 // indirect
	github.com/aws/aws-sdk-go-v2/service/bedrockruntime v1.13.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.30 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.4.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.32.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.37.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.44.0 // indirect
	github.com/aws/smithy-go v1.27.3 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/blang/semver/v4 v4.0.0 // indirect
	github.com/cenkalti/backoff/v4 v4.3.0 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cilium/ebpf v0.16.0 // indirect
	github.com/cncf/xds/go v0.0.0-20260202195803-dba9d589def2 // indirect
	github.com/cockroachdb/errors v1.9.1 // indirect
	github.com/cockroachdb/logtags v0.0.0-20211118104740-dabe8e521a4f // indirect
	github.com/cockroachdb/redact v1.1.3 // indirect
	github.com/containerd/cgroups/v3 v3.0.5 // indirect
	github.com/containerd/log v0.1.0 // indirect
	github.com/coreos/go-semver v0.3.0 // indirect
	github.com/coreos/go-systemd/v22 v22.5.0 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/envoyproxy/protoc-gen-validate v1.3.3 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/fxamacker/cbor/v2 v2.7.0 // indirect
	github.com/getsentry/sentry-go v0.12.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-ole/go-ole v1.2.6 // indirect
	github.com/goccy/go-json v0.10.5 // indirect
	github.com/godbus/dbus/v5 v5.1.0 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang-jwt/jwt/v4 v4.5.2 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/google/btree v1.1.3 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/grpc-ecosystem/go-grpc-middleware v1.3.0 // indirect
	github.com/grpc-ecosystem/go-grpc-prometheus v1.2.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway v1.16.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.29.0 // indirect
	github.com/jonboulle/clockwork v0.5.0 // indirect
	github.com/json-iterator/go v1.1.13-0.20220915233716-71ac16282d12 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/knadh/koanf/maps v0.1.2 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/lufia/plan9stats v0.0.0-20211012122336-39d0f177ccd0 // indirect
	github.com/milvus-io/milvus-proto/go-api/v2 v2.6.8 // indirect
	github.com/milvus-io/milvus/client/v2 v2.6.2 // indirect
	github.com/milvus-io/milvus/pkg/v2 v2.6.8 // indirect
	github.com/mitchellh/copystructure v1.2.0 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/moby/sys/userns v0.1.0 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/opencontainers/runtime-spec v1.2.1 // indirect
	github.com/panjf2000/ants/v2 v2.11.3 // indirect
	github.com/pelletier/go-toml/v2 v2.2.4 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/planetscale/vtprotobuf v0.6.1-0.20240319094008-0393e58bdf10 // indirect
	github.com/power-devops/perfstat v0.0.0-20210106213030-5aafc221ea8c // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.19.2 // indirect
	github.com/redis/go-redis/v9 v9.22.0 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	github.com/samber/lo v1.27.0 // indirect
	github.com/shirou/gopsutil/v3 v3.23.12 // indirect
	github.com/shoenig/go-m1cpu v0.1.6 // indirect
	github.com/sirupsen/logrus v1.9.3 // indirect
	github.com/soheilhy/cmux v0.1.5 // indirect
	github.com/spaolacci/murmur3 v1.1.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/stoewer/go-strcase v1.3.1 // indirect
	github.com/tidwall/gjson v1.17.1 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.0 // indirect
	github.com/tklauser/go-sysconf v0.3.12 // indirect
	github.com/tklauser/numcpus v0.6.1 // indirect
	github.com/tmc/grpc-websocket-proxy v0.0.0-20201229170055-e5319fda7802 // indirect
	github.com/twpayne/go-geom v1.6.1 // indirect
	github.com/uber/jaeger-client-go v2.30.0+incompatible // indirect
	github.com/wso2/api-platform/sdk/ai v0.1.2 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	github.com/xeipuuv/gojsonpointer v0.0.0-20180127040702-4e3ac2762d5f // indirect
	github.com/xeipuuv/gojsonreference v0.0.0-20180127040603-bd5ef7bd5415 // indirect
	github.com/xeipuuv/gojsonschema v1.2.0 // indirect
	github.com/xiang90/probing v0.0.0-20190116061207-43a291ad63a2 // indirect
	github.com/yusufpapurcu/wmi v1.2.4 // indirect
	go.etcd.io/bbolt v1.4.3 // indirect
	go.etcd.io/etcd/api/v3 v3.5.23 // indirect
	go.etcd.io/etcd/client/pkg/v3 v3.5.23 // indirect
	go.etcd.io/etcd/client/v2 v2.305.23 // indirect
	go.etcd.io/etcd/client/v3 v3.5.23 // indirect
	go.etcd.io/etcd/pkg/v3 v3.5.23 // indirect
	go.etcd.io/etcd/raft/v3 v3.5.23 // indirect
	go.etcd.io/etcd/server/v3 v3.5.23 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.60.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.44.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.uber.org/automaxprocs v1.5.3 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.27.1 // indirect
	go.yaml.in/yaml/v2 v2.4.3 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/exp v0.0.0-20260112195511-716be5621a96 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	golang.org/x/time v0.14.0 // indirect
	google.golang.org/genproto v0.0.0-20250303144028-a0af3efb3deb // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.2.1 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	k8s.io/apimachinery v0.32.3 // indirect
	sigs.k8s.io/yaml v1.4.0 // indirect
)

replace github.com/wso2/api-platform/common => ../../../common

replace github.com/wso2/api-platform/httpkit => ../../../httpkit

// TEMPORARY, for local Docker build verification of the unreleased
// UpstreamRequestContext.RouteCluster and UpstreamResponseContext.RouteCluster
// fields (Task 1 of the model-failover policy implementation). Must be removed
// once sdk/core is tagged with these fields and the require above is bumped to
// that version — do not merge with this replace in place.
replace github.com/wso2/api-platform/sdk/core => ../../../sdk/core

replace github.com/wso2/gateway-controllers/policies/model-failover => ../../dev-policies/model-failover

replace github.com/wso2/api-platform/gateway/system-policies/analytics => ../../system-policies/analytics
